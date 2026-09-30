package state

import (
	"context"
	"sort"
	"time"
)

// SharedCredentialHealth is one credential's health as held by the cluster
// store. Epoch and Version order successive states of one store record; an
// empty Epoch means the store holds no record for the credential.
type SharedCredentialHealth struct {
	Epoch                   string
	Version                 uint64
	IdentityGeneration      uint64
	CooldownUntil           time.Time
	Blacklisted             bool
	FailureCount            int
	FailureGeneration       uint64
	ModelCooldownGeneration uint64
	ModelCooldowns          map[string]time.Time
	// AuthState applies only to the secret version AuthSecretVersion.
	AuthState         CredentialAuthState
	AuthSecretVersion uint64
}

// SharedHealthResult reports one shared health operation with the same
// meaning as the matching local CredentialRegistry mutation.
type SharedHealthResult struct {
	Accepted          bool
	Changed           bool
	FailureCount      int
	BecameBlacklisted bool
}

// SharedCredentialHealthStore owns credential health in cluster mode. Every
// method performs one store round trip and applies the resulting state to the
// local registry mirror. Callers must not hold a mutation stripe, publishMu,
// or a registry/stats lock while calling it.
type SharedCredentialHealthStore interface {
	// CooldownCredential extends the cooldown; a non-zero expectedVersion
	// rejects the change once the credential moved to another secret version.
	CooldownCredential(ctx context.Context, ref CredentialRef, until time.Time, expectedVersion uint64) (SharedHealthResult, error)
	// CooldownModel extends one model cooldown while ref's model cooldown
	// generation is still current.
	CooldownModel(ctx context.Context, ref CredentialRef, model string, until, now time.Time) (SharedHealthResult, error)
	// RecordFailure counts one failure and blacklists at threshold (> 0).
	RecordFailure(ctx context.Context, ref CredentialRef, threshold int) (SharedHealthResult, error)
	ClearFailure(ctx context.Context, ref CredentialRef) (SharedHealthResult, error)
	// Restore clears cooldown, blacklist, and failures when runtime is set,
	// and all model cooldowns when modelCooldowns is set.
	Restore(ctx context.Context, ref CredentialRef, runtime, modelCooldowns bool) (SharedHealthResult, error)
	// RecoverIfMatch lifts a blacklist observed at ref's failure generation
	// and, when cooldownUntil is non-nil, the cooldown observed with it.
	RecoverIfMatch(ctx context.Context, ref CredentialRef, cooldownUntil *time.Time) (SharedHealthResult, error)
	ClearCooldownIfMatch(ctx context.Context, ref CredentialRef, expected time.Time) (SharedHealthResult, error)
	// SetAuthState records the auth state of one secret version; an older
	// secret version than the stored one is rejected.
	SetAuthState(ctx context.Context, ref CredentialRef, authState CredentialAuthState, secretVersion uint64) (SharedHealthResult, error)
	// ReadHealth returns the stored records; a credential without a record
	// maps to the zero state with an empty Epoch.
	ReadHealth(ctx context.Context, credentialIDs []uint) (map[uint]SharedCredentialHealth, error)
	// ReplaceAuthState is SetAuthState conditioned on the record still being
	// the observed one (same Epoch and Version), so a republish computed from
	// an earlier read never overwrites a newer write.
	ReplaceAuthState(ctx context.Context, ref CredentialRef, authState CredentialAuthState, secretVersion uint64, observed SharedCredentialHealth) (SharedHealthResult, error)
}

// EnableSharedHealth switches the registry to mirror a cluster store: peer
// reloads keep mirrored health, and local health mutations become fallbacks
// that the next store state overrides. It must be called before loading.
func (r *CredentialRegistry) EnableSharedHealth() {
	r.mu.Lock()
	r.sharedHealth = true
	r.mu.Unlock()
}

