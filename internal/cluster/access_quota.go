package cluster

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/state"
)

// completeTimeout bounds cost settlement, which runs after the response and
// so is not on the client-visible latency path.
const completeTimeout = 2 * time.Second

// AccessQuotaStateReader reads persisted rule checkpoints. A rule without a
// checkpoint row is omitted from the result.
type AccessQuotaStateReader interface {
	ReadAccessQuotaStates(ctx context.Context, ruleIDs []uint) ([]accessquota.RestoredState, error)
}

// AccessQuota keeps AccessKey cost-limit state in Redis so every instance
// admits against one shared count. Rule definitions come from the caller's
// immutable snapshot; Redis holds only usage and windows, hydrated lazily
// from the database checkpoint when a rule is missing. No method performs a
// Redis call while holding a process lock.
type AccessQuota struct {
	client *Client
	states AccessQuotaStateReader

	mu       sync.Mutex
	dirty    map[uint]dirtyRule
	notifier func()

	overflowFaultTotal atomic.Uint64
}

// dirtyRule is a rule this instance changed in Redis and has not yet
// checkpointed at or beyond (revision, version).
type dirtyRule struct {
	accessKeyID uint
	revision    uint64
	version     uint64
}

// coveredBy reports whether a checkpoint at (revision, version) includes rule.
func (rule dirtyRule) coveredBy(revision, version uint64) bool {
	return revision > rule.revision || (revision == rule.revision && version >= rule.version)
}

// NewAccessQuota returns nil when cluster mode is disabled.
func NewAccessQuota(client *Client, states AccessQuotaStateReader) *AccessQuota {
	if client == nil {
		return nil
	}
	return &AccessQuota{client: client, states: states, dirty: make(map[uint]dirtyRule)}
}

// Check reports whether the AccessKey may start a request under snapshot.
func (quota *AccessQuota) Check(
	ctx context.Context,
	snapshot *state.ConfigSnapshot,
	accessKeyID uint,
	now time.Time,
) (accessquota.Decision, error) {
	rules := costLimitRules(snapshot, accessKeyID)
	if len(rules) == 0 {
		return allowedDecision(), nil
	}
	evaluation, err := quota.evaluate(ctx, "check", accessKeyID, rules, now)
	if err != nil {
		return accessquota.Decision{}, err
	}
	return accessquota.DecisionFor(rules, evaluation.states, now.UnixMilli()), nil
}

// Admit atomically re-checks the limits and opens expired periodic windows.
func (quota *AccessQuota) Admit(
	ctx context.Context,
	snapshot *state.ConfigSnapshot,
	accessKeyID uint,
	now time.Time,
) (accessquota.Ticket, accessquota.Decision, error) {
	ticket := accessquota.Ticket{AccessKeyID: accessKeyID}
	rules := costLimitRules(snapshot, accessKeyID)
	if len(rules) == 0 {
		return ticket, allowedDecision(), nil
	}
	evaluation, err := quota.evaluate(ctx, "admit", accessKeyID, rules, now)
	if err != nil {
		return ticket, accessquota.Decision{}, err
	}
	if !evaluation.allowed {
		return ticket, accessquota.DecisionFor(rules, evaluation.states, now.UnixMilli()), nil
	}
	changed := false
	ticket.Rules = make([]accessquota.TicketRule, 0, len(rules))
	for index, rule := range rules {
		current := evaluation.states[rule.ID]
		ticket.Rules = append(ticket.Rules, accessquota.TicketRule{
			RuleID: rule.ID, RuleRevision: rule.Revision, WindowGeneration: current.WindowGeneration,
		})
		if evaluation.opened[index] {
			quota.markDirty(accessKeyID, rule.ID, rule.Revision, current.SnapshotVersion)
			changed = true
		}
	}
	if changed {
		quota.notifyDirty()
	}
	return ticket, allowedDecision(), nil
}

