package cluster

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestJobLeaseRunsOncePerPeriodAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	leases := []*JobLease{
		NewJobLease(newClientForServer(t, server, "node-a")),
		NewJobLease(newClientForServer(t, server, "node-b")),
	}
	var runs atomic.Int32
	job := func(context.Context) error {
		runs.Add(1)
		return nil
	}
	var wait sync.WaitGroup
	for index := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			leases[index%2].RunOncePerPeriod(t.Context(), "retention", time.Hour, job)
		}()
	}
	wait.Wait()
	if runs.Load() != 1 {
		t.Fatalf("job ran %d times in one period, want 1", runs.Load())
	}
	if ttl := server.TTL("gl:lease:retention"); ttl != 54*time.Minute {
		t.Fatalf("claim TTL = %s, want 54m", ttl)
	}

	server.FastForward(54 * time.Minute)
	if !leases[1].RunOncePerPeriod(t.Context(), "retention", time.Hour, job) {
		t.Fatal("RunOncePerPeriod() after the claim expired = false, want true")
	}
	if runs.Load() != 2 {
		t.Fatalf("job ran %d times, want 2", runs.Load())
	}
}

func TestJobLeaseReleasesClaimWhenJobFails(t *testing.T) {
	server := miniredis.RunT(t)
	first := NewJobLease(newClientForServer(t, server, "node-a"))
	second := NewJobLease(newClientForServer(t, server, "node-b"))

	ran := first.RunOncePerPeriod(t.Context(), "catalog-sync", 24*time.Hour, func(context.Context) error {
		return errors.New("upstream unavailable")
	})
	if !ran {
		t.Fatal("RunOncePerPeriod() = false, want true")
	}
	if server.Exists("gl:lease:catalog-sync") {
		t.Fatal("failed job kept its claim")
	}
	if !second.RunOncePerPeriod(t.Context(), "catalog-sync", 24*time.Hour, func(context.Context) error { return nil }) {
		t.Fatal("another instance could not retry after a failed run")
	}
}

func TestJobLeaseFailedRunKeepsAnotherHoldersClaim(t *testing.T) {
	server := miniredis.RunT(t)
	lease := NewJobLease(newClientForServer(t, server, "node-a"))

	lease.RunOncePerPeriod(t.Context(), "validation", time.Minute, func(context.Context) error {
		// The claim expired mid-run and another instance claimed the job.
		if err := server.Set("gl:lease:validation", "node-b:token"); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		return errors.New("probe failed")
	})
	value, err := server.Get("gl:lease:validation")
	if err != nil || value != "node-b:token" {
		t.Fatalf("claim = %q, %v; want node-b:token", value, err)
	}
}

func TestJobLeaseSkipsRoundWhenRedisIsDown(t *testing.T) {
	server := miniredis.RunT(t)
	lease := NewJobLease(newClientForServer(t, server, "node-a"))
	server.Close()

	ran := lease.RunOncePerPeriod(t.Context(), "retention", time.Hour, func(context.Context) error {
		t.Fatal("job ran without a claim")
		return nil
	})
	if ran {
		t.Fatal("RunOncePerPeriod() = true, want false")
	}
}
