// Package subscription owns durable subscription credential lifecycle state.
// Provider wire conversion remains in the execution adapter.
package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/health"
	"gpt-load/internal/platform/encryption"
	"gpt-load/internal/state"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage/models"
	subscriptionruntime "gpt-load/internal/subscription/runtime"
)

const (
	refreshLeadTime        = 5 * time.Minute
	refreshFinalizeTimeout = 5 * time.Second
	maxRefreshRetryAfter   = time.Hour
	// refreshLeaseRetryInterval and refreshLeaseWaitLimit bound how a
	// refresh waits for a peer that holds the credential's refresh lease.
	refreshLeaseRetryInterval = 100 * time.Millisecond
	refreshLeaseWaitLimit     = 60 * time.Second
	refreshInProgressRetry    = 5 * time.Second
)

// refreshLease grants one cluster instance at a time the right to refresh a
// credential. Acquire does not wait; release stops renewal and frees it.
type refreshLease interface {
	Acquire(ctx context.Context, credentialID uint) (release func(), acquired bool, err error)
}

// configCommitter commits a credential change as a control-plane
// configuration change, so cluster peers reload it.
type configCommitter interface {
	CommitCredentialState(ctx context.Context, mutate func(*gorm.DB) error) error
}

// CredentialManager serializes subscription refreshes and keeps the database
// and runtime registry on the same durable credential version.
type CredentialManager struct {
	db             *gorm.DB
	encryption     encryption.Service
	registry       *state.CredentialRegistry
	mutations      *health.MutationCoordinator
	runtime        *subscriptionruntime.Runtime
	refresh        func(context.Context, subscriptionruntime.Driver, subscriptionruntime.Credential) (subscriptionruntime.Credential, error)
	replaceSecret  func(uint, uint64, uint64, string, string) bool
	reconcileGroup func(uint, []state.CredentialEntry) (bool, error)
	now            func() time.Time
	logger         *logrus.Logger
	passiveQuota   *passiveQuotaPending
	// Cluster coordination; all nil in single-instance mode.
	lease     refreshLease
	health    state.SharedCredentialHealthStore
	committer configCommitter
	// leaseRetryInterval and leaseWaitLimit default to the package limits.
	leaseRetryInterval time.Duration
	leaseWaitLimit     time.Duration
}

// SetClusterCoordination makes refreshes cluster-wide single flight: a
// refresh holds the credential's lease, auth states are shared through the
// health store, and a rotated secret is committed as a configuration change.
// It must be called before the manager serves requests.
func (manager *CredentialManager) SetClusterCoordination(
	lease refreshLease,
	health state.SharedCredentialHealthStore,
	committer configCommitter,
) {
	manager.lease, manager.health, manager.committer = lease, health, committer
}

// Runtime returns the immutable capability registry used by this manager.
func (manager *CredentialManager) Runtime() *subscriptionruntime.Runtime {
	if manager == nil {
		return nil
	}
	return manager.runtime
}

// NewCredentialManager creates the shared control-plane and data-plane
// lifecycle for all compiled subscription channels.
func NewCredentialManager(
	db *gorm.DB,
	encryptionService encryption.Service,
	registry *state.CredentialRegistry,
	mutations *health.MutationCoordinator,
	runtime *subscriptionruntime.Runtime,
) *CredentialManager {
	if mutations == nil {
		mutations = health.NewMutationCoordinator()
	}
	return &CredentialManager{
		db: db, encryption: encryptionService, registry: registry, mutations: mutations,
		runtime: runtime, passiveQuota: newPassiveQuotaPending(),
		refresh: func(ctx context.Context, driver subscriptionruntime.Driver, credential subscriptionruntime.Credential) (subscriptionruntime.Credential, error) {
			return driver.Refresh(ctx, credential)
		},
		now:                time.Now,
		logger:             logrus.StandardLogger(),
		leaseRetryInterval: refreshLeaseRetryInterval,
		leaseWaitLimit:     refreshLeaseWaitLimit,
		replaceSecret:      registry.ReplaceCredentialSecretIfMatch,
		reconcileGroup:     registry.ReconcileGroup,
	}
}

