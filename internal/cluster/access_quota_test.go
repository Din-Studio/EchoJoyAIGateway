package cluster

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/state"
)

// fakeQuotaStates is an in-memory checkpoint table keyed by rule ID.
type fakeQuotaStates struct {
	mu    sync.Mutex
	rows  map[uint]accessquota.RestoredState
	reads int
	err   error
}

func newFakeQuotaStates(rows ...accessquota.RestoredState) *fakeQuotaStates {
	states := &fakeQuotaStates{rows: make(map[uint]accessquota.RestoredState)}
	for _, row := range rows {
		states.rows[row.RuleID] = row
	}
	return states
}

func (states *fakeQuotaStates) ReadAccessQuotaStates(
	_ context.Context,
	ruleIDs []uint,
) ([]accessquota.RestoredState, error) {
	states.mu.Lock()
	defer states.mu.Unlock()
	states.reads++
	if states.err != nil {
		return nil, states.err
	}
	result := make([]accessquota.RestoredState, 0, len(ruleIDs))
	for _, id := range ruleIDs {
		if row, exists := states.rows[id]; exists {
			result = append(result, row)
		}
	}
	return result, nil
}

func (states *fakeQuotaStates) set(row accessquota.RestoredState) {
	states.mu.Lock()
	states.rows[row.RuleID] = row
	states.mu.Unlock()
}

func (states *fakeQuotaStates) readCount() int {
	states.mu.Lock()
	defer states.mu.Unlock()
	return states.reads
}

// freshCheckpoints mirrors the rows the control plane writes for new rules.
func freshCheckpoints(accessKeyID uint, rules []accessquota.Rule) []accessquota.RestoredState {
	rows := make([]accessquota.RestoredState, 0, len(rules))
	for _, rule := range rules {
		rows = append(rows, accessquota.RestoredState{
			AccessKeyID: accessKeyID, RuleID: rule.ID, RuleRevision: rule.Revision, SnapshotVersion: 1,
		})
	}
	return rows
}

func quotaSnapshot(accessKeyID uint, rules []accessquota.Rule) *state.ConfigSnapshot {
	return &state.ConfigSnapshot{AccessKeysByID: map[uint]state.AccessKeyView{
		accessKeyID: {ID: accessKeyID, CostLimitRules: rules},
	}}
}

// quotaPair drives the in-memory runtime and the Redis implementation with
// the same operations and fails on the first observable difference.
type quotaPair struct {
	t        *testing.T
	runtime  *accessquota.Runtime
	shared   *AccessQuota
	snapshot *state.ConfigSnapshot
	key      uint
}

func (pair quotaPair) admit(now time.Time) (accessquota.Ticket, accessquota.Ticket) {
	pair.t.Helper()
	wantTicket, wantDecision := pair.runtime.Admit(pair.key, now)
	gotTicket, gotDecision, err := pair.shared.Admit(pair.t.Context(), pair.snapshot, pair.key, now)
	if err != nil {
		pair.t.Fatalf("Admit() error = %v", err)
	}
	if !reflect.DeepEqual(gotTicket, wantTicket) || !reflect.DeepEqual(gotDecision, wantDecision) {
		pair.t.Fatalf("Admit() = %#v %#v, want %#v %#v", gotTicket, gotDecision, wantTicket, wantDecision)
	}
	pair.compare(now)
	return wantTicket, gotTicket
}

func (pair quotaPair) complete(runtimeTicket, sharedTicket accessquota.Ticket, cost int64, now time.Time) {
	pair.t.Helper()
	want := pair.runtime.Complete(runtimeTicket, cost)
	got, err := pair.shared.Complete(pair.t.Context(), sharedTicket, cost)
	if err != nil {
		pair.t.Fatalf("Complete() error = %v", err)
	}
	if got != want {
		pair.t.Fatalf("Complete(%d) = %#v, want %#v", cost, got, want)
	}
	if pair.shared.Stats() != pair.runtime.Stats() {
		pair.t.Fatalf("Stats() = %#v, want %#v", pair.shared.Stats(), pair.runtime.Stats())
	}
	pair.compare(now)
}

