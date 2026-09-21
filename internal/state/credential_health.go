package state

import (
	"sort"
	"time"
)

// CredentialHealth is what one instance decided would keep a credential out of
// selection: a cooldown, a blacklist, and the models it is refusing. It is the
// unit that crosses instance boundaries, so a peer that applies it stops
// picking the credential for the same reasons.
//
// Every field here is a reason to avoid the credential, and reasons only add
// up — a later cooldown, a blacklist, one more refused model. That is what
// makes the merge order-independent: two instances can decide at the same
// moment, in either order, and both end up avoiding the credential for the
// union of both reasons. The instance's own failure count is deliberately not
// here; it never decided selection, and sharing it is what used to let a stale
// record put a credential back into rotation.
//
// ResetGen is the exception that proves it. Clearing a credential is the one
// decision that takes reasons away, so it carries a counter: a record stamped
// below the reset never applies, and a reset always beats what came before it.
//
// The identity fields are not decoration. Health belongs to a credential as it
// exists in one group under one identity generation; a peer whose entry moved
// on must reject the health rather than apply it to a credential the decision
// was never about.
type CredentialHealth struct {
	CredentialID       uint
	GroupID            uint
	IdentityGeneration uint64
	ResetGen           uint64
	CooldownUntil      time.Time
	Blacklisted        bool
	ModelCooldowns     map[string]time.Time
}

// markHealthDirtyLocked records that this credential's health changed and has
// to be told to the other instances.
//
// Publication is deliberately deferred to a drain rather than performed here.
// Every health mutation runs under the registry lock, and reaching a network
// from under it would put an upstream failure's blast radius on every other
// credential's scheduling. The notifier only wakes the drain; it must not
// block, and it must not call back into the registry.
//
// Tracking does not wait for a notifier to be installed. A decision made in
// the moment between the registry being loaded and the watch loop starting is
// exactly the decision that must not be lost, and the set costs one credential
// id per credential whether or not anyone ever drains it.
func (r *CredentialRegistry) markHealthDirtyLocked(credentialID uint) {
	if credentialID == 0 {
		return
	}
	if r.healthDirty == nil {
		r.healthDirty = make(map[uint]struct{})
	}
	r.healthDirty[credentialID] = struct{}{}
	if r.healthNotifier != nil {
		r.healthNotifier()
	}
}

// SetHealthChangeNotifier installs the wake-up for the cross-instance health
// drain. It only makes the drain prompt; what has to be drained is recorded
// whether it is installed or not.
func (r *CredentialRegistry) SetHealthChangeNotifier(notifier func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthNotifier = notifier
}