// Prepare returns the currently usable credential, durably refreshing it when
// required. No provider request is sent after an uncertain refresh outcome.
func (manager *CredentialManager) Prepare(
	ctx context.Context,
	channelID channel.ID,
	snapshot execution.CredentialSnapshot,
	forceRefresh bool,
) (subscriptionruntime.Credential, *execution.ErrorEvidence) {
	return manager.prepare(ctx, channelID, snapshot, forceRefresh, true, false)
}

// PrepareForControl prepares a credential for an explicit control-plane
// operation. Explicit operator actions may retry while data-plane requests are
// held behind a refresh cooldown.
func (manager *CredentialManager) PrepareForControl(
	ctx context.Context,
	channelID channel.ID,
	snapshot execution.CredentialSnapshot,
	forceRefresh bool,
) (subscriptionruntime.Credential, *execution.ErrorEvidence) {
	return manager.prepare(ctx, channelID, snapshot, forceRefresh, false, false)
}

// RefreshForManualRecovery performs one operator-initiated credential refresh.
// It is the only entry point permitted to recover a credential from a failed
// auth state; every automatic path keeps the existing gate.
func (manager *CredentialManager) RefreshForManualRecovery(
	ctx context.Context,
	channelID channel.ID,
	snapshot execution.CredentialSnapshot,
) (subscriptionruntime.Credential, *execution.ErrorEvidence) {
	return manager.prepare(ctx, channelID, snapshot, true, false, true)
}

func (manager *CredentialManager) prepare(
	ctx context.Context,
	channelID channel.ID,
	snapshot execution.CredentialSnapshot,
	forceRefresh bool,
	respectCooldown bool,
	allowRecovery bool,
) (subscriptionruntime.Credential, *execution.ErrorEvidence) {
	driver, ok := manager.runtime.Driver(channelID)
	if !ok {
		return subscriptionruntime.Credential{}, localEvidence("credential_driver_unavailable", "subscription credential driver is unavailable")
	}
	credential, err := driver.Parse(snapshot.Data())
	if err != nil {
		return subscriptionruntime.Credential{}, localEvidence("credential_invalid", "subscription credential is invalid")
	}
	if !forceRefresh {
		if expiration, ok := credential.ExpiresAt(); !ok || expiration.After(manager.now().Add(refreshLeadTime)) {
			return credential, nil
		}
	}
	if manager.lease != nil {
		release, evidence := manager.acquireRefreshLease(ctx, snapshot.ID)
		if evidence != nil {
			return subscriptionruntime.Credential{}, evidence
		}
		defer release()
	}
	var prepared subscriptionruntime.Credential
	var prepareErr *execution.ErrorEvidence
	manager.mutations.Do(snapshot.ID, func() {
		prepared, prepareErr = manager.refreshCredentialLocked(
			ctx,
			channelID,
			driver,
			snapshot.ID,
			snapshot.Version,
			snapshot.IdentityGeneration,
			forceRefresh,
			respectCooldown,
			allowRecovery,
		)
	})
	return prepared, prepareErr
}

// acquireRefreshLease waits for the credential's cluster refresh lease. The
// holder is expected to finish soon; once it does, the refresh re-reads the
// committed row and usually returns the new secret without calling upstream.
// A lease error fails the refresh rather than refreshing without the lease,
// which could spend a one-time refresh token twice.
func (manager *CredentialManager) acquireRefreshLease(
	ctx context.Context,
	credentialID uint,
) (func(), *execution.ErrorEvidence) {
	waitCtx, cancel := context.WithTimeout(ctx, manager.leaseWaitLimit)
	defer cancel()
	for {
		release, acquired, err := manager.lease.Acquire(waitCtx, credentialID)
		if err != nil && waitCtx.Err() == nil {
			return nil, localEvidence("refresh_lease_unavailable", "subscription credential refresh coordination is unavailable")
		}
		if acquired {
			return release, nil
		}
		select {
		case <-waitCtx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				// The caller went away; no peer is known to hold the lease.
				return nil, localEvidence("refresh_canceled", "subscription credential refresh was canceled")
			}
			manager.logger.WithFields(logrus.Fields{
				"event": "subscription.refresh_lease_timeout", "credential_id": credentialID,
			}).Warn("Subscription credential refresh is still in progress on another instance")
			evidence := refreshTemporarilyUnavailableEvidence(
				subscriptionruntime.RefreshFailureDecision{RetryAfter: refreshInProgressRetry},
			)
			evidence.Code = "refresh_in_progress"
			evidence.Summary = "subscription credential refresh is in progress"
			return nil, evidence
		case <-time.After(manager.leaseRetryInterval):
		}
	}
}

