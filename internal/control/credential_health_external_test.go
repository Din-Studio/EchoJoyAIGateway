package control

import (
	"context"
	"testing"
	"time"

	"gpt-load/internal/coordination"
	"gpt-load/internal/state"
)

// healthInstance is one gateway process's credential registry plus its watch
// loop, sharing only Redis with its peer.
type healthInstance struct {
	registry *state.CredentialRegistry
}

// TestExternalRedisCredentialHealthCooldownSkipsTheCredentialEverywhere is
// opt-in so ordinary unit tests stay hermetic. It proves the criterion the
// whole phase exists for: a cooldown decided on one instance takes the
// credential out of every instance's selection.
func TestExternalRedisCredentialHealthCooldownSkipsTheCredentialEverywhere(t *testing.T) {
	deciding, observing := newHealthPair(t)

	now := time.Now()
	if candidates := observing.registry.CollectCredentialCandidates([]uint{9}, nil, now); len(candidates) != 2 {
		t.Fatalf("peer candidates before = %d, want both credentials", len(candidates))
	}

	// The upstream rate-limited the deciding instance; the observing one never
	// sent that request and has no way to know on its own.
	if exists, changed := deciding.registry.SetCooldownWithChange(1, now.Add(time.Minute)); !exists || !changed {
		t.Fatalf("SetCooldownWithChange() = (%t, %t), want the cooldown recorded", exists, changed)
	}

	waitFor(t, "the peer stops selecting the cooled-down credential", func() bool {
		candidates := observing.registry.CollectCredentialCandidates([]uint{9}, nil, time.Now())
		return len(candidates) == 1 && candidates[0].ID == 2
	})
}

// A blacklist is the harsher decision and must travel the same way; an
// operator's reset must travel back.
func TestExternalRedisCredentialHealthBlacklistAndResetBothTravel(t *testing.T) {
	deciding, observing := newHealthPair(t)

	if exists, changed := deciding.registry.SetBlacklistedWithChange(1); !exists || !changed {
		t.Fatalf("SetBlacklistedWithChange() = (%t, %t)", exists, changed)
	}
	waitFor(t, "the peer adopts the blacklist", func() bool {
		health, ok := observing.registry.CredentialHealthSnapshot(1)
		return ok && health.Blacklisted
	})

	// The operator clears it on whichever instance they happened to reach.
	if !observing.registry.RestoreRuntimeState(1) {
		t.Fatal("RestoreRuntimeState() = false")
	}
	waitFor(t, "the deciding instance adopts the reset", func() bool {
		health, ok := deciding.registry.CredentialHealthSnapshot(1)
		return ok && !health.Blacklisted
	})
}

// A model cooldown is per credential and model, and a peer that lost the model
// part of it would keep routing the one model that is failing.
func TestExternalRedisCredentialHealthCarriesModelCooldowns(t *testing.T) {
	deciding, observing := newHealthPair(t)

	ref, ok := deciding.registry.CredentialRef(1)
	if !ok {
		t.Fatal("CredentialRef() = false")
	}
	now := time.Now()
	until := now.Add(time.Minute).Truncate(time.Millisecond)
	if accepted, changed := deciding.registry.SetModelCooldown(ref, "gpt-4o", until, now); !accepted || !changed {
		t.Fatalf("SetModelCooldown() = (%t, %t)", accepted, changed)
	}

	waitFor(t, "the peer adopts the model cooldown", func() bool {
		cooldowns := observing.registry.ModelCooldowns(1, now)
		return len(cooldowns) == 1 && cooldowns["gpt-4o"].Equal(until)
	})
}

// An instance that starts after the decision was made has to learn it from the
// shared log, or a restarted replica would immediately re-pick the credential
// its peers are avoiding.
func TestExternalRedisCredentialHealthHydratesAJoiningInstance(t *testing.T) {
	client := healthRedisClient(t)
	deciding := startHealthInstance(t, client)
	if exists, _ := deciding.registry.SetBlacklistedWithChange(1); !exists {
		t.Fatal("SetBlacklistedWithChange() = false")
	}

	// Waiting on Redis rather than on a peer proves the joining instance has
	// something to read before it exists.
	shared := coordination.NewCredentialHealth(client)
	waitFor(t, "the decision reaches the shared log", func() bool {
		changes, _, err := shared.Changed(t.Context(), 0)
		return err == nil && len(changes) == 1 && changes[0].Blacklisted
	})

	joining := startHealthInstance(t, client)
	waitFor(t, "the joining instance adopts what it missed", func() bool {
		health, ok := joining.registry.CredentialHealthSnapshot(1)
		return ok && health.Blacklisted
	})
}

