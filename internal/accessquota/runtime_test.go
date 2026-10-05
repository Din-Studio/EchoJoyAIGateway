package accessquota

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func ptrInt64(value int64) *int64 { return &value }

func summarizeQuotaRules(rules []RuleView) string {
	parts := make([]string, 0, len(rules))
	for _, rule := range rules {
		parts = append(parts, fmt.Sprintf("%d %s used=%d left=%d gen=%d",
			rule.ID, rule.Status, rule.UsedNanoUSD, rule.RemainingNanoUSD, rule.WindowGeneration))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

func summarizeQuotaNext(next *int64) string {
	if next == nil {
		return "-"
	}
	return fmt.Sprint(*next)
}

func TestDecisionForAndViewForEvaluateRuleStates(t *testing.T) {
	// Rules are deliberately unsorted: the total rule comes first, then periodic rules by period.
	rules := []Rule{
		{ID: 12, Revision: 1, Kind: KindPeriodic, LimitNanoUSD: 30, PeriodSeconds: 24 * 60 * 60},
		{ID: 11, Revision: 1, Kind: KindPeriodic, LimitNanoUSD: 20, PeriodSeconds: 5 * 60 * 60},
		{ID: 10, Revision: 1, Kind: KindTotal, LimitNanoUSD: 100},
	}
	started := time.Date(2026, time.August, 20, 10, 37, 0, 0, time.UTC)
	at := func(offset time.Duration) *int64 { return ptrInt64(started.Add(offset).UnixMilli()) }
	firstWindows := map[uint]RestoredState{
		10: {AccessKeyID: 1, RuleID: 10, RuleRevision: 1, UsedNanoUSD: 25, SnapshotVersion: 2},
		11: {AccessKeyID: 1, RuleID: 11, RuleRevision: 1, UsedNanoUSD: 25, WindowStartedAtMS: at(0),
			WindowEndsAtMS: at(5 * time.Hour), WindowGeneration: 1, SnapshotVersion: 3},
		12: {AccessKeyID: 1, RuleID: 12, RuleRevision: 1, UsedNanoUSD: 25, WindowStartedAtMS: at(0),
			WindowEndsAtMS: at(24 * time.Hour), WindowGeneration: 1, SnapshotVersion: 3},
	}
	exhausted := map[uint]RestoredState{
		10: {AccessKeyID: 1, RuleID: 10, RuleRevision: 1, UsedNanoUSD: 125, SnapshotVersion: 3},
		11: {AccessKeyID: 1, RuleID: 11, RuleRevision: 1, UsedNanoUSD: 100, WindowStartedAtMS: at(25 * time.Hour),
			WindowEndsAtMS: at(30 * time.Hour), WindowGeneration: 2, SnapshotVersion: 5},
		12: {AccessKeyID: 1, RuleID: 12, RuleRevision: 1, UsedNanoUSD: 100, WindowStartedAtMS: at(25 * time.Hour),
			WindowEndsAtMS: at(49 * time.Hour), WindowGeneration: 2, SnapshotVersion: 5},
	}
	for _, test := range []struct {
		name            string
		now             time.Time
		states          map[uint]RestoredState
		wantAllowed     bool
		wantRecoverable bool
		wantNext        string
		wantBlocking    []uint
		wantRules       string
	}{
		{
			name: "no state", now: started, wantAllowed: true, wantRecoverable: true, wantNext: "-",
			wantRules: "[10 available used=0 left=100 gen=0; 11 inactive used=0 left=20 gen=0; 12 inactive used=0 left=30 gen=0]",
		},
		{
			name: "5h window exhausted", now: started.Add(time.Hour), states: firstWindows,
			wantRecoverable: true, wantNext: fmt.Sprint(*at(5 * time.Hour)), wantBlocking: []uint{11},
			wantRules: "[10 available used=25 left=75 gen=0; 11 exhausted used=25 left=0 gen=1; 12 available used=25 left=5 gen=1]",
		},
		{
			name: "5h window expired", now: started.Add(5 * time.Hour), states: firstWindows,
			wantAllowed: true, wantRecoverable: true, wantNext: "-",
			wantRules: "[10 available used=25 left=75 gen=0; 11 inactive used=0 left=20 gen=1; 12 available used=25 left=5 gen=1]",
		},
		{
			name: "all periodic windows expired", now: started.Add(25 * time.Hour), states: firstWindows,
			wantAllowed: true, wantRecoverable: true, wantNext: "-",
			wantRules: "[10 available used=25 left=75 gen=0; 11 inactive used=0 left=20 gen=1; 12 inactive used=0 left=30 gen=1]",
		},
		{
			name: "total exhausted", now: started.Add(26 * time.Hour), states: exhausted,
			wantNext: "-", wantBlocking: []uint{10, 11, 12},
			wantRules: "[10 exhausted used=125 left=0 gen=0; 11 exhausted used=100 left=0 gen=2; 12 exhausted used=100 left=0 gen=2]",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := DecisionFor(rules, test.states, test.now.UnixMilli())
			blocking := make([]uint, 0, len(decision.BlockingRules))
			for _, rule := range decision.BlockingRules {
				blocking = append(blocking, rule.ID)
			}
			if decision.Allowed != test.wantAllowed || decision.Recoverable != test.wantRecoverable ||
				summarizeQuotaNext(decision.NextAvailableAtMS) != test.wantNext ||
				fmt.Sprint(blocking) != fmt.Sprint(append([]uint{}, test.wantBlocking...)) {
				t.Fatalf("DecisionFor() = allowed %t recoverable %t next %s blocking %v",
					decision.Allowed, decision.Recoverable, summarizeQuotaNext(decision.NextAvailableAtMS), blocking)
			}
			view := ViewFor(rules, test.states, test.now)
			if view.ObservedAtMS != test.now.UnixMilli() || view.Allowed != test.wantAllowed ||
				view.Recoverable != test.wantRecoverable || summarizeQuotaNext(view.NextAvailableAtMS) != test.wantNext {
				t.Fatalf("ViewFor() = %#v", view)
			}
			if got := summarizeQuotaRules(view.Rules); got != test.wantRules {
				t.Fatalf("ViewFor() rules = %s, want %s", got, test.wantRules)
			}
		})
	}

	if got := ViewFor(nil, nil, started); !got.Allowed || len(got.Rules) != 0 {
		t.Fatalf("ViewFor(no rules) = %#v, want empty allowed view", got)
	}
}

func TestValidateRestoredStateRejectsImpossibleStates(t *testing.T) {
	periodic := Rule{ID: 11, Revision: 2, Kind: KindPeriodic, LimitNanoUSD: 20, PeriodSeconds: 60}
	start, end, wrongEnd := int64(1000), int64(61000), int64(2000)
	valid := RestoredState{
		AccessKeyID: 1, RuleID: 11, RuleRevision: 2, UsedNanoUSD: 5,
		WindowStartedAtMS: &start, WindowEndsAtMS: &end, WindowGeneration: 1, SnapshotVersion: 3,
	}
	if err := ValidateRestoredState(periodic, valid); err != nil {
		t.Fatalf("ValidateRestoredState(valid) error = %v", err)
	}
	invalid := map[string]RestoredState{
		"revision mismatch": func() RestoredState { s := valid; s.RuleRevision = 1; return s }(),
		"rule mismatch":     func() RestoredState { s := valid; s.RuleID = 12; return s }(),
		"zero version":      func() RestoredState { s := valid; s.SnapshotVersion = 0; return s }(),
		"wrong duration":    func() RestoredState { s := valid; s.WindowEndsAtMS = &wrongEnd; return s }(),
		"no generation":     func() RestoredState { s := valid; s.WindowGeneration = 0; return s }(),
		"inactive with usage": func() RestoredState {
			s := valid
			s.WindowStartedAtMS, s.WindowEndsAtMS, s.WindowGeneration = nil, nil, 0
			return s
		}(),
	}
	for name, state := range invalid {
		if err := ValidateRestoredState(periodic, state); err == nil {
			t.Fatalf("ValidateRestoredState(%s) error = nil", name)
		}
	}
	total := Rule{ID: 10, Revision: 1, Kind: KindTotal, LimitNanoUSD: 100}
	windowed := RestoredState{AccessKeyID: 1, RuleID: 10, RuleRevision: 1, SnapshotVersion: 1, WindowStartedAtMS: &start, WindowEndsAtMS: &end}
	if err := ValidateRestoredState(total, windowed); err == nil {
		t.Fatal("ValidateRestoredState(total with window) error = nil")
	}
}