func (manager *CredentialManager) refreshCredentialLocked(
	ctx context.Context,
	channelID channel.ID,
	driver subscriptionruntime.Driver,
	credentialID uint,
	expectedVersion uint64,
	expectedIdentityGeneration uint64,
	forceRefresh bool,
	respectCooldown bool,
	allowRecovery bool,
) (subscriptionruntime.Credential, *execution.ErrorEvidence) {
	var row models.Credential
	if err := manager.db.WithContext(ctx).First(&row, credentialID).Error; err != nil {
		return subscriptionruntime.Credential{}, localEvidence("credential_unavailable", "subscription credential is unavailable")
	}
	var group models.Group
	if err := manager.db.WithContext(ctx).Select("id", "channel_id", "connection_type", "params").First(&group, row.GroupID).Error; err != nil ||
		group.ChannelID != string(channelID) || group.ConnectionType != models.ConnectionTypeSubscription {
		return subscriptionruntime.Credential{}, localEvidence("credential_target_mismatch", "subscription credential target does not match")
	}
	// URL 变更与凭据刷新共用 mutation 锁；此处读取持久身份，既阻止旧目标刷新，
	// 也允许同一目标的人工恢复修复尚未同步的 registry。
	currentIdentityGeneration := stateloader.CredentialIdentityGeneration(
		row.IdentityFingerprint, group.ChannelID, string(group.ConnectionType), json.RawMessage(group.Params))
	if currentIdentityGeneration != expectedIdentityGeneration {
		return subscriptionruntime.Credential{}, localEvidence("credential_target_mismatch", "subscription credential target does not match")
	}
	if manager.lease != nil && row.AuthState == models.CredentialAuthStateRefreshing && !allowRecovery {
		// This instance holds the lease, so the refresh that wrote this state
		// lost its lease: its holder crashed or stalled. Mark it interrupted;
		// a stalled holder can still commit because the commit only checks
		// the secret version.
		stateValue, code := refreshRestoreState(row)
		if err := manager.transitionAuthState(ctx, row, row.SecretVersion, stateValue, code); err != nil {
			return subscriptionruntime.Credential{}, localEvidence("refresh_state_commit_failed", "subscription credential state could not be saved")
		}
		return subscriptionruntime.Credential{}, authEvidence(code)
	}
	if row.AuthState != models.CredentialAuthStateReady && !allowRecovery {
		return subscriptionruntime.Credential{}, authEvidence(string(row.AuthState))
	}
	// A refresh that never produces a new credential must leave the account on
	// the state it already had. Restoring Ready unconditionally would let one
	// transient failure clear a real authorization problem.
	restoreState, restoreCode := refreshRestoreState(row)
	plaintext, err := manager.encryption.Decrypt(row.Data)
	if err != nil {
		return subscriptionruntime.Credential{}, localEvidence("credential_decrypt_failed", "subscription credential is unavailable")
	}
	current, err := driver.Parse([]byte(plaintext))
	plaintext = ""
	if err != nil {
		return subscriptionruntime.Credential{}, localEvidence("credential_invalid", "subscription credential is invalid")
	}
	// A newer durable version alone does not prove the account recovered: the
	// rotated secret may be committed while runtime publication failed.
	if forceRefresh && row.SecretVersion > expectedVersion &&
		row.AuthState == models.CredentialAuthStateReady {
		if err := manager.ensureRuntimeMatchesDurableSecret(ctx, row); err != nil {
			return subscriptionruntime.Credential{}, authEvidence("refresh_registry_mismatch")
		}
		return current, nil
	}
	if !forceRefresh {
		if expiration, ok := current.ExpiresAt(); !ok || expiration.After(manager.now().Add(refreshLeadTime)) {
			return current, nil
		}
	}
	var bypassedCooldown time.Time
	if respectCooldown {
		if evidence := manager.activeCredentialCooldownEvidence(row.ID); evidence != nil {
			return subscriptionruntime.Credential{}, evidence
		}
	} else {
		bypassedCooldown, _ = manager.registry.CredentialCooldownUntil(row.ID)
	}
	if err := manager.transitionAuthState(ctx, row, row.SecretVersion, models.CredentialAuthStateRefreshing, ""); err != nil {
		finalizeContext, cancel := refreshFinalizeContext(ctx)
		restoreErr := manager.setAuthState(finalizeContext, row.ID, row.SecretVersion, restoreState, restoreCode)
		if restoreErr == nil {
			restoreErr = manager.publishAuthState(finalizeContext, row, row.SecretVersion, restoreState)
		}
		cancel()
		if restoreErr != nil {
			manager.registry.SetCredentialAuthState(row.ID, state.CredentialAuthStateOutcomeUnknown)
			return subscriptionruntime.Credential{}, authEvidence("refresh_registry_mismatch")
		}
		return subscriptionruntime.Credential{}, localEvidence("refresh_start_failed", "subscription credential refresh could not start")
	}
	refreshStarted := time.Now()
	refreshed, refreshErr := manager.refresh(ctx, driver, current)
	if refreshErr != nil {
		failure := driver.ClassifyRefreshFailure(refreshErr)
		manager.logRefreshFailure(
			row,
			channelID,
			forceRefresh,
			failure,
			refreshErr,
			time.Since(refreshStarted),
		)
		if failure.Kind == subscriptionruntime.RefreshFailureRetryable {
			evidence := refreshTemporarilyUnavailableEvidence(failure)
			cooldown := evidence.RetryAfter
			if cooldown <= 0 {
				cooldown = subscriptionruntime.DefaultRefreshFailureCooldown
			}
			if !manager.setRefreshCooldown(ctx, row.ID, manager.now().Add(cooldown)) {
				if markErr := manager.markRefreshOutcomeUnknown(
					ctx,
					row,
					row.SecretVersion,
					"refresh_registry_mismatch",
				); markErr != nil {
					manager.registry.SetCredentialAuthState(row.ID, state.CredentialAuthStateOutcomeUnknown)
					return subscriptionruntime.Credential{}, localEvidence(
						"refresh_state_commit_failed",
						"subscription credential state could not be saved",
					)
				}
				return subscriptionruntime.Credential{}, authEvidence("refresh_registry_mismatch")
			}
			if err := manager.transitionAuthState(
				ctx,
				row,
				row.SecretVersion,
				restoreState,
				restoreCode,
			); err != nil {
				manager.registry.SetCredentialAuthState(row.ID, state.CredentialAuthStateOutcomeUnknown)
				return subscriptionruntime.Credential{}, localEvidence(
					"refresh_state_commit_failed",
					"subscription credential state could not be saved",
				)
			}
			return subscriptionruntime.Credential{}, evidence
		}
		stateValue, code := models.CredentialAuthStateOutcomeUnknown, "refresh_outcome_unknown"
		if restoreState != models.CredentialAuthStateReady {
			stateValue, code = restoreState, restoreCode
		}
		// The persisted code describes the account, the evidence describes this
		// attempt. A recovery retry that never reached the token endpoint must
		// not report the account's older authorization error as its own outcome.
		evidenceCode := "refresh_outcome_unknown"
		switch failure.Kind {
		case subscriptionruntime.RefreshFailureIdentityChanged:
			stateValue, code = models.CredentialAuthStateReauthorizationRequired, "refresh_identity_changed"
			evidenceCode = "refresh_identity_changed"
		case subscriptionruntime.RefreshFailureReauthorizationRequired:
			stateValue, code = models.CredentialAuthStateReauthorizationRequired, "refresh_rejected"
			evidenceCode = "refresh_rejected"
		}
		if err := manager.transitionAuthState(ctx, row, row.SecretVersion, stateValue, code); err != nil {
			manager.registry.SetCredentialAuthState(row.ID, state.CredentialAuthStateOutcomeUnknown)
			return subscriptionruntime.Credential{}, localEvidence("refresh_state_commit_failed", "subscription credential state could not be saved")
		}
		return subscriptionruntime.Credential{}, authEvidence(evidenceCode)
	}
	if !subscriptionruntime.RefreshPreservesIdentity(driver, current, refreshed) {
		if err := manager.transitionAuthState(ctx, row, row.SecretVersion, models.CredentialAuthStateReauthorizationRequired, "refresh_identity_changed"); err != nil {
			manager.registry.SetCredentialAuthState(row.ID, state.CredentialAuthStateOutcomeUnknown)
			return subscriptionruntime.Credential{}, localEvidence("refresh_state_commit_failed", "subscription credential state could not be saved")
		}
		return subscriptionruntime.Credential{}, authEvidence("refresh_identity_changed")
	}
	canonical := refreshed.Canonical()
	if len(canonical) == 0 {
		if markErr := manager.markRefreshOutcomeUnknown(ctx, row, row.SecretVersion, "refresh_persist_failed"); markErr != nil {
			return subscriptionruntime.Credential{}, localEvidence("refresh_state_commit_failed", "subscription credential state could not be saved")
		}
		return subscriptionruntime.Credential{}, authEvidence("refresh_persist_failed")
	}
	ciphertext, err := manager.encryption.Encrypt(string(canonical))
	fingerprint := manager.encryption.Hash(string(canonical))
	clear(canonical)
	if err != nil {
		if markErr := manager.markRefreshOutcomeUnknown(ctx, row, row.SecretVersion, "refresh_persist_failed"); markErr != nil {
			return subscriptionruntime.Credential{}, localEvidence("refresh_state_commit_failed", "subscription credential state could not be saved")
		}
		return subscriptionruntime.Credential{}, authEvidence("refresh_persist_failed")
	}
	nextVersion := row.SecretVersion + 1
	finalizeContext, cancelFinalize := refreshFinalizeContext(ctx)
	// The commit is conditioned on the secret version alone. Do not add
	// auth_state = 'refreshing' back: a sweep that wrongly judged this refresh
	// interrupted (a lost lease, a startup reset) must not discard the rotated
	// token, which upstream has already made the only valid one. Every real
	// competitor, such as a reauthorization or another refresh, bumps the
	// secret version first, so it still wins.
	commit := func(tx *gorm.DB) error {
		updated := tx.Model(&models.Credential{}).
			Where("id = ? AND secret_version = ?", row.ID, row.SecretVersion).
			Updates(map[string]any{
				"data": ciphertext, "fingerprint": fingerprint, "secret_version": nextVersion,
				"auth_state": models.CredentialAuthStateReady, "auth_error_code": "", "updated_at_ms": manager.now().UnixMilli(),
			})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return fmt.Errorf("credential secret commit affected %d rows", updated.RowsAffected)
		}
		return nil
	}
	var commitErr error
	if manager.committer != nil {
		commitErr = manager.committer.CommitCredentialState(finalizeContext, commit)
	} else {
		commitErr = commit(manager.db.WithContext(finalizeContext))
	}
	cancelFinalize()
	if commitErr != nil {
		if markErr := manager.markRefreshOutcomeUnknown(ctx, row, row.SecretVersion, "refresh_commit_failed"); markErr != nil {
			return subscriptionruntime.Credential{}, localEvidence("refresh_state_commit_failed", "subscription credential state could not be saved")
		}
		return subscriptionruntime.Credential{}, authEvidence("refresh_commit_failed")
	}
	if manager.replaceSecret == nil || !manager.replaceSecret(row.ID, row.SecretVersion, nextVersion, fingerprint, ciphertext) {
		// The rotated token is durable. Reconcile this Group from DB truth so a
		// failed incremental publication cannot leave control and data planes at
		// different secret versions.
		reconcileContext, cancelReconcile := refreshFinalizeContext(ctx)
		entries, reconcileErr := stateloader.BuildGroupCredentialEntriesWithProxy(
			reconcileContext, manager.db, row.GroupID, manager.encryption,
		)
		if reconcileErr != nil {
			cancelReconcile()
		} else if manager.reconcileGroup == nil {
			reconcileErr = errors.New("credential registry reconciliation is unavailable")
			cancelReconcile()
		} else {
			_, reconcileErr = manager.reconcileGroup(row.GroupID, entries)
			cancelReconcile()
		}
		if reconcileErr != nil {
			if markErr := manager.markRefreshOutcomeUnknown(ctx, row, nextVersion, "refresh_registry_mismatch"); markErr != nil {
				manager.registry.SetCredentialAuthState(row.ID, state.CredentialAuthStateOutcomeUnknown)
				return subscriptionruntime.Credential{}, localEvidence("refresh_state_commit_failed", "subscription credential state could not be saved")
			}
			return subscriptionruntime.Credential{}, authEvidence("refresh_registry_mismatch")
		}
	}
	if manager.health != nil {
		manager.shareAuthState(ctx, row.ID, state.CredentialAuthStateReady, nextVersion)
	}
	if !bypassedCooldown.IsZero() {
		manager.clearRefreshCooldown(ctx, row.ID, bypassedCooldown)
	}
	return refreshed, nil
}

