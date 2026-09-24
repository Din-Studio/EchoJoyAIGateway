package control

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpt-load/internal/channel"
	"gpt-load/internal/cluster"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/state"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/subscription"
	subscriptionproviders "gpt-load/internal/subscription/providers"
	"gpt-load/internal/subscription/providers/codex"
	subscriptionruntime "gpt-load/internal/subscription/runtime"
)

// externalClusterTarget returns the real PostgreSQL DSN and Redis address,
// skipping the test when either is unavailable.
func externalClusterTarget(t *testing.T) (string, string) {
	t.Helper()
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
	return dsn, redisAddr
}

// healthContractInstance is one simulated process with shared credential
// health: its own registry mirror, Redis store, and background loops.
type healthContractInstance struct {
	clusterContractInstance
	client *cluster.Client
	health *cluster.CredentialHealth
	leases *cluster.RefreshLease
}

func newHealthContractInstance(t *testing.T, dsn, redisAddr, keyPrefix, instanceID string) healthContractInstance {
	t.Helper()
	base := newClusterContractInstance(t, dsn, redisAddr, keyPrefix, instanceID)
	base.fixture.registry.EnableSharedHealth()
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{redisAddr}, RedisKeyPrefix: keyPrefix, InstanceID: instanceID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	store := cluster.NewCredentialHealth(client, base.fixture.registry)
	leases := cluster.NewRefreshLease(client)
	base.fixture.service.sharedHealth = store
	base.fixture.service.refreshLeases = leases
	return healthContractInstance{clusterContractInstance: base, client: client, health: store, leases: leases}
}

func (instance healthContractInstance) start(t *testing.T) {
	t.Helper()
	configSync := NewClusterConfigSync(instance.fixture.service, instance.bus)
	configSync.pollInterval = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	var loops sync.WaitGroup
	for _, run := range []func(context.Context){configSync.Run, instance.health.Run} {
		loops.Add(1)
		go func() {
			defer loops.Done()
			run(ctx)
		}()
	}
	t.Cleanup(func() {
		cancel()
		loops.Wait()
	})
}

func schedulable(registry *state.CredentialRegistry, groupID, credentialID uint) bool {
	for _, meta := range registry.CollectCredentialCandidates([]uint{groupID}, nil, time.Now()) {
		if meta.ID == credentialID {
			return true
		}
	}
	return false
}

