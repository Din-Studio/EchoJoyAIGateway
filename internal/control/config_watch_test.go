package control

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeConfigVersionSource struct {
	mu          sync.Mutex
	version     int64
	err         error
	doorbell    chan struct{}
	reads       chan int64
	released    chan struct{}
	releaseOnce sync.Once
	subscribes  int
}

func newFakeConfigVersionSource(version int64) *fakeConfigVersionSource {
	return &fakeConfigVersionSource{
		version:  version,
		doorbell: make(chan struct{}, 1),
		reads:    make(chan int64, 64),
		released: make(chan struct{}),
	}
}

func (source *fakeConfigVersionSource) Current(context.Context) (int64, error) {
	source.mu.Lock()
	version, err := source.version, source.err
	source.mu.Unlock()
	source.reads <- version
	return version, err
}

func (source *fakeConfigVersionSource) Subscribe(context.Context) (<-chan struct{}, func()) {
	source.mu.Lock()
	source.subscribes++
	source.mu.Unlock()
	return source.doorbell, func() {
		source.releaseOnce.Do(func() { close(source.released) })
	}
}

func (source *fakeConfigVersionSource) set(version int64, err error) {
	source.mu.Lock()
	source.version, source.err = version, err
	source.mu.Unlock()
}

func (source *fakeConfigVersionSource) ring() {
	select {
	case source.doorbell <- struct{}{}:
	default:
	}
}

type fakeConfigReloader struct {
	mu        sync.Mutex
	reloads   int
	retries   int
	err       error
	onReload  func()
	completed chan struct{}
}

func (reloader *fakeConfigReloader) ReloadCommittedConfiguration(context.Context) error {
	reloader.mu.Lock()
	reloader.reloads++
	err, hook := reloader.err, reloader.onReload
	reloader.mu.Unlock()
	if hook != nil {
		hook()
	}
	if reloader.completed != nil {
		reloader.completed <- struct{}{}
	}
	return err
}

func (reloader *fakeConfigReloader) RetryPendingBroadcast(context.Context) {
	reloader.mu.Lock()
	reloader.retries++
	reloader.mu.Unlock()
}

func (reloader *fakeConfigReloader) counts() (int, int) {
	reloader.mu.Lock()
	defer reloader.mu.Unlock()
	return reloader.reloads, reloader.retries
}

func (reloader *fakeConfigReloader) setError(err error) {
	reloader.mu.Lock()
	reloader.err = err
	reloader.mu.Unlock()
}

type configWatchHarness struct {
	source   *fakeConfigVersionSource
	reloader *fakeConfigReloader
	ticker   *fakeRuntimeTicker
	stopped  chan struct{}
}

func newConfigWatchHarness(t *testing.T, version int64) *configWatchHarness {
	t.Helper()
	harness := &configWatchHarness{
		source:   newFakeConfigVersionSource(version),
		reloader: &fakeConfigReloader{completed: make(chan struct{}, 64)},
		ticker:   newFakeRuntimeTicker(),
		stopped:  make(chan struct{}),
	}
	runtime := &Runtime{configVersion: harness.source, configReload: harness.reloader}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(harness.stopped)
		runtime.runConfigWatch(ctx, harness.ticker)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-harness.stopped:
		case <-time.After(5 * time.Second):
			t.Error("config watch did not stop after context cancellation")
		}
	})
	// The loop reads the version once at startup.
	harness.awaitRead(t)
	return harness
}

func (harness *configWatchHarness) awaitRead(t *testing.T) int64 {
	t.Helper()
	select {
	case version := <-harness.source.reads:
		return version
	case <-time.After(5 * time.Second):
		t.Fatal("config watch did not read the shared version")
		return 0
	}
}

// tick drives one loop iteration and returns once the iteration after it has
// started reading, which makes the previous iteration's effects observable.
func (harness *configWatchHarness) tick(t *testing.T) {
	t.Helper()
	harness.ticker.ticks <- time.Now()
	harness.awaitRead(t)
}

// settle drives one extra iteration so the previous one has fully completed.
// It is only valid where that extra iteration is itself a no-op.
func (harness *configWatchHarness) settle(t *testing.T) {
	t.Helper()
	harness.tick(t)
}

// awaitReload waits for one reload attempt to return.
func (harness *configWatchHarness) awaitReload(t *testing.T) {
	t.Helper()
	select {
	case <-harness.reloader.completed:
	case <-time.After(5 * time.Second):
		t.Fatal("config watch did not attempt a reload")
	}
}

func TestConfigWatchReloadsOnlyWhenTheVersionMoves(t *testing.T) {
	t.Parallel()
	harness := newConfigWatchHarness(t, 5)

	harness.tick(t)
	harness.settle(t)
	if reloads, retries := harness.reloader.counts(); reloads != 0 || retries < 1 {
		t.Fatalf("reloads = %d, retries = %d for an unchanged version; want 0 reloads", reloads, retries)
	}

	harness.source.set(6, nil)
	harness.tick(t)
	harness.settle(t)
	if reloads, _ := harness.reloader.counts(); reloads != 1 {
		t.Fatalf("reloads = %d after the version moved, want 1", reloads)
	}

	harness.tick(t)
	harness.settle(t)
	if reloads, _ := harness.reloader.counts(); reloads != 1 {
		t.Fatalf("reloads = %d after reapplying the same version, want 1", reloads)
	}
	if _, retries := harness.reloader.counts(); retries < 4 {
		t.Fatalf("retries = %d, want one pending-broadcast retry per wakeup", retries)
	}
}

