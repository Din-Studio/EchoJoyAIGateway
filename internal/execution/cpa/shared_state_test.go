package cpa

import (
	"context"
	"testing"
	"time"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/cluster"
	"gpt-load/internal/gateway"
	"gpt-load/internal/state"
	"gpt-load/internal/testutil/clustertest"
)

// newGatewaySharedState builds the gateway's Redis-backed shared state on a
// fresh miniredis, hydrating cost-limit rules from the manager's published
// snapshot.
func newGatewaySharedState(
	t *testing.T,
	manager *state.Manager,
	registry *state.CredentialRegistry,
) (gateway.SharedState, *cluster.AccessQuota) {
	t.Helper()
	_, client := clustertest.NewClient(t)
	quota := cluster.NewAccessQuota(client, publishedQuotaCheckpoints{manager: manager})
	return gateway.SharedState{
		AccessQuota:      quota,
		Health:           cluster.NewCredentialHealth(client, registry),
		ResponseBindings: cluster.NewResponseBindings(client, time.Hour),
		Affinity:         cluster.NewAffinity(client),
	}, quota
}

// publishedQuotaCheckpoints models the fresh checkpoint rows the control
// plane writes for every published cost-limit rule.
type publishedQuotaCheckpoints struct {
	manager *state.Manager
}

func (checkpoints publishedQuotaCheckpoints) ReadAccessQuotaStates(
	_ context.Context,
	ruleIDs []uint,
) ([]accessquota.RestoredState, error) {
	snapshot := checkpoints.manager.Current()
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
