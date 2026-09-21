package accessquota

import (
	"math"
	"time"
)

// SharedLedger is the cross-instance authority for what an access key has
// already spent. Without one, N instances each admit against their own counter
// and a limit configured once is enforced N times over.
//
// The ledger owns the counters and the periodic windows, and nothing else: the
// rule definitions, the decision, the ticket and the faults all stay here, so
// there is exactly one description of what a limit means.
//
// Both methods answer with the authoritative state of every rule they were
// given, which the runtime merges into its own entry. A returned error means
// the state is unknown, never that it is unchanged.
type SharedLedger interface {
	// Sync reports each rule's authoritative counter, starting a new periodic
	// window when roll is set and the current one has ended. Rules the ledger
	// has never seen are seeded from the baseline carried in SharedRule, which
	// is how a database checkpoint becomes the shared starting point.
	Sync(accessKeyID uint, rules []SharedRule, nowMS int64, roll bool) ([]SharedState, error)
	// Add charges costNanoUSD to every rule whose revision and window
	// generation still match the ticket, and reports their new counters.
	Add(accessKeyID uint, rules []SharedTicketRule, costNanoUSD int64) ([]SharedState, error)
}

// SharedRule is one rule as the ledger needs to see it: its identity, the
// shape of its window, and the local values to seed from when the ledger has
// no state of its own.
type SharedRule struct {
	RuleID            uint
	Revision          uint64
	Periodic          bool
	PeriodSeconds     int64
	UsedNanoUSD       int64
	WindowStartedAtMS *int64
	WindowEndsAtMS    *int64
	WindowGeneration  uint64
}

// SharedTicketRule identifies the rule state a completed request was admitted
// against. A charge lands only while both parts still match, which is what
// keeps a request from being billed to a window it never ran in.
type SharedTicketRule struct {
	RuleID           uint
	Revision         uint64
	WindowGeneration uint64
}

// SharedState is a rule's authoritative counter and window. A zero RuleID
// means the ledger declined to answer for that rule.
type SharedState struct {
	RuleID            uint
	UsedNanoUSD       int64
	WindowStartedAtMS *int64
	WindowEndsAtMS    *int64
	WindowGeneration  uint64
	// Saturated marks a charge the counter could not represent, so the runtime
	// reports the same overflow fault it would have reported in memory.
	Saturated bool
}

// SetSharedLedger makes a shared ledger the authority for this runtime. It is
// installed once at startup; a runtime that has served a request must not
// change authorities underneath it.
func (runtime *Runtime) SetSharedLedger(ledger SharedLedger) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	runtime.shared = ledger
	runtime.mu.Unlock()
}

func (runtime *Runtime) sharedLedger() SharedLedger {
	if runtime == nil {
		return nil
	}
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.shared
}

// admitShared is Admit with the counters read from, and the window rolled by,
// the shared ledger.
//
// The ledger call happens outside the entry lock. Holding it across a network
// round trip would serialize an access key's whole traffic on one RTT, and it
// buys nothing: the ledger call is atomic on its own, so two concurrent
// admissions receive answers that are already consistent with each other. What
// the lock must protect is the merge, and mergeSharedState only ever moves a
// counter forward, so the two answers commute no matter which lands first.
func (runtime *Runtime) admitShared(
	ledger SharedLedger,
	accessKeyID uint,
	now time.Time,
) (Ticket, Decision) {
	rules, exists := runtime.sharedRules(accessKeyID)
	if !exists {
		return Ticket{AccessKeyID: accessKeyID}, allowedDecision()
	}
	states, err := ledger.Sync(accessKeyID, rules, now.UnixMilli(), true)
	if err != nil {
		return Ticket{AccessKeyID: accessKeyID}, unavailableDecision()
	}

	entry := runtime.lockEntry(accessKeyID)
	if entry == nil {
		return Ticket{AccessKeyID: accessKeyID}, allowedDecision()
	}
	dirty := runtime.mergeSharedStates(entry, states)
	nowMS := now.UnixMilli()
	decision := decisionLocked(entry, nowMS)
	ticket := Ticket{AccessKeyID: accessKeyID}
	if decision.Allowed {
		ticket.Rules = ticketRulesLocked(entry)
	}
	entry.mu.Unlock()
	if dirty {
		runtime.notifyDirty()
	}
	return ticket, decision
}

