package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/cluster"
	"gpt-load/internal/state"
	"gpt-load/internal/testutil/clustertest"
)

// testResponseBindingTTL keeps Responses ownership for the length of a test.
const testResponseBindingTTL = time.Hour

// newTestSharedState builds the Redis-backed shared state on a fresh
// miniredis, mirroring credential health into registry.
func newTestSharedState(t testing.TB, registry *state.CredentialRegistry) SharedState {
	t.Helper()
	_, client := clustertest.NewClient(t)
	return newTestSharedStateOn(client, registry)
}

// newTestSharedStateOn builds the shared state on client, for tests that model
// several instances sharing one Redis.
func newTestSharedStateOn(client *cluster.Client, registry *state.CredentialRegistry) SharedState {
	return SharedState{
		AccessQuota:           cluster.NewAccessQuota(client, emptyAccessQuotaStates{}),
		Health:                cluster.NewCredentialHealth(client, registry),
		ResponseBindings:      cluster.NewResponseBindings(client, testResponseBindingTTL),
		Affinity:              cluster.NewAffinity(client),
		CredentialConcurrency: cluster.NewCredentialConcurrency(client),
	}
}

// emptyAccessQuotaStates is a database without cost-limit checkpoints, so
// every rule starts with a fresh window.
type emptyAccessQuotaStates struct{}

func (emptyAccessQuotaStates) ReadAccessQuotaStates(context.Context, []uint) ([]accessquota.RestoredState, error) {
	return nil, nil
}

// unavailableSharedHealth fails every shared health write, so the handler
// takes its local fallback as it does while Redis is unreachable.
func unavailableSharedHealth() *recordingHealthStore {
	return &recordingHealthStore{err: errors.New("redis unavailable")}
}

// assertNoCredentialHealthEffects fails when any credential carries a
// cooldown, a failure streak, or a blacklist.
func assertNoCredentialHealthEffects(t *testing.T, registry *state.CredentialRegistry) {
	t.Helper()
	for _, view := range registry.Snapshot() {
		if !view.CooldownUntil.IsZero() || view.FailureCount != 0 || view.Blacklisted {
			t.Fatalf("credential %d health = %#v, want untouched", view.ID, view)
		}
	}
}

// useSharedAccessQuota gives handler a quota gate on its own miniredis that
// hydrates from the published rules, and returns it for direct setup.
func useSharedAccessQuota(t testing.TB, handler *Handler) *cluster.AccessQuota {
	t.Helper()
	_, client := clustertest.NewClient(t)
	return useSharedAccessQuotaOn(client, handler)
}

// useSharedAccessQuotaOn is useSharedAccessQuota on a Redis several handlers
// share.
func useSharedAccessQuotaOn(client *cluster.Client, handler *Handler) *cluster.AccessQuota {
	quota := cluster.NewAccessQuota(client, clustertest.PublishedQuotaCheckpoints{Manager: handler.manager})
	handler.accessQuota = quota
	return quota
}
