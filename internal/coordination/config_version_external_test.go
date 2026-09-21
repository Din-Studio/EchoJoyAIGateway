package coordination

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestExternalRedisConfigVersion is opt-in so ordinary unit tests stay
// hermetic. It proves the version and doorbell contract against a live Redis.
func TestExternalRedisConfigVersion(t *testing.T) {
	client, version := externalConfigVersion(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	current, err := version.Current(ctx)
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if current != 0 {
		t.Fatalf("Current() = %d before any bump, want 0", current)
	}

	doorbell, release := version.Subscribe(ctx)
	defer release()
	// Wait for the subscription to be established so the first bump cannot
	// race ahead of it.
	if _, err := client.Redis().Ping(ctx).Result(); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}

	for want := int64(1); want <= 2; want++ {
		bumped, err := version.Bump(ctx)
		if err != nil {
			t.Fatalf("Bump() error = %v", err)
		}
		if bumped != want {
			t.Fatalf("Bump() = %d, want %d", bumped, want)
		}
		current, err := version.Current(ctx)
		if err != nil {
			t.Fatalf("Current() error = %v", err)
		}
		if current != want {
			t.Fatalf("Current() = %d, want %d", current, want)
		}
		select {
		case <-doorbell:
		case <-time.After(5 * time.Second):
			t.Fatalf("no doorbell after bump %d", want)
		}
	}
}

func TestExternalRedisConfigVersionSubscriptionReleaseStopsGoroutine(t *testing.T) {
	_, version := externalConfigVersion(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	before := runtime.NumGoroutine()
	doorbell, release := version.Subscribe(ctx)
	if _, err := version.Bump(ctx); err != nil {
		t.Fatalf("Bump() error = %v", err)
	}
	select {
	case <-doorbell:
	case <-time.After(5 * time.Second):
		t.Fatal("no doorbell before release")
	}

	release()
	release() // releasing twice must stay safe.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, open := <-doorbell; !open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("doorbell channel stayed open after release")
		}
	}
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines = %d after release, want at most %d", runtime.NumGoroutine(), before+1)
}

// externalConfigVersion opens a live Redis and namespaces this run's keys so
// parallel CI jobs cannot observe each other's versions.
func externalConfigVersion(t *testing.T) (*Client, *ConfigVersion) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("GPT_LOAD_REDIS_TEST_DSN"))
	if dsn == "" {
		t.Skip("GPT_LOAD_REDIS_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	version := NewConfigVersion(client)
	namespace := fmt.Sprintf("test-%d-%s", time.Now().UnixNano(), t.Name())
	version.key = Key(namespace, "config", "version")
	version.channel = Key(namespace, "config")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := client.Redis().Del(cleanupCtx, version.key).Err(); err != nil {
			t.Errorf("delete test config version key: %v", err)
		}
	})
	return client, version
}
