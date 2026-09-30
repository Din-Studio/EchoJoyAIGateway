package container

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/affinity"
	"gpt-load/internal/cluster"
	"gpt-load/internal/control"
	"gpt-load/internal/gateway"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/ratelimit"
	"gpt-load/internal/requestlog"
	"gpt-load/internal/state"
)

func TestClusterModeSelectsSharedLimitState(t *testing.T) {
	server := miniredis.RunT(t)
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{server.Addr()}, RedisKeyPrefix: "gl", InstanceID: "container-test",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	manager := state.NewManager()

	if runtime := newAccessQuotaRuntime(client); runtime != nil {
		t.Fatal("cluster mode must not create the in-process quota runtime")
	}
	local := ratelimit.NewAccessKeyRPM()
	if _, ok := newAccessKeyRPMLimiter(client, local).(*cluster.AccessKeyRPM); !ok {
		t.Fatal("cluster mode RPM limiter is not the shared Redis implementation")
	}
	shared := cluster.NewAccessQuota(client, requestlog.AccessQuotaStateReader{})
	if gate, ok := newAccessQuotaGate(shared, manager, nil).(*cluster.AccessQuota); !ok || gate != shared {
		t.Fatal("cluster mode quota gate is not the shared Redis implementation")
	}
}

func TestSingleInstanceModeSelectsInProcessLimitState(t *testing.T) {
	runtime := newAccessQuotaRuntime(nil)
	if runtime == nil {
		t.Fatal("single-instance mode must create the in-process quota runtime")
	}
	local := ratelimit.NewAccessKeyRPM()
	if limiter := newAccessKeyRPMLimiter(nil, local); limiter != local {
		t.Fatalf("single-instance RPM limiter = %T, want the in-process singleton", limiter)
	}
	gate := newAccessQuotaGate(nil, state.NewManager(), runtime)
	if gate == nil {
		t.Fatal("single-instance quota gate is nil")
	}
	if _, shared := gate.(*cluster.AccessQuota); shared {
		t.Fatal("single-instance mode selected the shared quota gate")
	}
	var _ gateway.AccessQuotaGate = gate
}

func TestClusterModeSelectsSharedCredentialHealth(t *testing.T) {
	server := miniredis.RunT(t)
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{server.Addr()}, RedisKeyPrefix: "gl", InstanceID: "container-test",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	registry := newCredentialRegistry(client)
	shared := cluster.NewCredentialHealth(client, registry)
	if store, ok := newSharedCredentialHealthStore(shared).(*cluster.CredentialHealth); !ok || store != shared {
		t.Fatal("cluster mode health store is not the shared Redis implementation")
	}
	if hydrator := newCredentialHealthHydrator(shared); hydrator == nil {
		t.Fatal("cluster mode checkpoint does not hydrate from Redis")
	}
	// Shared mode keeps mirrored health across a peer reload.
	entry := state.CredentialEntry{
		ID: 1, GroupID: 1, Version: 1, IdentityGeneration: 1, Fingerprint: "f",
		Status: state.CredentialStatusActive, EncryptedValue: "c",
	}
	if err := registry.ReplaceCredentials([]state.CredentialEntry{entry}); err != nil {
		t.Fatal(err)
	}
	registry.SetBlacklisted(1)
	entry.Status = state.CredentialStatusDisabled
	if _, err := registry.ReconcileGroup(1, []state.CredentialEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if !registry.Snapshot()[0].Blacklisted {
		t.Fatal("cluster registry dropped health on reload")
	}
}

func TestSingleInstanceModeKeepsCredentialHealthLocal(t *testing.T) {
	if store := newSharedCredentialHealthStore(nil); store != nil {
		t.Fatalf("single-instance health store = %T, want nil interface", store)
	}
	if hydrator := newCredentialHealthHydrator(nil); hydrator != nil {
		t.Fatalf("single-instance hydrator = %T, want nil interface", hydrator)
	}
	if cluster.NewCredentialHealth(nil, state.NewCredentialRegistry()) != nil || cluster.NewRefreshLease(nil) != nil {
		t.Fatal("single-instance mode created cluster health objects")
	}
	// Coordination is skipped without a lease; a nil manager would panic.
	coordinateSubscriptionRefresh(nil, nil, nil, nil)
}

func TestClusterModeSharesResponseOwnershipAndAffinity(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := &config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{server.Addr()}, RedisKeyPrefix: "gl", InstanceID: "container-test",
		ResponseBindingTTL: time.Hour,
	}}
	client, err := cluster.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	local := newResponseBindings(client)
	if local != nil {
		t.Fatal("cluster mode created the in-process ownership index")
	}
	if _, ok := newResponseBindingStore(cfg, client, local).(*cluster.ResponseBindings); !ok {
		t.Fatal("cluster mode ownership store is not the shared Redis implementation")
	}
	if _, ok := newAffinityStore(client).(*cluster.Affinity); !ok {
		t.Fatal("cluster mode affinity store is not the shared Redis implementation")
	}
}

func TestSingleInstanceModeKeepsResponseOwnershipAndAffinityLocal(t *testing.T) {
	cfg := &config.Config{}
	local := newResponseBindings(nil)
	if local == nil {
		t.Fatal("single-instance mode has no in-process ownership index")
	}
	if store, ok := newResponseBindingStore(cfg, nil, local).(*state.ResponseBindings); !ok || store != local {
		t.Fatal("single-instance ownership store is not the checkpointed in-process index")
	}
	if _, ok := newAffinityStore(nil).(*affinity.Cache); !ok {
		t.Fatal("single-instance affinity store is not the in-process cache")
	}
}

func TestClusterModeSharesBackgroundJobsCatalogAndAdminLockout(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := &config.Config{DataDir: t.TempDir(), Cluster: config.ClusterConfig{
		RedisAddrs: []string{server.Addr()}, RedisKeyPrefix: "gl", InstanceID: "container-test",
	}}
	client, err := cluster.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if cluster.NewJobLease(client) == nil || cluster.NewAuthFailures(client) == nil {
		t.Fatal("cluster mode did not create the shared job lease or admin lockout")
	}
	shared := cluster.NewCatalogStore(client)
	if shared == nil {
		t.Fatal("cluster mode did not create the shared catalog store")
	}
	bootstrap := control.NewCatalogBootstrap(cfg, shared)
	if bootstrap.CachePath != "" || bootstrap.HasLKG {
		t.Fatalf("cluster bootstrap = path %q, LKG %t; want the empty shared catalog, not a cache file",
			bootstrap.CachePath, bootstrap.HasLKG)
	}
}

func TestSingleInstanceModeKeepsBackgroundJobsCatalogAndAdminLockoutLocal(t *testing.T) {
	if cluster.NewJobLease(nil) != nil || cluster.NewCatalogStore(nil) != nil || cluster.NewAuthFailures(nil) != nil {
		t.Fatal("single-instance mode created shared background job, catalog, or lockout state")
	}
	cfg := &config.Config{DataDir: t.TempDir()}
	if bootstrap := control.NewCatalogBootstrap(cfg, nil); bootstrap.CachePath == "" {
		t.Fatal("single-instance bootstrap does not use the DATA_DIR cache file")
	}
}
