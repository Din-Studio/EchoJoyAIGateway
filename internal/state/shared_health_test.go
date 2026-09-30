package state

import (
	"testing"
	"time"
)

func newSharedHealthRegistry(t *testing.T, entries ...CredentialEntry) *CredentialRegistry {
	t.Helper()
	registry := NewCredentialRegistry()
	registry.EnableSharedHealth()
	mustReplaceKeyEntries(t, registry, entries)
	return registry
}

func sharedHealthEntry(id uint) CredentialEntry {
	return CredentialEntry{
		ID: id, GroupID: 10, Status: CredentialStatusActive, Version: 3, IdentityGeneration: 7,
		Fingerprint: "fingerprint", EncryptedValue: "cipher",
	}
}

func onlyView(t *testing.T, registry *CredentialRegistry) CredentialRuntimeView {
	t.Helper()
	views := registry.Snapshot()
	if len(views) != 1 {
		t.Fatalf("Snapshot() = %#v, want one entry", views)
	}
	return views[0]
}

func TestApplySharedHealthOrdersStatesByEpochAndVersion(t *testing.T) {
	registry := newSharedHealthRegistry(t, sharedHealthEntry(1))
	cooldown := time.Now().Add(time.Hour).Truncate(time.Millisecond)

	if !registry.ApplySharedHealth(1, SharedCredentialHealth{
		Epoch: "e1", Version: 5, IdentityGeneration: 7, CooldownUntil: cooldown, FailureCount: 2, FailureGeneration: 4,
	}) {
		t.Fatal("first state was not applied")
	}
	if registry.ApplySharedHealth(1, SharedCredentialHealth{Epoch: "e1", Version: 4, IdentityGeneration: 7}) {
		t.Fatal("older version in the same epoch was applied")
	}
	// The same version carries the same content, so reapplying it is safe.
	if !registry.ApplySharedHealth(1, SharedCredentialHealth{
		Epoch: "e1", Version: 5, IdentityGeneration: 7, CooldownUntil: cooldown, FailureCount: 2, FailureGeneration: 4,
	}) {
		t.Fatal("repeated version was not reapplied")
	}
	if registry.ApplySharedHealth(1, SharedCredentialHealth{Epoch: "e1", Version: 9, IdentityGeneration: 8, Blacklisted: true}) {
		t.Fatal("state of another identity generation was applied")
	}
	view := onlyView(t, registry)
	if !view.CooldownUntil.Equal(cooldown) || view.FailureCount != 2 || view.Blacklisted {
		t.Fatalf("view = %#v, want first state", view)
	}
	// A new epoch means the store record was recreated; its lower version wins.
	if !registry.ApplySharedHealth(1, SharedCredentialHealth{Epoch: "e2", Version: 1, IdentityGeneration: 7, Blacklisted: true}) {
		t.Fatal("new epoch was not applied")
	}
	view = onlyView(t, registry)
	if !view.Blacklisted || !view.CooldownUntil.IsZero() || view.FailureCount != 0 {
		t.Fatalf("view after new epoch = %#v", view)
	}
	// An empty state after a mirrored one means the store lost the record.
	if !registry.ApplySharedHealth(1, SharedCredentialHealth{}) {
		t.Fatal("lost record was not applied")
	}
	if view = onlyView(t, registry); view.Blacklisted {
		t.Fatalf("view after lost record = %#v, want zero health", view)
	}
	if registry.ApplySharedHealth(1, SharedCredentialHealth{}) {
		t.Fatal("empty state was applied to an entry that mirrors nothing")
	}
}

func TestApplySharedHealthAuthStateRequiresMatchingSecretVersion(t *testing.T) {
	registry := newSharedHealthRegistry(t, sharedHealthEntry(1))
	registry.ApplySharedHealth(1, SharedCredentialHealth{
		Epoch: "e", Version: 1, IdentityGeneration: 7,
		AuthState: CredentialAuthStateRefreshing, AuthSecretVersion: 2,
	})
	if state, _ := registry.CredentialAuthStateOf(1); state != CredentialAuthStateReady {
		t.Fatalf("auth state = %q after stale secret version, want ready", state)
	}
	registry.ApplySharedHealth(1, SharedCredentialHealth{
		Epoch: "e", Version: 2, IdentityGeneration: 7,
		AuthState: CredentialAuthStateRefreshing, AuthSecretVersion: 3,
	})
	if state, _ := registry.CredentialAuthStateOf(1); state != CredentialAuthStateRefreshing {
		t.Fatalf("auth state = %q, want refreshing", state)
	}
	if got := registry.CollectCredentialCandidates([]uint{10}, nil, time.Now()); len(got) != 0 {
		t.Fatalf("candidates = %#v, want refreshing credential excluded", got)
	}
}

func TestApplySharedHealthExcludesCooledCredentialAndSyncsScheduling(t *testing.T) {
	registry := newSharedHealthRegistry(t, sharedHealthEntry(1), sharedHealthEntry(2))
	cooldown := time.Now().Add(time.Minute)
	registry.ApplySharedHealth(1, SharedCredentialHealth{Epoch: "e", Version: 1, IdentityGeneration: 7, CooldownUntil: cooldown})

	candidates := registry.CollectCredentialCandidates([]uint{10}, nil, time.Now())
	if len(candidates) != 1 || candidates[0].ID != 2 {
		t.Fatalf("candidates = %#v, want only credential 2", candidates)
	}
	var member SchedulingMember
	registry.SchedulingState().WithLock(func(ledger *SchedulingLedger) { member = *ledger.Members[1] })
	if !member.cooldownUntil.Equal(cooldown) || !member.suspended {
		t.Fatalf("scheduling member = %#v, want suspended until %v", member, cooldown)
	}
}

