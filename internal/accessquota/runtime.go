// Package accessquota defines AccessKey estimated-cost limit rules and the
// decisions and views derived from their shared state.
package accessquota

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	MinPeriodSeconds int64 = 60
	MaxPeriodSeconds int64 = 365 * 24 * 60 * 60
	MaxPeriodicRules       = 10
)

// ErrStaleRules reports that the caller's rule definitions are older than the
// state owned by another writer, so the request must retry on a newer snapshot.
var ErrStaleRules = errors.New("access quota rules are older than the shared state")

type Kind string

const (
	KindTotal    Kind = "total"
	KindPeriodic Kind = "periodic"
)

type Rule struct {
	ID            uint
	Revision      uint64
	Kind          Kind
	LimitNanoUSD  int64
	PeriodSeconds int64
}

type RestoredState struct {
	AccessKeyID       uint
	RuleID            uint
	RuleRevision      uint64
	UsedNanoUSD       int64
	WindowStartedAtMS *int64
	WindowEndsAtMS    *int64
	WindowGeneration  uint64
	SnapshotVersion   uint64
}

type TicketRule struct {
	RuleID           uint
	RuleRevision     uint64
	WindowGeneration uint64
}

type Ticket struct {
	AccessKeyID uint
	Rules       []TicketRule
}

type RuleStatus string

const (
	RuleStatusAvailable RuleStatus = "available"
	RuleStatusInactive  RuleStatus = "inactive"
	RuleStatusExhausted RuleStatus = "exhausted"
)

type RuleView struct {
	Rule
	UsedNanoUSD       int64
	RemainingNanoUSD  int64
	Status            RuleStatus
	WindowStartedAtMS *int64
	WindowEndsAtMS    *int64
	WindowGeneration  uint64
}

type Decision struct {
	Allowed           bool
	Recoverable       bool
	NextAvailableAtMS *int64
	BlockingRules     []RuleView
}

type View struct {
	ObservedAtMS      int64
	Allowed           bool
	Recoverable       bool
	NextAvailableAtMS *int64
	Rules             []RuleView
}

type CompletionFault string

const (
	CompletionFaultNegativeEstimate CompletionFault = "negative_estimate"
	CompletionFaultOverflow         CompletionFault = "overflow"
)

type CompletionResult struct {
	Fault CompletionFault
}

type accessKeyEntry struct {
	rules []*runtimeRule
}

type runtimeRule struct {
	definition        Rule
	usedNanoUSD       int64
	windowStartedAtMS *int64
	windowEndsAtMS    *int64
	windowGeneration  uint64
}

// ValidateDefinitions verifies all per-key rule identities and limits.
func ValidateDefinitions(definitions map[uint][]Rule) error {
	_, err := normalizeDefinitions(definitions)
	return err
}

// DecisionFor evaluates externally owned rule state. A rule without a state
// entry is treated as never used.
func DecisionFor(rules []Rule, states map[uint]RestoredState, nowMS int64) Decision {
	return decisionOf(entryFromStates(rules, states), nowMS)
}

// ViewFor projects externally owned rule state. A rule without a state entry
// is treated as never used.
func ViewFor(rules []Rule, states map[uint]RestoredState, now time.Time) View {
	return viewOf(entryFromStates(rules, states), now.UnixMilli())
}

// ValidateRestoredState verifies that persisted state is a legal state of rule.
func ValidateRestoredState(rule Rule, state RestoredState) error {
	if err := validateRestoredState(state); err != nil {
		return err
	}
	if state.RuleID != rule.ID || state.RuleRevision != rule.Revision {
		return fmt.Errorf("restore access quota rule %d: state identity or revision mismatch", rule.ID)
	}
	return applyRestoredState(&runtimeRule{definition: rule}, state)
}

func normalizeDefinitions(definitions map[uint][]Rule) (map[uint][]Rule, error) {
	normalized := make(map[uint][]Rule, len(definitions))
	globalRuleIDs := make(map[uint]uint)
	for accessKeyID, source := range definitions {
		if accessKeyID == 0 {
			return nil, fmt.Errorf("reconcile access quota runtime: access key ID is required")
		}
		if len(source) == 0 {
			continue
		}
		rules := append([]Rule(nil), source...)
		if err := validateRules(accessKeyID, rules, globalRuleIDs); err != nil {
			return nil, err
		}
		sortRules(rules)
		normalized[accessKeyID] = rules
	}
	return normalized, nil
}

