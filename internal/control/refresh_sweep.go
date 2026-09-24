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
// auth state. On failure every instance still excludes the credential: the
// mirror keeps refreshing until the next auth write realigns it.
func (s *Service) publishSweptAuthStates(ctx context.Context, swept []models.Credential) {
	if s.sharedHealth == nil {
		return
	}
	groups := make(map[uint]models.Group)
	for _, row := range swept {
		group, cached := groups[row.GroupID]
		if !cached {
			loaded, err := loadGroupRow(s.db.WithContext(ctx), row.GroupID)
			if err != nil {
				logrus.WithError(err).WithFields(logrus.Fields{
					"event": "credential_health.redis_unavailable", "credential_id": row.ID, "op": "auth",
				}).Warn("swept refresh auth state was not shared")
				continue
			}
			group, groups[row.GroupID] = loaded, loaded
		}
		ref := state.CredentialRef{
			ID: row.ID, GroupID: row.GroupID,
			IdentityGeneration: groupCollectionCredentialIdentity(row.IdentityFingerprint, group),
		}
		if _, err := s.sharedHealth.SetAuthState(ctx, ref, state.CredentialAuthStateOutcomeUnknown,
			groupCollectionCredentialVersion(row.SecretVersion)); err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"event": "credential_health.redis_unavailable", "credential_id": row.ID, "op": "auth",
			}).Warn("swept refresh auth state was not shared")
		}
	}
}
