package control

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/cluster"
	"gpt-load/internal/health"
	"gpt-load/internal/platform/config"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
)

// lockCheckingHealthStore wraps the real Redis store and fails the test when
// a caller holds the credential's mutation stripe or publishMu.
type lockCheckingHealthStore struct {
	state.SharedCredentialHealthStore
	t         *testing.T
	mutations *health.MutationCoordinator
	manager   interface {
		WithCurrentSnapshot(func(*state.ConfigSnapshot) bool) bool
	}
	err error

	mu    sync.Mutex
	calls []string
}

func (store *lockCheckingHealthStore) check(op string, ref state.CredentialRef) error {
	store.t.Helper()
	acquired := make(chan struct{})
	go func() {
		store.mutations.Do(ref.ID, func() {})
		store.manager.WithCurrentSnapshot(func(*state.ConfigSnapshot) bool { return true })
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		store.t.Errorf("%s called while holding the stripe or publishMu", op)
	}
	store.mu.Lock()
	store.calls = append(store.calls, op)
	store.mu.Unlock()
	return store.err
}

func (store *lockCheckingHealthStore) recorded() []string {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]string(nil), store.calls...)
}

func (store *lockCheckingHealthStore) Restore(ctx context.Context, ref state.CredentialRef, runtime, models bool) (state.SharedHealthResult, error) {
	if err := store.check("restore", ref); err != nil {
		return state.SharedHealthResult{}, err
	}
	return store.SharedCredentialHealthStore.Restore(ctx, ref, runtime, models)
}

func (store *lockCheckingHealthStore) RecoverIfMatch(ctx context.Context, ref state.CredentialRef, cooldown *time.Time) (state.SharedHealthResult, error) {
	if err := store.check("recover", ref); err != nil {
		return state.SharedHealthResult{}, err
	}
	return store.SharedCredentialHealthStore.RecoverIfMatch(ctx, ref, cooldown)
}

func (store *lockCheckingHealthStore) SetAuthState(ctx context.Context, ref state.CredentialRef, authState state.CredentialAuthState, version uint64) (state.SharedHealthResult, error) {
	if err := store.check("auth", ref); err != nil {
		return state.SharedHealthResult{}, err
	}
	return store.SharedCredentialHealthStore.SetAuthState(ctx, ref, authState, version)
}

type sharedHealthFixture struct {
	serviceFixture
	server *miniredis.Miniredis
	health *cluster.CredentialHealth
	store  *lockCheckingHealthStore
}

func newSharedHealthFixture(t *testing.T) sharedHealthFixture {
	t.Helper()
	fixture := newServiceFixture(t)
	fixture.registry.EnableSharedHealth()
	server := miniredis.RunT(t)
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{server.Addr()}, RedisKeyPrefix: "gl", InstanceID: "node-a",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	store := cluster.NewCredentialHealth(client, fixture.registry)
	checking := &lockCheckingHealthStore{
		SharedCredentialHealthStore: store, t: t, mutations: fixture.mutations, manager: fixture.manager,
	}
	fixture.service.sharedHealth = checking
	return sharedHealthFixture{serviceFixture: fixture, server: server, health: store, store: checking}
}

func (fixture sharedHealthFixture) ref(t *testing.T, id uint) state.CredentialRef {
	t.Helper()
	ref, ok := fixture.registry.CredentialRef(id)
	if !ok {
		t.Fatalf("credential %d missing", id)
	}
	return ref
}