func (manager *CredentialManager) activeCredentialCooldownEvidence(
	credentialID uint,
) *execution.ErrorEvidence {
	until, ok := manager.registry.CredentialCooldownUntil(credentialID)
	if !ok {
		return nil
	}
	remaining := until.Sub(manager.now())
	if remaining <= 0 {
		return nil
	}
	return refreshTemporarilyUnavailableEvidence(
		subscriptionruntime.RefreshFailureDecision{RetryAfter: remaining},
	)
}

// refreshRestoreState returns the auth state and error code a refresh must
// restore when it ends without producing a new credential. Residual refreshing
// is not a state a credential can be left in, so it is normalized the same way
// interrupted refreshes are handled at startup.
func refreshRestoreState(row models.Credential) (models.CredentialAuthState, string) {
	switch row.AuthState {
	case models.CredentialAuthStateRefreshing:
		return models.CredentialAuthStateOutcomeUnknown, "refresh_interrupted"
	case models.CredentialAuthStateReady, "":
		return models.CredentialAuthStateReady, ""
	default:
		return row.AuthState, row.AuthErrorCode
	}
}

// ensureRuntimeMatchesDurableSecret confirms the registry already serves the
// durable secret version in a usable state, repairing it from database truth
// when it does not. Reconciliation alone is not enough: it compares persisted
// credential config, so a registry that already serves this version but lags on
// auth state looks identical to it and would be left untouched.
func (manager *CredentialManager) ensureRuntimeMatchesDurableSecret(
	ctx context.Context,
	row models.Credential,
) error {
	if manager.runtimeMatchesDurableSecret(row) {
		return nil
	}
	if ref, ok := manager.registry.CredentialRef(row.ID); ok && ref.Version == row.SecretVersion {
		manager.registry.SetCredentialAuthState(row.ID, state.CredentialAuthState(row.AuthState))
		if manager.runtimeMatchesDurableSecret(row) {
			return nil
		}
	}
	reconcileContext, cancel := refreshFinalizeContext(ctx)
	defer cancel()
	entries, err := stateloader.BuildGroupCredentialEntriesWithProxy(
		reconcileContext, manager.db, row.GroupID, manager.encryption,
	)
	if err != nil {
		return err
	}
	if manager.reconcileGroup == nil {
		return errors.New("credential registry reconciliation is unavailable")
	}
	if _, err := manager.reconcileGroup(row.GroupID, entries); err != nil {
		return err
	}
	if !manager.runtimeMatchesDurableSecret(row) {
		return errors.New("credential runtime state does not match the durable secret")
	}
	return nil
}