// DrainHealthChanges returns the health of every credential that changed since
// the last drain and forgets them. A credential that changed several times
// appears once, carrying its current state: peers need the outcome, not the
// history.
//
// A credential that disappeared between the change and the drain is dropped.
// Its health is no longer anyone's business, and there is nothing to tell.
func (r *CredentialRegistry) DrainHealthChanges() []CredentialHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.healthDirty) == 0 {
		return nil
	}
	ids := make([]uint, 0, len(r.healthDirty))
	for credentialID := range r.healthDirty {
		ids = append(ids, credentialID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	r.healthDirty = nil

	changes := make([]CredentialHealth, 0, len(ids))
	for _, credentialID := range ids {
		entry, ok := r.entryLocked(credentialID)
		if !ok {
			continue
		}
		changes = append(changes, healthLocked(entry))
	}
	return changes
}

// CredentialHealthSnapshot reports a credential's current health.
func (r *CredentialRegistry) CredentialHealthSnapshot(credentialID uint) (CredentialHealth, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entryLocked(credentialID)
	if !ok {
		return CredentialHealth{}, false
	}
	return healthLocked(entry), true
}

// ApplyRemoteHealth merges a peer's health record into this instance's,
// reporting whether anything changed. It is not marked dirty: republishing
// what a peer just said would make every decision echo around the fleet
// forever.
//
// Merging, not overwriting. Every reason to avoid a credential is kept if
// either side holds it — the later cooldown, a blacklist from either side, the
// union of refused models. That makes the result independent of the order
// records arrive in, which is the only thing that can be relied on here: a
// record is read one or two round trips after it was written, and the registry
// has no way to know what was decided in between. Overwriting whatever the
// local entry held is how a record that predates a live decision used to put a
// failing credential straight back into rotation.
//
// The one decision that removes reasons is a reset, and it carries ResetGen so
// it can be ordered against them. A record stamped below this entry's reset was
// decided before it and is ignored; a higher one is a reset this instance has
// not seen and clears what came before it.
//
// What must never happen is applying health to the wrong credential, which is
// what the identity check prevents.
func (r *CredentialRegistry) ApplyRemoteHealth(health CredentialHealth) bool {
	if health.CredentialID == 0 {
		return false
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entryLocked(health.CredentialID)
	if !ok || entry.GroupID != health.GroupID ||
		entry.IdentityGeneration != health.IdentityGeneration {
		return false
	}

	changed := false
	if health.ResetGen > entry.ResetGen {
		entry.ResetGen = health.ResetGen
		if !entry.CooldownUntil.IsZero() || entry.Blacklisted || entry.FailureCount != 0 {
			entry.CooldownUntil = time.Time{}
			entry.Blacklisted = false
			entry.FailureCount = 0
			changed = true
		}
	}
	// Equal generations mean neither side has heard of a reset the other has,
	// so both records describe the same round and their reasons add up. A
	// lower one was decided before this instance's reset and says nothing.
	if health.ResetGen == entry.ResetGen {
		if health.CooldownUntil.After(entry.CooldownUntil) {
			entry.CooldownUntil = health.CooldownUntil
			changed = true
		}
		if health.Blacklisted && !entry.Blacklisted {
			entry.Blacklisted = true
			changed = true
		}
	}
	// Model cooldowns are not reset-gated: nothing clears them across
	// instances, and each one expires on its own at an absolute time.
	if mergeModelCooldownsLocked(entry, health.ModelCooldowns, now) {
		entry.ModelCooldownGeneration++
		changed = true
	}
	if !changed {
		return false
	}
	// The failure generation is what in-flight decisions on this instance are
	// validated against, so an adopted change has to invalidate them exactly
	// as a local one would.
	entry.FailureGeneration++
	r.scheduling.SyncCredential(runtimeView(entry))
	return true
}

// mergeModelCooldownsLocked keeps the later expiry for every model either side
// is refusing. An entry that has already expired is dropped rather than
// merged: it cannot take the credential out of selection, and adding it back
// would report a change on every read for as long as some peer still holds it.
func mergeModelCooldownsLocked(
	entry *CredentialEntry,
	remote map[string]time.Time,
	now time.Time,
) bool {
	changed := false
	for model, until := range remote {
		if model == "" || !until.After(now) {
			continue
		}
		if current, held := entry.ModelCooldowns[model]; held && !until.After(current) {
			continue
		}
		if entry.ModelCooldowns == nil {
			entry.ModelCooldowns = make(map[string]time.Time, len(remote))
		}
		entry.ModelCooldowns[model] = until
		changed = true
	}
	return changed
}

// healthLocked is the record this instance publishes. Expired model cooldowns
// are left out: they refuse nothing, and sending them would make every peer
// report a change for a credential nobody decided anything new about.
func healthLocked(entry *CredentialEntry) CredentialHealth {
	models := cloneModelCooldowns(entry.ModelCooldowns)
	pruneModelCooldowns(models, time.Now())
	return CredentialHealth{
		CredentialID:       entry.ID,
		GroupID:            entry.GroupID,
		IdentityGeneration: entry.IdentityGeneration,
		ResetGen:           entry.ResetGen,
		CooldownUntil:      entry.CooldownUntil,
		Blacklisted:        entry.Blacklisted,
		ModelCooldowns:     models,
	}
}

func sameModelCooldowns(left, right map[string]time.Time) bool {
	if len(left) != len(right) {
		return false
	}
	for model, until := range left {
		other, exists := right[model]
		if !exists || !other.Equal(until) {
			return false
		}
	}
	return true
}
