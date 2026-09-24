// Package container assembles the 2.0 dependency graph with dig.
package container

import (
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
	"gpt-load/internal/cluster"
	"gpt-load/internal/control"
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

// BuildContainer creates the 2.0 runtime foundation dependency graph.
func BuildContainer() (*dig.Container, error) {
	dependencyContainer := dig.New()

	providers := []any{
		config.Load,
		func(cfg *config.Config) (encryption.Service, error) {
			return encryption.NewServiceWithKeyFile(cfg.EncryptionKey, cfg.DataDir)
		},
		func(cfg *config.Config) (*gorm.DB, error) {
			db, err := storage.OpenConfigured(cfg)
			if err == nil {
				logrus.WithField("event", "startup.database_open").Info("database opened")
			}
			return db, err
		},
		cluster.NewClient,
		cluster.NewConfigEventBus,
		control.NewClusterConfigSync,
		app.NewReadinessProbe,
		httplifecycle.NewCoordinator,
		app.NewEngineWithLifecycle,
		webui.NewServer,
		newCredentialRegistry,
		cluster.NewCredentialHealth,
		cluster.NewRefreshLease,
		newSharedCredentialHealthStore,
		state.NewResponseBindings,
		newAccessQuotaRuntime,
		func(client *cluster.Client, db *gorm.DB) *cluster.AccessQuota {
			return cluster.NewAccessQuota(client, requestlog.AccessQuotaStateReader{DB: db})
		},
		newAccessQuotaGate,
		channel.CompileRegistry,
		control.NewPriceRuntime,
		control.NewCatalogBootstrap,
		func(bootstrap *control.CatalogBootstrap) *catalog.Runtime { return bootstrap.Runtime },
		health.NewStatsStore,
		health.NewMutationCoordinator,
		ratelimit.NewAccessKeyRPM,
		newAccessKeyRPMLimiter,
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
			sharedQuota *cluster.AccessQuota,
			subscriptionCredentials *subscription.CredentialManager,
		) *requestlog.Service {
			service := requestlog.NewService(db, redactor, retention, quotaRuntime)
			if sharedQuota != nil {
				service.SetAccessQuotaCheckpointSource(sharedQuota)
			}
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
		func(
			cfg *config.Config,
			registry *state.CredentialRegistry,
			stats *health.StatsStore,
			responseBindings *state.ResponseBindings,
			credentialHealth *cluster.CredentialHealth,
		) app.RuntimeStateCheckpoint {
			return app.NewFileRuntimeStateCheckpoint(
				cfg.DataDir, registry, stats, responseBindings, newCredentialHealthHydrator(credentialHealth),
			)
		},
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
	if err := dependencyContainer.Invoke(coordinateSubscriptionRefresh); err != nil {
		return nil, fmt.Errorf("coordinate subscription refresh: %w", err)
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

// newAccessQuotaRuntime owns in-process quota state only in single-instance
// mode; cluster mode keeps it in Redis, so loaders and checkpoints skip it.
func newAccessQuotaRuntime(client *cluster.Client) *accessquota.Runtime {
	if client != nil {
		return nil
	}
	return accessquota.NewRuntime()
}

// newAccessQuotaGate selects the shared Redis gate in cluster mode and the
// snapshot-pinned in-process gate otherwise, never boxing a nil pointer.
func newAccessQuotaGate(
	shared *cluster.AccessQuota,
	manager *state.Manager,
	runtime *accessquota.Runtime,
) gateway.AccessQuotaGate {
	if shared != nil {
		return shared
	}
	return gateway.NewLocalAccessQuotaGate(manager, runtime)
}

// newCredentialRegistry mirrors cluster-shared credential health when
// cluster mode is enabled, before any credential is loaded.
func newCredentialRegistry(client *cluster.Client) *state.CredentialRegistry {
	registry := state.NewCredentialRegistry()
	if client != nil {
		registry.EnableSharedHealth()
	}
	return registry
}

// newSharedCredentialHealthStore exposes the Redis health store in cluster
// mode and nil otherwise, never boxing a nil pointer.
func newSharedCredentialHealthStore(shared *cluster.CredentialHealth) state.SharedCredentialHealthStore {
	if shared == nil {
		return nil
	}
	return shared
}

// newCredentialHealthHydrator restores health from Redis instead of the
// checkpoint file in cluster mode.
func newCredentialHealthHydrator(shared *cluster.CredentialHealth) app.CredentialHealthHydrator {
	if shared == nil {
		return nil
	}
	return shared
}

// coordinateSubscriptionRefresh makes subscription refreshes cluster-wide
// single flight; it leaves the manager untouched in single-instance mode.
func coordinateSubscriptionRefresh(
	credentials *subscription.CredentialManager,
	lease *cluster.RefreshLease,
	shared *cluster.CredentialHealth,
	service *control.Service,
) {
	if lease == nil || shared == nil {
		return
	}
	credentials.SetClusterCoordination(lease, shared, service)
}

// newAccessKeyRPMLimiter selects the shared Redis window in cluster mode and
// the in-process window otherwise.
func newAccessKeyRPMLimiter(client *cluster.Client, local *ratelimit.AccessKeyRPM) gateway.AccessKeyRPMLimiter {
	if shared := cluster.NewAccessKeyRPM(client); shared != nil {
		return shared
	}
	return local
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
	if reconciler.accessQuota == nil {
		// Cluster mode keeps quota state in Redis; state.Compile has already
		// validated the rule definitions.
		return nil
	}
	return reconciler.accessQuota.Reconcile(snapshot.AccessQuotaDefinitions())
}

func newHTTPRegistry(
	gatewayHandler *gateway.Handler,
	controlServer *control.Server,
	webUIServer *webui.Server,
	probe app.ReadinessProbe,
) (*httproute.Registry, error) {
	return httproute.NewRegistry(
		app.HTTPModule(probe),
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
