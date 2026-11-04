package control

import (
	"testing"

	"gpt-load/internal/state"
)

func TestGroupPriorityReloadPreservesRuntimeAndRecalibratesLedger(t *testing.T) {
	pair := newClusterPair(t)
	// Match production assembly: publication must update each replica's ledger.
	pair.a.manager.SetSchedulingState(pair.a.registry.SchedulingState())
	pair.b.manager.SetSchedulingState(pair.b.registry.SchedulingState())
	id := createGroupWithCredentials(t, pair.a, "sk-priority-reload")
	pair.reloadB(t)
	views := registryViewsForGroup(pair.b.registry, id)
	if len(views) != 1 {
		t.Fatalf("credentials = %d", len(views))
	}
	credentialID := views[0].ID
	pair.b.registry.SetBlacklisted(credentialID)
	ledger := pair.b.registry.SchedulingState()
	ledger.WithLock(func(d *state.SchedulingLedger) {
		d.Sequence = 123
		d.Members[credentialID].Progress.Whole = 456
		d.Members[credentialID].Pending = false
	})
	before := pair.reloadB(t)
	_, err := pair.a.service.UpdateGroupSettings(t.Context(), id, GroupSettingsUpdateRequest{
		Priority: optionalField[int32]{Set: true, Value: -100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pair.b.manager.Current().Groups[id].Priority; got != 0 {
		t.Fatalf("peer saw un-reloaded priority %d", got)
	}
	if got := pair.reloadB(t); got <= before {
		t.Fatalf("revision = %d, before = %d", got, before)
	}
	if got := pair.b.manager.Current().Groups[id].Priority; got != -100 {
		t.Fatalf("reloaded priority = %d", got)
	}
	if !registryViewsForGroup(pair.b.registry, id)[0].Blacklisted {
		t.Fatal("priority reload lost credential health")
	}
	ledger.WithLock(func(d *state.SchedulingLedger) {
		member := d.Members[credentialID]
		if d.Sequence != 123 || member.Progress.Whole != 456 || !member.Pending {
			t.Fatalf("priority reload reset history or missed reentry: sequence=%d member=%+v", d.Sequence, member)
		}
	})
}
