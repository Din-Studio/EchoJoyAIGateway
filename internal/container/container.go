// Package container assembles the 2.0 dependency graph with dig.
package container

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/dig"
	"gorm.io/gorm"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/app"
	"gpt-load/internal/catalog"
	"gpt-load/internal/channel"
	"gpt-load/internal/control"
	"gpt-load/internal/coordination"
	"gpt-load/internal/dialect"
	"gpt-load/internal/execution"
	bifrostexecutor "gpt-load/internal/execution/bifrost"
	cpaexecutor "gpt-load/internal/execution/cpa"
	"gpt-load/internal/gateway"
	"gpt-load/internal/health"
	"gpt-load/internal/httplifecycle"
	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/platform/encryption"
	"gpt-load/internal/platform/httpclient"
	"gpt-load/internal/platform/httproute"
	"gpt-load/internal/platform/redact"
	"gpt-load/internal/pricing"
	"gpt-load/internal/provideradapter"
	"gpt-load/internal/ratelimit"
	"gpt-load/internal/releasecheck"
	"gpt-load/internal/requestlog"
	"gpt-load/internal/state"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage"
	"gpt-load/internal/subscription"
	subscriptionproviders "gpt-load/internal/subscription/providers"
	subscriptionruntime "gpt-load/internal/subscription/runtime"
	"gpt-load/internal/telemetry"
	"gpt-load/internal/webui"

	"github.com/sirupsen/logrus"
)

// redisOpenTimeout bounds the startup connection attempt to the coordination
// backend so a black-holed Redis cannot hang process startup.
const redisOpenTimeout = 5 * time.Second

