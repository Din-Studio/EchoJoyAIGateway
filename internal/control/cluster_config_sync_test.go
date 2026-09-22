package control

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
)

type fakeClusterReloader struct {
	mu       sync.Mutex
	calls    int
	revision uint64
	block    chan struct{}
	started  chan struct{}
	err      error
}

func (reloader *fakeClusterReloader) reload(context.Context) (uint64, error) {
	reloader.mu.Lock()
	reloader.calls++
	started := reloader.started
	block := reloader.block
	revision := reloader.revision
	err := reloader.err
	reloader.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	return revision, err
}

func (reloader *fakeClusterReloader) count() int {
	reloader.mu.Lock()
	defer reloader.mu.Unlock()
	return reloader.calls
}

func (reloader *fakeClusterReloader) setRevision(revision uint64) {
	reloader.mu.Lock()
	reloader.revision = revision
	reloader.mu.Unlock()
}

func newSyncTestBus(t *testing.T, instanceID string) (*miniredis.Miniredis, *cluster.ConfigEventBus) {
	t.Helper()
	server := miniredis.RunT(t)
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{server.Addr()}, RedisKeyPrefix: "gl", InstanceID: instanceID,
	}})
	if err != nil {
		t.Fatalf("cluster.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return server, cluster.NewConfigEventBus(client)
}

func newTestClusterConfigSync(
	reloader *fakeClusterReloader,
	bus *cluster.ConfigEventBus,
	revision *atomic.Uint64,
	pollInterval time.Duration,
) *ClusterConfigSync {
	return &ClusterConfigSync{
		reload: reloader.reload,
		readRevision: func(context.Context) (uint64, error) {
			return revision.Load(), nil
		},
		bus:          bus,
		pollInterval: pollInterval,
		wake:         make(chan struct{}, 1),
	}
}

func runClusterConfigSync(t *testing.T, coordinator *ClusterConfigSync) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		coordinator.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("ClusterConfigSync.Run did not stop after cancellation")
		}
	})
	return cancel, done
}

func waitForReloadCount(t *testing.T, reloader *fakeClusterReloader, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reloader.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("reload count = %d, want at least %d", reloader.count(), want)
}

func assertReloadCountStays(t *testing.T, reloader *fakeClusterReloader, want int) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if got := reloader.count(); got != want {
		t.Fatalf("reload count = %d, want exactly %d", got, want)
	}
}

func publishAndAwait(t *testing.T, bus *cluster.ConfigEventBus, change cluster.ConfigChange) {
	t.Helper()
	if err := bus.Publish(t.Context(), change); err != nil {
		t.Fatalf("Publish(%#v) error = %v", change, err)
	}
}

// publishUntil republishes change until condition holds, so a test never
// depends on the subscriber being attached before the first publish. Repeated
// deliveries of one revision are idempotent for the coordinator.
func publishUntil(t *testing.T, bus *cluster.ConfigEventBus, change cluster.ConfigChange, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		publishAndAwait(t, bus, change)
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met after publishing %#v", change)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestClusterConfigSyncReloadsOnStartupAndPeerEvents(t *testing.T) {
	_, bus := newSyncTestBus(t, "node-b")
	reloader := &fakeClusterReloader{revision: 1}
	var revision atomic.Uint64
	revision.Store(1)
	coordinator := newTestClusterConfigSync(reloader, bus, &revision, time.Hour)
	runClusterConfigSync(t, coordinator)
	waitForReloadCount(t, reloader, 1)

	// Give the subscriber time to attach; the subscribed hook polls and finds
	// nothing newer, so no extra reload happens.
	assertReloadCountStays(t, reloader, 1)

	reloader.setRevision(2)
	revision.Store(2)
	publishUntil(t, bus, cluster.ConfigChange{Revision: 2, Origin: "node-a"}, func() bool {
		return reloader.count() >= 2
	})
	waitForReloadCount(t, reloader, 2)
	if got := coordinator.lastApplied.Load(); got != 2 {
		t.Fatalf("lastApplied = %d, want 2", got)
	}

	// Stale or self-originated events never trigger a reload.
	publishAndAwait(t, bus, cluster.ConfigChange{Revision: 2, Origin: "node-a"})
	publishAndAwait(t, bus, cluster.ConfigChange{Revision: 1, Origin: "node-a"})
	publishAndAwait(t, bus, cluster.ConfigChange{Revision: 3, Origin: "node-b"})
	assertReloadCountStays(t, reloader, 2)
	if got := coordinator.lastApplied.Load(); got != 3 {
		t.Fatalf("lastApplied after own event = %d, want 3", got)
	}
}

