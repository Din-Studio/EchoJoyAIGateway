package state

import (
	"sort"
	"time"
)

// CredentialHealth is everything one instance decided about a credential's
// health: whether it is cooling down, blacklisted, how many failures it has
// accumulated, and which models it is refusing. It is the unit that crosses
// instance boundaries, so a peer that applies it reaches the same scheduling
// decision as the instance that produced it.
//
// The identity fields are not decoration. Health belongs to a credential as it
// exists in one group under one identity generation; a peer whose entry moved
// on must reject the health rather than apply it to a credential the decision
// was never about.
type CredentialHealth struct {
	CredentialID       uint
	GroupID            uint
	IdentityGeneration uint64
	CooldownUntil      time.Time
	Blacklisted        bool
	FailureCount       int
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

// ApplyRemoteHealth adopts a peer's health decision, reporting whether
// anything changed. It is not marked dirty: republishing what a peer just said
// would make every decision echo around the fleet forever.
//
// Applying is last-writer-wins by design. Two instances can decide about the
// same credential at the same moment, and the loser's decision is not lost —
// it is published in turn and converges. What must never happen is applying
// health to the wrong credential, which is what the identity check prevents.
func (r *CredentialRegistry) ApplyRemoteHealth(health CredentialHealth) bool {
	if health.CredentialID == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entryLocked(health.CredentialID)
	if !ok || entry.GroupID != health.GroupID ||
		entry.IdentityGeneration != health.IdentityGeneration {
		return false
	}

	changed := false
	if !entry.CooldownUntil.Equal(health.CooldownUntil) {
		entry.CooldownUntil = health.CooldownUntil
		changed = true
	}
	if entry.Blacklisted != health.Blacklisted {
		entry.Blacklisted = health.Blacklisted
		changed = true
	}
	if entry.FailureCount != health.FailureCount {
		entry.FailureCount = health.FailureCount
		changed = true
	}
	if !sameModelCooldowns(entry.ModelCooldowns, health.ModelCooldowns) {
		entry.ModelCooldowns = cloneModelCooldowns(health.ModelCooldowns)
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

func healthLocked(entry *CredentialEntry) CredentialHealth {
	return CredentialHealth{
		CredentialID:       entry.ID,
		GroupID:            entry.GroupID,
		IdentityGeneration: entry.IdentityGeneration,
		CooldownUntil:      entry.CooldownUntil,
		Blacklisted:        entry.Blacklisted,
		FailureCount:       entry.FailureCount,
		ModelCooldowns:     cloneModelCooldowns(entry.ModelCooldowns),
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
