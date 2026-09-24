package container

import (
	"testing"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/cluster"
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