func (pair quotaPair) compare(now time.Time) {
	pair.t.Helper()
	gotDecision, err := pair.shared.Check(pair.t.Context(), pair.snapshot, pair.key, now)
	if err != nil {
		pair.t.Fatalf("Check() error = %v", err)
	}
	if want := pair.runtime.Check(pair.key, now); !reflect.DeepEqual(gotDecision, want) {
		pair.t.Fatalf("Check(%d) = %#v, want %#v", now.UnixMilli(), gotDecision, want)
	}
	gotView, err := pair.shared.View(pair.t.Context(), pair.snapshot, pair.key, now)
	if err != nil {
		pair.t.Fatalf("View() error = %v", err)
	}
	if want := pair.runtime.Snapshot(pair.key, now); !reflect.DeepEqual(gotView, want) {
		pair.t.Fatalf("View(%d) = %#v, want %#v", now.UnixMilli(), gotView, want)
	}
}

func TestAccessQuotaMatchesRuntimeContract(t *testing.T) {
	_, client := newTestClient(t)
	rules := []accessquota.Rule{
		{ID: 10, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: 100},
		{ID: 11, Revision: 1, Kind: accessquota.KindPeriodic, LimitNanoUSD: 20, PeriodSeconds: 5 * 60 * 60},
		{ID: 12, Revision: 1, Kind: accessquota.KindPeriodic, LimitNanoUSD: 30, PeriodSeconds: 24 * 60 * 60},
	}
	runtime := accessquota.NewRuntime()
	if err := runtime.Reconcile(map[uint][]accessquota.Rule{1: rules}); err != nil {
		t.Fatal(err)
	}
	states := newFakeQuotaStates(freshCheckpoints(1, rules)...)
	pair := quotaPair{t: t, runtime: runtime, shared: NewAccessQuota(client, states), snapshot: quotaSnapshot(1, rules), key: 1}
	started := time.Date(2026, time.August, 20, 10, 37, 0, 123_456_789, time.UTC)

	pair.compare(started)
	runtimeTicket, sharedTicket := pair.admit(started)
	pair.complete(runtimeTicket, sharedTicket, 20, started.Add(time.Second))
	pair.compare(started.Add(time.Hour))             // 5h window exhausted
	blocked, _ := pair.admit(started.Add(time.Hour)) // denied without opening windows
	if len(blocked.Rules) != 0 {
		t.Fatalf("blocked ticket = %#v", blocked)
	}
	boundary := started.Add(5 * time.Hour)
	runtimeTicket, sharedTicket = pair.admit(boundary) // reopens the 5h window only
	pair.complete(runtimeTicket, sharedTicket, 15, boundary)
	pair.compare(started.Add(25 * time.Hour))
	runtimeTicket, sharedTicket = pair.admit(started.Add(25 * time.Hour))
	pair.complete(runtimeTicket, sharedTicket, 0, started.Add(25*time.Hour))
	pair.complete(runtimeTicket, sharedTicket, 100, started.Add(25*time.Hour)) // total exhausted
	pair.admit(started.Add(26 * time.Hour))

	wantDirty := runtime.DirtySnapshots(-1)
	gotDirty, err := pair.shared.DirtySnapshots(t.Context(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotDirty, wantDirty) {
		t.Fatalf("DirtySnapshots() = %#v, want %#v", gotDirty, wantDirty)
	}

	// A control-plane reset bumps the revision and zeroes the checkpoint row.
	oldRuntimeTicket, oldSharedTicket := runtimeTicket, sharedTicket
	next := append([]accessquota.Rule(nil), rules...)
	next[0].Revision = 2
	if err := runtime.Reconcile(map[uint][]accessquota.Rule{1: next}); err != nil {
		t.Fatal(err)
	}
	states.set(accessquota.RestoredState{AccessKeyID: 1, RuleID: 10, RuleRevision: 2, SnapshotVersion: 1})
	oldSnapshot := pair.snapshot
	pair.snapshot = quotaSnapshot(1, next)
	now := started.Add(27 * time.Hour)
	pair.compare(now)
	pair.complete(oldRuntimeTicket, oldSharedTicket, 50, now) // stale ticket ignored for the reset rule
	if _, err := pair.shared.Check(t.Context(), oldSnapshot, 1, now); !errors.Is(err, accessquota.ErrStaleRules) {
		t.Fatalf("Check(old snapshot) error = %v, want ErrStaleRules", err)
	}
	runtimeTicket, sharedTicket = pair.admit(now)
	pair.complete(runtimeTicket, sharedTicket, 7, now)
}

func TestAccessQuotaSaturatesLikeRuntime(t *testing.T) {
	for _, test := range []struct {
		name string
		used int64
		cost int64
	}{
		{name: "fits below limit", used: math.MaxInt64 - 3, cost: 1},
		{name: "reaches limit", used: math.MaxInt64 - 3, cost: 2},
		{name: "lands exactly on MaxInt64", used: math.MaxInt64 - 3, cost: 3},
		{name: "overflows", used: math.MaxInt64 - 3, cost: 5},
		{name: "already saturated", used: math.MaxInt64, cost: 1},
		{name: "negative estimate", used: 10, cost: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, client := newTestClient(t)
			rules := []accessquota.Rule{{ID: 20, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: math.MaxInt64 - 1}}
			checkpoint := accessquota.RestoredState{AccessKeyID: 2, RuleID: 20, RuleRevision: 1, UsedNanoUSD: test.used, SnapshotVersion: 4}
			runtime := accessquota.NewRuntime()
			if err := runtime.Restore([]accessquota.RestoredState{checkpoint}); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Reconcile(map[uint][]accessquota.Rule{2: rules}); err != nil {
				t.Fatal(err)
			}
			pair := quotaPair{
				t: t, runtime: runtime, shared: NewAccessQuota(client, newFakeQuotaStates(checkpoint)),
				snapshot: quotaSnapshot(2, rules), key: 2,
			}
			now := time.Unix(1_000, 0)
			pair.compare(now)
			ticket := accessquota.Ticket{AccessKeyID: 2, Rules: []accessquota.TicketRule{{RuleID: 20, RuleRevision: 1}}}
			pair.complete(ticket, ticket, test.cost, now)
		})
	}
}

