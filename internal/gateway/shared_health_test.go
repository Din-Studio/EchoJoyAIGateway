package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"gpt-load/internal/health"
	"gpt-load/internal/state"
)

// recordingHealthStore records store calls and, like the cluster store,
// never runs inside a mutation stripe: it takes the stripe itself to prove
// the caller does not hold it.
type recordingHealthStore struct {
	mu        sync.Mutex
	calls     []string
	threshold int
	err       error
	result    state.SharedHealthResult
	mutations *health.MutationCoordinator
}

func (store *recordingHealthStore) record(call string, ref state.CredentialRef) (state.SharedHealthResult, error) {
	if store.mutations != nil {
		store.mutations.Do(ref.ID, func() {})
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.calls = append(store.calls, call)
	return store.result, store.err
}

func (store *recordingHealthStore) recorded() []string {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]string(nil), store.calls...)
}

func (store *recordingHealthStore) CooldownCredential(_ context.Context, ref state.CredentialRef, _ time.Time, _ uint64) (state.SharedHealthResult, error) {
	return store.record("cooldown", ref)
}

func (store *recordingHealthStore) CooldownModel(_ context.Context, ref state.CredentialRef, _ string, _, _ time.Time) (state.SharedHealthResult, error) {
	return store.record("model_cooldown", ref)
}

func (store *recordingHealthStore) RecordFailure(_ context.Context, ref state.CredentialRef, threshold int) (state.SharedHealthResult, error) {
	store.mu.Lock()
	store.threshold = threshold
	store.mu.Unlock()
	return store.record("fail", ref)
}

func (store *recordingHealthStore) ClearFailure(_ context.Context, ref state.CredentialRef) (state.SharedHealthResult, error) {
	return store.record("clear_failure", ref)
}

func (store *recordingHealthStore) Restore(_ context.Context, ref state.CredentialRef, _, _ bool) (state.SharedHealthResult, error) {
	return store.record("restore", ref)
}

func (store *recordingHealthStore) RecoverIfMatch(_ context.Context, ref state.CredentialRef, _ *time.Time) (state.SharedHealthResult, error) {
	return store.record("recover", ref)
}

func (store *recordingHealthStore) ClearCooldownIfMatch(_ context.Context, ref state.CredentialRef, _ time.Time) (state.SharedHealthResult, error) {
	return store.record("clear_cooldown", ref)
}

func (store *recordingHealthStore) SetAuthState(_ context.Context, ref state.CredentialRef, _ state.CredentialAuthState, _ uint64) (state.SharedHealthResult, error) {
	return store.record("auth", ref)
}

func newSharedHealthHandler(t *testing.T, store *recordingHealthStore) (*Handler, *state.CredentialRegistry) {
	t.Helper()
	registry := state.NewCredentialRegistry()
	registry.EnableSharedHealth()
	if err := registry.ReplaceCredentials([]state.CredentialEntry{{
		ID: 1, GroupID: 1, Version: 1, IdentityGeneration: 1,
		Fingerprint: "test-1", Status: state.CredentialStatusActive, EncryptedValue: "cipher",
	}}); err != nil {
		t.Fatal(err)
	}
	mutations := health.NewMutationCoordinator()
	store.mutations = mutations
	handler := &Handler{
		registry: registry, stats: health.NewStatsStore(), mutations: mutations,
		logger: newGatewayJSONLogger(&discardBuffer{}), sharedHealth: store,
	}
	return handler, registry
}

type discardBuffer struct{}

func (*discardBuffer) Write(value []byte) (int, error) { return len(value), nil }

var sharedHealthRef = state.CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}

