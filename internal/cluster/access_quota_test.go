package cluster

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
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

// quotaScenario drives the Redis implementation and compares every observable
// result with expectations recorded from the former in-process reference.
type quotaScenario struct {
	t        *testing.T
	quota    *AccessQuota
	snapshot *state.ConfigSnapshot
	key      uint
}

func (scenario quotaScenario) observe(now time.Time) string {
	scenario.t.Helper()
	decision, err := scenario.quota.Check(scenario.t.Context(), scenario.snapshot, scenario.key, now)
	if err != nil {
		scenario.t.Fatalf("Check() error = %v", err)
	}
	view, err := scenario.quota.View(scenario.t.Context(), scenario.snapshot, scenario.key, now)
	if err != nil {
		scenario.t.Fatalf("View() error = %v", err)
	}
	return "check: " + formatQuotaDecision(decision) + "\nview: " + formatQuotaView(view)
}

func (scenario quotaScenario) check(now time.Time, want string) {
	scenario.t.Helper()
	assertQuotaText(scenario.t, fmt.Sprintf("Check(%d)", now.UnixMilli()), scenario.observe(now), want)
}

func (scenario quotaScenario) admit(now time.Time, want string) accessquota.Ticket {
	scenario.t.Helper()
	ticket, decision, err := scenario.quota.Admit(scenario.t.Context(), scenario.snapshot, scenario.key, now)
	if err != nil {
		scenario.t.Fatalf("Admit() error = %v", err)
	}
	got := "ticket: " + formatQuotaTicket(ticket) + "\ndecision: " + formatQuotaDecision(decision) +
		"\n" + scenario.observe(now)
	assertQuotaText(scenario.t, fmt.Sprintf("Admit(%d)", now.UnixMilli()), got, want)
	return ticket
}

func (scenario quotaScenario) complete(ticket accessquota.Ticket, cost int64, now time.Time, want string) {
	scenario.t.Helper()
	result, err := scenario.quota.Complete(scenario.t.Context(), ticket, cost)
	if err != nil {
		scenario.t.Fatalf("Complete() error = %v", err)
	}
	got := fmt.Sprintf("complete: fault=%q\n", result.Fault) +
		scenario.observe(now)
	assertQuotaText(scenario.t, fmt.Sprintf("Complete(%d)", cost), got, want)
}

func assertQuotaText(t *testing.T, operation, got, want string) {
	t.Helper()
	if got != strings.TrimSpace(want) {
		t.Fatalf("%s =\n%s\nwant\n%s", operation, got, strings.TrimSpace(want))
	}
}