func TestAccessQuotaHydratesFromCheckpointOnce(t *testing.T) {
	server, client := newTestClient(t)
	rules := []accessquota.Rule{{ID: 30, Revision: 3, Kind: accessquota.KindTotal, LimitNanoUSD: 100}}
	states := newFakeQuotaStates(accessquota.RestoredState{
		AccessKeyID: 3, RuleID: 30, RuleRevision: 3, UsedNanoUSD: 60, SnapshotVersion: 9,
	})
	quota := NewAccessQuota(client, states)
	snapshot := quotaSnapshot(3, rules)
	now := time.Unix(2_000, 0)

	for range 2 {
		view, err := quota.View(t.Context(), snapshot, 3, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Rules) != 1 || view.Rules[0].UsedNanoUSD != 60 {
			t.Fatalf("View() = %#v, want checkpoint usage", view)
		}
	}
	if states.readCount() != 1 {
		t.Fatalf("checkpoint reads = %d, want 1", states.readCount())
	}

	// Redis losing the key falls back to the checkpoint again.
	server.Del(client.Key(accessKeyHashTag(3), "quota", "30"))
	if _, err := quota.Check(t.Context(), snapshot, 3, now); err != nil {
		t.Fatal(err)
	}
	if states.readCount() != 2 {
		t.Fatalf("checkpoint reads after key loss = %d, want 2", states.readCount())
	}

	// Keys without rules never touch Redis or the database.
	if decision, err := quota.Check(t.Context(), quotaSnapshot(4, nil), 4, now); err != nil || !decision.Allowed {
		t.Fatalf("Check(no rules) = %#v, %v", decision, err)
	}
	if _, _, err := quota.Admit(t.Context(), nil, 4, now); err != nil {
		t.Fatalf("Admit(nil snapshot) error = %v", err)
	}
}