func TestConfigWatchReloadsOnDoorbellWithoutWaitingForTheTicker(t *testing.T) {
	t.Parallel()
	harness := newConfigWatchHarness(t, 1)

	harness.source.set(2, nil)
	harness.source.ring()
	if version := harness.awaitRead(t); version != 2 {
		t.Fatalf("doorbell read version = %d, want 2", version)
	}
	harness.settle(t)
	if reloads, _ := harness.reloader.counts(); reloads != 1 {
		t.Fatalf("reloads = %d after a doorbell, want 1", reloads)
	}
}

func TestConfigWatchReloadsWhenTheVersionFallsBack(t *testing.T) {
	t.Parallel()
	harness := newConfigWatchHarness(t, 9)

	// A flushed or rebuilt Redis restarts the counter; this instance still has
	// to reconverge on the database.
	harness.source.set(1, nil)
	harness.tick(t)
	harness.settle(t)
	if reloads, _ := harness.reloader.counts(); reloads != 1 {
		t.Fatalf("reloads = %d after the version fell back, want 1", reloads)
	}
}

func TestConfigWatchRetriesUntilTheReloadSucceeds(t *testing.T) {
	t.Parallel()
	harness := newConfigWatchHarness(t, 1)
	harness.reloader.setError(errors.New("database is unreachable"))

	harness.source.set(2, nil)
	harness.ticker.ticks <- time.Now()
	harness.awaitReload(t)

	// A failed reload must not record the version as applied, so the next
	// wakeup has to try the same version again.
	harness.ticker.ticks <- time.Now()
	harness.awaitReload(t)

	harness.reloader.setError(nil)
	harness.ticker.ticks <- time.Now()
	harness.awaitReload(t)
	if reloads, _ := harness.reloader.counts(); reloads != 3 {
		t.Fatalf("reloads = %d, want 3", reloads)
	}

	// Drain the reads accumulated by the attempts above, then prove the
	// applied version ends the retries.
	for len(harness.source.reads) > 0 {
		<-harness.source.reads
	}
	harness.tick(t)
	harness.settle(t)
	if reloads, _ := harness.reloader.counts(); reloads != 3 {
		t.Fatalf("reloads = %d once the version is applied, want 3", reloads)
	}
}

func TestConfigWatchSeesAVersionCommittedDuringAReload(t *testing.T) {
	t.Parallel()
	harness := newConfigWatchHarness(t, 1)
	harness.reloader.onReload = func() {
		// Another instance commits while this reload is running. Because the
		// version was read before the reload, the newer one must still be
		// treated as outstanding.
		harness.source.set(3, nil)
	}

	harness.source.set(2, nil)
	harness.tick(t)
	harness.settle(t)
	if reloads, _ := harness.reloader.counts(); reloads < 2 {
		t.Fatalf("reloads = %d; a version committed during a reload was swallowed", reloads)
	}
}

func TestConfigWatchDegradesToPollingWhenTheDoorbellCloses(t *testing.T) {
	t.Parallel()
	harness := newConfigWatchHarness(t, 1)

	// A closed doorbell wakes the loop once; it must then stop selecting on it
	// rather than spinning, and keep converging through the ticker.
	close(harness.source.doorbell)
	harness.awaitRead(t)
	harness.source.set(2, nil)
	harness.tick(t)
	harness.settle(t)
	if reloads, _ := harness.reloader.counts(); reloads != 1 {
		t.Fatalf("reloads = %d after degrading to polling, want 1", reloads)
	}
}

func TestConfigWatchReleasesItsSubscriptionOnShutdown(t *testing.T) {
	t.Parallel()
	source := newFakeConfigVersionSource(1)
	reloader := &fakeConfigReloader{}
	runtime := &Runtime{configVersion: source, configReload: reloader}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runtime.runConfigWatch(ctx, newFakeRuntimeTicker())
	}()
	select {
	case <-source.reads:
	case <-time.After(5 * time.Second):
		t.Fatal("config watch did not start")
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("config watch did not return after context cancellation")
	}
	select {
	case <-source.released:
	case <-time.After(5 * time.Second):
		t.Fatal("config watch did not release its subscription")
	}
}

func TestConfigWatchStartsAppliedSoStartupDoesNotReload(t *testing.T) {
	t.Parallel()
	harness := newConfigWatchHarness(t, 7)

	harness.tick(t)
	harness.settle(t)
	if reloads, _ := harness.reloader.counts(); reloads != 0 {
		t.Fatalf("reloads = %d; startup already loaded committed configuration", reloads)
	}
}

func TestConfigWatchReloadsWhenTheStartupVersionReadFails(t *testing.T) {
	t.Parallel()
	source := newFakeConfigVersionSource(0)
	source.set(0, errors.New("redis is unreachable"))
	reloader := &fakeConfigReloader{}
	ticker := newFakeRuntimeTicker()
	runtime := &Runtime{configVersion: source, configReload: reloader}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runtime.runConfigWatch(ctx, ticker)
	}()
	select {
	case <-source.reads:
	case <-time.After(5 * time.Second):
		t.Fatal("config watch did not start")
	}

	// The version stayed unknown, so the first readable version must be
	// applied even though it equals the zero placeholder.
	source.set(0, nil)
	ticker.ticks <- time.Now()
	select {
	case <-source.reads:
	case <-time.After(5 * time.Second):
		t.Fatal("config watch did not re-read the version")
	}
	ticker.ticks <- time.Now()
	select {
	case <-source.reads:
	case <-time.After(5 * time.Second):
		t.Fatal("config watch did not re-read the version")
	}
	if reloads, _ := reloader.counts(); reloads != 1 {
		t.Fatalf("reloads = %d after an unreadable startup version, want 1", reloads)
	}
	cancel()
	<-stopped
}