func TestAccessQuotaEnforcesTotalAndPeriodicWindows(t *testing.T) {
	_, client := newTestClient(t)
	rules := []accessquota.Rule{
		{ID: 10, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: 100},
		{ID: 11, Revision: 1, Kind: accessquota.KindPeriodic, LimitNanoUSD: 20, PeriodSeconds: 5 * 60 * 60},
		{ID: 12, Revision: 1, Kind: accessquota.KindPeriodic, LimitNanoUSD: 30, PeriodSeconds: 24 * 60 * 60},
	}
	states := newFakeQuotaStates(freshCheckpoints(1, rules)...)
	scenario := quotaScenario{t: t, quota: NewAccessQuota(client, states), snapshot: quotaSnapshot(1, rules), key: 1}
	started := time.Date(2026, time.August, 20, 10, 37, 0, 123_456_789, time.UTC)

	scenario.check(started, `
check: allowed=true recoverable=true next=- blocking=[]
view: at=1787222220123 allowed=true recoverable=true next=- rules=[10@1 used=0 left=100 available window=-..-#0; 11@1 used=0 left=20 inactive window=-..-#0; 12@1 used=0 left=30 inactive window=-..-#0]`)
	ticket := scenario.admit(started, `
ticket: key=1 [10@1#0 11@1#1 12@1#1]
decision: allowed=true recoverable=true next=- blocking=[]
check: allowed=true recoverable=true next=- blocking=[]
view: at=1787222220123 allowed=true recoverable=true next=- rules=[10@1 used=0 left=100 available window=-..-#0; 11@1 used=0 left=20 available window=1787222220123..1787240220123#1; 12@1 used=0 left=30 available window=1787222220123..1787308620123#1]`)
	scenario.complete(ticket, 20, started.Add(time.Second), `
complete: fault=""
check: allowed=false recoverable=true next=1787240220123 blocking=[11@1 used=20 left=0 exhausted window=1787222220123..1787240220123#1]
view: at=1787222221123 allowed=false recoverable=true next=1787240220123 rules=[10@1 used=20 left=80 available window=-..-#0; 11@1 used=20 left=0 exhausted window=1787222220123..1787240220123#1; 12@1 used=20 left=10 available window=1787222220123..1787308620123#1]`)
	// The 5h window is exhausted.
	scenario.check(started.Add(time.Hour), `
check: allowed=false recoverable=true next=1787240220123 blocking=[11@1 used=20 left=0 exhausted window=1787222220123..1787240220123#1]
view: at=1787225820123 allowed=false recoverable=true next=1787240220123 rules=[10@1 used=20 left=80 available window=-..-#0; 11@1 used=20 left=0 exhausted window=1787222220123..1787240220123#1; 12@1 used=20 left=10 available window=1787222220123..1787308620123#1]`)
	// Denied without opening windows.
	scenario.admit(started.Add(time.Hour), `
ticket: key=1 []
decision: allowed=false recoverable=true next=1787240220123 blocking=[11@1 used=20 left=0 exhausted window=1787222220123..1787240220123#1]
check: allowed=false recoverable=true next=1787240220123 blocking=[11@1 used=20 left=0 exhausted window=1787222220123..1787240220123#1]
view: at=1787225820123 allowed=false recoverable=true next=1787240220123 rules=[10@1 used=20 left=80 available window=-..-#0; 11@1 used=20 left=0 exhausted window=1787222220123..1787240220123#1; 12@1 used=20 left=10 available window=1787222220123..1787308620123#1]`)
	boundary := started.Add(5 * time.Hour)
	// Reopens the 5h window only.
	ticket = scenario.admit(boundary, `
ticket: key=1 [10@1#0 11@1#2 12@1#1]
decision: allowed=true recoverable=true next=- blocking=[]
check: allowed=true recoverable=true next=- blocking=[]
view: at=1787240220123 allowed=true recoverable=true next=- rules=[10@1 used=20 left=80 available window=-..-#0; 11@1 used=0 left=20 available window=1787240220123..1787258220123#2; 12@1 used=20 left=10 available window=1787222220123..1787308620123#1]`)
	scenario.complete(ticket, 15, boundary, `
complete: fault=""
check: allowed=false recoverable=true next=1787308620123 blocking=[12@1 used=35 left=0 exhausted window=1787222220123..1787308620123#1]
view: at=1787240220123 allowed=false recoverable=true next=1787308620123 rules=[10@1 used=35 left=65 available window=-..-#0; 11@1 used=15 left=5 available window=1787240220123..1787258220123#2; 12@1 used=35 left=0 exhausted window=1787222220123..1787308620123#1]`)
	scenario.check(started.Add(25*time.Hour), `
check: allowed=true recoverable=true next=- blocking=[]
view: at=1787312220123 allowed=true recoverable=true next=- rules=[10@1 used=35 left=65 available window=-..-#0; 11@1 used=0 left=20 inactive window=-..-#2; 12@1 used=0 left=30 inactive window=-..-#1]`)
	ticket = scenario.admit(started.Add(25*time.Hour), `
ticket: key=1 [10@1#0 11@1#3 12@1#2]
decision: allowed=true recoverable=true next=- blocking=[]
check: allowed=true recoverable=true next=- blocking=[]
view: at=1787312220123 allowed=true recoverable=true next=- rules=[10@1 used=35 left=65 available window=-..-#0; 11@1 used=0 left=20 available window=1787312220123..1787330220123#3; 12@1 used=0 left=30 available window=1787312220123..1787398620123#2]`)
	scenario.complete(ticket, 0, started.Add(25*time.Hour), `
complete: fault=""
check: allowed=true recoverable=true next=- blocking=[]
view: at=1787312220123 allowed=true recoverable=true next=- rules=[10@1 used=35 left=65 available window=-..-#0; 11@1 used=0 left=20 available window=1787312220123..1787330220123#3; 12@1 used=0 left=30 available window=1787312220123..1787398620123#2]`)
	// The total quota is exhausted.
	scenario.complete(ticket, 100, started.Add(25*time.Hour), `
complete: fault=""
check: allowed=false recoverable=false next=- blocking=[10@1 used=135 left=0 exhausted window=-..-#0; 11@1 used=100 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=100 left=0 exhausted window=1787312220123..1787398620123#2]
view: at=1787312220123 allowed=false recoverable=false next=- rules=[10@1 used=135 left=0 exhausted window=-..-#0; 11@1 used=100 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=100 left=0 exhausted window=1787312220123..1787398620123#2]`)
	scenario.admit(started.Add(26*time.Hour), `
ticket: key=1 []
decision: allowed=false recoverable=false next=- blocking=[10@1 used=135 left=0 exhausted window=-..-#0; 11@1 used=100 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=100 left=0 exhausted window=1787312220123..1787398620123#2]
check: allowed=false recoverable=false next=- blocking=[10@1 used=135 left=0 exhausted window=-..-#0; 11@1 used=100 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=100 left=0 exhausted window=1787312220123..1787398620123#2]
view: at=1787315820123 allowed=false recoverable=false next=- rules=[10@1 used=135 left=0 exhausted window=-..-#0; 11@1 used=100 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=100 left=0 exhausted window=1787312220123..1787398620123#2]`)

	dirty, err := scenario.quota.DirtySnapshots(t.Context(), -1)
	if err != nil {
		t.Fatal(err)
	}
	assertQuotaText(t, "DirtySnapshots()", "dirty:\n"+formatQuotaStates(dirty), `
dirty:
key=1 rule=10@1 used=135 window=-..-#0 version=4
key=1 rule=11@1 used=100 window=1787312220123..1787330220123#3 version=7
key=1 rule=12@1 used=100 window=1787312220123..1787398620123#2 version=6`)

	// A control-plane reset bumps the revision and zeroes the checkpoint row.
	oldTicket := ticket
	next := append([]accessquota.Rule(nil), rules...)
	next[0].Revision = 2
	states.set(accessquota.RestoredState{AccessKeyID: 1, RuleID: 10, RuleRevision: 2, SnapshotVersion: 1})
	oldSnapshot := scenario.snapshot
	scenario.snapshot = quotaSnapshot(1, next)
	now := started.Add(27 * time.Hour)
	scenario.check(now, `
check: allowed=false recoverable=true next=1787398620123 blocking=[11@1 used=100 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=100 left=0 exhausted window=1787312220123..1787398620123#2]
view: at=1787319420123 allowed=false recoverable=true next=1787398620123 rules=[10@2 used=0 left=100 available window=-..-#0; 11@1 used=100 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=100 left=0 exhausted window=1787312220123..1787398620123#2]`)
	// The stale ticket is ignored for the reset rule.
	scenario.complete(oldTicket, 50, now, `
complete: fault=""
check: allowed=false recoverable=true next=1787398620123 blocking=[11@1 used=150 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=150 left=0 exhausted window=1787312220123..1787398620123#2]
view: at=1787319420123 allowed=false recoverable=true next=1787398620123 rules=[10@2 used=0 left=100 available window=-..-#0; 11@1 used=150 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=150 left=0 exhausted window=1787312220123..1787398620123#2]`)
	if _, err := scenario.quota.Check(t.Context(), oldSnapshot, 1, now); !errors.Is(err, accessquota.ErrStaleRules) {
		t.Fatalf("Check(old snapshot) error = %v, want ErrStaleRules", err)
	}
	ticket = scenario.admit(now, `
ticket: key=1 []
decision: allowed=false recoverable=true next=1787398620123 blocking=[11@1 used=150 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=150 left=0 exhausted window=1787312220123..1787398620123#2]
check: allowed=false recoverable=true next=1787398620123 blocking=[11@1 used=150 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=150 left=0 exhausted window=1787312220123..1787398620123#2]
view: at=1787319420123 allowed=false recoverable=true next=1787398620123 rules=[10@2 used=0 left=100 available window=-..-#0; 11@1 used=150 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=150 left=0 exhausted window=1787312220123..1787398620123#2]`)
	scenario.complete(ticket, 7, now, `
complete: fault=""
check: allowed=false recoverable=true next=1787398620123 blocking=[11@1 used=150 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=150 left=0 exhausted window=1787312220123..1787398620123#2]
view: at=1787319420123 allowed=false recoverable=true next=1787398620123 rules=[10@2 used=0 left=100 available window=-..-#0; 11@1 used=150 left=0 exhausted window=1787312220123..1787330220123#3; 12@1 used=150 left=0 exhausted window=1787312220123..1787398620123#2]`)
}