func TestAccessQuotaRejectsStaleOrInconsistentCheckpoints(t *testing.T) {
	rules := []accessquota.Rule{{ID: 40, Revision: 2, Kind: accessquota.KindTotal, LimitNanoUSD: 100}}
	now := time.Unix(3_000, 0)
	for _, test := range []struct {
		name      string
		rows      []accessquota.RestoredState
		readErr   error
		wantStale bool
	}{
		{name: "rule deleted", wantStale: true},
		{name: "checkpoint revision newer", rows: []accessquota.RestoredState{{AccessKeyID: 5, RuleID: 40, RuleRevision: 3, SnapshotVersion: 1}}, wantStale: true},
		{name: "checkpoint revision older", rows: []accessquota.RestoredState{{AccessKeyID: 5, RuleID: 40, RuleRevision: 1, SnapshotVersion: 1}}},
		{name: "checkpoint for other key", rows: []accessquota.RestoredState{{AccessKeyID: 6, RuleID: 40, RuleRevision: 2, SnapshotVersion: 1}}},
		{name: "invalid checkpoint", rows: []accessquota.RestoredState{{AccessKeyID: 5, RuleID: 40, RuleRevision: 2}}},
		{name: "database unavailable", readErr: errors.New("db down")},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, client := newTestClient(t)
			states := newFakeQuotaStates(test.rows...)
			states.err = test.readErr
			_, err := NewAccessQuota(client, states).Check(t.Context(), quotaSnapshot(5, rules), 5, now)
			if err == nil || errors.Is(err, accessquota.ErrStaleRules) != test.wantStale {
				t.Fatalf("Check() error = %v, want stale=%v", err, test.wantStale)
			}
		})
	}

	t.Run("redis holds a newer revision", func(t *testing.T) {
		_, client := newTestClient(t)
		newer := []accessquota.Rule{{ID: 40, Revision: 3, Kind: accessquota.KindTotal, LimitNanoUSD: 100}}
		states := newFakeQuotaStates(accessquota.RestoredState{AccessKeyID: 5, RuleID: 40, RuleRevision: 3, SnapshotVersion: 1})
		quota := NewAccessQuota(client, states)
		if _, err := quota.Check(t.Context(), quotaSnapshot(5, newer), 5, now); err != nil {
			t.Fatal(err)
		}
		if _, _, err := quota.Admit(t.Context(), quotaSnapshot(5, rules), 5, now); !errors.Is(err, accessquota.ErrStaleRules) {
			t.Fatalf("Admit(old snapshot) error = %v, want ErrStaleRules", err)
		}
	})
}

func TestAccessQuotaSharesCountAcrossInstances(t *testing.T) {
	_, client := newTestClient(t)
	rules := []accessquota.Rule{
		{ID: 50, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: 1_000},
		{ID: 51, Revision: 1, Kind: accessquota.KindPeriodic, LimitNanoUSD: 300, PeriodSeconds: 60},
	}
	states := newFakeQuotaStates(freshCheckpoints(7, rules)...)
	snapshot := quotaSnapshot(7, rules)
	instances := []*AccessQuota{NewAccessQuota(client, states), NewAccessQuota(client, states), NewAccessQuota(client, states)}
	runtime := accessquota.NewRuntime()
	if err := runtime.Reconcile(map[uint][]accessquota.Rule{7: rules}); err != nil {
		t.Fatal(err)
	}

	const price = 70
	start := time.Unix(10_000, 0)
	sharedAdmitted, runtimeAdmitted := 0, 0
	for request := range 60 {
		now := start.Add(time.Duration(request) * 5 * time.Second)
		instance := instances[request%len(instances)]
		ticket, decision, err := instance.Admit(t.Context(), snapshot, 7, now)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Allowed {
			sharedAdmitted++
			if _, err := instance.Complete(t.Context(), ticket, price); err != nil {
				t.Fatal(err)
			}
		}
		if ticket, decision := runtime.Admit(7, now); decision.Allowed {
			runtimeAdmitted++
			runtime.Complete(ticket, price)
		}
	}
	if sharedAdmitted != runtimeAdmitted || sharedAdmitted == 0 {
		t.Fatalf("shared admitted %d, single instance admitted %d", sharedAdmitted, runtimeAdmitted)
	}
}

