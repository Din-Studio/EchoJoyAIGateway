package control

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/cluster"
	"gpt-load/internal/storage/models"
)

type recordingConfigEventPublisher struct {
	mu      sync.Mutex
	changes []cluster.ConfigChange
	err     error
}

func (publisher *recordingConfigEventPublisher) Publish(_ context.Context, change cluster.ConfigChange) error {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	publisher.changes = append(publisher.changes, change)
	return publisher.err
}

func (publisher *recordingConfigEventPublisher) published() []cluster.ConfigChange {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	return append([]cluster.ConfigChange(nil), publisher.changes...)
}

func readStoredClusterRevision(t *testing.T, db *gorm.DB) (string, bool) {
	t.Helper()
	var setting models.SystemSetting
	err := db.Where("key = ?", clusterConfigRevisionKey).Take(&setting).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read cluster revision: %v", err)
	}
	return setting.Value, true
}

func TestControlTransactionBumpsClusterRevisionAndPublishes(t *testing.T) {
	fixture := newServiceFixture(t)
	publisher := &recordingConfigEventPublisher{}
	fixture.service.clusterEvents = publisher

	groupID := createGroupWithCredentials(t, fixture, "sk-first")
	if value, ok := readStoredClusterRevision(t, fixture.db); !ok || value != "1" {
		t.Fatalf("revision after first write = %q (present=%v), want 1", value, ok)
	}
	if err := fixture.service.DeleteGroup(t.Context(), groupID); err != nil {
		t.Fatalf("DeleteGroup() error = %v", err)
	}
	if value, ok := readStoredClusterRevision(t, fixture.db); !ok || value != "2" {
		t.Fatalf("revision after second write = %q (present=%v), want 2", value, ok)
	}
	revision, err := readClusterConfigRevision(t.Context(), fixture.db)
	if err != nil || revision != 2 {
		t.Fatalf("readClusterConfigRevision() = %d, %v; want 2", revision, err)
	}

	changes := publisher.published()
	if len(changes) != 2 {
		t.Fatalf("published %d changes, want 2: %#v", len(changes), changes)
	}
	for index, change := range changes {
		if change.Revision != uint64(index+1) {
			t.Fatalf("change[%d] = %#v, want revision %d", index, change, index+1)
		}
	}
}

func TestControlTransactionDoesNotBumpOrPublishOnFailure(t *testing.T) {
	fixture := newServiceFixture(t)
	publisher := &recordingConfigEventPublisher{}
	fixture.service.clusterEvents = publisher

	boom := errors.New("boom")
	err := fixture.service.withControlTransaction(t.Context(), func(*gorm.DB) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("withControlTransaction() error = %v, want boom", err)
	}
	if _, ok := readStoredClusterRevision(t, fixture.db); ok {
		t.Fatal("failed transaction must not create the revision row")
	}
	if changes := publisher.published(); len(changes) != 0 {
		t.Fatalf("failed transaction published %#v", changes)
	}
}

func TestControlTransactionPublishFailureDoesNotFailWrite(t *testing.T) {
	fixture := newServiceFixture(t)
	publisher := &recordingConfigEventPublisher{err: errors.New("redis down")}
	fixture.service.clusterEvents = publisher

	createGroupWithCredentials(t, fixture, "sk-first")
	if value, ok := readStoredClusterRevision(t, fixture.db); !ok || value != "1" {
		t.Fatalf("revision = %q (present=%v), want 1 despite publish failure", value, ok)
	}
}

func TestBookkeepingTransactionsDoNotBumpOrPublish(t *testing.T) {
	fixture := newServiceFixture(t)
	publisher := &recordingConfigEventPublisher{}
	fixture.service.clusterEvents = publisher
	fixture.service.now = func() time.Time {
		return time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	}

	// The group commit bumps once; the stage advances that follow do not.
	mutations := 0
	input := newDurableGroupOperationInput(t, fixture, "3c0a8b54-6f1d-4e2a-9b7c-0123456789ab", &mutations)
	if _, err := fixture.service.executeIdempotentOperation(t.Context(), input); err != nil {
		t.Fatalf("executeIdempotentOperation() error = %v", err)
	}
	var operation models.ControlOperation
	if err := fixture.db.Take(&operation).Error; err != nil {
		t.Fatalf("read operation: %v", err)
	}
	if operation.LastCompletedStage != string(operationStageCompleted) {
		t.Fatalf("operation stage = %q, want completed", operation.LastCompletedStage)
	}
	assertClusterRevision(t, fixture, publisher, 1)

	if err := fixture.service.recordOperationFailureLocked(
		t.Context(), &operation, operationStageSnapshotPublished,
	); err != nil {
		t.Fatalf("recordOperationFailureLocked() error = %v", err)
	}
	compacted, err := fixture.service.CompactCompletedOperations(
		t.Context(), time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC),
	)
	if err != nil || compacted != 1 {
		t.Fatalf("CompactCompletedOperations() = %d, %v; want 1", compacted, err)
	}
	assertClusterRevision(t, fixture, publisher, 1)

	groupID, err := strconv.ParseUint(strings.TrimPrefix(operation.ResourceIdentity, "group:"), 10, 64)
	if err != nil {
		t.Fatalf("parse resource identity %q: %v", operation.ResourceIdentity, err)
	}
	if err := fixture.service.DeleteGroup(t.Context(), uint(groupID)); err != nil {
		t.Fatalf("DeleteGroup() error = %v", err)
	}
	assertClusterRevision(t, fixture, publisher, 2)
}

func assertClusterRevision(
	t *testing.T,
	fixture serviceFixture,
	publisher *recordingConfigEventPublisher,
	want uint64,
) {
	t.Helper()
	revision, err := readClusterConfigRevision(t.Context(), fixture.db)
	if err != nil || revision != want {
		t.Fatalf("stored revision = %d, %v; want %d", revision, err, want)
	}
	changes := publisher.published()
	if len(changes) != int(want) || changes[len(changes)-1].Revision != want {
		t.Fatalf("published %#v, want %d changes ending at revision %d", changes, want, want)
	}
}