// completeShared charges the shared counters and adopts the result. A failure
// is deliberately not retried and not reported: the request already ran, the
// charge is best effort, and the next admission reads whatever the ledger
// holds. The counter it failed to move is the only thing lost.
func (runtime *Runtime) completeShared(
	ledger SharedLedger,
	ticket Ticket,
	costNanoUSD int64,
) CompletionResult {
	completion := CompletionResult{}
	if costNanoUSD == 0 {
		return completion
	}
	if costNanoUSD < 0 {
		completion.Fault = CompletionFaultNegativeEstimate
		costNanoUSD = math.MaxInt64
	}
	rules := make([]SharedTicketRule, 0, len(ticket.Rules))
	for _, rule := range ticket.Rules {
		rules = append(rules, SharedTicketRule{
			RuleID: rule.RuleID, Revision: rule.RuleRevision,
			WindowGeneration: rule.WindowGeneration,
		})
	}
	states, err := ledger.Add(ticket.AccessKeyID, rules, costNanoUSD)
	if err != nil {
		return completion
	}
	for _, shared := range states {
		if shared.Saturated && completion.Fault == "" {
			completion.Fault = CompletionFaultOverflow
		}
	}

	entry := runtime.lockEntry(ticket.AccessKeyID)
	if entry == nil {
		return completion
	}
	dirty := runtime.mergeSharedStates(entry, states)
	entry.mu.Unlock()
	if completion.Fault != "" {
		runtime.overflowFaultTotal.Add(1)
	}
	if dirty {
		runtime.notifyDirty()
	}
	return completion
}

// syncSharedForDisplay refreshes this instance's counters from the ledger
// without rolling any window. Rolling is an admission's decision, and a
// management-plane read must not be able to start a budget period.
func (runtime *Runtime) syncSharedForDisplay(accessKeyID uint, now time.Time) {
	ledger := runtime.sharedLedger()
	if ledger == nil {
		return
	}
	rules, exists := runtime.sharedRules(accessKeyID)
	if !exists {
		return
	}
	states, err := ledger.Sync(accessKeyID, rules, now.UnixMilli(), false)
	if err != nil {
		return
	}
	entry := runtime.lockEntry(accessKeyID)
	if entry == nil {
		return
	}
	dirty := runtime.mergeSharedStates(entry, states)
	entry.mu.Unlock()
	if dirty {
		runtime.notifyDirty()
	}
}

// sharedRules snapshots an access key's rule definitions together with the
// local values the ledger seeds from when it has no state of its own.
func (runtime *Runtime) sharedRules(accessKeyID uint) ([]SharedRule, bool) {
	entry := runtime.lockEntry(accessKeyID)
	if entry == nil {
		return nil, false
	}
	defer entry.mu.Unlock()
	rules := make([]SharedRule, 0, len(entry.rules))
	for _, rule := range entry.rules {
		rules = append(rules, SharedRule{
			RuleID:            rule.definition.ID,
			Revision:          rule.definition.Revision,
			Periodic:          rule.definition.Kind == KindPeriodic,
			PeriodSeconds:     rule.definition.PeriodSeconds,
			UsedNanoUSD:       rule.usedNanoUSD,
			WindowStartedAtMS: cloneInt64(rule.windowStartedAtMS),
			WindowEndsAtMS:    cloneInt64(rule.windowEndsAtMS),
			WindowGeneration:  rule.windowGeneration,
		})
	}
	return rules, true
}

func (runtime *Runtime) mergeSharedStates(entry *accessKeyEntry, states []SharedState) bool {
	dirty := false
	for _, shared := range states {
		if shared.RuleID == 0 {
			continue
		}
		if rule := entry.byID[shared.RuleID]; rule != nil && mergeSharedState(rule, shared) {
			dirty = true
		}
	}
	return dirty
}

// mergeSharedState adopts an authoritative answer only when it is ahead of
// what this instance already knows. Two answers can arrive out of order — they
// were produced under the ledger's lock, not this one — and adopting the older
// one would walk a counter backwards and re-admit spending that already
// happened.
func mergeSharedState(rule *runtimeRule, shared SharedState) bool {
	switch {
	case shared.WindowGeneration > rule.windowGeneration:
	case shared.WindowGeneration < rule.windowGeneration:
		return false
	case shared.UsedNanoUSD > rule.usedNanoUSD:
	default:
		return false
	}
	rule.usedNanoUSD = shared.UsedNanoUSD
	rule.windowStartedAtMS = cloneInt64(shared.WindowStartedAtMS)
	rule.windowEndsAtMS = cloneInt64(shared.WindowEndsAtMS)
	rule.windowGeneration = shared.WindowGeneration
	advanceVersion(rule)
	return true
}

func ticketRulesLocked(entry *accessKeyEntry) []TicketRule {
	rules := make([]TicketRule, 0, len(entry.rules))
	for _, rule := range entry.rules {
		rules = append(rules, TicketRule{
			RuleID: rule.definition.ID, RuleRevision: rule.definition.Revision,
			WindowGeneration: rule.windowGeneration,
		})
	}
	return rules
}

func unavailableDecision() Decision {
	return Decision{Allowed: false, Recoverable: true, Unavailable: true, BlockingRules: []RuleView{}}
}