func TestAccessQuotaDirtyTrackingSurvivesConcurrentChanges(t *testing.T) {
	server, client := newTestClient(t)
	rules := []accessquota.Rule{{ID: 60, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: 1_000}}
	quota := NewAccessQuota(client, newFakeQuotaStates(freshCheckpoints(8, rules)...))
	wakes := 0
	quota.SetDirtyNotifier(func() { wakes++ })
	snapshot := quotaSnapshot(8, rules)
	now := time.Unix(20_000, 0)
	ticket, _, err := quota.Admit(t.Context(), snapshot, 8, now)
	if err != nil {
		t.Fatal(err)
	}
	if quota.HasDirty() {
		t.Fatal("Admit() without window changes marked the rule dirty")
	}
	if _, err := quota.Complete(t.Context(), ticket, 5); err != nil {
		t.Fatal(err)
	}
	dirty, err := quota.DirtySnapshots(t.Context(), 10)
	if err != nil || len(dirty) != 1 || dirty[0].UsedNanoUSD != 5 || dirty[0].SnapshotVersion != 2 {
		t.Fatalf("DirtySnapshots() = %#v, %v", dirty, err)
	}
	if _, err := quota.Complete(t.Context(), ticket, 6); err != nil {
		t.Fatal(err)
	}
	quota.Ack(8, 60, dirty[0].RuleRevision, dirty[0].SnapshotVersion)
	if !quota.HasDirty() {
		t.Fatal("Ack() of an older version dropped a newer change")
	}
	dirty, err = quota.DirtySnapshots(t.Context(), 10)
	if err != nil || len(dirty) != 1 || dirty[0].UsedNanoUSD != 11 {
		t.Fatalf("DirtySnapshots() = %#v, %v", dirty, err)
	}
	quota.Ack(8, 60, dirty[0].RuleRevision, dirty[0].SnapshotVersion)
	if quota.HasDirty() || wakes != 2 {
		t.Fatalf("HasDirty() = %v wakes = %d, want false 2", quota.HasDirty(), wakes)
	}

	// A dirty rule whose key vanished has nothing newer than the database.
	if _, err := quota.Complete(t.Context(), ticket, 1); err != nil {
		t.Fatal(err)
	}
	server.Del(client.Key(accessKeyHashTag(8), "quota", "60"))
	if dirty, err := quota.DirtySnapshots(t.Context(), 10); err != nil || len(dirty) != 0 || quota.HasDirty() {
		t.Fatalf("DirtySnapshots(vanished) = %#v, %v dirty=%v", dirty, err, quota.HasDirty())
	}

	// Rehydrating from a checkpoint older than the local change drops it too.
	ticket, _, _ = quota.Admit(t.Context(), snapshot, 8, now)
	for range 3 {
		if _, err := quota.Complete(t.Context(), ticket, 1); err != nil {
			t.Fatal(err)
		}
	}
	server.Del(client.Key(accessKeyHashTag(8), "quota", "60"))
	if _, err := quota.Check(t.Context(), snapshot, 8, now); err != nil {
		t.Fatal(err)
	}
	if dirty, err := quota.DirtySnapshots(t.Context(), 10); err != nil || len(dirty) != 0 || quota.HasDirty() {
		t.Fatalf("DirtySnapshots(regressed) = %#v, %v dirty=%v", dirty, err, quota.HasDirty())
	}

	if _, err := quota.Check(t.Context(), snapshot, 8, now); err != nil {
		t.Fatal(err)
	}
	ticket, _, _ = quota.Admit(t.Context(), snapshot, 8, now)
	if _, err := quota.Complete(t.Context(), ticket, 1); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if _, err := quota.DirtySnapshots(t.Context(), 10); err == nil {
		t.Fatal("DirtySnapshots() with Redis down error = nil")
	}
	if _, err := quota.Check(t.Context(), snapshot, 8, now); err == nil || errors.Is(err, accessquota.ErrStaleRules) {
		t.Fatalf("Check() with Redis down error = %v, want unavailable", err)
	}
	if _, err := quota.Complete(t.Context(), ticket, 1); err == nil {
		t.Fatal("Complete() with Redis down error = nil")
	}
}
