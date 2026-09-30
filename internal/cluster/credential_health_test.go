package cluster

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/state"
)

const healthTestGroup = 10

func healthTestEntry(id uint, identityGeneration uint64) state.CredentialEntry {
	return state.CredentialEntry{
		ID: id, GroupID: healthTestGroup, Status: state.CredentialStatusActive, Version: 3,
		IdentityGeneration: identityGeneration, Fingerprint: "fingerprint", EncryptedValue: "cipher",
	}
}

func newHealthRegistry(t *testing.T, shared bool, entries ...state.CredentialEntry) *state.CredentialRegistry {
	t.Helper()
	registry := state.NewCredentialRegistry()
	if shared {
		registry.EnableSharedHealth()
	}
	if err := registry.ReplaceCredentials(entries); err != nil {
		t.Fatalf("ReplaceCredentials() error = %v", err)
	}
	return registry
}

func newClientForServer(t *testing.T, server *miniredis.Miniredis, instanceID string) *Client {
	t.Helper()
	cfg := clusterTestConfig(t, server.Addr())
	cfg.Cluster.InstanceID = instanceID
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func mustRef(t *testing.T, registry *state.CredentialRegistry, id uint) state.CredentialRef {
	t.Helper()
	ref, ok := registry.CredentialRef(id)
	if !ok {
		t.Fatalf("CredentialRef(%d) missing", id)
	}
	return ref
}

type healthView struct {
	cooldown       int64
	blacklisted    bool
	failures       int
	modelCooldowns map[string]int64
	authState      state.CredentialAuthState
}

func viewOf(t *testing.T, registry *state.CredentialRegistry, id uint) healthView {
	t.Helper()
	for _, view := range registry.Snapshot() {
		if view.ID != id {
			continue
		}
		result := healthView{
			blacklisted: view.Blacklisted, failures: view.FailureCount, authState: view.AuthState,
			modelCooldowns: map[string]int64{},
		}
		if !view.CooldownUntil.IsZero() {
			result.cooldown = view.CooldownUntil.UnixMilli()
		}
		for model, until := range view.ModelCooldowns {
			result.modelCooldowns[model] = until.UnixMilli()
		}
		return result
	}
	t.Fatalf("credential %d missing from snapshot", id)
	return healthView{}
}

func sameView(left, right healthView) bool {
	if left.cooldown != right.cooldown || left.blacklisted != right.blacklisted ||
		left.failures != right.failures || left.authState != right.authState ||
		len(left.modelCooldowns) != len(right.modelCooldowns) {
		return false
	}
	for model, until := range left.modelCooldowns {
		if right.modelCooldowns[model] != until {
			return false
		}
	}
	return true
}

// TestCredentialHealthMatchesLocalRegistry is the differential contract:
// every step yields the same result and health on a local registry and on a
// registry mirroring the Redis store.
func TestCredentialHealthMatchesLocalRegistry(t *testing.T) {
	_, client := newTestClient(t)
	local := newHealthRegistry(t, false, healthTestEntry(1, 7))
	shared := newHealthRegistry(t, true, healthTestEntry(1, 7))
	store := NewCredentialHealth(client, shared)
	ctx := t.Context()
	now := time.Now().Truncate(time.Millisecond)
	at := func(offset time.Duration) time.Time { return now.Add(offset) }
	const threshold = 3

	type outcome struct {
		accepted, changed bool
		failures          int
		became            bool
	}
	var staleLocalRef, staleSharedRef state.CredentialRef
	steps := []struct {
		name   string
		local  func() outcome
		shared func() (state.SharedHealthResult, error)
	}{
		{"cooldown", func() outcome {
			exists, changed := local.SetCooldownWithChange(1, at(time.Minute))
			return outcome{accepted: exists, changed: changed}
		}, func() (state.SharedHealthResult, error) {
			return store.CooldownCredential(ctx, mustRef(t, shared, 1), at(time.Minute), 0)
		}},
		{"cooldown shorter keeps deadline", func() outcome {
			exists, changed := local.SetCooldownWithChange(1, at(time.Second))
			return outcome{accepted: exists, changed: changed}
		}, func() (state.SharedHealthResult, error) {
			return store.CooldownCredential(ctx, mustRef(t, shared, 1), at(time.Second), 0)
		}},
		{"cooldown at matching version", func() outcome {
			matched, changed := local.SetCooldownWithChangeIfVersion(1, 3, at(2*time.Minute))
			return outcome{accepted: matched, changed: changed}
		}, func() (state.SharedHealthResult, error) {
			return store.CooldownCredential(ctx, mustRef(t, shared, 1), at(2*time.Minute), 3)
		}},
		{"cooldown at stale version", func() outcome {
			matched, changed := local.SetCooldownWithChangeIfVersion(1, 2, at(time.Hour))
			return outcome{accepted: matched, changed: changed}
		}, func() (state.SharedHealthResult, error) {
			return store.CooldownCredential(ctx, mustRef(t, shared, 1), at(time.Hour), 2)
		}},
		{"clear cooldown mismatch", func() outcome {
			return outcome{accepted: local.ClearCooldownIfMatch(1, at(time.Minute))}
		}, func() (state.SharedHealthResult, error) {
			return store.ClearCooldownIfMatch(ctx, mustRef(t, shared, 1), at(time.Minute))
		}},
		{"clear cooldown match", func() outcome {
			cleared := local.ClearCooldownIfMatch(1, at(2*time.Minute))
			return outcome{accepted: cleared, changed: cleared}
		}, func() (state.SharedHealthResult, error) {
			return store.ClearCooldownIfMatch(ctx, mustRef(t, shared, 1), at(2*time.Minute))
		}},
	}
	failure := func(name string) struct {
		name   string
		local  func() outcome
		shared func() (state.SharedHealthResult, error)
	} {
		return struct {
			name   string
			local  func() outcome
			shared func() (state.SharedHealthResult, error)
		}{name, func() outcome {
			count, _ := local.IncrFailure(1)
			became := false
			if count >= threshold {
				_, became = local.SetBlacklistedWithChange(1)
			}
			return outcome{accepted: true, changed: true, failures: count, became: became}
		}, func() (state.SharedHealthResult, error) {
			return store.RecordFailure(ctx, mustRef(t, shared, 1), threshold)
		}}
	}
	steps = append(steps,
		failure("failure 1"),
		struct {
			name   string
			local  func() outcome
			shared func() (state.SharedHealthResult, error)
		}{"clear failure", func() outcome {
			return outcome{accepted: local.ClearFailure(1), changed: true}
		}, func() (state.SharedHealthResult, error) {
			return store.ClearFailure(ctx, mustRef(t, shared, 1))
		}},
		failure("failure 1 again"), failure("failure 2"), failure("failure 3 blacklists"), failure("failure 4 stays"),
	)
	steps = append(steps, []struct {
		name   string
		local  func() outcome
		shared func() (state.SharedHealthResult, error)
	}{
		{"model cooldown", func() outcome {
			staleLocalRef = mustRef(t, local, 1)
			accepted, changed := local.SetModelCooldown(staleLocalRef, "gpt-4o", at(time.Minute), now)
			return outcome{accepted: accepted, changed: changed}
		}, func() (state.SharedHealthResult, error) {
			staleSharedRef = mustRef(t, shared, 1)
			return store.CooldownModel(ctx, staleSharedRef, "gpt-4o", at(time.Minute), now)
		}},
		{"model cooldown expired entry pruned", func() outcome {
			accepted, changed := local.SetModelCooldown(mustRef(t, local, 1), "gpt-4.1", at(2*time.Minute), at(90*time.Second))
			return outcome{accepted: accepted, changed: changed}
		}, func() (state.SharedHealthResult, error) {
			return store.CooldownModel(ctx, mustRef(t, shared, 1), "gpt-4.1", at(2*time.Minute), at(90*time.Second))
		}},
		{"recover with stale failure generation", func() outcome {
			stale := mustRef(t, local, 1)
			stale.FailureGeneration--
			return outcome{accepted: local.RecoverIfMatch(stale)}
		}, func() (state.SharedHealthResult, error) {
			stale := mustRef(t, shared, 1)
			stale.FailureGeneration--
			return store.RecoverIfMatch(ctx, stale, nil)
		}},
		{"restore runtime and model cooldowns", func() outcome {
			local.RestoreRuntimeState(1)
			return outcome{accepted: local.ClearModelCooldowns(1), changed: true}
		}, func() (state.SharedHealthResult, error) {
			return store.Restore(ctx, mustRef(t, shared, 1), true, true)
		}},
		{"model cooldown with stale generation", func() outcome {
			accepted, changed := local.SetModelCooldown(staleLocalRef, "gpt-4o", at(time.Hour), now)
			return outcome{accepted: accepted, changed: changed}
		}, func() (state.SharedHealthResult, error) {
			return store.CooldownModel(ctx, staleSharedRef, "gpt-4o", at(time.Hour), now)
		}},
		failure("failure 1 after restore"), failure("failure 2 after restore"), failure("failure 3 after restore"),
		{"cooldown blacklisted", func() outcome {
			exists, changed := local.SetCooldownWithChange(1, at(5*time.Minute))
			return outcome{accepted: exists, changed: changed}
		}, func() (state.SharedHealthResult, error) {
			return store.CooldownCredential(ctx, mustRef(t, shared, 1), at(5*time.Minute), 0)
		}},
		{"recover with observed cooldown", func() outcome {
			return outcome{accepted: local.RestoreRuntimeStateIfMatch(mustRef(t, local, 1), at(5*time.Minute)), changed: true}
		}, func() (state.SharedHealthResult, error) {
			observed := at(5 * time.Minute)
			return store.RecoverIfMatch(ctx, mustRef(t, shared, 1), &observed)
		}},
		failure("failure after recovery"),
	}...)

	for _, step := range steps {
		want := step.local()
		got, err := step.shared()
		if err != nil {
			t.Fatalf("%s: shared error = %v", step.name, err)
		}
		if got.Accepted != want.accepted || (want.accepted && got.Changed != want.changed) ||
			want.failures != 0 && got.FailureCount != want.failures || got.BecameBlacklisted != want.became {
			t.Fatalf("%s: shared result = %#v, local = %#v", step.name, got, want)
		}
		if localView, sharedView := viewOf(t, local, 1), viewOf(t, shared, 1); !sameView(localView, sharedView) {
			t.Fatalf("%s: shared health = %#v, local = %#v", step.name, sharedView, localView)
		}
	}

	// A new identity resets health on both sides.
	moved := healthTestEntry(1, 8)
	for _, registry := range []*state.CredentialRegistry{local, shared} {
		if _, err := registry.ReconcileGroup(healthTestGroup, []state.CredentialEntry{moved}); err != nil {
			t.Fatal(err)
		}
	}
	local.SetCooldownWithChange(1, at(time.Minute))
	if _, err := store.CooldownCredential(ctx, mustRef(t, shared, 1), at(time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	if localView, sharedView := viewOf(t, local, 1), viewOf(t, shared, 1); !sameView(localView, sharedView) || sharedView.failures != 0 {
		t.Fatalf("after identity change: shared health = %#v, local = %#v", sharedView, localView)
	}
}

func TestCredentialHealthAuthStateFollowsSecretVersion(t *testing.T) {
	_, client := newTestClient(t)
	registry := newHealthRegistry(t, true, healthTestEntry(1, 7))
	store := NewCredentialHealth(client, registry)
	ref := mustRef(t, registry, 1)

	result, err := store.SetAuthState(t.Context(), ref, state.CredentialAuthStateRefreshing, 3)
	if err != nil || !result.Accepted || !result.Changed {
		t.Fatalf("SetAuthState(refreshing, 3) = %#v, %v", result, err)
	}
	if got, _ := registry.CredentialAuthStateOf(1); got != state.CredentialAuthStateRefreshing {
		t.Fatalf("mirror auth state = %q, want refreshing", got)
	}
	// Version 4 belongs to a secret this registry does not serve yet.
	if result, err = store.SetAuthState(t.Context(), ref, state.CredentialAuthStateReady, 4); err != nil || !result.Accepted {
		t.Fatalf("SetAuthState(ready, 4) = %#v, %v", result, err)
	}
	if got, _ := registry.CredentialAuthStateOf(1); got != state.CredentialAuthStateRefreshing {
		t.Fatalf("mirror auth state = %q, want refreshing kept for version 3", got)
	}
	if result, err = store.SetAuthState(t.Context(), ref, state.CredentialAuthStateOutcomeUnknown, 3); err != nil || result.Accepted {
		t.Fatalf("SetAuthState(older version) = %#v, %v; want rejected", result, err)
	}
	// A version-scoped cooldown for the superseded secret is rejected.
	if result, err = store.CooldownCredential(t.Context(), ref, time.Now().Add(time.Minute), 3); err != nil || result.Accepted {
		t.Fatalf("CooldownCredential(stale secret) = %#v, %v; want rejected", result, err)
	}
}

type healthInstance struct {
	registry *state.CredentialRegistry
	store    *CredentialHealth
}

func startHealthInstance(t *testing.T, server *miniredis.Miniredis, instanceID string, run bool) healthInstance {
	t.Helper()
	registry := newHealthRegistry(t, true, healthTestEntry(1, 7), healthTestEntry(2, 7))
	store := NewCredentialHealth(newClientForServer(t, server, instanceID), registry)
	if run {
		ctx, cancel := context.WithCancel(context.Background())
		var done sync.WaitGroup
		done.Add(1)
		go func() {
			defer done.Done()
			store.Run(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			done.Wait()
		})
	}
	return healthInstance{registry: registry, store: store}
}

func awaitCondition(t *testing.T, timeout time.Duration, what string, condition func() bool) time.Duration {
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

func candidateIDs(registry *state.CredentialRegistry) map[uint]bool {
	ids := make(map[uint]bool)
	for _, meta := range registry.CollectCredentialCandidates([]uint{healthTestGroup}, nil, time.Now()) {
		ids[meta.ID] = true
	}
	return ids
}

func TestCredentialHealthPropagatesAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	instanceA := startHealthInstance(t, server, "node-a", true)
	instanceB := startHealthInstance(t, server, "node-b", true)
	// Let both subscriptions attach before measuring the event path.
	time.Sleep(100 * time.Millisecond)

	if _, err := instanceA.store.CooldownCredential(t.Context(), mustRef(t, instanceA.registry, 1),
		time.Now().Add(time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	latency := awaitCondition(t, 200*time.Millisecond, "cooldown on instance B", func() bool {
		return !candidateIDs(instanceB.registry)[1]
	})
	t.Logf("cooldown propagation latency: %s", latency)
	if !candidateIDs(instanceB.registry)[2] {
		t.Fatal("instance B lost an unrelated candidate")
	}

	// Failures accumulate across instances to the shared threshold.
	for _, instance := range []healthInstance{instanceA, instanceA, instanceB} {
		if _, err := instance.store.RecordFailure(t.Context(), mustRef(t, instance.registry, 2), 3); err != nil {
			t.Fatal(err)
		}
	}
	for name, instance := range map[string]healthInstance{"A": instanceA, "B": instanceB} {
		awaitCondition(t, 200*time.Millisecond, "blacklist on instance "+name, func() bool {
			return viewOf(t, instance.registry, 2).blacklisted
		})
	}

	// A restore on B is visible on A.
	if _, err := instanceB.store.Restore(t.Context(), mustRef(t, instanceB.registry, 1), true, true); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, 200*time.Millisecond, "restore on instance A", func() bool {
		return candidateIDs(instanceA.registry)[1]
	})
}

func TestCredentialHealthHydrateRepairsLostEventsAndLostRecords(t *testing.T) {
	server := miniredis.RunT(t)
	instanceA := startHealthInstance(t, server, "node-a", false)
	instanceB := startHealthInstance(t, server, "node-b", false)

	cooldown := time.Now().Add(time.Minute)
	if _, err := instanceA.store.CooldownCredential(t.Context(), mustRef(t, instanceA.registry, 1), cooldown, 0); err != nil {
		t.Fatal(err)
	}
	if viewOf(t, instanceB.registry, 1).cooldown != 0 {
		t.Fatal("instance B saw the cooldown without an event")
	}
	if err := instanceB.store.Hydrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := viewOf(t, instanceB.registry, 1).cooldown; got != cooldown.UnixMilli() {
		t.Fatalf("instance B cooldown after hydrate = %d, want %d", got, cooldown.UnixMilli())
	}
	// Reordered or repeated delivery of an older record changes nothing.
	older := map[string]string{"ep": "x", "ver": "0", "idg": "7"}
	record, err := decodeCredentialHealth(older)
	if err != nil {
		t.Fatal(err)
	}
	instanceB.registry.ApplySharedHealth(1, record)
	if err := instanceB.store.Hydrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := viewOf(t, instanceB.registry, 1).cooldown; got != cooldown.UnixMilli() {
		t.Fatalf("instance B cooldown after replay = %d", got)
	}

	server.FlushAll()
	if err := instanceB.store.Hydrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := viewOf(t, instanceB.registry, 1); got.cooldown != 0 {
		t.Fatalf("instance B health after Redis lost the record = %#v, want zero", got)
	}
}

func TestCredentialHealthReconcileTickerRepairsWithoutEvents(t *testing.T) {
	server := miniredis.RunT(t)
	instance := startHealthInstance(t, server, "node-b", false)
	instance.store.reconcileInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		instance.store.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(50 * time.Millisecond)

	// A record written without its event, as if the publish was lost.
	server.HSet(instance.store.key(1), "ep", "lost", "ver", "1", "idg", "7", "bl", "1", "fc", "3", "fg", "4", "mg", "0")
	awaitCondition(t, time.Second, "ticker reconciliation", func() bool {
		return viewOf(t, instance.registry, 1).blacklisted
	})
}

func TestCredentialHealthFailsWhenRedisIsDown(t *testing.T) {
	server, client := newTestClient(t)
	registry := newHealthRegistry(t, true, healthTestEntry(1, 7))
	store := NewCredentialHealth(client, registry)
	server.Close()
	started := time.Now()
	if _, err := store.CooldownCredential(t.Context(), mustRef(t, registry, 1), time.Now().Add(time.Minute), 0); err == nil {
		t.Fatal("CooldownCredential() succeeded against a closed Redis")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("failure took %s, want within the state timeout", elapsed)
	}
	if NewCredentialHealth(nil, registry) != nil || NewRefreshLease(nil) != nil {
		t.Fatal("constructors must return nil when cluster mode is disabled")
	}
}

func TestReplaceAuthStateRefusesAfterAnyInterveningAuthWrite(t *testing.T) {
	_, client := newTestClient(t)
	registry := newHealthRegistry(t, true, healthTestEntry(1, 7))
	store := NewCredentialHealth(client, registry)
	ref := mustRef(t, registry, 1)
	if _, err := store.SetAuthState(t.Context(), ref, state.CredentialAuthStateRefreshing, 3); err != nil {
		t.Fatal(err)
	}
	observed, err := store.ReadHealth(t.Context(), []uint{1})
	if err != nil {
		t.Fatal(err)
	}
	// A new refresh writes the same value after the read; it still moves the record.
	if _, err := store.SetAuthState(t.Context(), ref, state.CredentialAuthStateRefreshing, 3); err != nil {
		t.Fatal(err)
	}
	result, err := store.ReplaceAuthState(t.Context(), ref, state.CredentialAuthStateReady, 3, observed[1])
	if err != nil || result.Accepted {
		t.Fatalf("ReplaceAuthState(stale read) = %#v, %v; want refused", result, err)
	}
	if got, _ := registry.CredentialAuthStateOf(1); got != state.CredentialAuthStateRefreshing {
		t.Fatalf("mirror auth = %q, want refreshing kept", got)
	}

	current, err := store.ReadHealth(t.Context(), []uint{1})
	if err != nil {
		t.Fatal(err)
	}
	if result, err = store.ReplaceAuthState(t.Context(), ref, state.CredentialAuthStateReady, 3, current[1]); err != nil || !result.Accepted {
		t.Fatalf("ReplaceAuthState(current read) = %#v, %v; want accepted", result, err)
	}
	if got, _ := registry.CredentialAuthStateOf(1); got != state.CredentialAuthStateReady {
		t.Fatalf("mirror auth = %q, want ready", got)
	}
	// A credential without a record is never created by a republish.
	if result, err = store.ReplaceAuthState(t.Context(), mustRef(t, registry, 1), state.CredentialAuthStateReady, 3, state.SharedCredentialHealth{}); err != nil || result.Accepted {
		t.Fatalf("ReplaceAuthState(no record) = %#v, %v", result, err)
	}
}

func TestHydrateSkipsUndecodableRecord(t *testing.T) {
	server := miniredis.RunT(t)
	instance := startHealthInstance(t, server, "node-a", false)
	server.HSet(instance.store.key(1), "ep", "e", "ver", "not-a-number", "idg", "7")
	server.HSet(instance.store.key(2), "ep", "e", "ver", "1", "idg", "7", "bl", "1", "fg", "1", "mg", "0")
	if err := instance.store.Hydrate(t.Context()); err != nil {
		t.Fatalf("Hydrate() error = %v, want the bad record skipped", err)
	}
	if !viewOf(t, instance.registry, 2).blacklisted {
		t.Fatal("a bad record stopped hydration of another credential")
	}
}
