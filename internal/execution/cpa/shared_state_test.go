package cpa

import (
	"testing"
	"time"

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
	quota := cluster.NewAccessQuota(client, clustertest.PublishedQuotaCheckpoints{Manager: manager})
	return gateway.SharedState{
		AccessQuota:           quota,
		Health:                cluster.NewCredentialHealth(client, registry),
		ResponseBindings:      cluster.NewResponseBindings(client, time.Hour),
		Affinity:              cluster.NewAffinity(client),
		CredentialConcurrency: cluster.NewCredentialConcurrency(client),
	}, quota
}
