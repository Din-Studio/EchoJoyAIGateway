package subscription

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gpt-load/internal/storage/models"
	providerobservation "gpt-load/internal/subscription/providers/observation"
)

// Force a second replica's committed write between the read and CAS. Both
// writes share a millisecond and the active observation version is unchanged.
func TestPassiveCreditsCASDetectsPeerWriteAtSameMillisecond(t *testing.T) {
	for _, scenario := range []string{"credit", "window-time"} {
		t.Run(scenario, func(t *testing.T) {
			manager, db, registry, _, credential := newCredentialManagerFixture(t, credentialJSON("access", "refresh", time.Now().Add(time.Hour)))
			original := newFlushableCredentialObservation(t, manager, credential.ID, models.CredentialObservationFresh,
				`{"plan_summary":{},"quota_windows":[{"id":"primary","used":5}],"credits":{"balance":"100","observed_at_ms":1000}}`)
			manager.now = func() time.Time { return time.UnixMilli(original.UpdatedAtMS) }
			ref, _ := registry.CredentialRef(credential.ID)
			at := int64(2000)
			manager.RecordPassiveQuotaObservation(credential.ID, ref.IdentityGeneration, at,
				[]providerobservation.QuotaWindow{{ID: "primary", Used: floatPointer(10)}},
				&providerobservation.CreditSummary{Balance: "50", ObservedAtMS: &at})
			var once sync.Once
			if err := db.Callback().Query().After("gorm:query").Register("test:peer_credit_write", func(tx *gorm.DB) {
				if tx.Statement.Table != "credential_observations" {
					return
				}
				once.Do(func() {
					updates := map[string]any{"observed_at_ms": int64(3000)}
					if scenario == "credit" {
						updates["observed_at_ms"] = original.ObservedAtMS
						updates["snapshot_json"] = models.JSON(`{"plan_summary":{},"quota_windows":[{"id":"primary","used":5}],"credits":{"balance":"0","observed_at_ms":3000}}`)
					}
					if err := db.Model(&models.CredentialObservation{}).Where("credential_id = ?", credential.ID).Updates(updates).Error; err != nil {
						t.Error(err)
					}
				})
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Callback().Query().Remove("test:peer_credit_write") })
			if _, err := manager.FlushPassiveQuotaObservations(t.Context()); err != nil {
				t.Fatal(err)
			}
			var row models.CredentialObservation
			if err := db.Take(&row, "credential_id = ?", credential.ID).Error; err != nil {
				t.Fatal(err)
			}
			var snapshot providerobservation.Snapshot
			if err := json.Unmarshal(row.SnapshotJSON, &snapshot); err != nil {
				t.Fatal(err)
			}
			if scenario == "credit" {
				if snapshot.Credits.Balance != "0" || *snapshot.Credits.ObservedAtMS != 3000 {
					t.Fatalf("peer credit overwritten: %s", row.SnapshotJSON)
				}
			} else if *row.ObservedAtMS != 3000 || *snapshot.QuotaWindows[0].Used != 5 || snapshot.Credits.Balance != "50" {
				t.Fatalf("peer window/time overwritten or independent credit lost: at=%d snapshot=%s", *row.ObservedAtMS, row.SnapshotJSON)
			}
		})
	}
}
