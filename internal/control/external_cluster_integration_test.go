package control

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/storage/models"
)

// clusterContractInstance is one simulated process: its own Service, Manager,
// Registry, and Redis client sharing the external PostgreSQL with its peer.
type clusterContractInstance struct {
	fixture serviceFixture
	bus     *cluster.ConfigEventBus
}

func newClusterContractInstance(t *testing.T, dsn, redisAddr, keyPrefix, instanceID string) clusterContractInstance {
	t.Helper()
	fixture := newServiceFixtureWithDSN(t, dsn)
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{redisAddr}, RedisKeyPrefix: keyPrefix, InstanceID: instanceID,
	}})
	if err != nil {
		t.Fatalf("cluster.NewClient(%s) error = %v", instanceID, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	bus := cluster.NewConfigEventBus(client)
	fixture.service.clusterEvents = bus
	return clusterContractInstance{fixture: fixture, bus: bus}
}

func awaitGroupVisible(t *testing.T, instance clusterContractInstance, groupID uint, timeout time.Duration) time.Duration {
	t.Helper()
	started := time.Now()
	deadline := started.Add(timeout)
	for time.Now().Before(deadline) {
		if snapshotHasGroup(instance.fixture.manager.Current(), groupID) &&
			len(registryViewsForGroup(instance.fixture.registry, groupID)) == 1 {
			return time.Since(started)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("group %d not visible on %s within %s", groupID, instance.bus.InstanceID(), timeout)
	return 0
}

// TestExternalClusterConfigPropagation proves the Phase 1 cluster contract on
// real PostgreSQL and Redis: a control-plane commit on instance A reaches
// instance B's snapshot and registry within one second, B's runtime health
// state survives the reload, and a lost Redis event is recovered by polling.
func TestExternalClusterConfigPropagation(t *testing.T) {
	// 不标记 t.Parallel()：依赖 GPT_LOAD_DATABASE_TEST_DSN 的共享外部数据库。
	dsn := strings.TrimSpace(os.Getenv("GPT_LOAD_DATABASE_TEST_DSN"))
	if dsn == "" {
		t.Skip("GPT_LOAD_DATABASE_TEST_DSN is not set")
	}
	redisAddr := strings.TrimSpace(os.Getenv("GPT_LOAD_REDIS_TEST_ADDR"))
	if redisAddr == "" {
		t.Skip("GPT_LOAD_REDIS_TEST_ADDR is not set")
	}
	database, err := config.ParseDatabaseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDatabaseDSN() error = %v", err)
	}
	if database.Driver != config.DatabaseDriverPostgreSQL {
		t.Skipf("cluster mode requires PostgreSQL, got %s", database.Driver)
	}

	keyPrefix := fmt.Sprintf("gltest-%d-%d", os.Getpid(), time.Now().UnixNano())
	instanceA := newClusterContractInstance(t, dsn, redisAddr, keyPrefix, "node-a")
	instanceB := newClusterContractInstance(t, dsn, redisAddr, keyPrefix, "node-b")
	var createdGroups []uint
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, groupID := range createdGroups {
			if err := instanceA.fixture.service.DeleteGroup(ctx, groupID); err != nil {
				t.Errorf("cleanup DeleteGroup(%d): %v", groupID, err)
			}
		}
		if err := instanceA.fixture.db.WithContext(ctx).
			Where("key = ?", clusterConfigRevisionKey).
			Delete(&models.SystemSetting{}).Error; err != nil {
			t.Errorf("cleanup cluster revision row: %v", err)
		}
	})

	steadyGroup := createGroupWithCredentials(t, instanceA.fixture, "sk-cluster-steady")
	createdGroups = append(createdGroups, steadyGroup)

	syncB := NewClusterConfigSync(instanceB.fixture.service, instanceB.bus)
	syncB.pollInterval = time.Second
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncB.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("instance B sync did not stop")
		}
	})
	awaitGroupVisible(t, instanceB, steadyGroup, 5*time.Second)
	// Let the subscriber attach before measuring the event fast path.
	time.Sleep(200 * time.Millisecond)

	steady := registryViewsForGroup(instanceB.fixture.registry, steadyGroup)
	cooldownUntil := time.Now().Add(10 * time.Minute).Truncate(time.Millisecond)
	if !instanceB.fixture.registry.SetCooldown(steady[0].ID, cooldownUntil) {
		t.Fatal("SetCooldown() = false")
	}

	propagatedGroup := createCompatibleGroup(t, instanceA.fixture, "https://cluster-propagated.example/v1", "sk-cluster-propagated")
	createdGroups = append(createdGroups, propagatedGroup)
	latency := awaitGroupVisible(t, instanceB, propagatedGroup, 5*time.Second)
	if latency > time.Second {
		t.Fatalf("control-plane change reached instance B after %s, want <= 1s", latency)
	}
	t.Logf("event propagation latency: %s", latency)

	steadyAfter := registryViewsForGroup(instanceB.fixture.registry, steadyGroup)
	if len(steadyAfter) != 1 || !steadyAfter[0].CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("instance B cooldown after reload = %#v, want %v", steadyAfter, cooldownUntil)
	}

	// Sever the fast path: instance A commits without a reachable publish so
	// only the one-second poll can carry the change to instance B.
	instanceA.fixture.service.clusterEvents = &recordingConfigEventPublisher{
		instance: "node-a", err: fmt.Errorf("redis publish blocked"),
	}
	if _, err := instanceA.fixture.service.UpdateGroupModels(t.Context(), propagatedGroup, GroupModelsUpdateRequest{
		Models: optionalGroupModels{Set: true, Values: []GroupModel{{ID: "gpt-4o"}, {ID: "gpt-4.1"}}},
	}); err != nil {
		t.Fatalf("UpdateGroupModels() error = %v", err)
	}
	started := time.Now()
	deadline := started.Add(3 * time.Second)
	for {
		var modelCount int
		for _, group := range instanceB.fixture.manager.Current().Groups {
			if group.ID == propagatedGroup {
				modelCount = len(group.Models)
			}
		}
		if modelCount == 2 {
			t.Logf("poll fallback latency: %s", time.Since(started))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance B did not observe the model change through polling within 3s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("poll fallback took %s, want <= 2s", elapsed)
	}
}