func TestAccessQuotaSaturatesAtLimit(t *testing.T) {
	for _, test := range []struct {
		name       string
		used       int64
		cost       int64
		wantBefore string
		wantAfter  string
	}{
		{
			name: "fits below limit", used: math.MaxInt64 - 3, cost: 1,
			wantBefore: `
check: allowed=true recoverable=true next=- blocking=[]
view: at=1000000 allowed=true recoverable=true next=- rules=[20@1 used=9223372036854775804 left=2 available window=-..-#0]`,
			wantAfter: `
complete: fault=""
check: allowed=true recoverable=true next=- blocking=[]
view: at=1000000 allowed=true recoverable=true next=- rules=[20@1 used=9223372036854775805 left=1 available window=-..-#0]`,
		},
		{
			name: "reaches limit", used: math.MaxInt64 - 3, cost: 2,
			wantBefore: `
check: allowed=true recoverable=true next=- blocking=[]
view: at=1000000 allowed=true recoverable=true next=- rules=[20@1 used=9223372036854775804 left=2 available window=-..-#0]`,
			wantAfter: `
complete: fault=""
check: allowed=false recoverable=false next=- blocking=[20@1 used=9223372036854775806 left=0 exhausted window=-..-#0]
view: at=1000000 allowed=false recoverable=false next=- rules=[20@1 used=9223372036854775806 left=0 exhausted window=-..-#0]`,
		},
		{
			name: "lands exactly on MaxInt64", used: math.MaxInt64 - 3, cost: 3,
			wantBefore: `
check: allowed=true recoverable=true next=- blocking=[]
view: at=1000000 allowed=true recoverable=true next=- rules=[20@1 used=9223372036854775804 left=2 available window=-..-#0]`,
			wantAfter: `
complete: fault=""
check: allowed=false recoverable=false next=- blocking=[20@1 used=9223372036854775807 left=0 exhausted window=-..-#0]
view: at=1000000 allowed=false recoverable=false next=- rules=[20@1 used=9223372036854775807 left=0 exhausted window=-..-#0]`,
		},
		{
			name: "overflows", used: math.MaxInt64 - 3, cost: 5,
			wantBefore: `
check: allowed=true recoverable=true next=- blocking=[]
view: at=1000000 allowed=true recoverable=true next=- rules=[20@1 used=9223372036854775804 left=2 available window=-..-#0]`,
			wantAfter: `
complete: fault="overflow"
check: allowed=false recoverable=false next=- blocking=[20@1 used=9223372036854775807 left=0 exhausted window=-..-#0]
view: at=1000000 allowed=false recoverable=false next=- rules=[20@1 used=9223372036854775807 left=0 exhausted window=-..-#0]`,
		},
		{
			name: "already saturated", used: math.MaxInt64, cost: 1,
			wantBefore: `
check: allowed=false recoverable=false next=- blocking=[20@1 used=9223372036854775807 left=0 exhausted window=-..-#0]
view: at=1000000 allowed=false recoverable=false next=- rules=[20@1 used=9223372036854775807 left=0 exhausted window=-..-#0]`,
			wantAfter: `
complete: fault="overflow"
check: allowed=false recoverable=false next=- blocking=[20@1 used=9223372036854775807 left=0 exhausted window=-..-#0]
view: at=1000000 allowed=false recoverable=false next=- rules=[20@1 used=9223372036854775807 left=0 exhausted window=-..-#0]`,
		},
		{
			name: "negative estimate", used: 10, cost: -1,
			wantBefore: `
check: allowed=true recoverable=true next=- blocking=[]
view: at=1000000 allowed=true recoverable=true next=- rules=[20@1 used=10 left=9223372036854775796 available window=-..-#0]`,
			wantAfter: `
complete: fault="negative_estimate"
check: allowed=false recoverable=false next=- blocking=[20@1 used=9223372036854775807 left=0 exhausted window=-..-#0]
view: at=1000000 allowed=false recoverable=false next=- rules=[20@1 used=9223372036854775807 left=0 exhausted window=-..-#0]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, client := newTestClient(t)
			rules := []accessquota.Rule{{ID: 20, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: math.MaxInt64 - 1}}
			checkpoint := accessquota.RestoredState{AccessKeyID: 2, RuleID: 20, RuleRevision: 1, UsedNanoUSD: test.used, SnapshotVersion: 4}
			scenario := quotaScenario{
				t: t, quota: NewAccessQuota(client, newFakeQuotaStates(checkpoint)),
				snapshot: quotaSnapshot(2, rules), key: 2,
			}
			now := time.Unix(1_000, 0)
			scenario.check(now, test.wantBefore)
			ticket := accessquota.Ticket{AccessKeyID: 2, Rules: []accessquota.TicketRule{{RuleID: 20, RuleRevision: 1}}}
			scenario.complete(ticket, test.cost, now, test.wantAfter)
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

	const price = 70
	start := time.Unix(10_000, 0)
	admitted := 0
	for request := range 60 {
		now := start.Add(time.Duration(request) * 5 * time.Second)
		instance := instances[request%len(instances)]
		ticket, decision, err := instance.Admit(t.Context(), snapshot, 7, now)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Allowed {
			admitted++
			if _, err := instance.Complete(t.Context(), ticket, price); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A single instance enforcing the same rules admits 15 of these requests.
	if admitted != 15 {
		t.Fatalf("shared admitted %d, want 15", admitted)
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

func TestAccessQuotaStateUsesSlidingTTL(t *testing.T) {
	server, client := newTestClient(t)
	rules := []accessquota.Rule{{ID: 70, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: 1_000}}
	states := newFakeQuotaStates(accessquota.RestoredState{AccessKeyID: 9, RuleID: 70, RuleRevision: 1, UsedNanoUSD: 40, SnapshotVersion: 3})
	quota := NewAccessQuota(client, states)
	snapshot := quotaSnapshot(9, rules)
	key := client.Key(accessKeyHashTag(9), "quota", "70")
	now := time.Unix(30_000, 0)
	assertFullTTL := func(step string) {
		t.Helper()
		if ttl := server.TTL(key); ttl != quotaStateTTL {
			t.Fatalf("%s: TTL = %v, want %v", step, ttl, quotaStateTTL)
		}
	}

	if _, err := quota.Check(t.Context(), snapshot, 9, now); err != nil {
		t.Fatal(err)
	}
	assertFullTTL("hydrate + check")
	server.FastForward(24 * time.Hour)
	ticket, _, err := quota.Admit(t.Context(), snapshot, 9, now)
	if err != nil {
		t.Fatal(err)
	}
	assertFullTTL("admit")
	server.FastForward(24 * time.Hour)
	if _, err := quota.Complete(t.Context(), ticket, 5); err != nil {
		t.Fatal(err)
	}
	assertFullTTL("complete")

	// An idle key expires and is rebuilt from the checkpoint on next use.
	server.FastForward(quotaStateTTL + time.Second)
	if server.Exists(key) {
		t.Fatal("idle quota state did not expire")
	}
	view, err := quota.View(t.Context(), snapshot, 9, now)
	if err != nil || view.Rules[0].UsedNanoUSD != 40 || states.readCount() != 2 {
		t.Fatalf("View() after expiry = %#v, %v, checkpoint reads %d", view, err, states.readCount())
	}
}

// These formatters render quota results as compact text so scenario
// expectations stay readable.
func formatQuotaMS(value *int64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatInt(*value, 10)
}

func formatQuotaRules(rules []accessquota.RuleView) string {
	parts := make([]string, 0, len(rules))
	for _, rule := range rules {
		parts = append(parts, fmt.Sprintf("%d@%d used=%d left=%d %s window=%s..%s#%d",
			rule.ID, rule.Revision, rule.UsedNanoUSD, rule.RemainingNanoUSD, rule.Status,
			formatQuotaMS(rule.WindowStartedAtMS), formatQuotaMS(rule.WindowEndsAtMS), rule.WindowGeneration))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

func formatQuotaDecision(decision accessquota.Decision) string {
	return fmt.Sprintf("allowed=%t recoverable=%t next=%s blocking=%s",
		decision.Allowed, decision.Recoverable, formatQuotaMS(decision.NextAvailableAtMS),
		formatQuotaRules(decision.BlockingRules))
}

func formatQuotaView(view accessquota.View) string {
	return fmt.Sprintf("at=%d allowed=%t recoverable=%t next=%s rules=%s",
		view.ObservedAtMS, view.Allowed, view.Recoverable, formatQuotaMS(view.NextAvailableAtMS),
		formatQuotaRules(view.Rules))
}

func formatQuotaTicket(ticket accessquota.Ticket) string {
	parts := make([]string, 0, len(ticket.Rules))
	for _, rule := range ticket.Rules {
		parts = append(parts, fmt.Sprintf("%d@%d#%d", rule.RuleID, rule.RuleRevision, rule.WindowGeneration))
	}
	return fmt.Sprintf("key=%d [%s]", ticket.AccessKeyID, strings.Join(parts, " "))
}

func formatQuotaStates(states []accessquota.RestoredState) string {
	parts := make([]string, 0, len(states))
	for _, row := range states {
		parts = append(parts, fmt.Sprintf("key=%d rule=%d@%d used=%d window=%s..%s#%d version=%d",
			row.AccessKeyID, row.RuleID, row.RuleRevision, row.UsedNanoUSD,
			formatQuotaMS(row.WindowStartedAtMS), formatQuotaMS(row.WindowEndsAtMS),
			row.WindowGeneration, row.SnapshotVersion))
	}
	return strings.Join(parts, "\n")
}
