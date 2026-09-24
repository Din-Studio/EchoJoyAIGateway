package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
)

// refreshLeaseProbe reports which subscription credentials have a live
// refresh lease. It is nil in single-instance mode.
type refreshLeaseProbe interface {
	Held(ctx context.Context, credentialIDs []uint) (map[uint]bool, error)
}

var errNoInterruptedRefresh = errors.New("no interrupted refresh")

// sweepInterruptedRefreshes marks refreshes whose holder is gone as
// outcome_unknown. In cluster mode a refresh is interrupted exactly when its
// row is refreshing and no instance holds the credential's refresh lease;
// the row's age is never consulted, so a slow refresh is never cut short.
// A holder judged interrupted by mistake still commits its token, because the
// secret commit is conditioned on the secret version alone.
func (s *Service) sweepInterruptedRefreshes(ctx context.Context) error {
	if s.refreshLeases == nil {
		return nil
	}
	var refreshing []models.Credential
	if err := s.db.WithContext(ctx).Model(&models.Credential{}).
		Select("id", "group_id", "secret_version", "identity_fingerprint").
		Where("auth_state = ?", models.CredentialAuthStateRefreshing).
		Order("id ASC").Find(&refreshing).Error; err != nil {
		return app_errors.ParseDBError(err)
	}
	if len(refreshing) == 0 {
		return nil
	}
	ids := make([]uint, 0, len(refreshing))
	for _, row := range refreshing {
		ids = append(ids, row.ID)
	}
	held, err := s.refreshLeases.Held(ctx, ids)
	if err != nil {
		return fmt.Errorf("sweep interrupted refreshes: %w", err)
	}
	orphaned := refreshing[:0]
	for _, row := range refreshing {
		if !held[row.ID] {
			orphaned = append(orphaned, row)
		}
	}
	if len(orphaned) == 0 {
		return nil
	}

	var swept []models.Credential
	err = s.withControlTransaction(ctx, func(tx *gorm.DB) error {
		swept = swept[:0]
		nowMS := s.now().UnixMilli()
		for _, row := range orphaned {
			result := tx.Model(&models.Credential{}).
				Where("id = ? AND auth_state = ? AND secret_version = ?",
					row.ID, models.CredentialAuthStateRefreshing, row.SecretVersion).
				Updates(map[string]any{
					"auth_state":      models.CredentialAuthStateOutcomeUnknown,
					"auth_error_code": "refresh_interrupted", "updated_at_ms": nowMS,
				})
			if result.Error != nil {
				return app_errors.ParseDBError(result.Error)
			}
			if result.RowsAffected == 1 {
				swept = append(swept, row)
			}
		}
		if len(swept) == 0 {
			// Every row finished meanwhile; roll back so the revision stays.
			return errNoInterruptedRefresh
		}
		return nil
	})
	if errors.Is(err, errNoInterruptedRefresh) {
		return nil
	}
	if err != nil {
		return err
	}
	s.publishSweptAuthStates(ctx, swept)
	logrus.WithFields(logrus.Fields{
		"event": "subscription.refresh_interrupted_swept", "count": len(swept),
	}).Info("interrupted subscription refreshes marked outcome_unknown")
	return nil
}

// publishSweptAuthStates writes the committed outcome_unknown to the shared
// auth state. A failed write is repaired by republishSubscriptionAuth on the
// next poll; meanwhile every instance still excludes the credential.
func (s *Service) publishSweptAuthStates(ctx context.Context, swept []models.Credential) {
	if s.sharedHealth == nil {
		return
	}
	groups := make(map[uint]models.Group)
	for _, row := range swept {
		ref, err := s.subscriptionAuthRef(ctx, row, groups)
		if err == nil {
			_, err = s.sharedHealth.SetAuthState(ctx, ref, state.CredentialAuthStateOutcomeUnknown,
				groupCollectionCredentialVersion(row.SecretVersion))
		}
		if err != nil {
			logSharedAuthUnavailable(row.ID, err)
		}
	}
}

