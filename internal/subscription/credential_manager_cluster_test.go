package subscription

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"gorm.io/gorm"

	"gpt-load/internal/channel"
	"gpt-load/internal/cluster"
	"gpt-load/internal/execution"
	"gpt-load/internal/health"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/platform/encryption"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/subscription/providers/codex"
	subscriptionruntime "gpt-load/internal/subscription/runtime"
)

// countingCommitter commits like control.Service.CommitCredentialState and
// counts the configuration changes it would broadcast.
type countingCommitter struct {
	db      *gorm.DB
	commits atomic.Int32
}

func (committer *countingCommitter) CommitCredentialState(ctx context.Context, mutate func(*gorm.DB) error) error {
	err := committer.db.WithContext(ctx).Transaction(mutate)
	if err == nil {
		committer.commits.Add(1)
	}
	return err
}

// blockingRefresh counts upstream calls and holds each until released.
type blockingRefresh struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func newBlockingRefresh() *blockingRefresh {
	return &blockingRefresh{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (refresh *blockingRefresh) fn() func(context.Context, subscriptionruntime.Driver, subscriptionruntime.Credential) (subscriptionruntime.Credential, error) {
	return adaptCodexRefresh(func(_ context.Context, current codex.Credential) (codex.Credential, error) {
		refresh.calls.Add(1)
		refresh.started <- struct{}{}
		<-refresh.release
		current.AccessToken = "new-access"
		current.RefreshToken = "new-refresh"
		current.Expire = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
		return current, nil
	})
}

type clusterManager struct {
	manager  *CredentialManager
	registry *state.CredentialRegistry
	health   *cluster.CredentialHealth
	client   *cluster.Client
}

type clusterRefreshFixture struct {
	db         *gorm.DB
	keyService encryption.Service
	row        models.Credential
	server     *miniredis.Miniredis
	committer  *countingCommitter
	a, b       clusterManager
}

func newClusterRefreshFixture(t *testing.T, expires time.Time) clusterRefreshFixture {
	t.Helper()
	managerA, db, registryA, keyService, row := newCredentialManagerFixture(t, credentialJSON("old-access", "old-refresh", expires))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	// One connection keeps every goroutine on the same in-memory database.
	sqlDB.SetMaxOpenConns(1)
	registryA.EnableSharedHealth()

	registryB := state.NewCredentialRegistry()
	registryB.EnableSharedHealth()
	if err := registryB.ReplaceCredentials(registryEntries(registryA)); err != nil {
		t.Fatal(err)
	}
	managerB := NewCredentialManager(db, keyService, registryB, health.NewMutationCoordinator(), managerA.runtime)

	server := miniredis.RunT(t)
	committer := &countingCommitter{db: db}
	fixture := clusterRefreshFixture{db: db, keyService: keyService, row: row, server: server, committer: committer}
	fixture.a = coordinate(t, server, "node-a", managerA, registryA, committer)
	fixture.b = coordinate(t, server, "node-b", managerB, registryB, committer)
	return fixture
}

func registryEntries(registry *state.CredentialRegistry) []state.CredentialEntry {
	var entries []state.CredentialEntry
	for _, view := range registry.Snapshot() {
		exact, _ := registry.SnapshotGroupCredentialEntriesExact(view.GroupID, []uint{view.ID})
		entries = append(entries, exact...)
	}
	return entries
}

func coordinate(
	t *testing.T,
	server *miniredis.Miniredis,
	instanceID string,
	manager *CredentialManager,
	registry *state.CredentialRegistry,
	committer configCommitter,
) clusterManager {
	t.Helper()
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{server.Addr()}, RedisKeyPrefix: "gl", InstanceID: instanceID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	store := cluster.NewCredentialHealth(client, registry)
	manager.SetClusterCoordination(cluster.NewRefreshLease(client), store, committer)
	return clusterManager{manager: manager, registry: registry, health: store, client: client}
}

func (instance clusterManager) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		instance.health.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func (fixture clusterRefreshFixture) leaseKey() string {
	return "gl:{cred:" + formatID(fixture.row.ID) + "}:refresh-lease"
}

func (fixture clusterRefreshFixture) healthKey() string {
	return "gl:{cred:" + formatID(fixture.row.ID) + "}:health"
}

func formatID(id uint) string {
	return strconv.FormatUint(uint64(id), 10)
}

func awaitStarted(t *testing.T, refresh *blockingRefresh) {
	t.Helper()
	select {
	case <-refresh.started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach upstream")
	}
}

func TestClusterRefreshCallsUpstreamOnceAcrossInstances(t *testing.T) {
	fixture := newClusterRefreshFixture(t, time.Now().Add(time.Hour))
	refresh := newBlockingRefresh()
	fixture.a.manager.refresh = refresh.fn()
	fixture.b.manager.refresh = refresh.fn()
	snapshot := credentialSnapshot(t, fixture.row, fixture.keyService)

	type prepared struct {
		credential subscriptionruntime.Credential
		evidence   *execution.ErrorEvidence
	}
	results := make(chan prepared, 2)
	var wait sync.WaitGroup
	for _, instance := range []clusterManager{fixture.a, fixture.b} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			credential, evidence := instance.manager.Prepare(context.Background(), channel.Codex, snapshot, true)
			results <- prepared{credential, evidence}
		}()
	}
	awaitStarted(t, refresh)
	time.Sleep(300 * time.Millisecond) // the other instance is now waiting on the lease
	close(refresh.release)
	wait.Wait()
	close(results)

	for result := range results {
		if result.evidence != nil || mustCodexCredential(t, result.credential).AccessToken != "new-access" {
			t.Fatalf("prepare = %#v evidence=%#v", result.credential, result.evidence)
		}
	}
	if calls := refresh.calls.Load(); calls != 1 {
		t.Fatalf("upstream refresh calls = %d, want 1", calls)
	}
	var stored models.Credential
	if err := fixture.db.First(&stored, fixture.row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.SecretVersion != 2 || stored.AuthState != models.CredentialAuthStateReady {
		t.Fatalf("stored = version %d state %q, want 2 ready", stored.SecretVersion, stored.AuthState)
	}
	if commits := fixture.committer.commits.Load(); commits != 1 {
		t.Fatalf("configuration commits = %d, want 1", commits)
	}
	if asv := fixture.server.HGet(fixture.healthKey(), "asv"); asv != "2" {
		t.Fatalf("shared auth secret version = %q, want 2", asv)
	}
	for name, instance := range map[string]clusterManager{"A": fixture.a, "B": fixture.b} {
		if ref, _ := instance.registry.CredentialRef(fixture.row.ID); ref.Version != 2 {
			t.Fatalf("instance %s serves version %d, want 2", name, ref.Version)
		}
	}
	if fixture.server.Exists(fixture.leaseKey()) {
		t.Fatal("refresh lease was not released")
	}
}

func TestClusterRefreshWaitingForLeaseTimesOutWithoutUpstreamCall(t *testing.T) {
	fixture := newClusterRefreshFixture(t, time.Now().Add(time.Minute))
	if err := fixture.server.Set(fixture.leaseKey(), "node-z:held"); err != nil {
		t.Fatal(err)
	}
	refresh := newBlockingRefresh()
	close(refresh.release)
	fixture.b.manager.refresh = refresh.fn()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	started := time.Now()
	_, evidence := fixture.b.manager.Prepare(ctx, channel.Codex, credentialSnapshot(t, fixture.row, fixture.keyService), false)
	if evidence == nil || evidence.Code != "refresh_in_progress" || evidence.Hint != execution.FailureHintRefreshUnavailable {
		t.Fatalf("evidence = %#v, want retryable refresh_in_progress", evidence)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("lease wait took %s, want the caller deadline", elapsed)
	}
	if refresh.calls.Load() != 0 {
		t.Fatal("upstream was called without the lease")
	}
}

func TestClusterRefreshCommitsAfterBeingSweptAsInterrupted(t *testing.T) {
	fixture := newClusterRefreshFixture(t, time.Now().Add(time.Minute))
	refresh := newBlockingRefresh()
	fixture.a.manager.refresh = refresh.fn()
	fixture.b.manager.refresh = refresh.fn()
	snapshot := credentialSnapshot(t, fixture.row, fixture.keyService)

	done := make(chan *execution.ErrorEvidence, 1)
	go func() {
		_, evidence := fixture.a.manager.Prepare(context.Background(), channel.Codex, snapshot, false)
		done <- evidence
	}()
	awaitStarted(t, refresh)
	// A's lease is lost and a sweep judges the refresh interrupted.
	fixture.server.Del(fixture.leaseKey())
	if err := fixture.db.Model(&models.Credential{}).Where("id = ? AND auth_state = ?", fixture.row.ID, models.CredentialAuthStateRefreshing).
		Updates(map[string]any{"auth_state": models.CredentialAuthStateOutcomeUnknown, "auth_error_code": "refresh_interrupted"}).Error; err != nil {
		t.Fatal(err)
	}
	// B takes the free lease but the outcome_unknown gate stops it.
	if _, evidence := fixture.b.manager.Prepare(context.Background(), channel.Codex, snapshot, false); evidence == nil ||
		evidence.Code != string(models.CredentialAuthStateOutcomeUnknown) {
		t.Fatalf("instance B evidence = %#v, want outcome_unknown gate", evidence)
	}

	close(refresh.release)
	if evidence := <-done; evidence != nil {
		t.Fatalf("instance A evidence = %#v, want committed refresh", evidence)
	}
	assertStoredAuthState(t, fixture.db, fixture.row.ID, models.CredentialAuthStateReady, "")
	var stored models.Credential
	if err := fixture.db.First(&stored, fixture.row.ID).Error; err != nil || stored.SecretVersion != 2 {
		t.Fatalf("stored version = %d, %v; want 2", stored.SecretVersion, err)
	}
	if calls := refresh.calls.Load(); calls != 1 {
		t.Fatalf("upstream refresh calls = %d, want 1", calls)
	}
}

func TestClusterRefreshMarksRefreshLeftByLostLeaseHolder(t *testing.T) {
	fixture := newClusterRefreshFixture(t, time.Now().Add(time.Minute))
	if err := fixture.db.Model(&models.Credential{}).Where("id = ?", fixture.row.ID).
		Update("auth_state", models.CredentialAuthStateRefreshing).Error; err != nil {
		t.Fatal(err)
	}
	refresh := newBlockingRefresh()
	close(refresh.release)
	fixture.b.manager.refresh = refresh.fn()

	_, evidence := fixture.b.manager.Prepare(t.Context(), channel.Codex, credentialSnapshot(t, fixture.row, fixture.keyService), false)
	if evidence == nil || evidence.Code != "refresh_interrupted" || refresh.calls.Load() != 0 {
		t.Fatalf("evidence = %#v calls = %d", evidence, refresh.calls.Load())
	}
	assertStoredAuthState(t, fixture.db, fixture.row.ID, models.CredentialAuthStateOutcomeUnknown, "refresh_interrupted")
}

func TestClusterRefreshingAuthStateReachesPeers(t *testing.T) {
	fixture := newClusterRefreshFixture(t, time.Now().Add(time.Minute))
	fixture.b.run(t)
	time.Sleep(100 * time.Millisecond) // let B's subscription attach
	refresh := newBlockingRefresh()
	fixture.a.manager.refresh = refresh.fn()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.a.manager.Prepare(context.Background(), channel.Codex, credentialSnapshot(t, fixture.row, fixture.keyService), false)
	}()
	awaitStarted(t, refresh)
	started := time.Now()
	for {
		if state, _ := fixture.b.registry.CredentialAuthStateOf(fixture.row.ID); state == "refreshing" {
			break
		}
		if time.Since(started) > 200*time.Millisecond {
			t.Fatal("instance B still schedules the credential 200ms after the refresh started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if candidates := fixture.b.registry.CollectCredentialCandidates([]uint{fixture.row.GroupID}, nil, time.Now()); len(candidates) != 0 {
		t.Fatalf("instance B candidates = %#v, want none while refreshing", candidates)
	}
	close(refresh.release)
	<-done
	if commits := fixture.committer.commits.Load(); commits != 1 {
		t.Fatalf("configuration commits = %d, want 1", commits)
	}
}

func TestClusterRefreshFailsClosedWhenLeaseIsUnavailable(t *testing.T) {
	fixture := newClusterRefreshFixture(t, time.Now().Add(time.Minute))
	refresh := newBlockingRefresh()
	close(refresh.release)
	fixture.a.manager.refresh = refresh.fn()
	fixture.server.Close()

	_, evidence := fixture.a.manager.Prepare(t.Context(), channel.Codex, credentialSnapshot(t, fixture.row, fixture.keyService), false)
	if evidence == nil || evidence.Code != "refresh_lease_unavailable" || refresh.calls.Load() != 0 {
		t.Fatalf("evidence = %#v calls = %d", evidence, refresh.calls.Load())
	}
}

// The commit only checks the secret version, so a single-instance refresh
// in flight while a restart reset its row still lands its rotated token.
func TestRefreshCommitsAfterStartupResetMarkedItInterrupted(t *testing.T) {
	manager, db, _, keyService, row := newCredentialManagerFixture(t, credentialJSON("old-access", "old-refresh", time.Now().Add(time.Minute)))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	refresh := newBlockingRefresh()
	manager.refresh = refresh.fn()
	done := make(chan *execution.ErrorEvidence, 1)
	go func() {
		_, evidence := manager.Prepare(context.Background(), channel.Codex, credentialSnapshot(t, row, keyService), false)
		done <- evidence
	}()
	awaitStarted(t, refresh)
	if err := db.Model(&models.Credential{}).Where("id = ?", row.ID).
		Update("auth_state", models.CredentialAuthStateOutcomeUnknown).Error; err != nil {
		t.Fatal(err)
	}
	close(refresh.release)
	if evidence := <-done; evidence != nil {
		t.Fatalf("evidence = %#v, want committed refresh", evidence)
	}
	assertStoredAuthState(t, db, row.ID, models.CredentialAuthStateReady, "")
}