func awaitClusterCondition(t *testing.T, timeout time.Duration, what string, condition func() bool) time.Duration {
	t.Helper()
	started := time.Now()
	for time.Since(started) < timeout {
		if condition() {
			return time.Since(started)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s not observed within %s", what, timeout)
	return 0
}

func cleanupClusterRevision(t *testing.T, fixture serviceFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fixture.db.WithContext(ctx).Where("key = ?", clusterConfigRevisionKey).
		Delete(&models.SystemSetting{}).Error; err != nil {
		t.Errorf("cleanup cluster revision row: %v", err)
	}
}

// TestExternalClusterCredentialHealth proves AC1 and AC2 on real PostgreSQL
// and Redis: a cooldown on A reaches B within 200 ms, survives B's reload of
// a configuration change, and a control-plane restore on A frees it on B.
func TestExternalClusterCredentialHealth(t *testing.T) {
	// 不标记 t.Parallel()：依赖共享的外部数据库与 Redis。
	dsn, redisAddr := externalClusterTarget(t)
	keyPrefix := fmt.Sprintf("gltest-health-%d-%d", os.Getpid(), time.Now().UnixNano())
	instanceA := newHealthContractInstance(t, dsn, redisAddr, keyPrefix, "node-a")
	instanceB := newHealthContractInstance(t, dsn, redisAddr, keyPrefix, "node-b")
	groupID := createGroupWithCredentials(t, instanceA.fixture, "sk-cluster-health-x\nsk-cluster-health-y")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := instanceA.fixture.service.DeleteGroup(ctx, groupID); err != nil {
			t.Errorf("cleanup DeleteGroup(%d): %v", groupID, err)
		}
		cleanupClusterRevision(t, instanceA.fixture)
	})
	instanceA.start(t)
	instanceB.start(t)
	awaitClusterCondition(t, 5*time.Second, "group on instance B", func() bool {
		return len(registryViewsForGroup(instanceB.fixture.registry, groupID)) == 2
	})
	time.Sleep(200 * time.Millisecond) // let both subscriptions attach

	credentialID := registryViewsForGroup(instanceA.fixture.registry, groupID)[0].ID
	ref, _ := instanceA.fixture.registry.CredentialRef(credentialID)
	if _, err := instanceA.health.CooldownCredential(t.Context(), ref, time.Now().Add(10*time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	latency := awaitClusterCondition(t, 200*time.Millisecond, "cooldown on instance B", func() bool {
		return !schedulable(instanceB.fixture.registry, groupID, credentialID)
	})
	t.Logf("cooldown propagation latency: %s", latency)

	// A configuration change reloaded by B keeps the mirrored cooldown.
	if _, err := instanceA.fixture.service.BatchGroupCredentials(t.Context(), groupID, CredentialBatchRequest{
		Action: CredentialBatchDisable, CredentialIDs: []uint{credentialID},
	}); err != nil {
		t.Fatal(err)
	}
	awaitClusterCondition(t, 3*time.Second, "disable on instance B", func() bool {
		view, _ := findRuntimeCredential(instanceB.fixture.registry.Snapshot(), credentialID)
		return view.Status == state.CredentialStatusDisabled
	})
	if _, err := instanceA.fixture.service.BatchGroupCredentials(t.Context(), groupID, CredentialBatchRequest{
		Action: CredentialBatchEnable, CredentialIDs: []uint{credentialID},
	}); err != nil {
		t.Fatal(err)
	}
	awaitClusterCondition(t, 3*time.Second, "enable on instance B", func() bool {
		view, _ := findRuntimeCredential(instanceB.fixture.registry.Snapshot(), credentialID)
		return view.Status == state.CredentialStatusActive
	})
	if view, _ := findRuntimeCredential(instanceB.fixture.registry.Snapshot(), credentialID); view.CooldownUntil.IsZero() {
		t.Fatal("instance B lost the shared cooldown on reload")
	}

	if _, err := instanceA.fixture.service.RestoreGroupCredential(t.Context(), groupID, credentialID); err != nil {
		t.Fatalf("RestoreGroupCredential() error = %v", err)
	}
	awaitClusterCondition(t, 200*time.Millisecond, "restore on instance B", func() bool {
		return schedulable(instanceB.fixture.registry, groupID, credentialID)
	})
}

// countingCodexDriver keeps the real codex parsing and identity rules but
// answers refreshes locally, counting token endpoint calls.
type countingCodexDriver struct {
	subscriptionruntime.BrowserAuthorizationDriver
	calls   *atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (driver countingCodexDriver) MatchesRefreshIdentity(current, refreshed subscriptionruntime.Credential) bool {
	return driver.BrowserAuthorizationDriver.(subscriptionruntime.RefreshIdentityMatcher).MatchesRefreshIdentity(current, refreshed)
}

func (driver countingCodexDriver) Refresh(_ context.Context, current subscriptionruntime.Credential) (subscriptionruntime.Credential, error) {
	driver.calls.Add(1)
	driver.started <- struct{}{}
	<-driver.release
	value, err := codex.ParseCredentialJSON(current.Canonical())
	if err != nil {
		return subscriptionruntime.Credential{}, err
	}
	value.AccessToken, value.RefreshToken = "rotated-access", "rotated-refresh"
	value.Expire = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	canonical, err := codex.MarshalCredential(value)
	if err != nil {
		return subscriptionruntime.Credential{}, err
	}
	return driver.Parse(canonical)
}

func countingSubscriptionRuntime(t *testing.T, channels *channel.Registry, driver *countingCodexDriver) *subscriptionruntime.Runtime {
	t.Helper()
	registrations := subscriptionproviders.Implementations()
	for index, registration := range registrations {
		for driverIndex, candidate := range registration.Drivers {
			if string(candidate.ID()) == string(channel.Codex) {
				driver.BrowserAuthorizationDriver = candidate.(subscriptionruntime.BrowserAuthorizationDriver)
				registrations[index].Drivers[driverIndex] = *driver
			}
		}
	}
	runtime, err := subscriptionruntime.NewRuntime(channels, registrations...)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

// TestExternalClusterSubscriptionRefreshOnce proves AC3 on real PostgreSQL
// and Redis: concurrent forced refreshes on two instances call the token
// endpoint once, bump the secret version and configuration revision once,
// and both instances end up serving the new secret.
func TestExternalClusterSubscriptionRefreshOnce(t *testing.T) {
	dsn, redisAddr := externalClusterTarget(t)
	keyPrefix := fmt.Sprintf("gltest-refresh-%d-%d", os.Getpid(), time.Now().UnixNano())
	instanceA := newHealthContractInstance(t, dsn, redisAddr, keyPrefix, "node-a")
	instanceB := newHealthContractInstance(t, dsn, redisAddr, keyPrefix, "node-b")

	canonical, err := codex.MarshalCredential(codex.Credential{
		Type: codex.Provider, AccessToken: "old-access", RefreshToken: "old-refresh",
		AccountID: "cluster-account", Email: "cluster@example.com",
		Expire: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	encryption := instanceA.fixture.encryption
	ciphertext, err := encryption.Encrypt(string(canonical))
	if err != nil {
		t.Fatal(err)
	}
	group := models.Group{
		Name: keyPrefix, ChannelID: string(channel.Codex), ConnectionType: models.ConnectionTypeSubscription,
		Params: models.JSON(`{}`), Models: models.JSON(`[]`), Overrides: models.JSON(`{}`), Enabled: true,
	}
	if err := instanceA.fixture.db.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	row := models.Credential{
		GroupID: group.ID, Data: ciphertext, Fingerprint: encryption.Hash(string(canonical)),
		IdentityFingerprint: encryption.Hash("identity|" + keyPrefix), SecretVersion: 1,
		AuthState: models.CredentialAuthStateReady, Status: models.CredentialStatusActive,
	}
	if err := instanceA.fixture.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := instanceA.fixture.service.DeleteGroup(ctx, group.ID); err != nil {
			t.Errorf("cleanup DeleteGroup(%d): %v", group.ID, err)
		}
		cleanupClusterRevision(t, instanceA.fixture)
	})
	revisionBefore, err := readClusterConfigRevision(t.Context(), instanceA.fixture.db)
	if err != nil {
		t.Fatal(err)
	}

	calls := &atomic.Int32{}
	started, release := make(chan struct{}, 2), make(chan struct{})
	managers := make([]*subscription.CredentialManager, 0, 2)
	for _, instance := range []healthContractInstance{instanceA, instanceB} {
		if _, err := instance.fixture.service.reloadCommittedConfig(t.Context()); err != nil {
			t.Fatal(err)
		}
		driver := &countingCodexDriver{calls: calls, started: started, release: release}
		manager := subscription.NewCredentialManager(instance.fixture.db, encryption, instance.fixture.registry,
			instance.fixture.mutations, countingSubscriptionRuntime(t, instance.fixture.channelRegistry, driver))
		manager.SetClusterCoordination(instance.leases, instance.health, instance.fixture.service)
		managers = append(managers, manager)
	}
	instanceB.start(t)

	identity := stateloader.CredentialIdentityGeneration(
		row.IdentityFingerprint, string(channel.Codex), string(models.ConnectionTypeSubscription), json.RawMessage(`{}`))
	snapshot := execution.NewCredentialSnapshot(row.ID, 1, identity, canonical)
	evidences := make(chan *execution.ErrorEvidence, 2)
	var wait sync.WaitGroup
	for _, manager := range managers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, evidence := manager.Prepare(context.Background(), channel.Codex, snapshot, true)
			evidences <- evidence
		}()
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no instance reached the token endpoint")
	}
	time.Sleep(300 * time.Millisecond)
	close(release)
	wait.Wait()
	close(evidences)
	for evidence := range evidences {
		if evidence != nil {
			t.Fatalf("Prepare() evidence = %#v", evidence)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", got)
	}
	var stored models.Credential
	if err := instanceA.fixture.db.First(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.SecretVersion != 2 || stored.AuthState != models.CredentialAuthStateReady {
		t.Fatalf("stored version %d state %q, want 2 ready", stored.SecretVersion, stored.AuthState)
	}
	revisionAfter, err := readClusterConfigRevision(t.Context(), instanceA.fixture.db)
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter <= revisionBefore {
		t.Fatalf("config revision = %d, want above %d", revisionAfter, revisionBefore)
	}
	for name, instance := range map[string]healthContractInstance{"A": instanceA, "B": instanceB} {
		awaitClusterCondition(t, 3*time.Second, "new secret on instance "+name, func() bool {
			ref, ok := instance.fixture.registry.CredentialRef(row.ID)
			authState, _ := instance.fixture.registry.CredentialAuthStateOf(row.ID)
			return ok && ref.Version == 2 && authState == state.CredentialAuthStateReady
		})
	}
}