func validateRules(accessKeyID uint, rules []Rule, globalRuleIDs map[uint]uint) error {
	totalCount := 0
	periodicCount := 0
	periods := make(map[int64]struct{})
	for _, rule := range rules {
		if rule.ID == 0 || rule.Revision == 0 || rule.LimitNanoUSD <= 0 {
			return fmt.Errorf("reconcile access quota rule for key %d: invalid identity, revision, or limit", accessKeyID)
		}
		if owner, exists := globalRuleIDs[rule.ID]; exists {
			return fmt.Errorf("reconcile access quota rule %d: duplicate across keys %d and %d", rule.ID, owner, accessKeyID)
		}
		globalRuleIDs[rule.ID] = accessKeyID
		switch rule.Kind {
		case KindTotal:
			totalCount++
			if rule.PeriodSeconds != 0 {
				return fmt.Errorf("reconcile total access quota rule %d: period must be zero", rule.ID)
			}
		case KindPeriodic:
			periodicCount++
			if rule.PeriodSeconds < MinPeriodSeconds || rule.PeriodSeconds > MaxPeriodSeconds {
				return fmt.Errorf("reconcile periodic access quota rule %d: period is out of range", rule.ID)
			}
			if _, exists := periods[rule.PeriodSeconds]; exists {
				return fmt.Errorf("reconcile periodic access quota rule %d: duplicate period", rule.ID)
			}
			periods[rule.PeriodSeconds] = struct{}{}
		default:
			return fmt.Errorf("reconcile access quota rule %d: invalid kind %q", rule.ID, rule.Kind)
		}
	}
	if totalCount > 1 || periodicCount > MaxPeriodicRules {
		return fmt.Errorf("reconcile access quota rules for key %d: rule count exceeds limit", accessKeyID)
	}
	return nil
}

func sortRules(rules []Rule) {
	sort.Slice(rules, func(i, j int) bool {
		left, right := rules[i], rules[j]
		if left.Kind != right.Kind {
			return left.Kind == KindTotal
		}
		if left.PeriodSeconds != right.PeriodSeconds {
			return left.PeriodSeconds < right.PeriodSeconds
		}
		return left.ID < right.ID
	})
}

func newAccessKeyEntry(rules []Rule) *accessKeyEntry {
	entry := &accessKeyEntry{rules: make([]*runtimeRule, 0, len(rules))}
	for _, definition := range rules {
		entry.rules = append(entry.rules, &runtimeRule{definition: definition})
	}
	return entry
}

func validateRestoredState(state RestoredState) error {
	if state.AccessKeyID == 0 || state.RuleID == 0 || state.RuleRevision == 0 ||
		state.UsedNanoUSD < 0 || state.SnapshotVersion == 0 {
		return fmt.Errorf("restore access quota rule %d: invalid persisted state", state.RuleID)
	}
	if (state.WindowStartedAtMS == nil) != (state.WindowEndsAtMS == nil) {
		return fmt.Errorf("restore access quota rule %d: incomplete window", state.RuleID)
	}
	if state.WindowStartedAtMS != nil &&
		(*state.WindowStartedAtMS < 0 || *state.WindowEndsAtMS <= *state.WindowStartedAtMS) {
		return fmt.Errorf("restore access quota rule %d: invalid window", state.RuleID)
	}
	return nil
}

func applyRestoredState(rule *runtimeRule, state RestoredState) error {
	if rule.definition.Kind == KindTotal {
		if state.WindowStartedAtMS != nil || state.WindowEndsAtMS != nil || state.WindowGeneration != 0 {
			return fmt.Errorf("restore total access quota rule %d: window state is not allowed", rule.definition.ID)
		}
	} else if state.WindowStartedAtMS == nil {
		if state.UsedNanoUSD != 0 || state.WindowGeneration != 0 {
			return fmt.Errorf("restore periodic access quota rule %d: inactive state has usage or generation", rule.definition.ID)
		}
	} else {
		if state.WindowGeneration == 0 {
			return fmt.Errorf("restore periodic access quota rule %d: active state has no generation", rule.definition.ID)
		}
		windowDurationMS := *state.WindowEndsAtMS - *state.WindowStartedAtMS
		if windowDurationMS != rule.definition.PeriodSeconds*int64(time.Second/time.Millisecond) {
			return fmt.Errorf("restore periodic access quota rule %d: window duration does not match period", rule.definition.ID)
		}
	}
	rule.usedNanoUSD = state.UsedNanoUSD
	rule.windowStartedAtMS = cloneInt64(state.WindowStartedAtMS)
	rule.windowEndsAtMS = cloneInt64(state.WindowEndsAtMS)
	rule.windowGeneration = state.WindowGeneration
	return nil
}

