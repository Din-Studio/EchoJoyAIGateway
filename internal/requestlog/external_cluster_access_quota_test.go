package requestlog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/platform/redact"
	"gpt-load/internal/state"
	"gpt-load/internal/storage"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/testutil/pgtest"
)

// externalClusterEnv returns the shared PostgreSQL DSN and Redis address. It
// skips on a non-PostgreSQL DSN before reading Redis, so the MySQL contract
// shard never requires Redis.
func externalClusterEnv(t *testing.T) (string, string) {
	t.Helper()
	dsn := pgtest.DSN(t)
	database, err := config.ParseDatabaseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDatabaseDSN() error = %v", err)
	}
	if database.Driver != config.DatabaseDriverPostgreSQL {
		t.Skipf("cluster mode requires PostgreSQL, got %s", database.Driver)
	}
	return dsn, pgtest.RedisAddr(t)
}

func externalClusterClient(t *testing.T, redisAddr, keyPrefix, instanceID string) *cluster.Client {
	t.Helper()
	// A comma-separated address lists every Redis Cluster node and selects
	// the cluster client.
	var addrs []string
	for _, addr := range strings.Split(redisAddr, ",") {
		if addr = strings.TrimSpace(addr); addr != "" {
			addrs = append(addrs, addr)
		}
	}
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: addrs, RedisKeyPrefix: keyPrefix, InstanceID: instanceID,
	}})
	if err != nil {
		t.Fatalf("cluster.NewClient(%s) error = %v", instanceID, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestExternalClusterAccessQuotaCheckpointHydration proves the Phase 3
// persistence contract on real PostgreSQL and Redis: instance A's checkpoint
// matches Redis, instance B rebuilds lost Redis state from that checkpoint,
// and a control-plane reset zeroes every instance while old snapshots retry.
func TestExternalClusterAccessQuotaCheckpointHydration(t *testing.T) {
	// 不标记 t.Parallel()：依赖共享的外部数据库与 Redis。
	dsn, redisAddr := externalClusterEnv(t)
	db, err := storage.OpenWithSource(dsn, config.DatabaseSourceExternal)
	if err != nil {
		t.Fatalf("OpenWithSource() error = %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := storage.AutoMigrate(db); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	unique := time.Now().UnixNano()
	accessKey := models.AccessKey{
		Name: fmt.Sprintf("external-cluster-quota-%d", unique), KeyValue: "cipher",
		KeyHash: fmt.Sprintf("external-cluster-quota-%d", unique), KeySuffix: "cafe",
		Status: "active", Filters: models.JSON(`{}`),
	}
	if err := db.Create(&accessKey).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = db.WithContext(ctx).Delete(&models.AccessKey{}, accessKey.ID).Error
	})
	ruleRow := models.AccessKeyCostLimitRule{
		AccessKeyID: accessKey.ID, Kind: models.AccessKeyCostLimitKindTotal, LimitNanoUSD: 1_000, RuleRevision: 1,
	}
	if err := db.Create(&ruleRow).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.AccessKeyCostLimitState{RuleID: ruleRow.ID, RuleRevision: 1, SnapshotVersion: 1}).Error; err != nil {
		t.Fatal(err)
	}

	keyPrefix := fmt.Sprintf("gltest-quota-%d-%d", os.Getpid(), unique)
	clientA := externalClusterClient(t, redisAddr, keyPrefix, "node-a")
	clientB := externalClusterClient(t, redisAddr, keyPrefix, "node-b")
	reader := AccessQuotaStateReader{DB: db}
	quotaA := cluster.NewAccessQuota(clientA, reader)
	quotaB := cluster.NewAccessQuota(clientB, reader)
	serviceA := NewService(db, redact.New(), staticRetentionPolicy{days: 7})
	serviceA.SetAccessQuotaCheckpointSource(quotaA)

	snapshotFor := func(revision uint64) *state.ConfigSnapshot {
		return &state.ConfigSnapshot{AccessKeysByID: map[uint]state.AccessKeyView{accessKey.ID: {
			ID: accessKey.ID, CostLimitRules: []accessquota.Rule{{
				ID: ruleRow.ID, Revision: revision, Kind: accessquota.KindTotal, LimitNanoUSD: 1_000,
			}},
		}}}
	}
	usedIn := func(quota *cluster.AccessQuota, snapshot *state.ConfigSnapshot) int64 {
		t.Helper()
		view, err := quota.View(t.Context(), snapshot, accessKey.ID, time.Now())
		if err != nil {
			t.Fatalf("View() error = %v", err)
		}
		return view.Rules[0].UsedNanoUSD
	}

	first := snapshotFor(1)
	for range 3 {
		ticket, decision, err := quotaA.Admit(t.Context(), first, accessKey.ID, time.Now())
		if err != nil || !decision.Allowed {
			t.Fatalf("Admit() = %#v, %v", decision, err)
		}
		if _, err := quotaA.Complete(t.Context(), ticket, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := serviceA.writeBatch(t.Context(), nil); err != nil {
		t.Fatalf("checkpoint flush error = %v", err)
	}
	var persisted models.AccessKeyCostLimitState
	if err := db.First(&persisted, ruleRow.ID).Error; err != nil {
		t.Fatal(err)
	}
	if redisUsed := usedIn(quotaA, first); persisted.UsedNanoUSD != 300 || redisUsed != 300 || quotaA.HasDirty() {
		t.Fatalf("checkpoint used=%d redis used=%d dirty=%v, want 300/300/false", persisted.UsedNanoUSD, redisUsed, quotaA.HasDirty())
	}

	// Redis loses the key: instance B rebuilds it from the checkpoint.
	ruleKey := clientA.Key(fmt.Sprintf("{ak:%d}", accessKey.ID), "quota", fmt.Sprint(ruleRow.ID))
	if err := clientA.Del(t.Context(), ruleKey).Err(); err != nil {
		t.Fatal(err)
	}
	if used := usedIn(quotaB, first); used != 300 {
		t.Fatalf("hydrated used = %d, want checkpoint 300", used)
	}

	// Control-plane reset: revision+1 and a zeroed checkpoint row.
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.AccessKeyCostLimitRule{}).Where("id = ?", ruleRow.ID).
			Update("rule_revision", 2).Error; err != nil {
			return err
		}
		return tx.Model(&models.AccessKeyCostLimitState{}).Where("rule_id = ?", ruleRow.ID).
			Updates(map[string]any{"rule_revision": 2, "used_nano_usd": 0, "snapshot_version": 1}).Error
	}); err != nil {
		t.Fatal(err)
	}
	second := snapshotFor(2)
	if used := usedIn(quotaB, second); used != 0 {
		t.Fatalf("used after reset = %d, want 0", used)
	}
	if _, err := quotaA.Check(t.Context(), first, accessKey.ID, time.Now()); !errors.Is(err, accessquota.ErrStaleRules) {
		t.Fatalf("Check(old snapshot) error = %v, want ErrStaleRules", err)
	}
}

