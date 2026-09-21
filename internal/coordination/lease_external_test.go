package coordination

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestExternalRedisLeaseClaimsOnePeriodPerInstance is opt-in so ordinary unit
// tests stay hermetic. It proves the period-claim contract against a live
// Redis: one claim wins the period, everyone else waits for the next one.
func TestExternalRedisLeaseClaimsOnePeriodPerInstance(t *testing.T) {
	client := externalLeaseClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	first := externalLease(t, client)
	second := externalLease(t, client)
	if first.Holder() == second.Holder() {
		t.Fatal("two leases share a holder identity; they cannot model two instances")
	}
	task := externalLeaseTask(t, client)

	const period = time.Hour
	claimed, err := first.Claim(ctx, task, period)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if !claimed {
		t.Fatal("Claim() = false on a free period, want true")
	}

	// The holder itself must not be able to run the task twice in one period:
	// the claim is never released after a completed sweep.
	for name, lease := range map[string]*Lease{"holder": first, "other instance": second} {
		claimed, err := lease.Claim(ctx, task, period)
		if err != nil {
			t.Fatalf("Claim() by %s error = %v", name, err)
		}
		if claimed {
			t.Fatalf("Claim() by %s = true inside a claimed period, want false", name)
		}
	}

	owner, err := client.Redis().Get(ctx, leaseKey(task)).Result()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if owner != first.Holder() {
		t.Fatalf("lease value = %q, want the first holder %q", owner, first.Holder())
	}
	ttl, err := client.Redis().PTTL(ctx, leaseKey(task)).Result()
	if err != nil {
		t.Fatalf("PTTL() error = %v", err)
	}
	if ttl <= 0 || ttl > period {
		t.Fatalf("PTTL() = %v, want a positive value no larger than the period %v", ttl, period)
	}
}

// A crashed holder costs one period, not the task: once the claim expires with
// the period, the next period is claimable by any instance.
func TestExternalRedisLeaseHealsAfterThePeriodExpires(t *testing.T) {
	client := externalLeaseClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	lease := externalLease(t, client)
	other := externalLease(t, client)
	task := externalLeaseTask(t, client)

	const period = 150 * time.Millisecond
	claimed, err := lease.Claim(ctx, task, period)
	if err != nil || !claimed {
		t.Fatalf("Claim() = %v, %v; want true, nil", claimed, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		claimed, err := other.Claim(ctx, task, period)
		if err != nil {
			t.Fatalf("Claim() error = %v", err)
		}
		if claimed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the next period was never claimable after the previous one expired")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// redisTestDSN skips the caller unless an operator pointed the suite at a live
// Redis, so ordinary unit tests stay hermetic.
func redisTestDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("GPT_LOAD_REDIS_TEST_DSN"))
	if dsn == "" {
		t.Skip("GPT_LOAD_REDIS_TEST_DSN is not set")
	}
	return dsn
}

func externalLeaseClient(t *testing.T) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Open(ctx, redisTestDSN(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return client
}

func externalLease(t *testing.T, client *Client) *Lease {
	t.Helper()
	lease, err := NewLease(client)
	if err != nil {
		t.Fatalf("NewLease() error = %v", err)
	}
	return lease
}

// externalLeaseTask namespaces the task name so parallel CI jobs cannot claim
// each other's periods.
func externalLeaseTask(t *testing.T, client *Client) string {
	t.Helper()
	task := fmt.Sprintf("test-%d-%s", time.Now().UnixNano(), t.Name())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Redis().Del(ctx, leaseKey(task)).Err(); err != nil {
			t.Errorf("delete test lease key: %v", err)
		}
	})
	return task
}
