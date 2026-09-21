package control

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/automodel"
	"gpt-load/internal/catalog"
	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/health"
	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/platform/encryption"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/pricing"
	"gpt-load/internal/requestlog"
	"gpt-load/internal/state"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage/dbtx"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/subscription"
	subscriptionruntime "gpt-load/internal/subscription/runtime"
)

const (
	defaultModelDiscoveryTimeout      = 30 * time.Second
	defaultSubscriptionControlTimeout = 30 * time.Second
	controlTransactionCleanupTimeout  = time.Second
	// configBroadcastTimeout bounds the post-commit doorbell so an
	// unreachable Redis cannot hold the control-plane write lock.
	configBroadcastTimeout = 2 * time.Second
)

type Service struct {
	db                                *gorm.DB
	manager                           *state.Manager
	registry                          *state.CredentialRegistry
	channelRegistry                   *channel.Registry
	channelDefaultBaseURLs            channelDefaultBaseURLProvider
	registrySnapshot                  func() []state.CredentialRuntimeView
	priceRuntime                      *PriceRuntime
	catalogRuntime                    *catalog.Runtime
	catalogSync                       *CatalogSyncCoordinator
	modelsDevAutoSyncOverride         *bool
	environmentProxy                  *outboundproxy.Config
	encryption                        encryption.Service
	executor                          execution.Executor
	subscriptions                     *subscriptionruntime.Runtime
	requestLogs                       RequestLogReader
	usageStats                        UsageStatReader
	credentialWindowUsage             credentialWindowUsageReader
	credentialActivity                credentialActivityReader
	homeStatistics                    HomeStatisticsReader
	stats                             *health.StatsStore
	mutations                         credentialMutationCoordinator
	requestLogStats                   RequestLogStatsReader
	accessQuota                       *accessquota.Runtime
	modelDiscoveryTimeout             time.Duration
	random                            io.Reader
	operationRandom                   io.Reader
	beginSubscriptionAuthorization    func(channel.ID) (subscriptionruntime.Authorization, error)
	completeSubscriptionAuthorization func(context.Context, channel.ID, subscriptionruntime.AuthorizationCompletion) (subscriptionruntime.Credential, error)
	beginDeviceAuthorization          func(context.Context, channel.ID) (subscriptionruntime.DeviceAuthorization, error)
	pollDeviceAuthorization           func(context.Context, channel.ID, []byte) (subscriptionruntime.DeviceAuthorizationPoll, error)
	refreshSubscriptionCredential     func(context.Context, channel.ID, subscriptionruntime.Credential) (subscriptionruntime.Credential, error)
	prepareSubscriptionCredential     func(context.Context, channel.ID, execution.CredentialSnapshot, bool) (subscriptionruntime.Credential, *execution.ErrorEvidence)
	recoverSubscriptionCredential     func(context.Context, channel.ID, execution.CredentialSnapshot) (subscriptionruntime.Credential, *execution.ErrorEvidence)
	discoverSubscriptionModels        func(context.Context, channel.ID, subscriptionruntime.Credential, subscriptionruntime.Target) ([]string, error)
	observeSubscriptionAccount        func(context.Context, channel.ID, subscriptionruntime.Credential, subscriptionruntime.Target) (subscriptionruntime.Observation, error)
	consumeSubscriptionResetCredit    func(context.Context, channel.ID, subscriptionruntime.Credential, subscriptionruntime.Target, string) (subscriptionruntime.ResetCreditResult, error)
	oauthCallback                     *OAuthCallbackManager
	now                               func() time.Time
	publishSnapshot                   func(state.CompileInput) (*state.ConfigSnapshot, error)
	reconcileRegistryGroup            func(uint, []state.CredentialEntry) (bool, error)
	applyBatchRegistryMutation        func(uint, []uint, CredentialBatchAction) error
	restoreBatchRegistryEntries       func(uint, []state.CredentialEntry) error
	beforeAdvanceOperationStage       func(
		context.Context,
		*models.ControlOperation,
		operationStage,
	) error
	configBroadcast       func(context.Context) (int64, error)
	taskLease             TaskLease
	broadcastPending      atomic.Bool
	operationRecoveryWake chan struct{}
	writeMu               sync.RWMutex
	observationMu         sync.Mutex
	observationFlights    map[observationFlightKey]*observationFlight
	observationSemaphore  chan struct{}
}

