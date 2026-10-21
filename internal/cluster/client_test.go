package cluster

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/platform/config"
)

func clusterTestConfig(t *testing.T, addr string) *config.Config {
	t.Helper()
	return &config.Config{
		Cluster: config.ClusterConfig{
			RedisAddrs:     []string{addr},
			RedisKeyPrefix: "gl",
			InstanceID:     "node-test",
		},
	}
}

func newTestClient(t *testing.T) (*miniredis.Miniredis, *Client) {
	t.Helper()
	server := miniredis.RunT(t)
	client, err := NewClient(clusterTestConfig(t, server.Addr()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return server, client
}

func TestNewClientFailsWhenRedisUnreachable(t *testing.T) {
	server := miniredis.RunT(t)
	addr := server.Addr()
	server.Close()

	client, err := NewClient(clusterTestConfig(t, addr))
	if err == nil {
		_ = client.Close()
		t.Fatal("NewClient() succeeded against a closed Redis")
	}
}

func TestClientKeyAndInstanceID(t *testing.T) {
	_, client := newTestClient(t)
	if got := client.Key("events"); got != "gl:events" {
		t.Fatalf("Key(events) = %q, want gl:events", got)
	}
	if got := client.Key("a", "b"); got != "gl:a:b" {
		t.Fatalf("Key(a,b) = %q, want gl:a:b", got)
	}
	if got := client.InstanceID(); got != "node-test" {
		t.Fatalf("InstanceID() = %q, want node-test", got)
	}
	if err := client.Ping(t.Context()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
}

func TestNewClientKeepsMinIdleConnsOpen(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := clusterTestConfig(t, server.Addr())
	cfg.Cluster.RedisMinIdleConns = 4
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	deadline := time.Now().Add(2 * time.Second)
	for client.PoolStats().IdleConns < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("idle connections = %d, want at least 4", client.PoolStats().IdleConns)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