func (manager *CredentialManager) runtimeMatchesDurableSecret(row models.Credential) bool {
	ref, ok := manager.registry.CredentialRef(row.ID)
	if !ok || ref.Version != row.SecretVersion {
		return false
	}
	authState, known := manager.registry.CredentialAuthStateOf(row.ID)
	return known && authState == state.CredentialAuthStateReady
}

func (manager *CredentialManager) markRefreshOutcomeUnknown(
	ctx context.Context,
	row models.Credential,
	version uint64,
	code string,
) error {
	return manager.transitionAuthState(ctx, row, version, models.CredentialAuthStateOutcomeUnknown, code)
}

func (manager *CredentialManager) setAuthState(
	ctx context.Context,
	credentialID uint,
	version uint64,
	authState models.CredentialAuthState,
	code string,
) error {
	result := manager.db.WithContext(ctx).Model(&models.Credential{}).
		Where("id = ? AND secret_version = ?", credentialID, version).
		Updates(map[string]any{"auth_state": authState, "auth_error_code": code, "updated_at_ms": manager.now().UnixMilli()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("credential auth state update affected %d rows", result.RowsAffected)
	}
	return nil
}

func (manager *CredentialManager) transitionAuthState(
	ctx context.Context,
	row models.Credential,
	version uint64,
	authState models.CredentialAuthState,
	code string,
) error {
	finalizeContext, cancel := refreshFinalizeContext(ctx)
	defer cancel()
	if err := manager.setAuthState(finalizeContext, row.ID, version, authState, code); err != nil {
		return err
	}
	return manager.publishAuthState(finalizeContext, row, version, authState)
}

// publishAuthState publishes the auth state committed for secretVersion. In
// cluster mode it goes through the shared store so peers stop or resume
// scheduling the credential; a store failure falls back to the local mirror.
func (manager *CredentialManager) publishAuthState(
	ctx context.Context,
	row models.Credential,
	secretVersion uint64,
	authState models.CredentialAuthState,
) error {
	runtimeState := state.CredentialAuthState(authState)
	if manager.health != nil && manager.shareAuthState(ctx, row.ID, runtimeState, secretVersion) {
		if current, known := manager.registry.CredentialAuthStateOf(row.ID); known && current == runtimeState {
			return nil
		}
	}
	if manager.registry.SetCredentialAuthState(row.ID, runtimeState) {
		return nil
	}
	entries, err := stateloader.BuildGroupCredentialEntriesWithProxy(
		ctx, manager.db, row.GroupID, manager.encryption,
	)
	if err != nil {
		return err
	}
	if manager.reconcileGroup == nil {
		return fmt.Errorf("credential registry reconciliation is unavailable")
	}
	_, err = manager.reconcileGroup(row.GroupID, entries)
	return err
}

// shareAuthState writes one auth state to the shared store and reports
// whether it succeeded; a failure is logged for the caller's fallback.
func (manager *CredentialManager) shareAuthState(
	ctx context.Context,
	credentialID uint,
	authState state.CredentialAuthState,
	secretVersion uint64,
) bool {
	ref, ok := manager.registry.CredentialRef(credentialID)
	if !ok {
		return false
	}
	if _, err := manager.health.SetAuthState(ctx, ref, authState, max(secretVersion, 1)); err != nil {
		manager.logSharedHealthUnavailable(credentialID, "auth", err)
		return false
	}
	return true
}

// setRefreshCooldown holds the credential back after a retryable refresh
// failure and reports whether the credential is still registered.
func (manager *CredentialManager) setRefreshCooldown(ctx context.Context, credentialID uint, until time.Time) bool {
	if manager.health != nil {
		if ref, ok := manager.registry.CredentialRef(credentialID); ok {
			_, err := manager.health.CooldownCredential(ctx, ref, until, 0)
			if err == nil {
				return true
			}
			manager.logSharedHealthUnavailable(credentialID, "cooldown", err)
		}
	}
	return manager.registry.SetCooldown(credentialID, until)
}

func (manager *CredentialManager) clearRefreshCooldown(ctx context.Context, credentialID uint, observed time.Time) {
	if manager.health != nil {
		if ref, ok := manager.registry.CredentialRef(credentialID); ok {
			_, err := manager.health.ClearCooldownIfMatch(ctx, ref, observed)
			if err == nil {
				return
			}
			manager.logSharedHealthUnavailable(credentialID, "clear_cooldown_if_match", err)
		}
	}
	manager.registry.ClearCooldownIfMatch(credentialID, observed)
}

func (manager *CredentialManager) logSharedHealthUnavailable(credentialID uint, op string, err error) {
	manager.logger.WithError(err).WithFields(logrus.Fields{
		"event": "credential_health.redis_unavailable", "credential_id": credentialID, "op": op,
	}).Warn("Shared credential health is unavailable; applying the change locally")
}

func refreshFinalizeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), refreshFinalizeTimeout)
}