type credentialRuntimeRetirer interface {
	RetireCredential(uint)
}

type credentialWindowUsageReader interface {
	QueryCredentialWindowUsage(
		context.Context,
		requestlog.CredentialWindowUsageQuery,
	) (requestlog.CredentialWindowUsage, error)
}

type credentialActivityReader interface {
	QueryCredentialActivity(
		context.Context,
		requestlog.CredentialActivityQuery,
	) (map[uint]requestlog.CredentialActivity, error)
}

type credentialMultiMutationCoordinator interface {
	DoMany([]uint, func())
}

func (s *Service) doCredentialMutations(credentialIDs []uint, fn func()) error {
	if fn == nil {
		return nil
	}
	if len(credentialIDs) == 0 || s.mutations == nil {
		fn()
		return nil
	}
	if len(credentialIDs) == 1 {
		s.mutations.Do(credentialIDs[0], fn)
		return nil
	}
	coordinator, ok := s.mutations.(credentialMultiMutationCoordinator)
	if !ok {
		return fmt.Errorf("credential mutation coordinator unavailable: %w", app_errors.ErrInternalServer)
	}
	coordinator.DoMany(credentialIDs, fn)
	return nil
}

func (s *Service) retireCredentialRuntime(credentialID uint) {
	if s == nil || credentialID == 0 {
		return
	}
	if runtime, ok := s.executor.(credentialRuntimeRetirer); ok {
		runtime.RetireCredential(credentialID)
	}
}

