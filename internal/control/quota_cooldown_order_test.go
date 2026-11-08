package control

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gpt-load/internal/storage/models"
	"gpt-load/internal/subscription/providers/codex"
)

func TestActiveQuotaRecoveryOrdersPersistedWindowAndCreditClocks(t *testing.T) {
	for _, test := range []struct {
		name         string
		windowOffset time.Duration
		wantRecovery bool
	}{
		{"newer exhausted window", 2 * time.Second, false},
		{"same millisecond exhausted window", time.Second, false},
		{"later credits with older window", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, groupID, id := newSubscriptionCredentialFixture(t)
			start := time.Now().UTC().Truncate(time.Millisecond)
			now := start
			fixture.service.now = func() time.Time { return now }
			used := 100
			setCodexAccountObservation(fixture.service, func(context.Context, codex.Credential) (codex.AccountObservation, error) {
				payload, _ := json.Marshal(map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{
					"used_percent": used, "limit_window_seconds": 604800, "reset_at": start.Add(7 * 24 * time.Hour).Unix(),
				}}})
				return codex.AccountObservation{Payload: payload}, nil
			})
			if _, err := fixture.service.RefreshCredentialObservation(t.Context(), groupID, id); err != nil {
				t.Fatal(err)
			}
			ref, _ := fixture.registry.CredentialRef(id)
			setQuotaCooldown(t, fixture.sharedHealth, ref, "", start.Add(time.Hour), start)
			setQuotaCooldown(t, fixture.sharedHealth, ref, "model", start.Add(time.Hour), start)
			now, used = start.Add(time.Second), 0
			coordinator := fixture.service.mutations
			fixture.service.mutations = quotaObservationMutationFunc(func(credentialID uint, finish func()) {
				// Simulate another replica committing after this response returned,
				// but before this replica acquires the local mutation stripe.
				var row models.CredentialObservation
				if err := fixture.db.Take(&row, "credential_id = ?", id).Error; err != nil {
					t.Fatal(err)
				}
				var snapshot map[string]any
				if err := json.Unmarshal(row.SnapshotJSON, &snapshot); err != nil {
					t.Fatal(err)
				}
				latestMS := start.Add(3 * time.Second).UnixMilli()
				snapshot["quota_observed_at_ms"] = start.Add(test.windowOffset).UnixMilli()
				snapshot["credits"] = map[string]any{"balance": "40", "observed_at_ms": latestMS}
				encoded, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if err := fixture.db.Model(&models.CredentialObservation{}).Where("credential_id = ?", id).Updates(map[string]any{
					"snapshot_json": models.JSON(encoded), "observed_at_ms": latestMS, "observation_version": row.ObservationVersion + 1, "updated_at_ms": latestMS,
				}).Error; err != nil {
					t.Fatal(err)
				}
				coordinator.Do(credentialID, finish)
			})
			response, err := fixture.service.RefreshCredentialObservation(t.Context(), groupID, id)
			if err != nil {
				t.Fatal(err)
			}
			view := fixture.registry.Snapshot()[0]
			if view.CooldownUntil.IsZero() != test.wantRecovery || view.ModelCooldowns["model"].IsZero() != test.wantRecovery {
				t.Fatal("wrong recovery after out-of-order response")
			}
			var stored models.CredentialObservation
			if err := fixture.db.Take(&stored, "credential_id = ?", id).Error; err != nil {
				t.Fatal(err)
			}
			latestMS := start.Add(3 * time.Second).UnixMilli()
			if stored.ObservedAtMS == nil || *stored.ObservedAtMS != latestMS || response.ObservedAtMS == nil || *response.ObservedAtMS != latestMS {
				t.Fatal("total observation clock regressed")
			}
			var snapshot struct {
				CredentialObservationSnapshot
				QuotaObservedAtMS int64 `json:"quota_observed_at_ms"`
			}
			if err := json.Unmarshal(stored.SnapshotJSON, &snapshot); err != nil {
				t.Fatal(err)
			}
			wantUsed, wantWindowMS := 100.0, start.Add(test.windowOffset).UnixMilli()
			if test.wantRecovery {
				wantUsed, wantWindowMS = 0, now.UnixMilli()
			}
			if len(snapshot.QuotaWindows) != 1 || snapshot.QuotaWindows[0].Used == nil || *snapshot.QuotaWindows[0].Used != wantUsed || snapshot.QuotaObservedAtMS != wantWindowMS {
				t.Fatal("quota window or its clock overwritten by wrong sample")
			}
			if snapshot.Credits == nil || snapshot.Credits.Balance != "40" || snapshot.Credits.ObservedAtMS == nil || *snapshot.Credits.ObservedAtMS != latestMS {
				t.Fatal("newer credits overwritten")
			}
		})
	}
}
