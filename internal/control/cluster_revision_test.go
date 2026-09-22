package control

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/cluster"
	"gpt-load/internal/storage/models"
)

type recordingConfigEventPublisher struct {
	mu       sync.Mutex
	instance string
	changes  []cluster.ConfigChange
	err      error
}

func (publisher *recordingConfigEventPublisher) Publish(_ context.Context, change cluster.ConfigChange) error {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	publisher.changes = append(publisher.changes, change)
	return publisher.err
}

func (publisher *recordingConfigEventPublisher) InstanceID() string {
	return publisher.instance
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
	publisher := &recordingConfigEventPublisher{instance: "node-a"}
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
		if change.Revision != uint64(index+1) || change.Origin != "node-a" {
			t.Fatalf("change[%d] = %#v, want revision %d origin node-a", index, change, index+1)
		}
	}
}

func TestControlTransactionDoesNotBumpOrPublishOnFailure(t *testing.T) {
	fixture := newServiceFixture(t)
	publisher := &recordingConfigEventPublisher{instance: "node-a"}
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
	publisher := &recordingConfigEventPublisher{instance: "node-a", err: errors.New("redis down")}
	fixture.service.clusterEvents = publisher

	createGroupWithCredentials(t, fixture, "sk-first")
	if value, ok := readStoredClusterRevision(t, fixture.db); !ok || value != "1" {
		t.Fatalf("revision = %q (present=%v), want 1 despite publish failure", value, ok)
	}
}

func TestControlTransactionWithoutClusterLeavesNoRevisionRow(t *testing.T) {
	fixture := newServiceFixture(t)

	createGroupWithCredentials(t, fixture, "sk-first")
	if _, ok := readStoredClusterRevision(t, fixture.db); ok {
		t.Fatal("single-instance mode must not write the cluster revision row")
	}
	revision, err := readClusterConfigRevision(t.Context(), fixture.db)
	if err != nil || revision != 0 {
		t.Fatalf("readClusterConfigRevision() = %d, %v; want 0", revision, err)
	}
}