// NewService constructs the control-plane service and wires channel-specific
// subscription capabilities into its persistence and execution collaborators.
func NewService(
	db *gorm.DB,
	manager *state.Manager,
	registry *state.CredentialRegistry,
	priceRuntime *PriceRuntime,
	catalogRuntime *catalog.Runtime,
	cfg *config.Config,
	encryptionService encryption.Service,
	executor execution.Executor,
	subscriptionCredentials *subscription.CredentialManager,
	requestLogs RequestLogReader,
	usageStats UsageStatReader,
	homeStatistics HomeStatisticsReader,
	stats *health.StatsStore,
	mutations *health.MutationCoordinator,
	requestLogStats RequestLogStatsReader,
	accessQuota *accessquota.Runtime,
	channelRegistries ...*channel.Registry,
) *Service {
	channelRegistry := channel.NewRegistry()
	for _, candidate := range channelRegistries {
		if candidate != nil {
			channelRegistry = candidate
			break
		}
	}
	var subscriptions *subscriptionruntime.Runtime
	if subscriptionCredentials != nil {
		subscriptions = subscriptionCredentials.Runtime()
	}
	service := &Service{
		db: db, manager: manager, registry: registry,
		channelRegistry: channelRegistry,
		priceRuntime:    priceRuntime,
		catalogRuntime:  catalogRuntime,
		encryption:      encryptionService, executor: executor, subscriptions: subscriptions, requestLogs: requestLogs,
		usageStats: usageStats, homeStatistics: homeStatistics,
		stats: stats, mutations: mutations, requestLogStats: requestLogStats, accessQuota: accessQuota,
		modelDiscoveryTimeout: defaultModelDiscoveryTimeout,
		random:                rand.Reader,
		operationRandom:       rand.Reader,
		beginSubscriptionAuthorization: func(channelID channel.ID) (subscriptionruntime.Authorization, error) {
			browser, ok := subscriptionsBrowser(subscriptions, channelID)
			if !ok {
				return subscriptionruntime.Authorization{}, app_errors.ErrAuthorizationUnavailable
			}
			authorization, err := browser.BeginAuthorization()
			if err == nil {
				if callback, local := subscriptions.LocalCallback(channelID); local {
					authorization.RedirectURI = callback.RedirectURI
				}
			}
			return authorization, err
		},
		completeSubscriptionAuthorization: func(ctx context.Context, channelID channel.ID, completion subscriptionruntime.AuthorizationCompletion) (subscriptionruntime.Credential, error) {
			browser, ok := subscriptionsBrowser(subscriptions, channelID)
			if !ok {
				return subscriptionruntime.Credential{}, app_errors.ErrAuthorizationUnavailable
			}
			return browser.CompleteAuthorization(ctx, completion)
		},
		beginDeviceAuthorization: func(ctx context.Context, channelID channel.ID) (subscriptionruntime.DeviceAuthorization, error) {
			device, ok := subscriptionsDevice(subscriptions, channelID)
			if !ok {
				return subscriptionruntime.DeviceAuthorization{}, app_errors.ErrAuthorizationUnavailable
			}
			return device.BeginDeviceAuthorization(ctx)
		},
		pollDeviceAuthorization: func(ctx context.Context, channelID channel.ID, state []byte) (subscriptionruntime.DeviceAuthorizationPoll, error) {
			device, ok := subscriptionsDevice(subscriptions, channelID)
			if !ok {
				return subscriptionruntime.DeviceAuthorizationPoll{}, app_errors.ErrAuthorizationUnavailable
			}
			return device.PollDeviceAuthorization(ctx, state)
		},
		refreshSubscriptionCredential: func(ctx context.Context, channelID channel.ID, credential subscriptionruntime.Credential) (subscriptionruntime.Credential, error) {
			driver, ok := subscriptionsDriver(subscriptions, channelID)
			if !ok {
				return subscriptionruntime.Credential{}, app_errors.ErrAuthorizationUnavailable
			}
			return driver.Refresh(ctx, credential)
		},
		discoverSubscriptionModels: func(ctx context.Context, channelID channel.ID, credential subscriptionruntime.Credential, target subscriptionruntime.Target) ([]string, error) {
			capability, ok := subscriptions.ModelDiscovery(channelID)
			if !ok {
				return nil, app_errors.ErrValidation
			}
			return capability.DiscoverModels(ctx, credential, target)
		},
		observeSubscriptionAccount: func(ctx context.Context, channelID channel.ID, credential subscriptionruntime.Credential, target subscriptionruntime.Target) (subscriptionruntime.Observation, error) {
			capability, ok := subscriptions.QuotaObservation(channelID)
			if !ok {
				return subscriptionruntime.Observation{}, app_errors.ErrValidation
			}
			return capability.Observe(ctx, credential, target)
		},
		consumeSubscriptionResetCredit: func(ctx context.Context, channelID channel.ID, credential subscriptionruntime.Credential, target subscriptionruntime.Target, requestID string) (subscriptionruntime.ResetCreditResult, error) {
			capability, ok := subscriptions.ResetCreditAction(channelID)
			if !ok {
				return subscriptionruntime.ResetCreditResult{}, app_errors.ErrValidation
			}
			return capability.Consume(ctx, credential, target, requestID)
		},
		now:                   time.Now,
		operationRecoveryWake: make(chan struct{}, 1),
		observationFlights:    make(map[observationFlightKey]*observationFlight),
		observationSemaphore:  make(chan struct{}, 1),
	}
	if cfg != nil {
		service.environmentProxy = outboundproxy.Environment()
	}
	if subscriptionCredentials != nil {
		service.prepareSubscriptionCredential = subscriptionCredentials.PrepareForControl
		service.recoverSubscriptionCredential = subscriptionCredentials.RefreshForManualRecovery
		service.subscriptions = subscriptionCredentials.Runtime()
	}
	if reader, ok := usageStats.(credentialWindowUsageReader); ok {
		service.credentialWindowUsage = reader
	} else if reader, ok := requestLogs.(credentialWindowUsageReader); ok {
		service.credentialWindowUsage = reader
	}
	if reader, ok := usageStats.(credentialActivityReader); ok {
		service.credentialActivity = reader
	} else if reader, ok := requestLogs.(credentialActivityReader); ok {
		service.credentialActivity = reader
	}
	if provider, ok := executor.(channelDefaultBaseURLProvider); ok {
		service.channelDefaultBaseURLs = provider
	}
	if cfg != nil && cfg.ModelsDevAutoSyncOverride != nil {
		value := *cfg.ModelsDevAutoSyncOverride
		service.modelsDevAutoSyncOverride = &value
	}
	service.publishSnapshot = manager.Publish
	service.reconcileRegistryGroup = registry.ReconcileGroup
	service.applyBatchRegistryMutation = service.applyCredentialBatchRegistryMutation
	service.restoreBatchRegistryEntries = registry.RestoreGroupCredentialEntriesExact
	service.registrySnapshot = registry.Snapshot
	service.oauthCallback = NewOAuthCallbackManager(service)
	return service
}