func TestClusterConfigSyncCoalescesEventsDuringReload(t *testing.T) {
	_, bus := newSyncTestBus(t, "node-b")
	reloader := &fakeClusterReloader{revision: 1, block: make(chan struct{}), started: make(chan struct{}, 1)}
	var revision atomic.Uint64
	revision.Store(1)
	coordinator := newTestClusterConfigSync(reloader, bus, &revision, time.Hour)
	runClusterConfigSync(t, coordinator)

	// The startup reload is blocked; unblock it once so the subscriber attaches.
	<-reloader.started
	reloader.block <- struct{}{}
	waitForReloadCount(t, reloader, 1)
	assertReloadCountStays(t, reloader, 1)

	// First peer event starts a reload that blocks; nine more arrive meanwhile.
	publishUntil(t, bus, cluster.ConfigChange{Revision: 2, Origin: "node-a"}, func() bool {
		return reloader.count() >= 2
	})
	<-reloader.started
	for index := range 9 {
		publishAndAwait(t, bus, cluster.ConfigChange{Revision: uint64(3 + index), Origin: "node-a"})
	}
	time.Sleep(100 * time.Millisecond)
	reloader.setRevision(11)
	reloader.block <- struct{}{}
	<-reloader.started
	reloader.block <- struct{}{}
	waitForReloadCount(t, reloader, 3)
	assertReloadCountStays(t, reloader, 3)
}

func TestClusterConfigSyncPollsWhenRevisionAdvancesWithoutEvent(t *testing.T) {
	reloader := &fakeClusterReloader{revision: 1}
	var revision atomic.Uint64
	revision.Store(1)
	coordinator := newTestClusterConfigSync(reloader, nil, &revision, 20*time.Millisecond)
	runClusterConfigSync(t, coordinator)
	waitForReloadCount(t, reloader, 1)
	assertReloadCountStays(t, reloader, 1)

	reloader.setRevision(2)
	revision.Store(2)
	waitForReloadCount(t, reloader, 2)
	assertReloadCountStays(t, reloader, 2)
}

func TestClusterConfigSyncRetriesFailedReload(t *testing.T) {
	reloader := &fakeClusterReloader{revision: 2, err: errors.New("db down")}
	var revision atomic.Uint64
	revision.Store(2)
	coordinator := newTestClusterConfigSync(reloader, nil, &revision, 20*time.Millisecond)
	runClusterConfigSync(t, coordinator)
	waitForReloadCount(t, reloader, 2)
	if got := coordinator.lastApplied.Load(); got != 0 {
		t.Fatalf("lastApplied after failed reloads = %d, want 0", got)
	}

	reloader.mu.Lock()
	reloader.err = nil
	reloader.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for coordinator.lastApplied.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := coordinator.lastApplied.Load(); got != 2 {
		t.Fatalf("lastApplied after recovery = %d, want 2", got)
	}
}

func TestClusterConfigSyncStopsWhenContextCanceled(t *testing.T) {
	_, bus := newSyncTestBus(t, "node-b")
	reloader := &fakeClusterReloader{revision: 1}
	var revision atomic.Uint64
	coordinator := newTestClusterConfigSync(reloader, bus, &revision, time.Hour)
	cancel, done := runClusterConfigSync(t, coordinator)
	waitForReloadCount(t, reloader, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

func TestNewClusterConfigSyncRequiresBus(t *testing.T) {
	fixture := newServiceFixture(t)
	if NewClusterConfigSync(fixture.service, nil) != nil {
		t.Fatal("NewClusterConfigSync(service, nil) must return nil")
	}
	_, bus := newSyncTestBus(t, "node-b")
	coordinator := NewClusterConfigSync(fixture.service, bus)
	if coordinator == nil || coordinator.pollInterval != defaultClusterPollInterval {
		t.Fatalf("NewClusterConfigSync() = %#v", coordinator)
	}
}