// TestExternalClusterAccessKeyRPMExact proves the shared window on real
// Redis: three instances admit exactly the RPM limit under concurrency.
func TestExternalClusterAccessKeyRPMExact(t *testing.T) {
	_, redisAddr := externalClusterEnv(t)
	keyPrefix := fmt.Sprintf("gltest-rpm-%d-%d", os.Getpid(), time.Now().UnixNano())
	limiters := make([]*cluster.AccessKeyRPM, 0, 3)
	for _, instanceID := range []string{"node-a", "node-b", "node-c"} {
		limiters = append(limiters, cluster.NewAccessKeyRPM(externalClusterClient(t, redisAddr, keyPrefix, instanceID)))
	}
	// A call that outlasts the 200ms per-call state timeout may still have
	// been admitted by Redis, so its burst has no exact count. Loaded
	// machines and port-forwarded Redis occasionally stall a round trip that
	// long, so such a burst is retried on a fresh access key. Stalls come in
	// load spikes, so the retries back off to outlast one.
	const attempts = 8
	for accessKeyID := uint(1); ; accessKeyID++ {
		admitted, err := admitRPMBurst(limiters, accessKeyID)
		if err == nil {
			if admitted != 60 {
				t.Fatalf("admitted %d, want exactly 60", admitted)
			}
			return
		}
		if accessKeyID == attempts {
			t.Fatalf("every burst hit a Redis error, last: %v", err)
		}
		t.Logf("burst on access key %d: %v; retrying on a fresh key", accessKeyID, err)
		time.Sleep(time.Duration(accessKeyID) * 250 * time.Millisecond)
	}
}

// admitRPMBurst sends 200 concurrent requests for a limit of 60 through a
// bounded worker pool spread over every instance, and reports the first
// Redis error.
func admitRPMBurst(limiters []*cluster.AccessKeyRPM, accessKeyID uint) (int32, error) {
	requests := make(chan struct{}, 200)
	for range 200 {
		requests <- struct{}{}
	}
	close(requests)
	var admitted atomic.Int32
	var firstErr error
	var errOnce sync.Once
	var wait sync.WaitGroup
	for worker := range 8 * len(limiters) {
		limiter := limiters[worker%len(limiters)]
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range requests {
				decision, err := limiter.Allow(context.Background(), accessKeyID, 60)
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					continue
				}
				if decision.Allowed {
					admitted.Add(1)
				}
			}
		}()
	}
	wait.Wait()
	return admitted.Load(), firstErr
}
