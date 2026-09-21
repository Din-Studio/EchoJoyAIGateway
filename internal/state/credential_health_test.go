package state

import (
	"testing"
	"time"
)

func healthRegistry(t *testing.T) (*CredentialRegistry, chan struct{}) {
	t.Helper()
	registry := NewCredentialRegistry()
	if err := registry.ReplaceCredentials([]CredentialEntry{{
		ID: 1, GroupID: 9, Version: 1, IdentityGeneration: 1, Fingerprint: "fp-1",
		Status: CredentialStatusActive, AuthState: CredentialAuthStateReady, EncryptedValue: "enc-1",
	}, {
		ID: 2, GroupID: 9, Version: 1, IdentityGeneration: 1, Fingerprint: "fp-2",
		Status: CredentialStatusActive, AuthState: CredentialAuthStateReady, EncryptedValue: "enc-2",
	}}); err != nil {
		t.Fatalf("ReplaceCredentials() error = %v", err)
	}
	woken := make(chan struct{}, 8)
	registry.SetHealthChangeNotifier(func() {
		select {
		case woken <- struct{}{}:
		default:
		}
	})
	return registry, woken
}

// Every health decision has to reach the other instances, so every mutation
// that changes health has to reach the drain.
func TestHealthChangesAreDrainedForEveryMutation(t *testing.T) {
	cooldown := time.Now().Add(time.Minute).UTC()
	for name, mutate := range map[string]func(*CredentialRegistry){
		"cooldown":         func(r *CredentialRegistry) { r.SetCooldownWithChange(1, cooldown) },
		"cooldown version": func(r *CredentialRegistry) { r.SetCooldownWithChangeIfVersion(1, 1, cooldown) },
		"blacklist":        func(r *CredentialRegistry) { r.SetBlacklistedWithChange(1) },
		"failure":          func(r *CredentialRegistry) { r.IncrFailure(1) },
		"restore":          func(r *CredentialRegistry) { r.RestoreRuntimeState(1) },
		"model cooldown": func(r *CredentialRegistry) {
			ref, _ := r.CredentialRef(1)
			r.SetModelCooldown(ref, "gpt-4o", cooldown, time.Now())
		},
		"clear model cooldowns": func(r *CredentialRegistry) { r.ClearModelCooldowns(1) },
		"clear cooldown": func(r *CredentialRegistry) {
			r.SetCooldown(1, cooldown)
			r.DrainHealthChanges()
			r.ClearCooldownIfMatch(1, cooldown)
		},
		"clear failure": func(r *CredentialRegistry) {
			r.IncrFailure(1)
			r.DrainHealthChanges()
			r.ClearFailure(1)
		},
		"recover": func(r *CredentialRegistry) {
			r.SetBlacklistedWithChange(1)
			r.DrainHealthChanges()
			r.Recover(1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			registry, woken := healthRegistry(t)
			mutate(registry)
			select {
			case <-woken:
			default:
				t.Fatal("the drain was never woken; the decision would stay on this instance")
			}
			changes := registry.DrainHealthChanges()
			if len(changes) != 1 || changes[0].CredentialID != 1 {
				t.Fatalf("DrainHealthChanges() = %#v, want credential 1", changes)
			}
			if changes[0].GroupID != 9 || changes[0].IdentityGeneration != 1 {
				t.Fatalf("change identity = %#v; a peer could not match it", changes[0])
			}
		})
	}
}

// The drain reports the outcome, not the history: peers need where a
// credential ended up, and replaying every step would only slow that down.
func TestDrainHealthChangesCollapsesRepeatedDecisions(t *testing.T) {
	registry, _ := healthRegistry(t)
	registry.IncrFailure(1)
	registry.IncrFailure(1)
	registry.IncrFailure(2)

	changes := registry.DrainHealthChanges()
	if len(changes) != 2 {
		t.Fatalf("DrainHealthChanges() = %d entries, want one per credential", len(changes))
	}
	if changes[0].CredentialID != 1 || changes[0].FailureCount != 2 {
		t.Fatalf("first change = %#v, want credential 1 at 2 failures", changes[0])
	}
	if drained := registry.DrainHealthChanges(); drained != nil {
		t.Fatalf("second DrainHealthChanges() = %#v, want nothing left", drained)
	}
}

