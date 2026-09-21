package gateway

import (
	"context"
	"errors"

	"gpt-load/internal/automodel"
	"gpt-load/internal/state"
)

// errCoordinationUnavailable marks an ownership decision the gateway could not
// make, as opposed to one it made. It keeps a coordination outage from being
// reported to the client as a missing or conflicting response id, and keeps it
// off the credential's health record: the failure is the gateway's own.
var errCoordinationUnavailable = errors.New("coordination backend unavailable")

// ResponseBindingStore resolves and establishes which credential owns a
// response id. It is exported because the container chooses the
// implementation: the in-process index when this instance stands alone, the
// shared one when it coordinates with peers.
//
// A returned error means the answer is unknown. Callers must fail closed on
// it; only (_, false, nil) is an actual absence, and only (false, nil) is an
// actual conflict.
type ResponseBindingStore interface {
	Lookup(ctx context.Context, accessKeyID uint, responseID string) (state.ResponseBinding, bool, error)
	Record(
		ctx context.Context,
		accessKeyID uint,
		responseID string,
		ref state.CredentialRef,
		auto *automodel.Selection,
	) (bool, error)
}

// LocalResponseBindings adapts the in-process ownership index to the store
// interface. Every answer is authoritative and immediate, so no call can fail
// and the context is unused.
type LocalResponseBindings struct {
	bindings *state.ResponseBindings
}

// NewLocalResponseBindings wraps the in-process index. A nil index keeps the
// tolerance the index itself defines: lookups miss and recordings are refused.
func NewLocalResponseBindings(bindings *state.ResponseBindings) *LocalResponseBindings {
	return &LocalResponseBindings{bindings: bindings}
}

func (local *LocalResponseBindings) Lookup(
	_ context.Context,
	accessKeyID uint,
	responseID string,
) (state.ResponseBinding, bool, error) {
	binding, found := local.bindings.Lookup(accessKeyID, responseID)
	return binding, found, nil
}

func (local *LocalResponseBindings) Record(
	_ context.Context,
	accessKeyID uint,
	responseID string,
	ref state.CredentialRef,
	auto *automodel.Selection,
) (bool, error) {
	return local.bindings.Record(accessKeyID, responseID, ref, auto), nil
}
