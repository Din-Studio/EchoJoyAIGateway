package control

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/state"
)

type recordingBroadcaster struct {
	calls   int
	version int64
	err     error
}

func (broadcaster *recordingBroadcaster) Broadcast(context.Context) (int64, error) {
	broadcaster.calls++
	if broadcaster.err != nil {
		return 0, broadcaster.err
	}
	broadcaster.version++
	return broadcaster.version, nil
}

func TestControlTransactionBroadcastsOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	broadcaster := &recordingBroadcaster{}
	fixture.service.SetConfigBroadcaster(broadcaster.Broadcast)

	if _, err := fixture.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		return tx.Create(validControlGroup("broadcast-commit")).Error
	}, nil); err != nil {
		t.Fatalf("writeConfig() error = %v", err)
	}
	if broadcaster.calls != 1 {
		t.Fatalf("broadcast calls after commit = %d, want 1", broadcaster.calls)
	}

	_, err := fixture.service.writeConfig(t.Context(), func(*gorm.DB) error {
		return errors.New("forced mutation failure")
	}, nil)
	if err == nil {
		t.Fatal("writeConfig() error = nil for a failing mutation")
	}
	if broadcaster.calls != 1 {
		t.Fatalf("broadcast calls after rollback = %d, want 1", broadcaster.calls)
	}
	if fixture.service.broadcastPending.Load() {
		t.Fatal("broadcastPending = true after successful broadcasts")
	}
}

func TestControlTransactionBroadcastsCommittedChangeDespiteLocalPublishFailure(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	broadcaster := &recordingBroadcaster{}
	fixture.service.SetConfigBroadcaster(broadcaster.Broadcast)
	fixture.service.publishSnapshot = func(state.CompileInput) (*state.ConfigSnapshot, error) {
		return nil, errors.New("forced snapshot publication failure")
	}

	_, err := fixture.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		return tx.Create(validControlGroup("committed-but-unpublished")).Error
	}, nil)
	if err == nil {
		t.Fatal("writeConfig() error = nil, want the publication failure")
	}
	// The change is committed, so the other instances must already know about
	// it even though this instance could not apply it locally.
	if broadcaster.calls != 1 {
		t.Fatalf("broadcast calls = %d, want 1", broadcaster.calls)
	}
	assertGroupCount(t, fixture.db, 1)
}

func TestBroadcastFailureKeepsControlWriteSuccessfulAndMarksRetry(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	broadcaster := &recordingBroadcaster{err: errors.New("redis is unreachable")}
	fixture.service.SetConfigBroadcaster(broadcaster.Broadcast)

	if _, err := fixture.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		return tx.Create(validControlGroup("broadcast-failure")).Error
	}, nil); err != nil {
		t.Fatalf("writeConfig() error = %v, want success despite the broadcast failure", err)
	}
	assertGroupCount(t, fixture.db, 1)
	if !fixture.service.broadcastPending.Load() {
		t.Fatal("broadcastPending = false after a failed broadcast")
	}

	fixture.service.RetryPendingBroadcast(t.Context())
	if broadcaster.calls != 2 {
		t.Fatalf("broadcast calls = %d after a failing retry, want 2", broadcaster.calls)
	}
	if !fixture.service.broadcastPending.Load() {
		t.Fatal("broadcastPending = false after a failed retry")
	}

	broadcaster.err = nil
	fixture.service.RetryPendingBroadcast(t.Context())
	if broadcaster.calls != 3 {
		t.Fatalf("broadcast calls = %d after a successful retry, want 3", broadcaster.calls)
	}
	if fixture.service.broadcastPending.Load() {
		t.Fatal("broadcastPending = true after a successful retry")
	}

	fixture.service.RetryPendingBroadcast(t.Context())
	if broadcaster.calls != 3 {
		t.Fatalf("broadcast calls = %d with nothing pending, want 3", broadcaster.calls)
	}
}

func TestControlTransactionWithoutBroadcasterStaysSingleInstance(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)

	if _, err := fixture.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		return tx.Create(validControlGroup("single-instance")).Error
	}, nil); err != nil {
		t.Fatalf("writeConfig() error = %v", err)
	}
	// RetryPendingBroadcast must stay inert rather than reach for a
	// coordination backend this deployment does not have.
	fixture.service.RetryPendingBroadcast(t.Context())
	if fixture.service.broadcastPending.Load() {
		t.Fatal("broadcastPending = true without a broadcaster")
	}
	mustReloadCommittedConfiguration(t, fixture)
	assertSnapshotMatchesDatabase(t, fixture)
}