// Complete adds the settled cost to every ticket rule that is still current.
func (quota *AccessQuota) Complete(
	ctx context.Context,
	ticket accessquota.Ticket,
	costNanoUSD int64,
) (accessquota.CompletionResult, error) {
	if ticket.AccessKeyID == 0 || len(ticket.Rules) == 0 || costNanoUSD == 0 {
		return accessquota.CompletionResult{}, nil
	}
	keys := make([]string, 0, len(ticket.Rules))
	headroom := int64(math.MaxInt64)
	if costNanoUSD > 0 {
		headroom -= costNanoUSD
	}
	args := make([]any, 0, 2+len(ticket.Rules)*2)
	args = append(args, strconv.FormatInt(costNanoUSD, 10), strconv.FormatInt(headroom, 10))
	for _, rule := range ticket.Rules {
		keys = append(keys, quota.ruleKey(ticket.AccessKeyID, rule.RuleID))
		args = append(args, strconv.FormatUint(rule.RuleRevision, 10), strconv.FormatUint(rule.WindowGeneration, 10))
	}
	callCtx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()
	reply, err := runStrings(callCtx, quota.client, completeScript, keys, args)
	if err != nil {
		return accessquota.CompletionResult{}, quota.unavailable(ticket.AccessKeyID, err)
	}
	if len(reply) == 0 || (len(reply)-1)%3 != 0 {
		return accessquota.CompletionResult{}, fmt.Errorf("complete access quota: malformed reply %q", reply)
	}
	completion := accessquota.CompletionResult{Fault: accessquota.CompletionFault(reply[0])}
	for offset := 1; offset < len(reply); offset += 3 {
		index, indexErr := strconv.Atoi(reply[offset])
		revision, revisionErr := strconv.ParseUint(reply[offset+1], 10, 64)
		version, versionErr := strconv.ParseUint(reply[offset+2], 10, 64)
		if err := errors.Join(indexErr, revisionErr, versionErr); err != nil || index < 1 || index > len(ticket.Rules) {
			return completion, fmt.Errorf("complete access quota: malformed reply %q", reply)
		}
		quota.markDirty(ticket.AccessKeyID, ticket.Rules[index-1].RuleID, revision, version)
	}
	if completion.Fault != "" {
		quota.overflowFaultTotal.Add(1)
	}
	if len(reply) > 1 {
		quota.notifyDirty()
	}
	return completion, nil
}

// View projects the shared state of every rule for control-plane display.
func (quota *AccessQuota) View(
	ctx context.Context,
	snapshot *state.ConfigSnapshot,
	accessKeyID uint,
	now time.Time,
) (accessquota.View, error) {
	rules := costLimitRules(snapshot, accessKeyID)
	if len(rules) == 0 {
		return accessquota.ViewFor(nil, nil, now), nil
	}
	evaluation, err := quota.evaluate(ctx, "check", accessKeyID, rules, now)
	if err != nil {
		return accessquota.View{}, err
	}
	return accessquota.ViewFor(rules, evaluation.states, now), nil
}

// Stats reports saturated-accounting faults observed by this instance.
func (quota *AccessQuota) Stats() accessquota.Stats {
	if quota == nil {
		return accessquota.Stats{}
	}
	return accessquota.Stats{OverflowFaultTotal: quota.overflowFaultTotal.Load()}
}

// SetDirtyNotifier installs the non-blocking checkpoint wake-up.
func (quota *AccessQuota) SetDirtyNotifier(notifier func()) {
	quota.mu.Lock()
	quota.notifier = notifier
	quota.mu.Unlock()
}

// HasDirty reports whether this instance changed rules it has not yet
// checkpointed. It reads only local state.
func (quota *AccessQuota) HasDirty() bool {
	quota.mu.Lock()
	defer quota.mu.Unlock()
	return len(quota.dirty) > 0
}