func subscriptionsDriver(runtime *subscriptionruntime.Runtime, channelID channel.ID) (subscriptionruntime.Driver, bool) {
	if runtime == nil {
		return nil, false
	}
	return runtime.Driver(channelID)
}

func subscriptionsBrowser(runtime *subscriptionruntime.Runtime, channelID channel.ID) (subscriptionruntime.BrowserAuthorizationDriver, bool) {
	if runtime == nil {
		return nil, false
	}
	return runtime.BrowserAuthorization(channelID)
}

func subscriptionsDevice(runtime *subscriptionruntime.Runtime, channelID channel.ID) (subscriptionruntime.DeviceAuthorizationDriver, bool) {
	if runtime == nil {
		return nil, false
	}
	return runtime.DeviceAuthorization(channelID)
}

type configMutationPublication struct {
	ConfigInput state.CompileInput
	PriceTable  *pricing.Table
}

func (s *Service) writeGroupConfig(
	ctx context.Context,
	mutate func(*gorm.DB) error,
	afterCommitBeforePublish func() error,
) (*state.ConfigSnapshot, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.enforceOperationRecoveryBarrierLocked(ctx, 0); err != nil {
		return nil, err
	}
	var credentialIDs []uint
	if err := s.db.WithContext(ctx).Model(&models.Credential{}).
		Order("id ASC").Pluck("id", &credentialIDs).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	var snapshot *state.ConfigSnapshot
	var resultErr error
	apply := func() {
		snapshot, resultErr = s.writeGroupConfigLocked(ctx, mutate, afterCommitBeforePublish)
	}
	if err := s.doCredentialMutations(credentialIDs, apply); err != nil {
		return nil, err
	}
	return snapshot, resultErr
}

