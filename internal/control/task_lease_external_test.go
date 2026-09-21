package control

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/coordination"
	"gpt-load/internal/storage/models"
)

// namespacedLease gives one test run its own task names so parallel CI jobs
// cannot claim each other's periods while still exercising the real claim.
type namespacedLease struct {
	lease     *coordination.Lease
	namespace string
}

func (lease namespacedLease) Claim(ctx context.Context, task string, period time.Duration) (bool, error) {
	return lease.lease.Claim(ctx, task+":"+lease.namespace, period)
}

func (lease namespacedLease) Holder() string { return lease.lease.Holder() }

// TestExternalRedisTaskLeaseRunsRetentionOnOneInstanceOnly is opt-in so
// ordinary unit tests stay hermetic. Two instances tick the same period
// against a live Redis; each database sweep must happen once while the
// process-local work happens on both.
func TestExternalRedisTaskLeaseRunsRetentionOnOneInstanceOnly(t *testing.T) {
	client := externalTaskLeaseClient(t)
	namespace := externalTaskNamespace(t, client,
		taskRequestLogRetention, taskCredentialStageCleanup)
	base := time.Date(2026, time.September, 10, 9, 0, 0, 0, time.UTC)

	type instance struct {
		runtime  *Runtime
		cleaner  *countingRequestLogCleaner
		stages   *controlledStageCleaner
		registry interface {
			ModelCooldowns(uint, time.Time) map[string]time.Time
		}
	}
	instances := make([]instance, 0, 2)
	for range 2 {
		lease, err := coordination.NewLease(client)
		if err != nil {
			t.Fatalf("NewLease() error = %v", err)
		}
		registry := newCooldownRegistry(t, base)
		cleaner := &countingRequestLogCleaner{}
		stages := &controlledStageCleaner{calls: make(chan time.Time, 2)}
		instances = append(instances, instance{
			runtime: &Runtime{
				registry: registry, requestLogCleaner: cleaner, stageCleaner: stages,
				taskLease: namespacedLease{lease: lease, namespace: namespace},
			},
			cleaner: cleaner, stages: stages, registry: registry,
		})
	}

	sweepAt := base.Add(time.Hour)
	for _, current := range instances {
		current.runtime.sweepRetention(t.Context(), sweepAt)
	}

	sweeps := instances[0].cleaner.count() + instances[1].cleaner.count()
	if sweeps != 1 {
		t.Fatalf("request log sweeps across two instances = %d, want 1", sweeps)
	}
	cleanups := len(instances[0].stages.calls) + len(instances[1].stages.calls)
	if cleanups != 1 {
		t.Fatalf("credential stage cleanups across two instances = %d, want 1", cleanups)
	}
	for index, current := range instances {
		if cooldowns := current.registry.ModelCooldowns(1, base); len(cooldowns) != 0 {
			t.Fatalf("instance %d model cooldowns = %v, want expiry on every instance", index, cooldowns)
		}
	}
}

// Compaction is a database sweep, so two instances sharing a coordination
// backend must produce one compaction even though each owns its own database.
func TestExternalRedisTaskLeaseCompactsOnOneInstanceOnly(t *testing.T) {
	client := externalTaskLeaseClient(t)
	namespace := externalTaskNamespace(t, client, taskOperationCompaction)

	compacted := 0
	for index := range 2 {
		fixture := newServiceFixture(t)
		lease, err := coordination.NewLease(client)
		if err != nil {
			t.Fatalf("NewLease() error = %v", err)
		}
		fixture.service.taskLease = namespacedLease{lease: lease, namespace: namespace}
		completeOneOperation(t, fixture, byte(0x51+index))

		fixture.service.compactOnSchedule(
			t.Context(), time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC),
		)

		var row models.ControlOperation
		if err := fixture.db.First(&row).Error; err != nil {
			t.Fatalf("read operation: %v", err)
		}
		if row.CompactedAtMS != nil {
			compacted++
		}
	}
	if compacted != 1 {
		t.Fatalf("compactions across two instances = %d, want 1", compacted)
	}
}

func externalTaskLeaseClient(t *testing.T) *coordination.Client {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("GPT_LOAD_REDIS_TEST_DSN"))
	if dsn == "" {
		t.Skip("GPT_LOAD_REDIS_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := coordination.Open(ctx, dsn)
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

func externalTaskNamespace(t *testing.T, client *coordination.Client, tasks ...string) string {
	t.Helper()
	namespace := fmt.Sprintf("test-%d-%s", time.Now().UnixNano(), t.Name())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, task := range tasks {
			key := coordination.Key("lease", task+":"+namespace)
			if err := client.Redis().Del(ctx, key).Err(); err != nil {
				t.Errorf("delete test lease key %q: %v", key, err)
			}
		}
	})
	return namespace
}