func authEvidence(code string) *execution.ErrorEvidence {
	summary := "subscription account requires reauthorization"
	switch code {
	case "outcome_unknown", "refreshing", "refresh_outcome_unknown", "refresh_persist_failed",
		"refresh_commit_failed", "refresh_registry_mismatch", "refresh_state_commit_failed":
		summary = "subscription credential refresh outcome is unknown"
	}
	return &execution.ErrorEvidence{
		Kind: execution.ErrorKindProvider, Hint: execution.FailureHintReauthorizationRequired,
		OriginHint: execution.ErrorOriginUpstream, ScopeHint: execution.ErrorScopeCredential,
		Code: code, Summary: summary,
	}
}

func refreshTemporarilyUnavailableEvidence(
	failure subscriptionruntime.RefreshFailureDecision,
) *execution.ErrorEvidence {
	statusCode := failure.StatusCode
	if statusCode < 100 || statusCode > 599 {
		statusCode = 0
	}
	return &execution.ErrorEvidence{
		Kind: execution.ErrorKindHTTP, Hint: execution.FailureHintRefreshUnavailable,
		OriginHint: execution.ErrorOriginUpstream, ScopeHint: execution.ErrorScopeCredential,
		StatusCode: statusCode, Type: safeRefreshDiagnosticCode(failure.OAuthCode),
		Code:         "refresh_temporarily_unavailable",
		RetryAfter:   boundedRefreshRetryAfter(failure.RetryAfter),
		Summary:      "subscription credential refresh is temporarily unavailable",
		ReplaySafety: execution.ReplaySafetyRejectedBeforeProcessing,
	}
}