// A Redis that lost its data issues sequence 1 again, below the cursor every
// running instance already holds. Without a fall-back that cursor matches
// nothing for the rest of the process's life and the instance goes silently
// deaf to its peers, so this is the counterpart of the configuration version
// watch's TestConfigWatchReloadsWhenTheVersionFallsBack.
func TestExternalRedisCredentialHealthReconvergesAfterTheSequenceFallsBack(t *testing.T) {
	client := healthRedisClient(t)
	deciding := startHealthInstance(t, client)
	observing := startHealthInstance(t, client)

	// Two decisions first, so the observing instance's cursor is above the
	// sequence a rebuilt store would hand out next.
	if exists, _ := deciding.registry.SetBlacklistedWithChange(1); !exists {
		t.Fatal("SetBlacklistedWithChange() = false")
	}
	waitFor(t, "the peer adopts the blacklist", func() bool {
		health, ok := observing.registry.CredentialHealthSnapshot(1)
		return ok && health.Blacklisted
	})
	if exists, _ := deciding.registry.SetCooldownWithChange(2, time.Now().Add(time.Minute)); !exists {
		t.Fatal("SetCooldownWithChange() = false")
	}
	waitFor(t, "the peer adopts the cooldown", func() bool {
		health, ok := observing.registry.CredentialHealthSnapshot(2)
		return ok && !health.CooldownUntil.IsZero()
	})

	// The store loses everything: a restart without persistence, a FLUSHDB, or
	// the sequence key falling to an eviction policy.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Redis().FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush the shared health store: %v", err)
	}

	// The operator clears the blacklist on the instance that set it. That
	// decision is published at sequence 1, under both instances' cursors.
	if !deciding.registry.RestoreRuntimeState(1) {
		t.Fatal("RestoreRuntimeState() = false")
	}
	waitFor(t, "the peer adopts a decision published after the store restarted", func() bool {
		health, ok := observing.registry.CredentialHealthSnapshot(1)
		return ok && !health.Blacklisted
	})
}

func newHealthPair(t *testing.T) (*healthInstance, *healthInstance) {
	t.Helper()
	client := healthRedisClient(t)
	return startHealthInstance(t, client), startHealthInstance(t, client)
}

// healthRedisClient gives the test its own Redis database and clears the
// shared health log so instances start from a known state.
func healthRedisClient(t *testing.T) *coordination.Client {
	t.Helper()
	dsn := requireExternalRedisDSN(t)
	client := openExternalRedis(t, dsn)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Redis().FlushDB(ctx).Err(); err != nil {
			t.Logf("flush test redis database: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Redis().FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush test redis database: %v", err)
	}
	return client
}

// startHealthInstance builds one instance's registry and runs its watch loop.
// The poll is shortened so a test that has to fall back to it still finishes;
// the production interval is credentialHealthPollInterval.
func startHealthInstance(t *testing.T, client *coordination.Client) *healthInstance {
	t.Helper()
	registry := state.NewCredentialRegistry()
	if err := registry.ReplaceCredentials([]state.CredentialEntry{{
		ID: 1, GroupID: 9, Version: 1, IdentityGeneration: 1, Fingerprint: "fp-1",
		Status: state.CredentialStatusActive, AuthState: state.CredentialAuthStateReady,
		EncryptedValue: "enc-1",
	}, {
		ID: 2, GroupID: 9, Version: 1, IdentityGeneration: 1, Fingerprint: "fp-2",
		Status: state.CredentialStatusActive, AuthState: state.CredentialAuthStateReady,
		EncryptedValue: "enc-2",
	}}); err != nil {
		t.Fatalf("ReplaceCredentials() error = %v", err)
	}

	runtime := &Runtime{registry: registry, credentialHealth: coordination.NewCredentialHealth(client)}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runtime.runCredentialHealthWatch(ctx, standardRuntimeTicker{
			ticker: time.NewTicker(fastPollInterval),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(propagationTimeout):
			t.Error("credential health watch did not stop")
		}
	})
	return &healthInstance{registry: registry}
}
