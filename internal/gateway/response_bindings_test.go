package gateway

import (
	"context"
	"sync"
	"testing"

	"gpt-load/internal/automodel"
	"gpt-load/internal/state"
)

// countingResponseBindingStore delegates to a real in-process index so
// ordinary ownership semantics stay intact, while counting the calls and
// letting a test make the backend unavailable.
type countingResponseBindingStore struct {
	mu        sync.Mutex
	delegate  *LocalResponseBindings
	lookups   int
	records   int
	lookupErr error
	recordErr error
}

func newCountingResponseBindingStore() *countingResponseBindingStore {
	return &countingResponseBindingStore{delegate: NewLocalResponseBindings(state.NewResponseBindings())}
}

func (store *countingResponseBindingStore) Lookup(
	ctx context.Context,
	accessKeyID uint,
	responseID string,
) (state.ResponseBinding, bool, error) {
	store.mu.Lock()
	store.lookups++
	err := store.lookupErr
	store.mu.Unlock()
	if err != nil {
		return state.ResponseBinding{}, false, err
	}
	return store.delegate.Lookup(ctx, accessKeyID, responseID)
}

func (store *countingResponseBindingStore) Record(
	ctx context.Context,
	accessKeyID uint,
	responseID string,
	ref state.CredentialRef,
	auto *automodel.Selection,
) (bool, error) {
	store.mu.Lock()
	store.records++
	err := store.recordErr
	store.mu.Unlock()
	if err != nil {
		return false, err
	}
	return store.delegate.Record(ctx, accessKeyID, responseID, ref, auto)
}

func (store *countingResponseBindingStore) counts() (int, int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.lookups, store.records
}

func (store *countingResponseBindingStore) failLookups(err error) {
	store.mu.Lock()
	store.lookupErr = err
	store.mu.Unlock()
}

func (store *countingResponseBindingStore) failRecords(err error) {
	store.mu.Lock()
	store.recordErr = err
	store.mu.Unlock()
}

func recordTestBinding(
	t *testing.T,
	handler *Handler,
	accessKeyID uint,
	responseID string,
	ref state.CredentialRef,
	auto *automodel.Selection,
) bool {
	t.Helper()
	recorded, err := handler.responseBindings.Record(t.Context(), accessKeyID, responseID, ref, auto)
	if err != nil {
		t.Fatalf("Record(%q) error = %v", responseID, err)
	}
	return recorded
}

func lookupTestBinding(
	t *testing.T,
	handler *Handler,
	accessKeyID uint,
	responseID string,
) (state.ResponseBinding, bool) {
	t.Helper()
	binding, found, err := handler.responseBindings.Lookup(t.Context(), accessKeyID, responseID)
	if err != nil {
		t.Fatalf("Lookup(%q) error = %v", responseID, err)
	}
	return binding, found
}

// handlerLocalBindings reaches the in-process index a default handler owns, so
// a test can drive the runtime checkpoint that persists it.
func handlerLocalBindings(t *testing.T, handler *Handler) *state.ResponseBindings {
	t.Helper()
	local, ok := handler.responseBindings.(*LocalResponseBindings)
	if !ok {
		t.Fatalf("response binding store = %T, want the in-process adapter", handler.responseBindings)
	}
	return local.bindings
}