// A decision made before the watch loop installed its notifier is exactly the
// one that must not be lost, so tracking cannot depend on the notifier.
func TestHealthChangesAreTrackedBeforeANotifierExists(t *testing.T) {
	registry := NewCredentialRegistry()
	if err := registry.ReplaceCredentials([]CredentialEntry{{
		ID: 1, GroupID: 9, Version: 1, IdentityGeneration: 1, Fingerprint: "fp-1",
		Status: CredentialStatusActive, AuthState: CredentialAuthStateReady, EncryptedValue: "enc-1",
	}}); err != nil {
		t.Fatalf("ReplaceCredentials() error = %v", err)
	}
	registry.IncrFailure(1)

	registry.SetHealthChangeNotifier(func() {})
	changes := registry.DrainHealthChanges()
	if len(changes) != 1 || changes[0].FailureCount != 1 {
		t.Fatalf("DrainHealthChanges() = %#v, want the decision made before the notifier", changes)
	}
}

// Adopting a peer's decision is what makes the cooldown global; it has to
// change what selection sees.
func TestApplyRemoteHealthRemovesTheCredentialFromSelection(t *testing.T) {
	registry, _ := healthRegistry(t)
	now := time.Now()
	if candidates := registry.CollectCredentialCandidates([]uint{9}, nil, now); len(candidates) != 2 {
		t.Fatalf("candidates before = %d, want 2", len(candidates))
	}

	if !registry.ApplyRemoteHealth(CredentialHealth{
		CredentialID: 1, GroupID: 9, IdentityGeneration: 1,
		CooldownUntil: now.Add(time.Minute),
	}) {
		t.Fatal("ApplyRemoteHealth() = false, want the peer's cooldown adopted")
	}
	candidates := registry.CollectCredentialCandidates([]uint{9}, nil, now)
	if len(candidates) != 1 || candidates[0].ID != 2 {
		t.Fatalf("candidates after = %#v, want only credential 2", candidates)
	}

	// The cooldown is absolute, so it expires on every instance at once.
	if later := registry.CollectCredentialCandidates([]uint{9}, nil, now.Add(2*time.Minute)); len(later) != 2 {
		t.Fatalf("candidates after expiry = %d, want 2", len(later))
	}
}

// An adopted decision must not be published back, or one instance's cooldown
// would bounce around the fleet forever.
func TestApplyRemoteHealthIsNotRepublished(t *testing.T) {
	registry, _ := healthRegistry(t)
	registry.ApplyRemoteHealth(CredentialHealth{
		CredentialID: 1, GroupID: 9, IdentityGeneration: 1, Blacklisted: true,
	})
	if changes := registry.DrainHealthChanges(); changes != nil {
		t.Fatalf("DrainHealthChanges() = %#v after adopting a peer's decision, want nothing", changes)
	}
}

// Health belongs to a credential under one identity. A peer that decided about
// the credential this instance has already replaced must be ignored.
func TestApplyRemoteHealthRejectsAStaleIdentity(t *testing.T) {
	registry, _ := healthRegistry(t)
	for name, health := range map[string]CredentialHealth{
		"wrong group":      {CredentialID: 1, GroupID: 8, IdentityGeneration: 1, Blacklisted: true},
		"wrong generation": {CredentialID: 1, GroupID: 9, IdentityGeneration: 2, Blacklisted: true},
		"unknown":          {CredentialID: 99, GroupID: 9, IdentityGeneration: 1, Blacklisted: true},
	} {
		if registry.ApplyRemoteHealth(health) {
			t.Errorf("ApplyRemoteHealth(%s) = true, want the mismatch rejected", name)
		}
	}
	if snapshot, _ := registry.CredentialHealthSnapshot(1); snapshot.Blacklisted {
		t.Fatal("a mismatched decision reached the credential")
	}
}

// Adopting a change has to invalidate in-flight decisions on this instance the
// same way a local change does, or a late result could undo it.
func TestApplyRemoteHealthAdvancesTheFailureGeneration(t *testing.T) {
	registry, _ := healthRegistry(t)
	before, _ := registry.CredentialRef(1)
	registry.ApplyRemoteHealth(CredentialHealth{
		CredentialID: 1, GroupID: 9, IdentityGeneration: 1, Blacklisted: true,
	})
	after, _ := registry.CredentialRef(1)
	if after.FailureGeneration == before.FailureGeneration {
		t.Fatal("the failure generation did not move; a stale result could still recover the credential")
	}
}

// Re-applying what this instance already holds is the common case — every
// instance reads back its own published changes — and must be free of effect.
func TestApplyRemoteHealthIsIdempotent(t *testing.T) {
	registry, _ := healthRegistry(t)
	health := CredentialHealth{
		CredentialID: 1, GroupID: 9, IdentityGeneration: 1,
		FailureCount: 3, ModelCooldowns: map[string]time.Time{"gpt-4o": time.Now().Add(time.Minute)},
	}
	if !registry.ApplyRemoteHealth(health) {
		t.Fatal("first ApplyRemoteHealth() = false")
	}
	if registry.ApplyRemoteHealth(health) {
		t.Fatal("second ApplyRemoteHealth() = true; an unchanged value reported a change")
	}
}