func TestHandlerWritesHealthEffectsThroughSharedStore(t *testing.T) {
	now := time.Now()
	store := &recordingHealthStore{result: state.SharedHealthResult{Accepted: true, Changed: true}}
	handler, registry := newSharedHealthHandler(t, store)

	handler.applyDecisionEffect(sharedHealthRef, health.Decision{
		Category: health.FailureCategoryRateLimited, Effect: health.EffectCooldownCredential,
		CooldownUntil: now.Add(time.Minute),
	}, http.StatusTooManyRequests, now)
	handler.applyGroupDecisionEffect(state.GroupView{BlacklistThreshold: 5}, sharedHealthRef, 0, health.Decision{
		Category: health.FailureCategoryInvalidKey, Effect: health.EffectRecordCredentialFailure,
	}, http.StatusUnauthorized, now, "")
	handler.applyDecisionEffect(sharedHealthRef, health.Decision{
		Category: health.FailureCategoryRateLimited, Effect: health.EffectCooldownModel,
		CooldownUntil: now.Add(time.Minute),
	}, http.StatusTooManyRequests, now)
	// A healthy success with no mirrored failures stays off the store.
	handler.recordCredentialSuccess(sharedHealthRef, now)

	want := []string{"cooldown", "fail", "model_cooldown"}
	if got := store.recorded(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("store calls = %v, want %v", got, want)
	}
	if store.threshold != 5 {
		t.Fatalf("RecordFailure threshold = %d, want 5", store.threshold)
	}
	// The store owns the mirror; the handler must not mutate it locally.
	if view := registry.Snapshot()[0]; !view.CooldownUntil.IsZero() || view.FailureCount != 0 {
		t.Fatalf("local registry was mutated: %#v", view)
	}
	if snapshot := handler.stats.Snapshot(1, now); snapshot.Problem != 3 || snapshot.Failure != 1 || snapshot.Success != 1 {
		t.Fatalf("stats did not record the accepted effects: %#v", snapshot)
	}

	// A mirrored failure streak is cleared through the store.
	registry.ApplySharedHealth(1, state.SharedCredentialHealth{Epoch: "e", Version: 1, IdentityGeneration: 1, FailureCount: 2})
	handler.recordCredentialSuccess(sharedHealthRef, now)
	if got := store.recorded(); got[len(got)-1] != "clear_failure" {
		t.Fatalf("store calls = %v, want clear_failure last", got)
	}

	// A result for a moved credential never reaches the store.
	before := len(store.recorded())
	handler.applyDecisionEffect(state.CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 2}, health.Decision{
		Effect: health.EffectCooldownCredential, CooldownUntil: now.Add(time.Minute),
	}, http.StatusTooManyRequests, now)
	if len(store.recorded()) != before {
		t.Fatal("stale target reached the store")
	}
}

func TestHandlerFallsBackToLocalHealthWhenSharedStoreFails(t *testing.T) {
	now := time.Now()
	store := &recordingHealthStore{err: errors.New("redis down")}
	handler, registry := newSharedHealthHandler(t, store)

	handler.applyDecisionEffect(sharedHealthRef, health.Decision{
		Category: health.FailureCategoryRateLimited, Effect: health.EffectCooldownCredential,
		CooldownUntil: now.Add(time.Minute),
	}, http.StatusTooManyRequests, now)
	threshold := state.DefaultRuntimeSettings().BlacklistThreshold
	for range threshold {
		handler.applyDecisionEffect(sharedHealthRef, health.Decision{
			Category: health.FailureCategoryInvalidKey, Effect: health.EffectRecordCredentialFailure,
		}, http.StatusUnauthorized, now)
	}
	view := registry.Snapshot()[0]
	if view.CooldownUntil.IsZero() || !view.Blacklisted || view.FailureCount != threshold {
		t.Fatalf("local fallback view = %#v, want cooldown and blacklist", view)
	}
	handler.recordCredentialSuccess(sharedHealthRef, now)
	if registry.Snapshot()[0].FailureCount != 0 {
		t.Fatal("local fallback did not clear the failure streak")
	}
}

func (store *recordingHealthStore) ReadHealth(context.Context, []uint) (map[uint]state.SharedCredentialHealth, error) {
	return nil, store.err
}

func (store *recordingHealthStore) ReplaceAuthState(_ context.Context, ref state.CredentialRef, _ state.CredentialAuthState, _ uint64, _ state.SharedCredentialHealth) (state.SharedHealthResult, error) {
	return store.record("replace_auth", ref)
}