// syncSubscriptionAuth aligns refresh auth state across the cluster; it
// runs at startup and on every configuration poll.
func (s *Service) syncSubscriptionAuth(ctx context.Context) error {
	return errors.Join(s.sweepInterruptedRefreshes(ctx), s.republishSubscriptionAuth(ctx))
}

// republishSubscriptionAuth restores PostgreSQL as the only source of truth
// for subscription auth state. Auth changes commit to the database first and
// are copied to Redis best effort, so a copy that failed can leave Redis with
// an older state of the same secret version, which every mirror would trust.
// Redis is read before the database and each correction is conditioned on
// the record still being the one read: any auth write after that read moves
// the record version, so a correction never overwrites a newer state.
func (s *Service) republishSubscriptionAuth(ctx context.Context) error {
	if s.sharedHealth == nil {
		return nil
	}
	var groupIDs []uint
	if err := s.db.WithContext(ctx).Model(&models.Group{}).
		Where("connection_type = ?", models.ConnectionTypeSubscription).
		Pluck("id", &groupIDs).Error; err != nil {
		return app_errors.ParseDBError(err)
	}
	if len(groupIDs) == 0 {
		return nil
	}
	var ids []uint
	if err := s.db.WithContext(ctx).Model(&models.Credential{}).
		Where("group_id IN ?", groupIDs).Order("id ASC").Pluck("id", &ids).Error; err != nil {
		return app_errors.ParseDBError(err)
	}
	if len(ids) == 0 {
		return nil
	}
	records, err := s.sharedHealth.ReadHealth(ctx, ids)
	if err != nil {
		return fmt.Errorf("republish subscription auth: %w", err)
	}
	var rows []models.Credential
	if err := s.db.WithContext(ctx).Model(&models.Credential{}).
		Select("id", "group_id", "secret_version", "identity_fingerprint", "auth_state").
		Where("id IN ?", ids).Order("id ASC").Find(&rows).Error; err != nil {
		return app_errors.ParseDBError(err)
	}
	groups := make(map[uint]models.Group)
	republished := 0
	for _, row := range rows {
		record := records[row.ID]
		secretVersion := groupCollectionCredentialVersion(row.SecretVersion)
		committed := normalizeRuntimeCredentialAuthState(row.AuthState)
		// Records without an auth state, or of another secret version, are
		// ignored by every mirror and need no correction.
		if record.Epoch == "" || record.AuthState == "" || record.AuthSecretVersion != secretVersion ||
			record.AuthState == committed {
			continue
		}
		ref, err := s.subscriptionAuthRef(ctx, row, groups)
		if err != nil {
			logSharedAuthUnavailable(row.ID, err)
			continue
		}
		if record.IdentityGeneration != ref.IdentityGeneration {
			continue
		}
		result, err := s.sharedHealth.ReplaceAuthState(ctx, ref, committed, secretVersion, record)
		if err != nil {
			logSharedAuthUnavailable(row.ID, err)
			continue
		}
		if result.Accepted {
			republished++
		}
	}
	if republished > 0 {
		logrus.WithFields(logrus.Fields{
			"event": "subscription.auth_state_republished", "count": republished,
		}).Info("shared subscription auth state realigned with the database")
	}
	return nil
}

// subscriptionAuthRef builds the store reference of a credential row without
// the registry, which is not loaded yet when the startup sweep runs.
func (s *Service) subscriptionAuthRef(
	ctx context.Context,
	row models.Credential,
	groups map[uint]models.Group,
) (state.CredentialRef, error) {
	group, cached := groups[row.GroupID]
	if !cached {
		loaded, err := loadGroupRow(s.db.WithContext(ctx), row.GroupID)
		if err != nil {
			return state.CredentialRef{}, err
		}
		group, groups[row.GroupID] = loaded, loaded
	}
	return state.CredentialRef{
		ID: row.ID, GroupID: row.GroupID,
		IdentityGeneration: groupCollectionCredentialIdentity(row.IdentityFingerprint, group),
	}, nil
}

func logSharedAuthUnavailable(credentialID uint, err error) {
	logrus.WithError(err).WithFields(logrus.Fields{
		"event": "credential_health.redis_unavailable", "credential_id": credentialID, "op": "auth",
	}).Warn("subscription auth state was not shared")
}
