package control

import (
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/storage/models"
)

func TestResetCreditAdmissionSerializesReplicas(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_key_%t", sameKey), func(t *testing.T) {
			fixture, groupID, credentialID := newClaudeResetCreditFixture(t)
			var credential models.Credential
			if err := fixture.db.Take(&credential, credentialID).Error; err != nil {
				t.Fatal(err)
			}
			digest := resetCreditRequestDigest(groupID, credentialID, credential.IdentityFingerprint, "")
			// Hold the shared row lock before all three independent services start.
			// Removing admission's row lock makes the early-result assertion fail.
			tx := fixture.db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			if err := tx.Exec("SELECT id FROM credentials WHERE id = ? FOR UPDATE", credentialID).Error; err != nil {
				t.Fatal(err)
			}
			results := make(chan error, 3)
			for i := range 3 {
				service := &Service{db: fixture.db, now: time.Now}
				key := fmt.Sprintf("9f0f4c32-89d2-4bcb-9e19-%012d", i+1)
				if sameKey {
					key = resetCreditTestKey
				}
				go func() {
					_, _, err := service.beginResetCreditOperation(t.Context(), groupID, credentialID, digest, key)
					results <- err
				}()
			}
			select {
			case err := <-results:
				t.Fatalf("admission bypassed the shared row lock: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if err := tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			winners := 0
			for range 3 {
				select {
				case err := <-results:
					if err == nil {
						winners++
					} else {
						assertAPIErrorCode(t, err, app_errors.ErrResetCreditOutcomeUnknown.Code)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("replica admission did not finish")
				}
			}
			var count int64
			if err := fixture.db.Model(&models.CredentialResetOperation{}).Where("credential_id = ?", credentialID).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if winners != 1 || count != 1 {
				t.Fatalf("winners/operations = %d/%d, want 1/1", winners, count)
			}
		})
	}
}

func TestResetCreditRetryReadsCommittedSelection(t *testing.T) {
	fixture, groupID, credentialID := newClaudeResetCreditFixture(t)
	var credential models.Credential
	if err := fixture.db.Take(&credential, credentialID).Error; err != nil {
		t.Fatal(err)
	}
	digest := resetCreditRequestDigest(groupID, credentialID, credential.IdentityFingerprint, "")
	old := models.CredentialResetOperation{
		IdempotencyKey: resetCreditTestKey, RedeemRequestID: resetCreditTestKey,
		RequestDigest: digest[:], GroupID: groupID, CredentialID: credentialID,
		State:       models.CredentialResetOperationPrepared,
		UpdatedAtMS: time.Now().Add(-2 * defaultSubscriptionControlTimeout).UnixMilli(),
	}
	if err := fixture.db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	// Replica A is saving its selection while replica B retries an expired claim.
	tx := fixture.db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("SELECT idempotency_key FROM credential_reset_operations WHERE idempotency_key = ? FOR UPDATE", resetCreditTestKey).Error; err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}, 1), make(chan struct{})
	if err := fixture.db.Callback().Query().After("gorm:query").Register("test:reset_selection", func(db *gorm.DB) {
		if db.Statement.Table == "credential_reset_operations" {
			read <- struct{}{}
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer fixture.db.Callback().Query().Remove("test:reset_selection")
	type outcome struct {
		operation models.CredentialResetOperation
		err       error
	}
	result := make(chan outcome, 1)
	go func() {
		operation, _, err := fixture.service.beginResetCreditOperation(t.Context(), groupID, credentialID, digest, resetCreditTestKey)
		result <- outcome{operation, err}
	}()
	earlyRead := false
	select {
	case <-read:
		earlyRead = true
	case <-time.After(100 * time.Millisecond):
	}
	selection := models.JSON(`{"request":{"grant_id":"original","organization_id":"org-one"}}`)
	saveErr := tx.Model(&models.CredentialResetOperation{}).Where("idempotency_key = ?", resetCreditTestKey).
		Updates(map[string]any{"result_json": selection, "updated_at_ms": old.UpdatedAtMS}).Error
	commitErr := tx.Commit().Error
	close(release)
	if saveErr != nil || commitErr != nil {
		t.Fatalf("save/commit: %v/%v", saveErr, commitErr)
	}
	select {
	case got := <-result:
		if earlyRead || got.err != nil || string(got.operation.ResultJSON) != string(selection) {
			t.Fatalf("retry read stale selection: early=%t operation=%#v err=%v", earlyRead, got.operation, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not finish")
	}
}

func TestResetCreditSameMillisecondRetryFencesOldAttempt(t *testing.T) {
	fixture, groupID, credentialID := newClaudeResetCreditFixture(t)
	now := time.Now()
	fixture.service.now = func() time.Time { return now }
	var credential models.Credential
	if err := fixture.db.Take(&credential, credentialID).Error; err != nil {
		t.Fatal(err)
	}
	digest := resetCreditRequestDigest(groupID, credentialID, credential.IdentityFingerprint, "")
	old, _, err := fixture.service.beginResetCreditOperation(t.Context(), groupID, credentialID, digest, resetCreditTestKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.finishResetCreditOperation(old, models.CredentialResetOperationOutcomeUnknown, []byte(`{"request":{"grant_id":"one"}}`), app_errors.ErrResetCreditOutcomeUnknown.Code); err != nil {
		t.Fatal(err)
	}
	current, _, err := fixture.service.beginResetCreditOperation(t.Context(), groupID, credentialID, digest, resetCreditTestKey)
	if err != nil {
		t.Fatal(err)
	}
	if current.UpdatedAtMS <= old.UpdatedAtMS || string(current.ResultJSON) != `{"request":{"grant_id":"one"}}` {
		t.Fatalf("retry lost fencing or selection: %#v", current)
	}
	if err := fixture.service.finishResetCreditOperation(old, models.CredentialResetOperationRejected, nil, "late"); err == nil {
		t.Fatal("old attempt overwrote the new claim")
	}
	if err := fixture.service.finishResetCreditOperation(current, models.CredentialResetOperationOutcomeUnknown, nil, app_errors.ErrResetCreditOutcomeUnknown.Code); err != nil {
		t.Fatal(err)
	}
	// A newer successful record must not hide an unresolved operation.
	newer := current
	newer.IdempotencyKey = "9f0f4c32-89d2-4bcb-9e19-052940dc2f17"
	newer.RedeemRequestID = newer.IdempotencyKey
	newer.State = models.CredentialResetOperationSucceeded
	newer.UpdatedAtMS += 100
	if err := fixture.db.Create(&newer).Error; err != nil {
		t.Fatal(err)
	}
	_, _, err = fixture.service.beginResetCreditOperation(t.Context(), groupID, credentialID, digest, "9f0f4c32-89d2-4bcb-9e19-052940dc2f18")
	assertAPIErrorCode(t, err, app_errors.ErrResetCreditOutcomeUnknown.Code)
}
