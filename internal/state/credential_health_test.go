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

// Every decision that takes a credential out of selection has to reach the
// other instances, so every mutation that makes one has to reach the drain.
func TestHealthChangesAreDrainedForEveryMutation(t *testing.T) {
	cooldown := time.Now().Add(time.Minute).UTC()
	for name, mutate := range map[string]func(*CredentialRegistry){
		"cooldown":         func(r *CredentialRegistry) { r.SetCooldownWithChange(1, cooldown) },
		"cooldown version": func(r *CredentialRegistry) { r.SetCooldownWithChangeIfVersion(1, 1, cooldown) },
		"blacklist":        func(r *CredentialRegistry) { r.SetBlacklistedWithChange(1) },
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

// The failure count is this instance's own progress toward the threshold, not
// a decision about the credential. Sharing it would let a success on any
// instance reset every instance's progress, so a credential failing steadily
// across a busy fleet might never reach the threshold at all.
func TestFailureCountingStaysOnThisInstance(t *testing.T) {
	registry, woken := healthRegistry(t)

	if _, ok := registry.IncrFailure(1); !ok {
		t.Fatal("IncrFailure(1) = false")
	}
	if !registry.ClearFailure(1) {
		t.Fatal("ClearFailure(1) = false")
	}
	select {
	case <-woken:
		t.Fatal("counting a failure woke the drain; the count does not cross instances")
	default:
	}
	if changes := registry.DrainHealthChanges(); changes != nil {
		t.Fatalf("DrainHealthChanges() = %#v, want nothing to publish", changes)
	}

	// Crossing the threshold is a decision, and that one travels.
	if _, changed := registry.SetBlacklistedWithChange(1); !changed {
		t.Fatal("SetBlacklistedWithChange() reported no change")
	}
	if changes := registry.DrainHealthChanges(); len(changes) != 1 || !changes[0].Blacklisted {
		t.Fatalf("DrainHealthChanges() = %#v, want the blacklist", changes)
	}
}

// The drain reports the outcome, not the history: peers need where a
// credential ended up, and replaying every step would only slow that down.
func TestDrainHealthChangesCollapsesRepeatedDecisions(t *testing.T) {
	registry, _ := healthRegistry(t)
	now := time.Now()
	registry.SetCooldownWithChange(1, now.Add(time.Minute))
	registry.SetCooldownWithChange(1, now.Add(2*time.Minute))
	registry.SetCooldownWithChange(2, now.Add(time.Minute))

	changes := registry.DrainHealthChanges()
	if len(changes) != 2 {
		t.Fatalf("DrainHealthChanges() = %d entries, want one per credential", len(changes))
	}
	if changes[0].CredentialID != 1 || !changes[0].CooldownUntil.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("first change = %#v, want credential 1 at the later cooldown", changes[0])
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
	registry.SetBlacklistedWithChange(1)

	registry.SetHealthChangeNotifier(func() {})
	changes := registry.DrainHealthChanges()
	if len(changes) != 1 || !changes[0].Blacklisted {
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
		CredentialID: 1, GroupID: 9, IdentityGeneration: 1, Blacklisted: true,
		ModelCooldowns: map[string]time.Time{"gpt-4o": time.Now().Add(time.Minute)},
	}
	if !registry.ApplyRemoteHealth(health) {
		t.Fatal("first ApplyRemoteHealth() = false")
	}
	if registry.ApplyRemoteHealth(health) {
		t.Fatal("second ApplyRemoteHealth() = true; an unchanged value reported a change")
	}
}

// A record is read one or two round trips after it was written, so it always
// describes a moment that may already be stale. It must never take a reason to
// avoid the credential away — that is the whole job: once a credential is out
// of selection somewhere, it stays out everywhere until a reset says otherwise.
func TestApplyRemoteHealthNeverUndoesAReasonToAvoidTheCredential(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Minute)

	for name, check := range map[string]struct {
		local  func(*CredentialRegistry)
		remote CredentialHealth
		want   func(*testing.T, CredentialRuntimeState, CredentialHealth)
	}{
		"a record with no cooldown cannot shorten one": {
			local:  func(r *CredentialRegistry) { r.SetCooldownWithChange(1, later) },
			remote: CredentialHealth{CredentialID: 1, GroupID: 9, IdentityGeneration: 1},
			want: func(t *testing.T, gotState CredentialRuntimeState, got CredentialHealth) {
				if gotState != CredentialRuntimeCooldown || !got.CooldownUntil.Equal(later) {
					t.Fatalf("state = %v, cooldown = %v; the live cooldown was dropped", gotState, got.CooldownUntil)
				}
			},
		},
		"a record without the blacklist cannot lift one": {
			local:  func(r *CredentialRegistry) { r.SetBlacklistedWithChange(1) },
			remote: CredentialHealth{CredentialID: 1, GroupID: 9, IdentityGeneration: 1},
			want: func(t *testing.T, gotState CredentialRuntimeState, _ CredentialHealth) {
				if gotState != CredentialRuntimeBlacklisted {
					t.Fatalf("state = %v, want the credential still blacklisted", gotState)
				}
			},
		},
		"a record without a model cannot lift its cooldown": {
			local: func(r *CredentialRegistry) {
				ref, _ := r.CredentialRef(1)
				r.SetModelCooldown(ref, "gpt-4o", later, now)
			},
			remote: CredentialHealth{
				CredentialID: 1, GroupID: 9, IdentityGeneration: 1,
				ModelCooldowns: map[string]time.Time{"claude": later},
			},
			want: func(t *testing.T, _ CredentialRuntimeState, got CredentialHealth) {
				if len(got.ModelCooldowns) != 2 || !got.ModelCooldowns["gpt-4o"].Equal(later) {
					t.Fatalf("model cooldowns = %v, want both models refused", got.ModelCooldowns)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			registry, _ := healthRegistry(t)
			check.local(registry)
			registry.ApplyRemoteHealth(check.remote)

			got, ok := registry.CredentialHealthSnapshot(1)
			if !ok {
				t.Fatal("CredentialHealthSnapshot() = false")
			}
			candidates := registry.CollectCredentialCandidates([]uint{9}, nil, now)
			state := CredentialRuntimeAvailable
			if len(candidates) == 1 && candidates[0].ID == 2 {
				if got.Blacklisted {
					state = CredentialRuntimeBlacklisted
				} else {
					state = CredentialRuntimeCooldown
				}
			}
			check.want(t, state, got)
		})
	}
}

// Merging has to be order-independent, because the order two records reach a
// third instance in says nothing about the order they were decided in.
func TestApplyRemoteHealthMergesInEitherOrder(t *testing.T) {
	now := time.Now()
	first := CredentialHealth{
		CredentialID: 1, GroupID: 9, IdentityGeneration: 1,
		CooldownUntil:  now.Add(time.Minute),
		ModelCooldowns: map[string]time.Time{"gpt-4o": now.Add(time.Minute)},
	}
	second := CredentialHealth{
		CredentialID: 1, GroupID: 9, IdentityGeneration: 1,
		Blacklisted:    true,
		ModelCooldowns: map[string]time.Time{"claude": now.Add(2 * time.Minute)},
	}

	snapshots := make([]CredentialHealth, 0, 2)
	for _, order := range [][]CredentialHealth{{first, second}, {second, first}} {
		registry, _ := healthRegistry(t)
		for _, change := range order {
			registry.ApplyRemoteHealth(change)
		}
		got, _ := registry.CredentialHealthSnapshot(1)
		snapshots = append(snapshots, got)
	}

	left, right := snapshots[0], snapshots[1]
	if !left.Blacklisted || !right.Blacklisted ||
		!left.CooldownUntil.Equal(right.CooldownUntil) ||
		!sameModelCooldowns(left.ModelCooldowns, right.ModelCooldowns) ||
		len(left.ModelCooldowns) != 2 {
		t.Fatalf("order changed the result:\n first→second = %#v\n second→first = %#v", left, right)
	}
}

// A reset is the only decision that takes reasons away, so it is the only one
// that needs to be ordered against them. A record decided before the reset
// must not resurrect what the reset cleared.
func TestApplyRemoteHealthOrdersResetsAgainstOlderRecords(t *testing.T) {
	t.Run("a record from before the reset is ignored", func(t *testing.T) {
		registry, _ := healthRegistry(t)
		registry.SetBlacklistedWithChange(1)
		if !registry.RestoreRuntimeState(1) {
			t.Fatal("RestoreRuntimeState() = false")
		}
		stale := CredentialHealth{
			CredentialID: 1, GroupID: 9, IdentityGeneration: 1, Blacklisted: true,
		}
		if registry.ApplyRemoteHealth(stale) {
			t.Fatal("a record decided before the reset was applied over it")
		}
		if got, _ := registry.CredentialHealthSnapshot(1); got.Blacklisted {
			t.Fatal("the reset was undone by a record that predates it")
		}
	})

	t.Run("a peer's reset clears what this instance decided before it", func(t *testing.T) {
		registry, _ := healthRegistry(t)
		registry.SetBlacklistedWithChange(1)
		reset := CredentialHealth{
			CredentialID: 1, GroupID: 9, IdentityGeneration: 1, ResetGen: 1,
		}
		if !registry.ApplyRemoteHealth(reset) {
			t.Fatal("ApplyRemoteHealth() = false; the peer's reset did not land")
		}
		if got, _ := registry.CredentialHealthSnapshot(1); got.Blacklisted {
			t.Fatal("the credential is still blacklisted after a peer reset it")
		}
	})

	t.Run("a decision made after the reset survives", func(t *testing.T) {
		registry, _ := healthRegistry(t)
		registry.SetBlacklistedWithChange(1)
		fresh := CredentialHealth{
			CredentialID: 1, GroupID: 9, IdentityGeneration: 1, ResetGen: 1, Blacklisted: true,
		}
		registry.ApplyRemoteHealth(fresh)
		if got, _ := registry.CredentialHealthSnapshot(1); !got.Blacklisted {
			t.Fatal("a blacklist decided after the reset was dropped with it")
		}
	})
}
