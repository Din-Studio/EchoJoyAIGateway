package coordination

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestExternalRedisOpenPingClose is opt-in so ordinary unit tests stay
// hermetic. It proves the real connection lifecycle against a live Redis.
func TestExternalRedisOpenPingClose(t *testing.T) {
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
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := client.Ping(ctx); err == nil {
		t.Fatal("Ping() error = nil after Close(), want a closed-client error")
	}
}
