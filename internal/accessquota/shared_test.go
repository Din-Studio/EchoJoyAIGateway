package accessquota

import (
	"errors"
	"testing"
	"time"
)

// stubLedger records what the runtime asked for and answers with whatever the
// test wants the shared authority to hold.
type stubLedger struct {
	syncRules  []SharedRule
	syncRoll   bool
	syncStates []SharedState
	syncErr    error

	addRules  []SharedTicketRule
	addCost   int64
	addStates []SharedState
	addErr    error
}

func (ledger *stubLedger) Sync(_ uint, rules []SharedRule, _ int64, roll bool) ([]SharedState, error) {
	ledger.syncRules = rules
	ledger.syncRoll = roll
	return ledger.syncStates, ledger.syncErr
}

func (ledger *stubLedger) Add(_ uint, rules []SharedTicketRule, cost int64) ([]SharedState, error) {
	ledger.addRules = rules
	ledger.addCost = cost
	return ledger.addStates, ledger.addErr
}

func sharedRuntime(t *testing.T, ledger SharedLedger, rule Rule) *Runtime {
	t.Helper()
	runtime := NewRuntime()
	if err := runtime.Reconcile(map[uint][]Rule{7: {rule}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	runtime.SetSharedLedger(ledger)
	return runtime
}

// A budget the shared ledger reports as spent must block here too, even though
// this instance never charged a request against it.
func TestAdmitRefusesOnAPeerInstancesSpending(t *testing.T) {
	ledger := &stubLedger{syncStates: []SharedState{{RuleID: 1, UsedNanoUSD: 1_000}}}
	runtime := sharedRuntime(t, ledger, Rule{ID: 1, Revision: 1, Kind: KindTotal, LimitNanoUSD: 1_000})

	ticket, decision := runtime.Admit(7, time.Now())
	if decision.Allowed {
		t.Fatalf("Admit() allowed with the shared budget exhausted: %#v", decision)
	}
	if decision.Unavailable {
		t.Fatal("Unavailable = true for a decided refusal")
	}
	if len(ticket.Rules) != 0 {
		t.Fatalf("ticket = %#v, want no rules for a refused admission", ticket)
	}
	if len(ledger.syncRules) != 1 || ledger.syncRules[0].RuleID != 1 || !ledger.syncRoll {
		t.Fatalf("Sync() called with %#v, roll = %v; want the rule with the window rolled",
			ledger.syncRules, ledger.syncRoll)
	}
}

// Fail-closed is the whole point of routing admission through the ledger: an
// unknown balance must not be spent.
func TestAdmitFailsClosedWhenTheLedgerIsUnreachable(t *testing.T) {
	ledger := &stubLedger{syncErr: errors.New("redis is gone")}
	runtime := sharedRuntime(t, ledger, Rule{ID: 1, Revision: 1, Kind: KindTotal, LimitNanoUSD: 1_000})

	_, decision := runtime.Admit(7, time.Now())
	if decision.Allowed {
		t.Fatal("Admit() allowed against an unreachable ledger")
	}
	if !decision.Unavailable {
		t.Fatal("Unavailable = false; the caller cannot tell an outage from an exhausted budget")
	}
}

// The charge is the ledger's, not this instance's: a runtime that also added
// the cost locally would double-count it on the next merge.
func TestCompleteChargesTheLedgerAndAdoptsItsCounter(t *testing.T) {
	ledger := &stubLedger{
		syncStates: []SharedState{{RuleID: 1}},
		addStates:  []SharedState{{RuleID: 1, UsedNanoUSD: 900}},
	}
	runtime := sharedRuntime(t, ledger, Rule{ID: 1, Revision: 1, Kind: KindTotal, LimitNanoUSD: 1_000})

	ticket, decision := runtime.Admit(7, time.Now())
	if !decision.Allowed {
		t.Fatalf("Admit() = %#v, want allowed", decision)
	}
	if completion := runtime.Complete(ticket, 300); completion.Fault != "" {
		t.Fatalf("Complete() fault = %q, want none", completion.Fault)
	}
	if ledger.addCost != 300 || len(ledger.addRules) != 1 || ledger.addRules[0].RuleID != 1 {
		t.Fatalf("Add() called with cost %d and %#v", ledger.addCost, ledger.addRules)
	}
	// 900 is the ledger's answer, not 300: peers spent the rest.
	if view := runtime.Snapshot(7, time.Now()); view.Rules[0].UsedNanoUSD != 900 {
		t.Fatalf("used = %d, want the ledger's 900", view.Rules[0].UsedNanoUSD)
	}
}

// A counter that saturated in the ledger has to surface as the same overflow
// fault the in-memory runtime reports, or the operator loses the signal.
func TestCompleteReportsTheLedgersSaturation(t *testing.T) {
	ledger := &stubLedger{
		syncStates: []SharedState{{RuleID: 1}},
		addStates:  []SharedState{{RuleID: 1, UsedNanoUSD: 9_223_372_036_854_775_807, Saturated: true}},
	}
	runtime := sharedRuntime(t, ledger, Rule{ID: 1, Revision: 1, Kind: KindTotal, LimitNanoUSD: 1_000})
	ticket, _ := runtime.Admit(7, time.Now())

	if completion := runtime.Complete(ticket, 5); completion.Fault != CompletionFaultOverflow {
		t.Fatalf("Complete() fault = %q, want %q", completion.Fault, CompletionFaultOverflow)
	}
	if runtime.Stats().OverflowFaultTotal != 1 {
		t.Fatalf("OverflowFaultTotal = %d, want 1", runtime.Stats().OverflowFaultTotal)
	}
}

// A negative estimate is a local defect, not a ledger one, and is clamped
// before it reaches the ledger so the shared counter never moves backwards.
func TestCompleteClampsANegativeEstimateBeforeCharging(t *testing.T) {
	ledger := &stubLedger{
		syncStates: []SharedState{{RuleID: 1}},
		addStates:  []SharedState{{RuleID: 1, UsedNanoUSD: 9_223_372_036_854_775_807}},
	}
	runtime := sharedRuntime(t, ledger, Rule{ID: 1, Revision: 1, Kind: KindTotal, LimitNanoUSD: 1_000})
	ticket, _ := runtime.Admit(7, time.Now())

	completion := runtime.Complete(ticket, -1)
	if completion.Fault != CompletionFaultNegativeEstimate {
		t.Fatalf("Complete() fault = %q, want %q", completion.Fault, CompletionFaultNegativeEstimate)
	}
	if ledger.addCost <= 0 {
		t.Fatalf("Add() charged %d; a negative charge would refund the shared budget", ledger.addCost)
	}
}

// Answers produced under the ledger's lock can reach this instance in either
// order. The older one must not undo the newer one.
func TestMergeSharedStateNeverWalksACounterBackwards(t *testing.T) {
	rule := &runtimeRule{
		definition:  Rule{ID: 1, Revision: 1, Kind: KindTotal, LimitNanoUSD: 1_000},
		usedNanoUSD: 500,
	}
	if mergeSharedState(rule, SharedState{RuleID: 1, UsedNanoUSD: 400}) {
		t.Fatal("merged a stale counter")
	}
	if rule.usedNanoUSD != 500 {
		t.Fatalf("used = %d after a stale answer, want 500", rule.usedNanoUSD)
	}
	if !mergeSharedState(rule, SharedState{RuleID: 1, UsedNanoUSD: 600}) {
		t.Fatal("refused a newer counter")
	}
	if rule.usedNanoUSD != 600 {
		t.Fatalf("used = %d, want 600", rule.usedNanoUSD)
	}
}

// A rolled window resets the counter, so "smaller" is exactly what a newer
// answer looks like. Generation is what tells the two apart.
func TestMergeSharedStateAdoptsARolledWindow(t *testing.T) {
	started, ends := int64(2_000), int64(62_000)
	rule := &runtimeRule{
		definition:       Rule{ID: 1, Revision: 1, Kind: KindPeriodic, LimitNanoUSD: 1_000, PeriodSeconds: 60},
		usedNanoUSD:      900,
		windowGeneration: 3,
	}
	if !mergeSharedState(rule, SharedState{
		RuleID: 1, UsedNanoUSD: 0, WindowGeneration: 4,
		WindowStartedAtMS: &started, WindowEndsAtMS: &ends,
	}) {
		t.Fatal("refused a newer window generation")
	}
	if rule.usedNanoUSD != 0 || rule.windowGeneration != 4 ||
		rule.windowStartedAtMS == nil || *rule.windowEndsAtMS != ends {
		t.Fatalf("rule = %#v, want the rolled window adopted", rule)
	}

	// The generation that was just replaced must not come back.
	if mergeSharedState(rule, SharedState{RuleID: 1, UsedNanoUSD: 900, WindowGeneration: 3}) {
		t.Fatal("merged an answer from the previous window")
	}
}

// Check deliberately does not consult the ledger. That is only sound because a
// local counter is never ahead of the shared one, so the cheap pre-filter can
// delay a refusal but can never invent one.
func TestCheckStaysLocalUnderASharedLedger(t *testing.T) {
	ledger := &stubLedger{syncErr: errors.New("redis is gone")}
	runtime := sharedRuntime(t, ledger, Rule{ID: 1, Revision: 1, Kind: KindTotal, LimitNanoUSD: 1_000})

	if decision := runtime.Check(7, time.Now()); !decision.Allowed {
		t.Fatalf("Check() = %#v, want allowed without reaching the ledger", decision)
	}
}