func (fixture sharedHealthFixture) blacklist(t *testing.T, id uint, cooldown time.Time) {
	t.Helper()
	if _, err := fixture.health.RecordFailure(t.Context(), fixture.ref(t, id), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.health.CooldownCredential(t.Context(), fixture.ref(t, id), cooldown, 0); err != nil {
		t.Fatal(err)
	}
}

func sharedView(t *testing.T, registry *state.CredentialRegistry, id uint) state.CredentialRuntimeView {
	t.Helper()
	view, ok := findRuntimeCredential(registry.Snapshot(), id)
	if !ok {
		t.Fatalf("credential %d missing", id)
	}
	return view
}

func TestSharedHealthRestoresGoThroughStoreOutsideLocks(t *testing.T) {
	fixture := newSharedHealthFixture(t)
	groupID := createGroupWithCredentials(t, fixture.serviceFixture, "shared-one\nshared-two")
	rows := batchRestoreRows(t, fixture.serviceFixture, groupID)
	cooldown := time.Now().Add(time.Hour)

	fixture.blacklist(t, rows[0].ID, cooldown)
	if _, err := fixture.service.RestoreGroupCredential(t.Context(), groupID, rows[0].ID); err != nil {
		t.Fatalf("RestoreGroupCredential() error = %v", err)
	}
	if view := sharedView(t, fixture.registry, rows[0].ID); view.Blacklisted || !view.CooldownUntil.IsZero() {
		t.Fatalf("single restore left %#v", view)
	}

	for _, row := range rows {
		fixture.blacklist(t, row.ID, cooldown)
	}
	result, err := fixture.service.BatchGroupCredentials(t.Context(), groupID,
		CredentialBatchRequest{Action: CredentialBatchRestore, Scope: CredentialBatchScopeAll})
	if err != nil || len(result.AffectedCredentialIDs) != 2 {
		t.Fatalf("batch restore = %#v, %v", result, err)
	}
	for _, row := range rows {
		if view := sharedView(t, fixture.registry, row.ID); view.Blacklisted {
			t.Fatalf("batch restore left %#v", view)
		}
	}

	fixture.blacklist(t, rows[1].ID, cooldown)
	if restored, err := fixture.service.restoreCredentialRuntimeAfterReset(t.Context(), rows[1].ID); err != nil || !restored {
		t.Fatalf("restore after reset = %t, %v", restored, err)
	}
	if view := sharedView(t, fixture.registry, rows[1].ID); view.Blacklisted {
		t.Fatalf("reset restore left %#v", view)
	}
	if got := fixture.store.recorded(); len(got) != 4 {
		t.Fatalf("store calls = %v, want four restores", got)
	}
}

func TestSharedHealthRestoreReturnsStoreErrorsWithoutLocalMutation(t *testing.T) {
	fixture := newSharedHealthFixture(t)
	groupID := createGroupWithCredentials(t, fixture.serviceFixture, "shared-error")
	id := batchRestoreRows(t, fixture.serviceFixture, groupID)[0].ID
	fixture.blacklist(t, id, time.Now().Add(time.Hour))
	fixture.store.err = errors.New("redis down")

	if _, err := fixture.service.RestoreGroupCredential(t.Context(), groupID, id); err == nil {
		t.Fatal("RestoreGroupCredential() succeeded with the store down")
	}
	if _, err := fixture.service.BatchGroupCredentials(t.Context(), groupID,
		CredentialBatchRequest{Action: CredentialBatchRestore, Scope: CredentialBatchScopeAll}); err == nil {
		t.Fatal("batch restore succeeded with the store down")
	}
	if restored, err := fixture.service.restoreCredentialRuntimeAfterReset(t.Context(), id); err == nil || restored {
		t.Fatalf("restore after reset = %t, %v; want store error", restored, err)
	}
	if view := sharedView(t, fixture.registry, id); !view.Blacklisted {
		t.Fatalf("local registry changed despite the store error: %#v", view)
	}
}

func TestSharedHealthTestedRestoreRejectsChangedSharedState(t *testing.T) {
	fixture := newSharedHealthFixture(t)
	groupID := createGroupWithCredentials(t, fixture.serviceFixture, "shared-probe")
	id := batchRestoreRows(t, fixture.serviceFixture, groupID)[0].ID
	fixture.blacklist(t, id, time.Now().Add(time.Hour))
	fixture.service.executor = &credentialProbeTestExecutor{result: successfulCredentialProbeResult()}
	probe, err := fixture.service.TestGroupCredential(t.Context(), groupID, id, CredentialProbeRequest{
		Protocol: optionalField[protocol.Protocol]{Set: true, Value: protocol.OpenAIEmbeddings},
		Model:    optionalField[string]{Set: true, Value: "probe-model"},
	})
	if err != nil || probe.RestoreProof == nil {
		t.Fatalf("probe = %#v, %v", probe, err)
	}

	// A peer changed the failure generation after the probe; its event has
	// not reached this instance yet.
	key := "gl:{cred:" + formatUint(id) + "}:health"
	generation := fixture.server.HGet(key, "fg")
	fixture.server.HSet(key, "fg", generation+"0")
	if _, err := fixture.service.RestoreTestedGroupCredential(t.Context(), groupID, id, *probe.RestoreProof); !errors.Is(err, app_errors.ErrCredentialVersionConflict) {
		t.Fatalf("tested restore error = %v, want version conflict", err)
	}
	fixture.server.HSet(key, "fg", generation)
	if _, err := fixture.service.RestoreTestedGroupCredential(t.Context(), groupID, id, *probe.RestoreProof); err != nil {
		t.Fatalf("tested restore error = %v", err)
	}
	if view := sharedView(t, fixture.registry, id); view.Blacklisted || !view.CooldownUntil.IsZero() {
		t.Fatalf("tested restore left %#v", view)
	}
}

func TestSharedHealthValidationRecoversAfterPublicationBoundary(t *testing.T) {
	fixture := newSharedHealthFixture(t)
	registry := fixture.registry
	if err := registry.ReplaceCredentials([]state.CredentialEntry{{
		ID: 1, GroupID: 1, Version: 1, IdentityGeneration: 1, Fingerprint: "fingerprint",
		Status: state.CredentialStatusActive, EncryptedValue: "key-1",
	}}); err != nil {
		t.Fatal(err)
	}
	ref, _ := registry.CredentialRef(1)
	if _, err := fixture.health.RecordFailure(t.Context(), ref, 1); err != nil {
		t.Fatal(err)
	}
	worker := newRealRegistryValidationWorker(registry, &validationProbeRecorder{})
	worker.sharedHealth = fixture.store
	fixture.store.mutations = worker.mutations.(*health.MutationCoordinator)
	fixture.store.manager = worker.snapshots
	stats := worker.stats.(*health.StatsStore)
	stats.RecordFailure(1, health.FailureCategoryInvalidKey, http.StatusUnauthorized, time.Now())

	worker.Validate(t.Context())

	if view := sharedView(t, registry, 1); view.Blacklisted {
		t.Fatalf("validation left %#v", view)
	}
	if got := fixture.store.recorded(); len(got) != 1 || got[0] != "recover" {
		t.Fatalf("store calls = %v, want one recover", got)
	}
	if stats.Snapshot(1, time.Now()).Failure != 0 {
		t.Fatal("validation recovery did not reset stats")
	}
}

type fakeRefreshLeases struct {
	held map[uint]bool
	err  error
}

func (leases fakeRefreshLeases) Held(context.Context, []uint) (map[uint]bool, error) {
	return leases.held, leases.err
}

func refreshingSubscriptionRows(t *testing.T, fixture serviceFixture, count int) []models.Credential {
	t.Helper()
	group := models.Group{
		Name: "sweep-" + formatUint(uint(testIdempotencySequence.Add(1))), ChannelID: "codex",
		ConnectionType: models.ConnectionTypeSubscription, Params: models.JSON(`{}`),
		Models: models.JSON(`[]`), Overrides: models.JSON(`{}`), Enabled: true,
	}
	if err := fixture.db.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	rows := make([]models.Credential, 0, count)
	for index := range count {
		row := models.Credential{
			GroupID: group.ID, Data: "cipher", Fingerprint: "fp-" + formatUint(uint(index)),
			IdentityFingerprint: "identity-" + formatUint(uint(index)), SecretVersion: 4,
			AuthState: models.CredentialAuthStateRefreshing, Status: models.CredentialStatusActive,
			UpdatedAtMS: 1, // ancient: age must not matter
		}
		if err := fixture.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func storedAuthState(t *testing.T, fixture serviceFixture, id uint) models.CredentialAuthState {
	t.Helper()
	var row models.Credential
	if err := fixture.db.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	return row.AuthState
}

func TestSweepMarksOnlyRefreshesWithoutLease(t *testing.T) {
	fixture := newSharedHealthFixture(t)
	events := &recordingConfigEventPublisher{instance: "node-a"}
	fixture.service.clusterEvents = events
	rows := refreshingSubscriptionRows(t, fixture.serviceFixture, 2)
	fixture.service.refreshLeases = fakeRefreshLeases{held: map[uint]bool{rows[0].ID: true}}

	if err := fixture.service.sweepInterruptedRefreshes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := storedAuthState(t, fixture.serviceFixture, rows[0].ID); got != models.CredentialAuthStateRefreshing {
		t.Fatalf("leased row = %q, want refreshing", got)
	}
	if got := storedAuthState(t, fixture.serviceFixture, rows[1].ID); got != models.CredentialAuthStateOutcomeUnknown {
		t.Fatalf("orphaned row = %q, want outcome_unknown", got)
	}
	if calls := fixture.store.recorded(); len(calls) != 1 || calls[0] != "auth" {
		t.Fatalf("store calls = %v, want one auth write", calls)
	}
	record := fixture.server.HGet("gl:{cred:"+formatUint(rows[1].ID)+"}:health", "auth")
	if record != string(state.CredentialAuthStateOutcomeUnknown) {
		t.Fatalf("shared auth = %q, want outcome_unknown", record)
	}
	revision := readRevisionForTest(t, fixture.serviceFixture)

	// Nothing left to sweep: no write transaction, no revision bump.
	fixture.service.refreshLeases = fakeRefreshLeases{held: map[uint]bool{rows[0].ID: true}}
	if err := fixture.service.sweepInterruptedRefreshes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := readRevisionForTest(t, fixture.serviceFixture); got != revision {
		t.Fatalf("revision = %d after an empty sweep, want %d", got, revision)
	}

	// A lease check failure skips the round without touching any row.
	fixture.service.refreshLeases = fakeRefreshLeases{err: errors.New("redis down")}
	if err := fixture.service.sweepInterruptedRefreshes(t.Context()); err == nil {
		t.Fatal("sweep succeeded while the lease check failed")
	}
	if got := storedAuthState(t, fixture.serviceFixture, rows[0].ID); got != models.CredentialAuthStateRefreshing {
		t.Fatalf("leased row = %q after a failed lease check", got)
	}
}

func TestClusterBootstrapSweepsOnlyRefreshesWithoutLease(t *testing.T) {
	fixture := newSharedHealthFixture(t)
	fixture.service.clusterEvents = &recordingConfigEventPublisher{instance: "node-a"}
	rows := refreshingSubscriptionRows(t, fixture.serviceFixture, 2)
	fixture.service.refreshLeases = fakeRefreshLeases{held: map[uint]bool{rows[0].ID: true}}

	if err := fixture.service.EnsureInitialState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := storedAuthState(t, fixture.serviceFixture, rows[0].ID); got != models.CredentialAuthStateRefreshing {
		t.Fatalf("leased row = %q after bootstrap, want refreshing", got)
	}
	if got := storedAuthState(t, fixture.serviceFixture, rows[1].ID); got != models.CredentialAuthStateOutcomeUnknown {
		t.Fatalf("orphaned row = %q after bootstrap, want outcome_unknown", got)
	}
}

func readRevisionForTest(t *testing.T, fixture serviceFixture) uint64 {
	t.Helper()
	revision, err := readClusterConfigRevision(t.Context(), fixture.db)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func formatUint(value uint) string {
	return strconv.FormatUint(uint64(value), 10)
}