// BuildContainer creates the 2.0 runtime foundation dependency graph.
func BuildContainer() (*dig.Container, error) {
	dependencyContainer := dig.New()

	providers := []any{
		config.Load,
		func(cfg *config.Config) (encryption.Service, error) {
			return encryption.NewServiceWithKeyFile(cfg.EncryptionKey, cfg.DataDir)
		},
		func(cfg *config.Config) (*coordination.Client, error) {
			if cfg.InstanceMode != config.InstanceModeDistributed {
				return nil, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), redisOpenTimeout)
			defer cancel()
			return coordination.Open(ctx, cfg.RedisDSN)
		},
		func(client *coordination.Client) app.Coordination {
			// A nil *coordination.Client stored in an interface is not a nil
			// interface value; single-instance mode must hand out a true nil.
			if client == nil {
				return nil
			}
			return client
		},
		newConfigVersion,
		newConfigVersionSource,
		newTaskLease,
		newControlTaskLease,
		newResponseBindingStore,
		func(cfg *config.Config) (*gorm.DB, error) {
			db, err := storage.OpenConfigured(cfg)
			if err == nil {
				logrus.WithField("event", "startup.database_open").Info("database opened")
			}
			return db, err
		},
		httplifecycle.NewCoordinator,
		app.NewEngineWithLifecycle,
		webui.NewServer,
		state.NewCredentialRegistry,
		state.NewResponseBindings,
		accessquota.NewRuntime,
		channel.CompileRegistry,
		control.NewPriceRuntime,
		control.NewCatalogBootstrap,
		func(bootstrap *control.CatalogBootstrap) *catalog.Runtime { return bootstrap.Runtime },
		health.NewStatsStore,
		health.NewMutationCoordinator,
		ratelimit.NewAccessKeyRPM,
		func(limiter *ratelimit.AccessKeyRPM) gateway.AccessKeyRPMLimiter {
			return limiter
		},
		func(manager *state.Manager) requestlog.RetentionPolicyProvider {
			return retentionSnapshotProvider{manager: manager}
		},
		func(runtime *control.PriceRuntime) gateway.PriceTableProvider {
			return priceRuntimeProvider{runtime: runtime}
		},
		func(
			db *gorm.DB,
			redactor *redact.Redactor,
			retention requestlog.RetentionPolicyProvider,
			quotaRuntime *accessquota.Runtime,
			subscriptionCredentials *subscription.CredentialManager,
		) *requestlog.Service {
			service := requestlog.NewService(db, redactor, retention, quotaRuntime)
			service.SetPassiveQuotaFlusher(subscriptionCredentials)
			return service
		},
		func(service *requestlog.Service) telemetry.RequestLogSink {
			return service
		},
		func(service *requestlog.Service) control.RequestLogReader {
			return service
		},
		func(service *requestlog.Service) control.UsageStatReader {
			return service
		},
		func(service *requestlog.Service) control.HomeStatisticsReader {
			return service
		},
		func(service *requestlog.Service) control.RequestLogStatsReader {
			return service
		},
		func(service *requestlog.Service) control.RequestLogCleaner {
			return service
		},
		func(service *requestlog.Service) app.RequestLogRuntime {
			return service
		},
		newRuntimeStateCheckpoint,
		control.NewRuntime,
		func(runtime *control.Runtime) app.ControlRuntime { return runtime },
		httpclient.NewHTTPClientManager,
		newSystemOutboundProxyProvider,
		releasecheck.NewClient,
		releasecheck.NewChecker,
		func(
			manager *httpclient.HTTPClientManager,
			proxyProvider httpclient.OutboundProxyProvider,
		) *catalog.Client {
			return catalog.NewClient(manager, proxyProvider)
		},
		redact.New,
		func(manager *httpclient.HTTPClientManager) *http.Client {
			return manager.GetClient(&httpclient.Config{
				ConnectTimeout:        15 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   20,
				ResponseHeaderTimeout: 120 * time.Second,
				DisableCompression:    true,
				WriteBufferSize:       32 * 1024,
				ReadBufferSize:        32 * 1024,
				ForceAttemptHTTP2:     true,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: time.Second,
			})
		},
		dialect.NewOpenAI,
		dialect.NewOpenAIResponses,
		dialect.NewOpenAIImages,
		dialect.NewOpenAIEmbeddings,
		dialect.NewRerank,
		dialect.NewAnthropic,
		dialect.NewGemini,
		func(
			openAI *dialect.OpenAI,
			openAIResponses *dialect.OpenAIResponses,
			openAIImages *dialect.OpenAIImages,
			openAIEmbeddings *dialect.OpenAIEmbeddings,
			rerank *dialect.Rerank,
			anthropic *dialect.Anthropic,
			gemini *dialect.Gemini,
		) dialect.Set {
			return dialect.NewSet(openAI, openAIResponses, openAIImages, openAIEmbeddings, rerank, anthropic, gemini)
		},
		func(registry *channel.Registry) (*bifrostexecutor.RuntimeManager, error) {
			return bifrostexecutor.NewManagedRuntime(registry)
		},
		func(adapters *provideradapter.Registry, quotaRuntime *accessquota.Runtime, credentials *state.CredentialRegistry) *state.Manager {
			manager := state.NewManager()
			manager.SetSchedulingState(credentials.SchedulingState())
			manager.SetSnapshotReconciler(runtimeSnapshotReconciler{
				adapters: adapters, accessQuota: quotaRuntime,
			})
			return manager
		},
		newSubscriptionRuntime,
		subscription.NewCredentialManager,
		cpaexecutor.NewAdapter,
		newProviderAdapterRegistry,
		func(registry *provideradapter.Registry) execution.Executor { return registry },
		func(runtime *bifrostexecutor.RuntimeManager) app.ExecutionRuntime { return runtime },
		gateway.NewExecutionForwarder,
		func(forwarder *gateway.ExecutionForwarder) gateway.AttemptForwarder { return forwarder },
		gateway.NewHandlerWithLifecycle,
		control.NewService,
		control.NewCatalogSyncCoordinator,
		func(service *control.Service) app.StartupBootstrap { return service },
		func(service *control.Service) app.StartupRecovery { return service },
		func(
			cfg *config.Config,
			service *control.Service,
			checker *releasecheck.Checker,
		) *control.Server {
			return control.NewServerWithReleaseUpdateChecker(cfg, service, checker)
		},
		newHTTPRegistry,
		func(
			db *gorm.DB,
			manager *state.Manager,
			registry *state.CredentialRegistry,
			channelRegistry *channel.Registry,
			subscriptions *subscriptionruntime.Runtime,
			encryptionService encryption.Service,
			quotaRuntime *accessquota.Runtime,
		) app.RuntimeStateLoader {
			return stateloader.NewWithCredentialValidation(
				db,
				manager,
				registry,
				channelRegistry,
				subscriptions,
				encryptionService,
				quotaRuntime,
			)
		},
		app.NewApp,
	}

	for _, provider := range providers {
		if err := dependencyContainer.Provide(provider); err != nil {
			return nil, err
		}
	}
	// The broadcaster is wired after construction, mirroring how the service
	// receives its other coordination collaborators. Single-instance mode
	// leaves it unset and never reaches Redis.
	if err := dependencyContainer.Invoke(func(
		service *control.Service,
		version *coordination.ConfigVersion,
		lease *coordination.Lease,
	) error {
		if version != nil {
			service.SetConfigBroadcaster(version.Bump)
		}
		if lease != nil {
			service.SetTaskLease(lease)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("wire coordination collaborators: %w", err)
	}
	if err := dependencyContainer.Invoke(func(
		engine *gin.Engine,
		registry *httproute.Registry,
		gatewayHandler *gateway.Handler,
	) error {
		engine.Use(gatewayHandler.DownstreamHeadersMiddleware())
		return registry.Bind(engine)
	}); err != nil {
		return nil, fmt.Errorf("register HTTP routes: %w", err)
	}
	return dependencyContainer, nil
}

// newConfigVersion binds the shared configuration version to the coordination
// client. Single-instance mode has no client and therefore no version.
func newConfigVersion(client *coordination.Client) *coordination.ConfigVersion {
	if client == nil {
		return nil
	}
	return coordination.NewConfigVersion(client)
}

// newConfigVersionSource hands the control plane a true nil in single-instance
// mode; a nil *coordination.ConfigVersion stored in an interface is not a nil
// interface value, and the configuration watch loop is assembled on that test.
func newConfigVersionSource(version *coordination.ConfigVersion) control.ConfigVersionSource {
	if version == nil {
		return nil
	}
	return version
}

// newRuntimeStateCheckpoint assembles the best-effort restart checkpoint.
// Distributed mode leaves response ownership out of it: Redis is the source of
// truth there, so the file would capture and restore an index nobody reads.
func newRuntimeStateCheckpoint(
	cfg *config.Config,
	registry *state.CredentialRegistry,
	stats *health.StatsStore,
	responseBindings *state.ResponseBindings,
) app.RuntimeStateCheckpoint {
	if cfg.InstanceMode == config.InstanceModeDistributed {
		responseBindings = nil
	}
	return app.NewFileRuntimeStateCheckpoint(cfg.DataDir, registry, stats, responseBindings)
}

// newTaskLease binds periodic task claims to the coordination client.
// Single-instance mode has no client and therefore claims nothing.
func newTaskLease(client *coordination.Client) (*coordination.Lease, error) {
	if client == nil {
		return nil, nil
	}
	return coordination.NewLease(client)
}

// newControlTaskLease hands the control plane a true nil in single-instance
// mode; a nil *coordination.Lease stored in an interface is not a nil
// interface value, and every claim is decided on that test.
func newControlTaskLease(lease *coordination.Lease) control.TaskLease {
	if lease == nil {
		return nil
	}
	return lease
}

// newResponseBindingStore picks where response ownership lives: the shared
// index when this instance coordinates with peers, the in-process one when it
// stands alone. The in-process index is provided either way because
// single-instance mode still checkpoints it to disk.
func newResponseBindingStore(
	client *coordination.Client,
	local *state.ResponseBindings,
) gateway.ResponseBindingStore {
	if client == nil {
		return gateway.NewLocalResponseBindings(local)
	}
	return coordination.NewResponseBindings(client)
}

func newSystemOutboundProxyProvider(manager *state.Manager) httpclient.OutboundProxyProvider {
	fallback, err := outboundproxy.Resolve(nil, nil, nil, outboundproxy.Environment())
	if err != nil {
		fallback = outboundproxy.Effective{
			Config: outboundproxy.Config{Mode: outboundproxy.ModeDirect},
			Source: outboundproxy.SourceDefault,
		}
	}
	return func() outboundproxy.Effective {
		if manager != nil {
			if snapshot := manager.Current(); snapshot != nil {
				return snapshot.GlobalProxy
			}
		}
		return fallback
	}
}

type runtimeSnapshotReconciler struct {
	adapters    *provideradapter.Registry
	accessQuota *accessquota.Runtime
}

func newSubscriptionRuntime(registry *channel.Registry) (*subscriptionruntime.Runtime, error) {
	return subscriptionruntime.NewRuntime(registry, subscriptionproviders.Implementations()...)
}

func newProviderAdapterRegistry(
	channels *channel.Registry,
	bifrost *bifrostexecutor.RuntimeManager,
	cpa *cpaexecutor.Adapter,
) (*provideradapter.Registry, error) {
	bindings := []provideradapter.Binding{
		{ProviderKind: channel.ProviderOpenAI, Adapter: bifrost},
		{ProviderKind: channel.ProviderAnthropic, Adapter: bifrost},
		{ProviderKind: channel.ProviderGemini, Adapter: bifrost},
		{ProviderKind: channel.ProviderMultiProtocolGateway, Adapter: bifrost},
		{ProviderKind: channel.ProviderOpenAICompatible, Adapter: bifrost},
		{ProviderKind: channel.ProviderAzureOpenAI, Adapter: bifrost},
		{ProviderKind: channel.ProviderAWSBedrock, Adapter: bifrost},
		{ProviderKind: channel.ProviderGoogleVertex, Adapter: bifrost},
		{ProviderKind: channel.ProviderDeepSeek, Adapter: bifrost},
		{ProviderKind: channel.ProviderOpenRouter, Adapter: bifrost},
		{ProviderKind: channel.ProviderGroq, Adapter: bifrost},
		{ProviderKind: channel.ProviderXAI, Adapter: bifrost},
		{ProviderKind: channel.ProviderCodex, Adapter: cpa},
		{ProviderKind: channel.ProviderClaude, Adapter: cpa},
		{ProviderKind: channel.ProviderAntigravity, Adapter: cpa},
		{ProviderKind: channel.ProviderGrok, Adapter: cpa},
	}
	return provideradapter.NewRegistry(channels, bindings)
}

func (reconciler runtimeSnapshotReconciler) ReconcileConfigSnapshot(snapshot *state.ConfigSnapshot) error {
	if reconciler.adapters == nil {
		return fmt.Errorf("reconcile provider runtimes: adapter registry is unavailable")
	}
	if reconciler.accessQuota == nil {
		return fmt.Errorf("reconcile access key cost limits: runtime is unavailable")
	}
	targets := make([]provideradapter.RuntimeTarget, 0)
	if snapshot != nil {
		targets = make([]provideradapter.RuntimeTarget, 0, len(snapshot.Groups))
		for _, group := range snapshot.Groups {
			targets = append(targets, provideradapter.RuntimeTarget{
				Target: group.ResolvedTarget,
				Proxy:  group.Proxy,
			})
		}
	}
	if err := reconciler.adapters.ReconcileTargets(targets); err != nil {
		return err
	}
	return reconciler.accessQuota.Reconcile(snapshot.AccessQuotaDefinitions())
}

func newHTTPRegistry(
	gatewayHandler *gateway.Handler,
	controlServer *control.Server,
	webUIServer *webui.Server,
	coordinationClient app.Coordination,
) (*httproute.Registry, error) {
	return httproute.NewRegistry(
		app.HTTPModule(coordinationClient),
		controlServer.HTTPModule(),
		gatewayHandler.HTTPModule(),
		webUIServer.HTTPModule(),
	)
}

type priceRuntimeProvider struct {
	runtime *control.PriceRuntime
}

func (provider priceRuntimeProvider) Load() *pricing.Table {
	return provider.runtime.Load()
}
