package control

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/cluster"
	"gpt-load/internal/testutil/clustertest"
)

type countingRetentionCleaner struct {
	sweeps      atomic.Int32
	stages      atomic.Int32
	compactions atomic.Int32
}

func (cleaner *countingRetentionCleaner) Sweep(context.Context, time.Time) { cleaner.sweeps.Add(1) }

func (cleaner *countingRetentionCleaner) CleanupCredentialStages(context.Context, time.Time) error {
	cleaner.stages.Add(1)
	return nil
}

func (cleaner *countingRetentionCleaner) CompactCompletedOperations(context.Context, time.Time) (int64, error) {
	cleaner.compactions.Add(1)
	return 0, nil
}

type countingValidationSweep struct{ calls atomic.Int32 }

func (sweep *countingValidationSweep) Validate(context.Context) { sweep.calls.Add(1) }

func TestClusterRetentionRunsOncePerPeriodAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	cleaner := &countingRetentionCleaner{}
	runtimes := make([]*Runtime, 3)
	for index := range runtimes {
		runtimes[index] = &Runtime{
			requestLogCleaner: cleaner,
			controlCleaner:    cleaner,
			jobLease:          cluster.NewJobLease(clustertest.Connect(t, server, "node-"+string(rune('a'+index)))),
		}
	}
	now := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	var wait sync.WaitGroup
	for _, runtime := range runtimes {
		wait.Add(1)
		go func() {
			defer wait.Done()
			runtime.sweepRetention(t.Context(), now)
		}()
	}
	wait.Wait()
	if cleaner.sweeps.Load() != 1 || cleaner.stages.Load() != 1 || cleaner.compactions.Load() != 1 {
		t.Fatalf("cleanups = sweep %d, stages %d, compaction %d; want 1 each",
			cleaner.sweeps.Load(), cleaner.stages.Load(), cleaner.compactions.Load())
	}

	server.FastForward(retentionInterval)
	runtimes[2].sweepRetention(t.Context(), now.Add(retentionInterval))
	if cleaner.sweeps.Load() != 2 || cleaner.compactions.Load() != 2 {
		t.Fatalf("next period cleanups = sweep %d, compaction %d; want 2 each",
			cleaner.sweeps.Load(), cleaner.compactions.Load())
	}
}

func TestClusterValidationRunsOncePerIntervalAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	validator := &countingValidationSweep{}
	type instance struct {
		ticker  *fakeRuntimeTicker
		created chan time.Duration
		cancel  context.CancelFunc
		done    <-chan struct{}
	}
	instances := make([]instance, 2)
	for index := range instances {
		ticker := newFakeRuntimeTicker()
		created := make(chan time.Duration, 2)
		runtime := newTestRuntime(t, validator, ticker, created, time.Now)
		runtime.jobLease = cluster.NewJobLease(clustertest.Connect(t, server, "node-"+string(rune('a'+index))))
		cancel, done := startRuntime(t, runtime)
		awaitTickers(t, created)
		instances[index] = instance{ticker: ticker, created: created, cancel: cancel, done: done}
	}
	for _, instance := range instances {
		instance.ticker.ticks <- time.Now()
	}
	deadline := time.Now().Add(time.Second)
	for validator.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := validator.calls.Load(); got != 1 {
		t.Fatalf("validation ran %d times in one interval, want 1", got)
	}

	server.FastForward(30 * time.Minute)
	instances[1].ticker.ticks <- time.Now()
	deadline = time.Now().Add(time.Second)
	for validator.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := validator.calls.Load(); got != 2 {
		t.Fatalf("validation ran %d times after the interval, want 2", got)
	}
	for _, instance := range instances {
		stopRuntime(t, instance.cancel, instance.done)
	}
}
