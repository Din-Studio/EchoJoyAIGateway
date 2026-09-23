package cluster

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpt-load/internal/ratelimit"
)

type rpmClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *rpmClock) current() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *rpmClock) advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

func TestAccessKeyRPMMatchesInMemoryLimiter(t *testing.T) {
	_, client := newTestClient(t)
	clock := &rpmClock{now: time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)}
	shared := NewAccessKeyRPM(client)
	shared.now = clock.current
	local := ratelimit.NewAccessKeyRPMWithClock(clock.current)

	steps := []struct {
		advance time.Duration
		limit   int64
	}{
		{0, 3}, {100 * time.Millisecond, 3}, {200 * time.Millisecond, 3},
		{300 * time.Millisecond, 3},                // rejected: window full
		{10 * time.Second, 3},                      // rejected with a shorter Retry-After
		{49*time.Second + 400*time.Millisecond, 3}, // first entry exactly 60 s old expires
		{0, 3},
		{time.Second, 5}, // limit raised
		{0, 5},
		{0, 2}, // limit lowered below the current count
		{61 * time.Second, 2},
	}
	for index, step := range steps {
		clock.advance(step.advance)
		want, err := local.Allow(t.Context(), 9, step.limit)
		if err != nil {
			t.Fatal(err)
		}
		got, err := shared.Allow(t.Context(), 9, step.limit)
		if err != nil {
			t.Fatalf("step %d: Allow() error = %v", index, err)
		}
		if got != want {
			t.Fatalf("step %d: Allow() = %#v, want %#v", index, got, want)
		}
	}
}

func TestAccessKeyRPMAdmitsExactlyTheLimitAcrossInstances(t *testing.T) {
	_, client := newTestClient(t)
	instances := []*AccessKeyRPM{NewAccessKeyRPM(client), NewAccessKeyRPM(client), NewAccessKeyRPM(client)}
	var admitted, rejected atomic.Int32
	var wait sync.WaitGroup
	for request := range 200 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			decision, err := instances[request%len(instances)].Allow(t.Context(), 11, 60)
			if err != nil {
				t.Error(err)
				return
			}
			if decision.Allowed {
				admitted.Add(1)
			} else if decision.RetryAfter >= time.Second {
				rejected.Add(1)
			}
		}()
	}
	wait.Wait()
	if admitted.Load() != 60 || rejected.Load() != 140 {
		t.Fatalf("admitted %d rejected %d, want 60 and 140", admitted.Load(), rejected.Load())
	}
}

func TestAccessKeyRPMFailsWhenRedisIsDown(t *testing.T) {
	server, client := newTestClient(t)
	limiter := NewAccessKeyRPM(client)
	if decision, err := limiter.Allow(t.Context(), 12, 0); err != nil || !decision.Allowed {
		t.Fatalf("Allow(unlimited) = %#v, %v", decision, err)
	}
	server.Close()
	started := time.Now()
	if _, err := limiter.Allow(t.Context(), 12, 10); err == nil {
		t.Fatal("Allow() with Redis down error = nil")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Allow() with Redis down took %v, want about %v", elapsed, stateTimeout)
	}
	if decision, err := limiter.Allow(t.Context(), 12, 0); err != nil || !decision.Allowed {
		t.Fatalf("Allow(unlimited, Redis down) = %#v, %v", decision, err)
	}
	if NewAccessKeyRPM(nil) != nil || NewAccessQuota(nil, nil) != nil {
		t.Fatal("constructors must return nil when cluster mode is disabled")
	}
}