// DirtySnapshots reads the current shared state of up to limit rules this
// instance changed. Rules whose Redis key vanished or regressed below the
// recorded change are dropped: the database already holds everything Redis
// still knows, so there is nothing newer to persist.
func (quota *AccessQuota) DirtySnapshots(ctx context.Context, limit int) ([]accessquota.RestoredState, error) {
	type candidate struct {
		ruleID uint
		dirtyRule
	}
	quota.mu.Lock()
	candidates := make([]candidate, 0, len(quota.dirty))
	for ruleID, rule := range quota.dirty {
		candidates = append(candidates, candidate{ruleID: ruleID, dirtyRule: rule})
	}
	quota.mu.Unlock()
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ruleID < candidates[j].ruleID })
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}

	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	pipeline := quota.client.Pipeline()
	commands := make([]*redis.SliceCmd, 0, len(candidates))
	for _, item := range candidates {
		commands = append(commands, pipeline.HMGet(callCtx, quota.ruleKey(item.accessKeyID, item.ruleID),
			"rev", "used", "ws", "we", "gen", "ver"))
	}
	if _, err := pipeline.Exec(callCtx); err != nil {
		return nil, fmt.Errorf("read dirty access quota state: %w", err)
	}

	snapshots := make([]accessquota.RestoredState, 0, len(candidates))
	for index, item := range candidates {
		fields, err := optionalStrings(commands[index].Val())
		if err != nil {
			return nil, fmt.Errorf("read dirty access quota rule %d: %w", item.ruleID, err)
		}
		if fields == nil {
			quota.dropDirty(item.ruleID, item.dirtyRule)
			continue
		}
		revision, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("read dirty access quota rule %d: %w", item.ruleID, err)
		}
		snapshot, err := parseRuleState(item.accessKeyID, item.ruleID, revision, fields[1:])
		if err != nil {
			return nil, err
		}
		if !item.coveredBy(snapshot.RuleRevision, snapshot.SnapshotVersion) {
			// Redis lost this change and was rehydrated from an older checkpoint.
			quota.dropDirty(item.ruleID, item.dirtyRule)
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

// Ack forgets a dirty rule once a checkpoint at or beyond its recorded
// revision and version is durable. A later local change keeps it dirty.
func (quota *AccessQuota) Ack(accessKeyID, ruleID uint, revision, snapshotVersion uint64) {
	quota.mu.Lock()
	defer quota.mu.Unlock()
	rule, exists := quota.dirty[ruleID]
	if exists && rule.accessKeyID == accessKeyID && rule.coveredBy(revision, snapshotVersion) {
		delete(quota.dirty, ruleID)
	}
}

func (quota *AccessQuota) dropDirty(ruleID uint, observed dirtyRule) {
	quota.mu.Lock()
	defer quota.mu.Unlock()
	if quota.dirty[ruleID] == observed {
		delete(quota.dirty, ruleID)
	}
}

// markDirty records the newest (revision, version) this instance produced.
func (quota *AccessQuota) markDirty(accessKeyID, ruleID uint, revision, version uint64) {
	quota.mu.Lock()
	defer quota.mu.Unlock()
	next := dirtyRule{accessKeyID: accessKeyID, revision: revision, version: version}
	if current, exists := quota.dirty[ruleID]; !exists || !next.coveredBy(current.revision, current.version) {
		quota.dirty[ruleID] = next
	}
}

func (quota *AccessQuota) notifyDirty() {
	quota.mu.Lock()
	notifier := quota.notifier
	quota.mu.Unlock()
	if notifier != nil {
		notifier()
	}
}

type quotaEvaluation struct {
	allowed bool
	states  map[uint]accessquota.RestoredState
	opened  []bool
}

// evaluate runs the quota script, hydrating missing rules from the database
// checkpoint once before giving up.
func (quota *AccessQuota) evaluate(
	ctx context.Context,
	mode string,
	accessKeyID uint,
	rules []accessquota.Rule,
	now time.Time,
) (quotaEvaluation, error) {
	keys := make([]string, 0, len(rules))
	args := make([]any, 0, 2+len(rules)*4)
	nowMS := now.UnixMilli()
	args = append(args, mode, strconv.FormatInt(nowMS, 10))
	for _, rule := range rules {
		keys = append(keys, quota.ruleKey(accessKeyID, rule.ID))
		windowEnd := nowMS + rule.PeriodSeconds*int64(time.Second/time.Millisecond)
		args = append(args, strconv.FormatUint(rule.Revision, 10), string(rule.Kind),
			strconv.FormatInt(rule.LimitNanoUSD, 10), strconv.FormatInt(windowEnd, 10))
	}
	for attempt := 0; ; attempt++ {
		reply, err := quota.runState(ctx, quotaScript, keys, args)
		if err != nil {
			return quotaEvaluation{}, quota.unavailable(accessKeyID, err)
		}
		if len(reply) == 0 {
			return quotaEvaluation{}, fmt.Errorf("evaluate access quota: empty reply")
		}
		switch reply[0] {
		case "STALE":
			return quotaEvaluation{}, accessquota.ErrStaleRules
		case "NEED_INIT":
			if attempt > 0 {
				return quotaEvaluation{}, fmt.Errorf("evaluate access quota for key %d: state still missing after hydration", accessKeyID)
			}
			if err := quota.hydrate(ctx, accessKeyID, rules, reply[1:]); err != nil {
				return quotaEvaluation{}, err
			}
		case "OK":
			return parseEvaluation(accessKeyID, rules, reply[1:])
		default:
			return quotaEvaluation{}, fmt.Errorf("evaluate access quota: unexpected reply %q", reply[0])
		}
	}
}

// hydrate copies the database checkpoint of the listed rules into Redis.
func (quota *AccessQuota) hydrate(
	ctx context.Context,
	accessKeyID uint,
	rules []accessquota.Rule,
	indexes []string,
) error {
	targets := make([]accessquota.Rule, 0, len(indexes))
	ruleIDs := make([]uint, 0, len(indexes))
	for _, value := range indexes {
		index, err := strconv.Atoi(value)
		if err != nil || index < 1 || index > len(rules) {
			return fmt.Errorf("hydrate access quota: malformed rule index %q", value)
		}
		targets = append(targets, rules[index-1])
		ruleIDs = append(ruleIDs, rules[index-1].ID)
	}
	if quota.states == nil {
		return fmt.Errorf("hydrate access quota: checkpoint reader is unavailable")
	}
	persisted, err := quota.states.ReadAccessQuotaStates(ctx, ruleIDs)
	if err != nil {
		logrus.WithError(err).WithFields(logrus.Fields{
			"event": "access_quota.checkpoint_read_failed", "access_key_id": accessKeyID,
		}).Warn("access key cost limit checkpoint read failed")
		return err
	}
	byRule := make(map[uint]accessquota.RestoredState, len(persisted))
	for _, checkpoint := range persisted {
		byRule[checkpoint.RuleID] = checkpoint
	}
	keys := make([]string, 0, len(targets))
	args := make([]any, 0, len(targets)*6)
	for _, rule := range targets {
		checkpoint, exists := byRule[rule.ID]
		switch {
		case !exists:
			// The rule was deleted after the caller's snapshot was published.
			return fmt.Errorf("hydrate access quota rule %d: checkpoint is missing: %w", rule.ID, accessquota.ErrStaleRules)
		case checkpoint.RuleRevision > rule.Revision:
			return fmt.Errorf("hydrate access quota rule %d: %w", rule.ID, accessquota.ErrStaleRules)
		case checkpoint.RuleRevision < rule.Revision:
			return fmt.Errorf("hydrate access quota rule %d: checkpoint revision %d is behind %d",
				rule.ID, checkpoint.RuleRevision, rule.Revision)
		case checkpoint.AccessKeyID != accessKeyID:
			return fmt.Errorf("hydrate access quota rule %d: checkpoint belongs to key %d", rule.ID, checkpoint.AccessKeyID)
		}
		if err := accessquota.ValidateRestoredState(rule, checkpoint); err != nil {
			return fmt.Errorf("hydrate access quota: %w", err)
		}
		keys = append(keys, quota.ruleKey(accessKeyID, rule.ID))
		args = append(args,
			strconv.FormatUint(checkpoint.RuleRevision, 10), strconv.FormatInt(checkpoint.UsedNanoUSD, 10),
			formatOptionalMS(checkpoint.WindowStartedAtMS), formatOptionalMS(checkpoint.WindowEndsAtMS),
			strconv.FormatUint(checkpoint.WindowGeneration, 10), strconv.FormatUint(checkpoint.SnapshotVersion, 10))
	}
	if _, err := quota.runState(ctx, initScript, keys, args); err != nil {
		return quota.unavailable(accessKeyID, err)
	}
	return nil
}

func (quota *AccessQuota) runState(ctx context.Context, script *redis.Script, keys []string, args []any) ([]string, error) {
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	return runStrings(callCtx, quota.client, script, keys, args)
}

func (quota *AccessQuota) unavailable(accessKeyID uint, err error) error {
	logrus.WithError(err).WithFields(logrus.Fields{
		"event": "access_quota.redis_unavailable", "access_key_id": accessKeyID,
	}).Warn("shared access key cost limit state is unavailable")
	return fmt.Errorf("access quota state for key %d: %w", accessKeyID, err)
}

// ruleKey shares the {ak:<id>} hash tag with every other per-key state so one
// script can touch all of an AccessKey's keys in Redis Cluster.
func (quota *AccessQuota) ruleKey(accessKeyID, ruleID uint) string {
	return quota.client.Key(accessKeyHashTag(accessKeyID), "quota", strconv.FormatUint(uint64(ruleID), 10))
}

func accessKeyHashTag(accessKeyID uint) string {
	return "{ak:" + strconv.FormatUint(uint64(accessKeyID), 10) + "}"
}

func costLimitRules(snapshot *state.ConfigSnapshot, accessKeyID uint) []accessquota.Rule {
	if snapshot == nil {
		return nil
	}
	return snapshot.AccessKeysByID[accessKeyID].CostLimitRules
}

func allowedDecision() accessquota.Decision {
	return accessquota.DecisionFor(nil, nil, 0)
}

func parseEvaluation(accessKeyID uint, rules []accessquota.Rule, fields []string) (quotaEvaluation, error) {
	const perRule = 6
	if len(fields) != 1+len(rules)*perRule {
		return quotaEvaluation{}, fmt.Errorf("evaluate access quota: malformed reply %q", fields)
	}
	evaluation := quotaEvaluation{
		allowed: fields[0] == "1",
		states:  make(map[uint]accessquota.RestoredState, len(rules)),
		opened:  make([]bool, len(rules)),
	}
	for index, rule := range rules {
		row := fields[1+index*perRule : 1+(index+1)*perRule]
		ruleState, err := parseRuleState(accessKeyID, rule.ID, rule.Revision, row[:5])
		if err != nil {
			return quotaEvaluation{}, err
		}
		evaluation.states[rule.ID] = ruleState
		evaluation.opened[index] = row[5] == "1"
	}
	return evaluation, nil
}

// parseRuleState decodes used, ws, we, gen, ver.
func parseRuleState(accessKeyID, ruleID uint, revision uint64, fields []string) (accessquota.RestoredState, error) {
	used, usedErr := strconv.ParseInt(fields[0], 10, 64)
	started, startedErr := parseOptionalMS(fields[1])
	ends, endsErr := parseOptionalMS(fields[2])
	generation, generationErr := strconv.ParseUint(fields[3], 10, 64)
	version, versionErr := strconv.ParseUint(fields[4], 10, 64)
	if err := errors.Join(usedErr, startedErr, endsErr, generationErr, versionErr); err != nil {
		return accessquota.RestoredState{}, fmt.Errorf("decode access quota rule %d state: %w", ruleID, err)
	}
	return accessquota.RestoredState{
		AccessKeyID: accessKeyID, RuleID: ruleID, RuleRevision: revision, UsedNanoUSD: used,
		WindowStartedAtMS: started, WindowEndsAtMS: ends, WindowGeneration: generation, SnapshotVersion: version,
	}, nil
}

func formatOptionalMS(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}

func parseOptionalMS(value string) (*int64, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func runStrings(ctx context.Context, client *Client, script *redis.Script, keys []string, args []any) ([]string, error) {
	values, err := script.Run(ctx, client, keys, args...).Slice()
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected script reply element %T", value)
		}
		result = append(result, text)
	}
	return result, nil
}

// optionalStrings converts an HMGET reply, returning nil when the key is
// absent and an error when it exists with missing fields.
func optionalStrings(values []any) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == nil {
			if len(result) == 0 {
				return nil, nil
			}
			return nil, fmt.Errorf("incomplete state hash")
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected state field %T", value)
		}
		result = append(result, text)
	}
	return result, nil
}
