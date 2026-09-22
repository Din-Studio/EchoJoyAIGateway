package cluster

import (
	"testing"

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
	if client == nil {
		t.Fatal("NewClient() = nil for enabled cluster config")
	}
	t.Cleanup(func() { _ = client.Close() })
	return server, client
}

func TestNewClientReturnsNilWhenClusterDisabled(t *testing.T) {
	client, err := NewClient(&config.Config{})
	if err != nil || client != nil {
		t.Fatalf("NewClient(disabled) = %v, %v; want nil, nil", client, err)
	}
	client, err = NewClient(nil)
	if err != nil || client != nil {
		t.Fatalf("NewClient(nil) = %v, %v; want nil, nil", client, err)
	}
	if NewConfigEventBus(nil) != nil {
		t.Fatal("NewConfigEventBus(nil) must return nil")
	}
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