func (s *Service) writeGroupConfigLocked(
	ctx context.Context,
	mutate func(*gorm.DB) error,
	afterCommitBeforePublish func() error,
) (*state.ConfigSnapshot, error) {
	var catalogSnapshot *catalog.Snapshot
	if s.catalogRuntime != nil {
		catalogSnapshot = s.catalogRuntime.Load()
	}
	publication := configMutationPublication{}
	err := s.withControlTransaction(ctx, func(tx *gorm.DB) error {
		if err := mutate(tx); err != nil {
			return err
		}
		if err := reconcileReferencedPrices(tx, catalogSnapshot); err != nil {
			return err
		}
		if err := cleanupUnreferencedAutomaticPrices(tx); err != nil {
			return err
		}
		input, err := stateloader.BuildCompileInputWithProxy(
			ctx, tx, s.encryption, s.environmentProxy, s.channelRegistry,
		)
		if err != nil {
			return err
		}
		if _, err := state.Compile(input); err != nil {
			if errors.Is(err, automodel.ErrInvalidConfig) {
				return app_errors.ErrValidation
			}
			return err
		}
		priceTable, err := loadPriceTable(ctx, tx)
		if err != nil {
			return err
		}
		publication = configMutationPublication{ConfigInput: input, PriceTable: priceTable}
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.priceRuntime.Publish(publication.PriceTable)
	if afterCommitBeforePublish != nil {
		if err := afterCommitBeforePublish(); err != nil {
			operationErr := newControlOperationError(stageApplyCommittedRegistryMutation)
			return nil, joinCommittedRuntimeRecovery(
				operationErr,
				s.recoverCommittedRuntime(ctx, true),
			)
		}
	}
	snapshot, err := s.publishSnapshot(publication.ConfigInput)
	if err != nil {
		operationErr := newControlOperationError(stagePublishCommittedSnapshot)
		return nil, joinCommittedRuntimeRecovery(
			operationErr,
			s.recoverCommittedRuntime(ctx, true),
		)
	}
	return snapshot, nil
}

func (s *Service) writeConfig(
	ctx context.Context,
	mutate func(*gorm.DB) error,
	afterCommitBeforePublish func() error,
) (*state.ConfigSnapshot, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.enforceOperationRecoveryBarrierLocked(ctx, 0); err != nil {
		return nil, err
	}

	var input state.CompileInput
	err := s.withControlTransaction(ctx, func(tx *gorm.DB) error {
		if err := mutate(tx); err != nil {
			return err
		}
		var err error
		input, err = stateloader.BuildCompileInputWithProxy(
			ctx, tx, s.encryption, s.environmentProxy, s.channelRegistry,
		)
		if err != nil {
			return err
		}
		if _, err := state.Compile(input); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if afterCommitBeforePublish != nil {
		if err := afterCommitBeforePublish(); err != nil {
			operationErr := newControlOperationError(stageApplyCommittedRegistryMutation)
			return nil, joinCommittedRuntimeRecovery(
				operationErr,
				s.recoverCommittedRuntime(ctx, false),
			)
		}
	}
	snapshot, err := s.publishSnapshot(input)
	if err != nil {
		operationErr := newControlOperationError(stagePublishCommittedSnapshot)
		return nil, joinCommittedRuntimeRecovery(
			operationErr,
			s.recoverCommittedRuntime(ctx, false),
		)
	}
	return snapshot, nil
}

func (s *Service) writeCredentialConfig(
	ctx context.Context,
	groupID uint,
	credentialID uint,
	mutate func(*gorm.DB) error,
	afterCommit func() error,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.enforceOperationRecoveryBarrierLocked(ctx, 0); err != nil {
		return err
	}
	var result error
	apply := func() {
		if err := s.withControlTransaction(ctx, mutate); err != nil {
			result = err
			return
		}
		if afterCommit == nil {
			return
		}
		if err := afterCommit(); err != nil {
			operationErr := withControlOperationContext(
				newControlOperationError(stageApplyCommittedRegistryMutation),
				groupID,
				credentialID,
			)
			result = joinCommittedRuntimeRecovery(
				operationErr,
				s.recoverCommittedCredentialRegistryGroup(ctx, groupID),
			)
		}
	}
	if credentialID != 0 && s.mutations != nil {
		s.mutations.Do(credentialID, apply)
	} else {
		apply()
	}
	return result
}

// ReloadCommittedConfiguration rebuilds this process's runtime configuration
// from committed database state. It is the receiving half of cross-instance
// configuration propagation: another instance committed a change, and this
// instance reapplies it without a restart.
//
// The call is repeatable and leaves no observable trace when nothing changed.
// Credentials whose persisted configuration is unchanged keep their registry
// entry pointer, so cooldown, blacklist and failure counters survive; the
// snapshot is published only when it differs from the current one.
//
// It deliberately does not run the operation-recovery barrier: a reload is not
// a write, and operation recovery already serializes against it through
// writeMu.
//
// writeMu is held across the database reads on purpose. recoverCommittedRuntime
// issues several independent, non-transactional queries against s.db, and their
// mutual consistency comes only from excluding concurrent control-plane writes.
// Moving those reads outside the lock would silently introduce torn reads.
func (s *Service) ReloadCommittedConfiguration(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var credentialIDs []uint
	if err := s.db.WithContext(ctx).Model(&models.Credential{}).
		Order("id ASC").Pluck("id", &credentialIDs).Error; err != nil {
		return app_errors.ParseDBError(err)
	}
	var result error
	apply := func() {
		result = s.recoverCommittedRuntime(ctx, true)
	}
	if err := s.doCredentialMutations(credentialIDs, apply); err != nil {
		return err
	}
	return result
}

func (s *Service) recoverCommittedRuntime(ctx context.Context, includePrices bool) error {
	input, err := stateloader.BuildCompileInputWithProxy(
		ctx, s.db, s.encryption, s.environmentProxy, s.channelRegistry,
	)
	if err != nil {
		return fmt.Errorf("reload committed configuration: %w", err)
	}
	if _, err := state.Compile(input); err != nil {
		return fmt.Errorf("compile committed configuration: %w", err)
	}
	var priceTable *pricing.Table
	if includePrices {
		entries, entriesErr := stateloader.BuildCredentialEntriesWithProxy(ctx, s.db, s.encryption)
		if entriesErr != nil {
			return fmt.Errorf("reload committed credentials: %w", entriesErr)
		}
		priceTable, err = loadPriceTable(ctx, s.db)
		if err != nil {
			return fmt.Errorf("reload committed prices: %w", err)
		}
		s.priceRuntime.Publish(priceTable)
		if err := s.registry.ReplaceCredentials(entries); err != nil {
			return fmt.Errorf("replace committed credentials: %w", err)
		}
		if err := s.restoreCredentialQuotaObservations(ctx); err != nil {
			return fmt.Errorf("restore committed credential quota observations: %w", err)
		}
	}
	// Publishing is gated on a difference so a reload that changed nothing
	// produces no observable churn: no snapshot revision bump and no wakeup
	// of the validation loop or of live WebSocket sessions.
	//
	// Matches only gates the snapshot. Prices, the credential registry and
	// quota observations are refreshed unconditionally because
	// CompileInput.Credentials carries no credential proxy identity, so a
	// proxy-only change is invisible to the comparison. Those steps are
	// themselves free of side effects when nothing changed.
	matches, err := s.manager.Matches(input)
	if err != nil {
		return fmt.Errorf("compare committed configuration: %w", err)
	}
	if matches {
		return nil
	}
	if _, err := s.manager.Publish(input); err != nil {
		return fmt.Errorf("publish committed configuration: %w", err)
	}
	return nil
}

func (s *Service) recoverCommittedCredentialRegistryGroup(ctx context.Context, groupID uint) error {
	entries, err := stateloader.BuildGroupCredentialEntriesWithProxy(ctx, s.db, groupID, s.encryption)
	if err != nil {
		return fmt.Errorf("reload committed group credentials: %w", err)
	}
	if _, err := s.reconcileRegistryGroup(groupID, entries); err != nil {
		return fmt.Errorf("reconcile committed group credentials: %w", err)
	}
	return nil
}

func joinCommittedRuntimeRecovery(operationErr, recoveryErr error) error {
	if recoveryErr == nil {
		return operationErr
	}
	return errors.Join(operationErr, recoveryErr)
}

// SetConfigBroadcaster installs the cross-instance doorbell rung after every
// committed control transaction. Single-instance deployments leave it unset,
// and every broadcast path then becomes a no-op.
func (s *Service) SetConfigBroadcaster(broadcast func(context.Context) (int64, error)) {
	s.configBroadcast = broadcast
}

// SetTaskLease installs the cross-instance claim used by the globally
// scheduled sweeps this service owns. Single-instance deployments leave it
// unset, and every period is then claimed locally without reaching Redis.
func (s *Service) SetTaskLease(lease TaskLease) {
	s.taskLease = lease
}

// withControlTransaction is the single entry point for control-plane writes,
// which is why the cross-instance doorbell is rung here and not next to the
// local snapshot publication. The commit is the point of no return: a change
// that committed but then failed to publish locally still has to reach the
// other instances, and hanging the broadcast off publication would miss
// exactly that window.
//
// Every control transaction rings, including those that write no
// configuration at all, such as operation recovery and bootstrap. Telling them
// apart would mean maintaining a "is this a configuration write" judgement at
// every call site — the very thing this placement avoids. An extra ring costs
// receivers one idempotent reload with no observable effect; a missed ring
// costs permanent divergence.
func (s *Service) withControlTransaction(
	ctx context.Context,
	mutate func(*gorm.DB) error,
) error {
	err := dbtx.Run(ctx, s.db, dbtx.Options{
		Mode:           dbtx.Write,
		CleanupTimeout: controlTransactionCleanupTimeout,
		Operation:      "control transaction",
	}, mutate)
	if dbtx.IsInfrastructure(err) {
		return fmt.Errorf("%v: %w", err, app_errors.ErrDatabase)
	}
	if err == nil {
		s.broadcastCommittedConfig()
	}
	return err
}

// broadcastCommittedConfig announces a committed change to the other
// instances. A failure never changes the control-plane result: the change is
// already committed, and reporting an error would tell the operator their
// write failed when it did not.
func (s *Service) broadcastCommittedConfig() {
	if s.configBroadcast == nil {
		return
	}
	// The request context may already be cancelled — an operator's connection
	// can drop the instant after the commit — but the committed change still
	// has to be announced, so the broadcast gets its own bounded context.
	ctx, cancel := context.WithTimeout(context.Background(), configBroadcastTimeout)
	defer cancel()
	version, err := s.configBroadcast(ctx)
	if err != nil {
		s.broadcastPending.Store(true)
		logConfigEvent(
			logrus.WarnLevel,
			logrus.Fields{"event": "config.broadcast_failed"},
			"Configuration broadcast failed; retry is pending",
		)
		return
	}
	logConfigEvent(
		logrus.DebugLevel,
		logrus.Fields{"event": "config.broadcast", "config_version": version},
		"Configuration change broadcast",
	)
}

// RetryPendingBroadcast re-announces a committed change whose doorbell failed.
// This is not defensive: while Redis was unreachable the shared version never
// advanced, so no amount of polling on the other instances can discover that
// commit. Re-ringing is that change's only route to the rest of the fleet.
func (s *Service) RetryPendingBroadcast(ctx context.Context) {
	if s.configBroadcast == nil {
		return
	}
	// Claim the pending mark before broadcasting so a broadcast that fails
	// concurrently can re-arm it without this success erasing it.
	if !s.broadcastPending.CompareAndSwap(true, false) {
		return
	}
	version, err := s.configBroadcast(ctx)
	if err != nil {
		s.broadcastPending.Store(true)
		logConfigEvent(
			logrus.WarnLevel,
			logrus.Fields{"event": "config.broadcast_failed"},
			"Configuration broadcast retry failed; retry remains pending",
		)
		return
	}
	logConfigEvent(
		logrus.InfoLevel,
		logrus.Fields{"event": "config.broadcast_retried", "config_version": version},
		"Pending configuration broadcast delivered",
	)
}

func (s *Service) withReadSnapshot(
	ctx context.Context,
	read func(*gorm.DB) error,
) error {
	err := dbtx.Run(ctx, s.db, dbtx.Options{
		Mode:           dbtx.ReadSnapshot,
		CleanupTimeout: controlTransactionCleanupTimeout,
		Operation:      "read snapshot",
	}, read)
	if dbtx.IsInfrastructure(err) {
		return fmt.Errorf("%v: %w", err, app_errors.ErrDatabase)
	}
	return err
}
