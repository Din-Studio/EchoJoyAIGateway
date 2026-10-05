// Package clustertest owns the in-process Redis that tests outside
// internal/cluster use to build real cluster primitives.
package clustertest

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/state"
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

// PublishedQuotaCheckpoints models the fresh checkpoint rows the control plane
// writes for every cost-limit rule in Manager's published snapshot.
type PublishedQuotaCheckpoints struct {
	Manager *state.Manager
}

func (checkpoints PublishedQuotaCheckpoints) ReadAccessQuotaStates(
	_ context.Context,
	ruleIDs []uint,
) ([]accessquota.RestoredState, error) {
	snapshot := checkpoints.Manager.Current()
	if snapshot == nil {
		return nil, nil
	}
	wanted := make(map[uint]bool, len(ruleIDs))
	for _, id := range ruleIDs {
		wanted[id] = true
	}
	var rows []accessquota.RestoredState
	for accessKeyID, key := range snapshot.AccessKeysByID {
		for _, rule := range key.CostLimitRules {
			if wanted[rule.ID] {
				rows = append(rows, accessquota.RestoredState{
					AccessKeyID: accessKeyID, RuleID: rule.ID, RuleRevision: rule.Revision, SnapshotVersion: 1,
				})
			}
		}
	}
	return rows, nil
}