func boundedRefreshRetryAfter(value time.Duration) time.Duration {
	if value <= 0 {
		return 0
	}
	if value > maxRefreshRetryAfter {
		return maxRefreshRetryAfter
	}
	return value
}

func (manager *CredentialManager) logRefreshFailure(
	row models.Credential,
	channelID channel.ID,
	forceRefresh bool,
	failure subscriptionruntime.RefreshFailureDecision,
	err error,
	duration time.Duration,
) {
	if manager == nil || manager.logger == nil {
		return
	}
	fields := logrus.Fields{
		"event":          "subscription.credential_refresh_failed",
		"credential_id":  row.ID,
		"group_id":       row.GroupID,
		"channel":        channelID,
		"secret_version": row.SecretVersion,
		"force_refresh":  forceRefresh,
		"classification": failure.Kind.String(),
		"stage":          "provider_refresh",
		"error_kind":     refreshErrorKind(err, failure),
		"duration_ms":    duration.Milliseconds(),
	}
	if failure.StatusCode >= 100 && failure.StatusCode <= 599 {
		fields["http_status"] = failure.StatusCode
	}
	if code := safeRefreshDiagnosticCode(failure.OAuthCode); code != "" {
		fields["oauth_error_code"] = code
	}
	manager.logger.WithFields(fields).Warn("Subscription credential refresh failed")
}

func refreshErrorKind(err error, failure subscriptionruntime.RefreshFailureDecision) string {
	if failure.StatusCode != 0 {
		return "token_endpoint"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return "transport"
	}
	return "provider"
}

func safeRefreshDiagnosticCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '.' || character == '_' || character == '-' {
			continue
		}
		return ""
	}
	return value
}

func localEvidence(code, summary string) *execution.ErrorEvidence {
	return &execution.ErrorEvidence{
		Kind: execution.ErrorKindInternal, OriginHint: execution.ErrorOriginInternal,
		Code: code, Summary: summary,
	}
}
