// Package clustertest owns the in-process Redis that tests outside
// internal/cluster use to build real cluster primitives.
package clustertest

import (
	"testing"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
)

// InstanceID is the cluster identity NewClient connects with.
const InstanceID = "node-test"

// NewClient starts a miniredis server and connects one cluster client to it.
func NewClient(t testing.TB) (*miniredis.Miniredis, *cluster.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	return server, Connect(t, server, InstanceID)
}

// Connect joins another instance to server, for tests that model several
// gateway instances sharing one Redis.
func Connect(t testing.TB, server *miniredis.Miniredis, instanceID string) *cluster.Client {
	t.Helper()
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{server.Addr()}, RedisKeyPrefix: "gl", InstanceID: instanceID,
	}})
	if err != nil {
		t.Fatalf("cluster.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
