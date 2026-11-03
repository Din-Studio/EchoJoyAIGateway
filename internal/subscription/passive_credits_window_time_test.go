package subscription

import (
	"encoding/json"
	"testing"
	"time"

	"gpt-load/internal/health"
	"gpt-load/internal/storage/models"
	providerobservation "gpt-load/internal/subscription/providers/observation"
)

func TestPassiveCreditsDoNotDiscardWindowEvidence(t *testing.T) {
	for _, order := range []string{"window-credit", "credit-window", "peer-credit-first", "handshake-credit"} {
		t.Run(order, func(t *testing.T) {
			manager, db, registry, crypt, credential := newCredentialManagerFixture(t, credentialJSON("access", "refresh", time.Now().Add(time.Hour)))
			newFlushableCredentialObservation(t, manager, credential.ID, models.CredentialObservationFresh,
				`{"plan_summary":{},"quota_windows":[{"id":"primary","scope":"account","state":"available","utilization":0.1,"reset_at_ms":1800000000000}],"credits":{"balance":"100","observed_at_ms":1000}}`)
			ref, _ := registry.CredentialRef(credential.ID)
			windows := []providerobservation.QuotaWindow{{ID: "primary", State: "exhausted", Utilization: floatPointer(1)}}
			at := int64(3000)
			credit := &providerobservation.CreditSummary{Balance: "50", ObservedAtMS: &at}
			recordWindow := func() {
				manager.RecordPassiveQuotaObservation(credential.ID, ref.IdentityGeneration, 2000, windows, nil)
			}
			recordCredit := func(m *CredentialManager) {
				m.RecordPassiveQuotaObservation(credential.ID, ref.IdentityGeneration, at, nil, credit)
			}
			flush := func(m *CredentialManager) {
				t.Helper()
				if remaining, err := m.FlushPassiveQuotaObservations(t.Context()); err != nil || remaining {
					t.Fatalf("flush: remaining=%v err=%v", remaining, err)
				}
			}
			switch order {
			case "window-credit":
				recordWindow()
				recordCredit(manager)
			case "credit-window":
				recordCredit(manager)
				recordWindow()
			case "peer-credit-first":
				recordWindow()
				peer := NewCredentialManager(db, crypt, registry, health.NewMutationCoordinator(), manager.runtime, manager.lease, manager.health)
				recordCredit(peer)
				flush(peer)
			case "handshake-credit":
				manager.RecordPassiveQuotaPair(credential.ID, ref.IdentityGeneration,
					PassiveQuotaSample{ObservedAtMS: 2000, Windows: windows}, PassiveQuotaSample{ObservedAtMS: 3000, Credits: credit})
			}
			flush(manager)
			var row models.CredentialObservation
			if err := db.Take(&row, "credential_id = ?", credential.ID).Error; err != nil {
				t.Fatal(err)
			}
			var snapshot struct {
				providerobservation.Snapshot
				WindowAt int64 `json:"quota_observed_at_ms"`
			}
			if err := json.Unmarshal(row.SnapshotJSON, &snapshot); err != nil {
				t.Fatal(err)
			}
			if *row.ObservedAtMS != 3000 || snapshot.WindowAt != 2000 || snapshot.Credits.Balance != "50" || *snapshot.Credits.ObservedAtMS != 3000 || snapshot.QuotaWindows[0].State != "exhausted" {
				t.Fatalf("independent window/credit evidence lost: time=%v snapshot=%s", row.ObservedAtMS, row.SnapshotJSON)
			}
			views := registry.Snapshot()
			if len(views) != 1 || views[0].ObservedQuotaRemaining() == nil || *views[0].ObservedQuotaRemaining() != 0 {
				t.Fatalf("stale remaining ratio: %+v", views)
			}
		})
	}
}

func TestPassiveCreditsEventSupersedesUncommittedHandshakeAtSameMillisecond(t *testing.T) {
	for _, storedAt := range []int64{1000, 2000} {
		at := int64(2000)
		manager := testCredentialManagerForPassiveQuota(t)
		manager.RecordPassiveQuotaPair(7, 100,
			PassiveQuotaSample{ObservedAtMS: at, Credits: &providerobservation.CreditSummary{Balance: "55", ObservedAtMS: &at}},
			PassiveQuotaSample{ObservedAtMS: at, Windows: []providerobservation.QuotaWindow{{ID: "primary"}}, Credits: &providerobservation.CreditSummary{Balance: "0", ObservedAtMS: &at}},
		)
		pending := manager.DirtyPassiveQuotaObservations(1)
		raw := []byte(`{"plan_summary":{},"quota_windows":[],"credits":{"balance":"100"}}`)
		merged, _, err := mergePassiveQuotaSamples(raw, &storedAt, pending[0])
		if err != nil {
			t.Fatal(err)
		}
		var snapshot providerobservation.Snapshot
		if err := json.Unmarshal(merged.Encoded, &snapshot); err != nil {
			t.Fatal(err)
		}
		want := "0"
		if storedAt == at {
			want = "100"
		}
		if snapshot.Credits.Balance != want {
			t.Fatalf("storedAt=%d balance=%s want=%s", storedAt, snapshot.Credits.Balance, want)
		}
	}
}

func TestPassiveCreditsWindowClockPreservesLegacyAndActiveRefresh(t *testing.T) {
	for _, marker := range []string{"", `,"quota_observed_at_ms":4000`} {
		raw := []byte(`{"plan_summary":{},"quota_windows":[{"id":"primary","state":"available"}],"credits":{"observed_at_ms":4000}` + marker + `}`)
		storedAt := int64(4000)
		at := int64(3000)
		merged, total, err := mergePassiveQuotaSamples(raw, &storedAt, PassiveQuotaObservation{
			ObservedAtMS: at, Windows: []providerobservation.QuotaWindow{{ID: "primary", State: "exhausted"}},
			Credits: &providerobservation.CreditSummary{Balance: "10", ObservedAtMS: &at},
		})
		if err != nil || merged.Matched || string(merged.Encoded) != string(raw) || total != storedAt {
			t.Fatalf("old evidence passed refresh: %s total=%d err=%v", merged.Encoded, total, err)
		}
	}
}