func TestLocalFallbackMutationYieldsToNextSharedState(t *testing.T) {
	registry := newSharedHealthRegistry(t, sharedHealthEntry(1))
	registry.ApplySharedHealth(1, SharedCredentialHealth{Epoch: "e", Version: 3, IdentityGeneration: 7})
	if !registry.SetBlacklisted(1) {
		t.Fatal("SetBlacklisted() = false")
	}
	// The same store version now overrides the local fallback.
	if !registry.ApplySharedHealth(1, SharedCredentialHealth{Epoch: "e", Version: 3, IdentityGeneration: 7}) {
		t.Fatal("store state did not replace the local fallback")
	}
	if onlyView(t, registry).Blacklisted {
		t.Fatal("local fallback blacklist survived the store state")
	}
}

func TestSharedReconcileKeepsHealthAcrossConfigChanges(t *testing.T) {
	entry := sharedHealthEntry(1)
	registry := newSharedHealthRegistry(t, entry)
	cooldown := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	models := map[string]time.Time{"gpt-4o": cooldown}
	registry.ApplySharedHealth(1, SharedCredentialHealth{
		Epoch: "e", Version: 4, IdentityGeneration: 7, CooldownUntil: cooldown, Blacklisted: true,
		FailureCount: 3, FailureGeneration: 6, ModelCooldownGeneration: 2, ModelCooldowns: models,
		AuthState: CredentialAuthStateRefreshing, AuthSecretVersion: 3,
	})

	// Weight change at the same secret version keeps health and auth state.
	reweighted := entry
	reweighted.WeightManual = intPointer(5)
	if changed, err := registry.ReconcileGroup(10, []CredentialEntry{reweighted}); err != nil || !changed {
		t.Fatalf("ReconcileGroup(weight) = %t, %v", changed, err)
	}
	view := onlyView(t, registry)
	ref, _ := registry.CredentialRef(1)
	if !view.CooldownUntil.Equal(cooldown) || !view.Blacklisted || view.FailureCount != 3 ||
		ref.FailureGeneration != 6 || ref.ModelCooldownGeneration != 2 ||
		!view.ModelCooldowns["gpt-4o"].Equal(cooldown) || view.AuthState != CredentialAuthStateRefreshing {
		t.Fatalf("view after weight reload = %#v ref=%#v", view, ref)
	}
	// The mirrored (epoch, version) survives too, so an older state stays out.
	if registry.ApplySharedHealth(1, SharedCredentialHealth{Epoch: "e", Version: 3, IdentityGeneration: 7}) {
		t.Fatal("reconcile dropped the mirrored version")
	}

	// A new secret version keeps health but takes the auth state from DB.
	rotated := reweighted
	rotated.Version, rotated.Fingerprint, rotated.EncryptedValue = 4, "fingerprint-2", "cipher-2"
	rotated.AuthState = CredentialAuthStateReady
	if _, err := registry.ReconcileGroup(10, []CredentialEntry{rotated}); err != nil {
		t.Fatal(err)
	}
	view = onlyView(t, registry)
	if !view.Blacklisted || view.AuthState != CredentialAuthStateReady {
		t.Fatalf("view after rotation = %#v, want blacklist kept and DB auth state", view)
	}

	// A new identity starts from zero health and zero generations.
	moved := rotated
	moved.IdentityGeneration = 8
	if _, err := registry.ReconcileGroup(10, []CredentialEntry{moved}); err != nil {
		t.Fatal(err)
	}
	view = onlyView(t, registry)
	ref, _ = registry.CredentialRef(1)
	if view.Blacklisted || !view.CooldownUntil.IsZero() || view.FailureCount != 0 || len(view.ModelCooldowns) != 0 ||
		ref.FailureGeneration != 0 || ref.ModelCooldownGeneration != 0 {
		t.Fatalf("view after identity change = %#v ref=%#v", view, ref)
	}
	if !registry.ApplySharedHealth(1, SharedCredentialHealth{Epoch: "e", Version: 1, IdentityGeneration: 8, FailureCount: 1}) {
		t.Fatal("first state of the new identity was not applied")
	}
}

func TestLocalReconcileStillResetsHealthOnConfigChange(t *testing.T) {
	registry := NewCredentialRegistry()
	entry := sharedHealthEntry(1)
	mustReplaceKeyEntries(t, registry, []CredentialEntry{entry})
	registry.SetBlacklisted(1)
	reweighted := entry
	reweighted.WeightManual = intPointer(5)
	if _, err := registry.ReconcileGroup(10, []CredentialEntry{reweighted}); err != nil {
		t.Fatal(err)
	}
	if onlyView(t, registry).Blacklisted {
		t.Fatal("single-instance reconcile kept health across a config change")
	}
}

func TestSharedReplaceCredentialsKeepsMirroredHealth(t *testing.T) {
	entry := sharedHealthEntry(1)
	registry := newSharedHealthRegistry(t, entry)
	registry.ApplySharedHealth(1, SharedCredentialHealth{
		Epoch: "e", Version: 2, IdentityGeneration: 7, Blacklisted: true, FailureCount: 3, FailureGeneration: 5,
	})
	// A post-commit recovery rebuilds every entry from the database.
	if err := registry.ReplaceCredentials([]CredentialEntry{entry}); err != nil {
		t.Fatal(err)
	}
	view := onlyView(t, registry)
	ref, _ := registry.CredentialRef(1)
	if !view.Blacklisted || view.FailureCount != 3 || ref.FailureGeneration != 5 {
		t.Fatalf("view after ReplaceCredentials = %#v ref=%#v, want mirrored health kept", view, ref)
	}
}
