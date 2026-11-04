package control

import (
	"bytes"
	"context"
	"encoding/json"

	"gpt-load/internal/storage/models"
	"testing"
	"time"

	"gpt-load/internal/subscription/providers/codex"
)

func TestRefreshCredentialCreditsStoresTimeZeroAndMissing(t *testing.T) {
	fixture, groupID, credentialID := newSubscriptionCredentialFixture(t)
	now := time.UnixMilli(1_800_000_000_000)
	fixture.service.now = func() time.Time { return now }
	for _, test := range []struct {
		payload string
		balance string
	}{
		{`{"credits":{"balance":"62500","has_credits":true,"unlimited":false}}`, "62500"},
		{`{"credits":{"balance":"0","has_credits":false,"unlimited":false}}`, "0"},
		{`{"plan_type":"pro"}`, ""},
	} {
		setCodexAccountObservation(fixture.service, func(context.Context, codex.Credential) (codex.AccountObservation, error) {
			return codex.AccountObservation{Payload: []byte(test.payload)}, nil
		})
		result, err := fixture.service.RefreshCredentialObservation(t.Context(), groupID, credentialID)
		if err != nil || result.Snapshot == nil {
			t.Fatalf("refresh failed: %#v, %v", result, err)
		}
		if test.balance == "" {
			if credits := result.Snapshot.Credits; credits == nil || credits.Balance != "" ||
				credits.HasCredits != nil || credits.Unlimited != nil ||
				credits.ObservedAtMS == nil || *credits.ObservedAtMS != now.UnixMilli() {
				t.Fatal("successful refresh without credit data retained the old balance")
			}
		} else if credits := result.Snapshot.Credits; credits == nil || credits.Balance != test.balance || credits.ObservedAtMS == nil || *credits.ObservedAtMS != now.UnixMilli() {
			t.Fatalf("credit observation time was not persisted: %#v", result.Snapshot.Credits)
		}
		var stored models.CredentialObservation
		if err := fixture.db.Take(&stored, "credential_id = ?", credentialID).Error; err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(stored.SnapshotJSON, []byte("quota_observed_at_ms")) {
			t.Fatal("active refresh retained the previous window clock")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(stored.SnapshotJSON, &fields); err != nil {
			t.Fatal(err)
		}
		fields["quota_observed_at_ms"] = json.RawMessage(`1000`)
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.db.Model(&models.CredentialObservation{}).Where("credential_id = ?", credentialID).
			Update("snapshot_json", models.JSON(encoded)).Error; err != nil {
			t.Fatal(err)
		}
		response, err := fixture.service.GetCredentialObservation(t.Context(), groupID, credentialID)
		if err != nil {
			t.Fatal(err)
		}
		public, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(public, []byte("quota_observed_at_ms")) || !bytes.Contains(public, []byte(`"credits"`)) {
			t.Fatalf("unexpected public credit snapshot: %s", public)
		}
		now = now.Add(time.Second)
	}
}
