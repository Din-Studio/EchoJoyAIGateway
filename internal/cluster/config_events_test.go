package cluster

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestConfigEventBusPublishSubscribeRoundTrip(t *testing.T) {
	server, client := newTestClient(t)
	bus := NewConfigEventBus(client)

	ctx, cancel := context.WithCancel(t.Context())
	subscribed := make(chan struct{})
	received := make(chan ConfigChange, 4)
	done := make(chan error, 1)
	go func() {
		done <- bus.Subscribe(ctx, func() { close(subscribed) }, func(change ConfigChange) {
			received <- change
		})
	}()
	awaitClosed(t, subscribed)

	// Malformed payloads are skipped without terminating the subscription.
	server.Publish("gl:events", "{not json")
	server.Publish("gl:events", `{"revision":0,"origin":"x"}`)
	want := ConfigChange{Revision: 7, Origin: "node-b"}
	if err := bus.Publish(t.Context(), want); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	select {
	case got := <-received:
		if got != want {
			t.Fatalf("received %#v, want %#v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive the published change")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Subscribe() after cancel error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe() did not return after context cancellation")
	}
}

func TestConfigEventBusSubscribeDoesNotLeakGoroutines(t *testing.T) {
	_, client := newTestClient(t)
	bus := NewConfigEventBus(client)
	before := runtime.NumGoroutine()

	for range 3 {
		ctx, cancel := context.WithCancel(t.Context())
		subscribed := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- bus.Subscribe(ctx, func() { close(subscribed) }, func(ConfigChange) {})
		}()
		awaitClosed(t, subscribed)
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines = %d, want at most %d", runtime.NumGoroutine(), before+1)
}

func TestNilConfigEventBusIsInert(t *testing.T) {
	var bus *ConfigEventBus
	if bus.InstanceID() != "" {
		t.Fatal("nil bus InstanceID must be empty")
	}
	if err := bus.Publish(t.Context(), ConfigChange{Revision: 1}); err != nil {
		t.Fatalf("nil bus Publish() error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := bus.Subscribe(ctx, nil, func(ConfigChange) {}); err != nil {
		t.Fatalf("nil bus Subscribe() error = %v", err)
	}
}

func awaitClosed(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for subscription")
	}
}