func entryFromStates(rules []Rule, states map[uint]RestoredState) *accessKeyEntry {
	sorted := append([]Rule(nil), rules...)
	sortRules(sorted)
	entry := newAccessKeyEntry(sorted)
	for _, rule := range entry.rules {
		state, exists := states[rule.definition.ID]
		if !exists {
			continue
		}
		rule.usedNanoUSD = state.UsedNanoUSD
		rule.windowStartedAtMS = cloneInt64(state.WindowStartedAtMS)
		rule.windowEndsAtMS = cloneInt64(state.WindowEndsAtMS)
		rule.windowGeneration = state.WindowGeneration
	}
	return entry
}

func viewOf(entry *accessKeyEntry, nowMS int64) View {
	view := View{ObservedAtMS: nowMS, Allowed: true, Recoverable: true, Rules: []RuleView{}}
	if entry == nil || len(entry.rules) == 0 {
		return view
	}
	view.Rules = make([]RuleView, 0, len(entry.rules))
	for _, rule := range entry.rules {
		view.Rules = append(view.Rules, ruleViewOf(rule, nowMS))
	}
	decision := decisionOf(entry, nowMS)
	view.Allowed = decision.Allowed
	view.Recoverable = decision.Recoverable
	view.NextAvailableAtMS = cloneInt64(decision.NextAvailableAtMS)
	return view
}

func decisionOf(entry *accessKeyEntry, nowMS int64) Decision {
	decision := allowedDecision()
	var next int64
	for _, rule := range entry.rules {
		view := ruleViewOf(rule, nowMS)
		if view.Status != RuleStatusExhausted {
			continue
		}
		decision.Allowed = false
		decision.BlockingRules = append(decision.BlockingRules, view)
		if rule.definition.Kind == KindTotal {
			decision.Recoverable = false
			decision.NextAvailableAtMS = nil
			continue
		}
		if decision.Recoverable && view.WindowEndsAtMS != nil && *view.WindowEndsAtMS > next {
			next = *view.WindowEndsAtMS
		}
	}
	if !decision.Allowed && decision.Recoverable && next > 0 {
		decision.NextAvailableAtMS = &next
	}
	return decision
}

func allowedDecision() Decision {
	return Decision{Allowed: true, Recoverable: true, BlockingRules: []RuleView{}}
}

func ruleViewOf(rule *runtimeRule, nowMS int64) RuleView {
	view := RuleView{
		Rule: rule.definition, RemainingNanoUSD: rule.definition.LimitNanoUSD,
		Status: RuleStatusAvailable, WindowGeneration: rule.windowGeneration,
	}
	if rule.definition.Kind == KindPeriodic && periodicInactive(rule, nowMS) {
		view.Status = RuleStatusInactive
		return view
	}
	view.UsedNanoUSD = rule.usedNanoUSD
	view.RemainingNanoUSD = remaining(rule.definition.LimitNanoUSD, rule.usedNanoUSD)
	view.WindowStartedAtMS = cloneInt64(rule.windowStartedAtMS)
	view.WindowEndsAtMS = cloneInt64(rule.windowEndsAtMS)
	if rule.usedNanoUSD >= rule.definition.LimitNanoUSD {
		view.Status = RuleStatusExhausted
	}
	return view
}

func periodicInactive(rule *runtimeRule, nowMS int64) bool {
	return rule.windowStartedAtMS == nil || rule.windowEndsAtMS == nil || nowMS >= *rule.windowEndsAtMS
}

func remaining(limit, used int64) int64 {
	if used >= limit {
		return 0
	}
	return limit - used
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