// CredentialIDs lists every registered credential in stable order.
func (r *CredentialRegistry) CredentialIDs() []uint {
	r.mu.RLock()
	ids := make([]uint, 0, len(r.credentialGroups))
	for id := range r.credentialGroups {
		ids = append(ids, id)
	}
	r.mu.RUnlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ApplySharedHealth mirrors one store state. States of another identity
// generation are ignored; within one epoch an older version never applies,
// so reordered deliveries converge. An equal version is applied again: the
// store bumps the version on every change, so it carries the same content,
// and reapplying it replaces any local fallback written while the store was
// unreachable. An empty state replaces a
// previously mirrored one because the store lost the record. A concurrent
// first write can race such an empty read and briefly roll the mirror back;
// the next event or reconciliation corrects it.
func (r *CredentialRegistry) ApplySharedHealth(credentialID uint, health SharedCredentialHealth) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entryLocked(credentialID)
	if !ok {
		return false
	}
	if health.Epoch == "" {
		if entry.sharedEpoch == "" {
			return false
		}
		health = SharedCredentialHealth{IdentityGeneration: entry.IdentityGeneration}
	} else if health.IdentityGeneration != entry.IdentityGeneration ||
		health.Epoch == entry.sharedEpoch && health.Version < entry.sharedVersion {
		return false
	}
	entry.CooldownUntil = health.CooldownUntil
	entry.Blacklisted = health.Blacklisted
	entry.FailureCount = health.FailureCount
	entry.FailureGeneration = health.FailureGeneration
	entry.ModelCooldownGeneration = health.ModelCooldownGeneration
	entry.ModelCooldowns = cloneModelCooldowns(health.ModelCooldowns)
	entry.sharedEpoch, entry.sharedVersion = health.Epoch, health.Version
	if health.AuthState != "" && health.AuthSecretVersion == entry.Version && health.AuthState.valid() {
		setEntryAuthStateLocked(entry, health.AuthState)
	}
	r.scheduling.SyncCredential(runtimeView(entry))
	return true
}

// CredentialFailureCount returns the mirrored consecutive failure count.
func (r *CredentialRegistry) CredentialFailureCount(credentialID uint) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entryLocked(credentialID)
	if !ok {
		return 0, false
	}
	return entry.FailureCount, true
}

// CredentialMatchesRef reports whether an active credential still has ref's
// durable identity. Health generations are intentionally excluded.
func (r *CredentialRegistry) CredentialMatchesRef(ref CredentialRef) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entryLocked(ref.ID)
	return ok && entry.Status == CredentialStatusActive && sameCredentialIdentity(entry, ref)
}

func sameCredentialIdentity(entry *CredentialEntry, ref CredentialRef) bool {
	return entry.GroupID == ref.GroupID && entry.Version == ref.Version &&
		entry.IdentityGeneration == ref.IdentityGeneration &&
		entry.Fingerprint == ref.Fingerprint && entry.EncryptedValue == ref.EncryptedValue &&
		entry.EncryptedProxy == ref.EncryptedProxy && entry.ProxyFingerprint == ref.ProxyFingerprint
}

// preserveHealthLocked carries runtime health from previous into an entry
// rebuilt from persisted configuration.
func (r *CredentialRegistry) preserveHealthLocked(next *CredentialEntry, previous *CredentialEntry) {
	if r.sharedHealth {
		preserveSharedHealth(next, previous)
		return
	}
	preserveModelCooldowns(next, previous)
}

// preserveSharedHealth carries mirrored health across a configuration
// replacement of the same identity. The auth state belongs to one secret
// version, so it survives only when the version is unchanged.
func preserveSharedHealth(next *CredentialEntry, previous *CredentialEntry) {
	if previous == nil || next.ID != previous.ID || next.GroupID != previous.GroupID ||
		next.IdentityGeneration != previous.IdentityGeneration {
		// A new identity starts with fresh generations, matching the store,
		// which resets its record when it sees the new identity.
		next.ModelCooldowns = nil
		next.ModelCooldownGeneration = 0
		next.FailureGeneration = 0
		return
	}
	next.CooldownUntil = previous.CooldownUntil
	next.Blacklisted = previous.Blacklisted
	next.FailureCount = previous.FailureCount
	next.FailureGeneration = previous.FailureGeneration
	next.ModelCooldowns = cloneModelCooldowns(previous.ModelCooldowns)
	next.ModelCooldownGeneration = previous.ModelCooldownGeneration
	next.sharedEpoch, next.sharedVersion = previous.sharedEpoch, previous.sharedVersion
	if next.Version == previous.Version {
		next.AuthState = previous.AuthState
	}
}
