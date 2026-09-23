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
