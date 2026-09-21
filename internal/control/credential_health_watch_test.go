package control

import (
	"context"
	"sync"
	"testing"
	"time"

	"gpt-load/internal/state"
)

// recordingHealthCoordinator is a shared health log that only remembers the
// order it was called in.
type recordingHealthCoordinator struct {
	mu       sync.Mutex
	calls    []string
	called   chan struct{}
	doorbell chan struct{}
}

func newRecordingHealthCoordinator() *recordingHealthCoordinator {
	return &recordingHealthCoordinator{
		called:   make(chan struct{}, 8),
		doorbell: make(chan struct{}),
	}
}

func (c *recordingHealthCoordinator) record(call string) {
	c.mu.Lock()
	c.calls = append(c.calls, call)
	c.mu.Unlock()
	select {
	case c.called <- struct{}{}:
	default:
	}
}

func (c *recordingHealthCoordinator) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *recordingHealthCoordinator) Publish(
	context.Context, []state.CredentialHealth,
) (int64, error) {
	c.record("publish")
	return 0, nil
}

func (c *recordingHealthCoordinator) Changed(
	_ context.Context, after int64,
) ([]state.CredentialHealth, int64, error) {
	c.record("changed")
	return nil, after, nil
}

func (c *recordingHealthCoordinator) Subscribe(context.Context) (<-chan struct{}, func()) {
	return c.doorbell, func() {}
}

// drivenTicker fires only when the test says so, so the order of calls is the
// test's and not the scheduler's.
type drivenTicker struct{ ticks chan time.Time }

func (t drivenTicker) C() <-chan time.Time { return t.ticks }
func (drivenTicker) Stop()                 {}

// The reset counter a record carries only means anything next to the fleet's.
// An instance that published before it read would stamp its decisions with the
// zero every credential starts at, and a peer that has seen a reset would
// correctly ignore them as older than its own — so the loop has to hydrate
// first, even though it has something to say.
func TestCredentialHealthWatchReadsBeforeItsFirstPublish(t *testing.T) {
	registry := state.NewCredentialRegistry()
	if err := registry.ReplaceCredentials([]state.CredentialEntry{{
		ID: 1, GroupID: 9, Version: 1, IdentityGeneration: 1, Fingerprint: "fp-1",
		Status: state.CredentialStatusActive, AuthState: state.CredentialAuthStateReady,
		EncryptedValue: "enc-1",
	}}); err != nil {
		t.Fatalf("ReplaceCredentials() error = %v", err)
	}
	// A decision this instance made before the loop existed: it has something
	// to publish from the very first wakeup.
	registry.SetBlacklistedWithChange(1)

	coordinator := newRecordingHealthCoordinator()
	runtime := &Runtime{registry: registry, credentialHealth: coordinator}
	ticker := drivenTicker{ticks: make(chan time.Time, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runtime.runCredentialHealthWatch(ctx, ticker)
	}()

	waitForCalls := func(want int) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			if len(coordinator.snapshot()) >= want {
				return
			}
			select {
			case <-coordinator.called:
			case <-deadline:
				t.Fatalf("calls = %v, want at least %d", coordinator.snapshot(), want)
			}
		}
	}
	// The hydrate happens before the loop ever waits for a wakeup.
	waitForCalls(1)
	ticker.ticks <- time.Now()
	// One iteration: publish the pending decision, then read again.
	waitForCalls(3)
	cancel()
	<-stopped

	calls := coordinator.snapshot()
	if calls[0] != "changed" {
		t.Fatalf("calls = %v, want the first one to be the hydrating read", calls)
	}
	if calls[1] != "publish" {
		t.Fatalf("calls = %v, want the pending decision published after the hydrate", calls)
	}
}
