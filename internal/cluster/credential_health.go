package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"gpt-load/internal/state"
)

const (
	credentialHealthEventsChannel = "credential-health-events"
	// credentialHealthTTL is the sliding lifetime of a credential's health
	// record, so records of deleted credentials expire on their own.
	credentialHealthTTL = 30 * 24 * time.Hour
	// credentialHealthReconcileInterval bounds how long a lost event can
	// keep a mirror stale.
	credentialHealthReconcileInterval = 60 * time.Second
	credentialHealthSubscribeRetry    = time.Second
	credentialHealthHydrateBatch      = 500
)

// CredentialHealth keeps credential health in Redis, one Hash per credential,
// and mirrors it into the local registry. Writes apply their result locally
// and publish it to peers; a periodic reconciliation repairs lost events. No
// method performs a Redis call while holding a process lock. It is nil in
// single-instance mode.
type CredentialHealth struct {
	client   *Client
	registry *state.CredentialRegistry
	// origin distinguishes this store's events from those of every other
	// store, including stores sharing one instance ID.
	origin            string
	reconcileInterval time.Duration
}

var _ state.SharedCredentialHealthStore = (*CredentialHealth)(nil)

// NewCredentialHealth returns nil when cluster mode is disabled.
func NewCredentialHealth(client *Client, registry *state.CredentialRegistry) *CredentialHealth {
	if client == nil {
		return nil
	}
	return &CredentialHealth{
		client: client, registry: registry,
		origin:            client.InstanceID() + ":" + randomToken(),
		reconcileInterval: credentialHealthReconcileInterval,
	}
}

type healthOp struct {
	name string
	args []string
}

// CooldownCredential implements state.SharedCredentialHealthStore.
func (health *CredentialHealth) CooldownCredential(
	ctx context.Context,
	ref state.CredentialRef,
	until time.Time,
	expectedVersion uint64,
) (state.SharedHealthResult, error) {
	if expectedVersion != 0 {
		current, ok := health.registry.CredentialRef(ref.ID)
		if !ok || current.Version != expectedVersion {
			return state.SharedHealthResult{}, nil
		}
	}
	expected := ""
	if expectedVersion != 0 {
		expected = strconv.FormatUint(expectedVersion, 10)
	}
	return health.apply(ctx, ref, healthOp{"cooldown", []string{formatHealthMS(until), expected}})
}

// CooldownModel implements state.SharedCredentialHealthStore.
func (health *CredentialHealth) CooldownModel(
	ctx context.Context,
	ref state.CredentialRef,
	model string,
	until, now time.Time,
) (state.SharedHealthResult, error) {
	if model == "" || strings.TrimSpace(model) != model || !until.After(now) {
		return state.SharedHealthResult{}, nil
	}
	return health.apply(ctx, ref, healthOp{"model_cooldown", []string{
		model, formatHealthMS(until), formatHealthMS(now), strconv.FormatUint(ref.ModelCooldownGeneration, 10),
	}})
}

// RecordFailure implements state.SharedCredentialHealthStore.
func (health *CredentialHealth) RecordFailure(
	ctx context.Context,
	ref state.CredentialRef,
	threshold int,
) (state.SharedHealthResult, error) {
	return health.apply(ctx, ref, healthOp{"fail", []string{strconv.Itoa(threshold)}})
}

// ClearFailure implements state.SharedCredentialHealthStore.
func (health *CredentialHealth) ClearFailure(ctx context.Context, ref state.CredentialRef) (state.SharedHealthResult, error) {
	return health.apply(ctx, ref, healthOp{"clear_failure", nil})
}

// Restore implements state.SharedCredentialHealthStore.
func (health *CredentialHealth) Restore(
	ctx context.Context,
	ref state.CredentialRef,
	runtime, modelCooldowns bool,
) (state.SharedHealthResult, error) {
	return health.apply(ctx, ref, healthOp{"restore", []string{formatFlag(runtime), formatFlag(modelCooldowns)}})
}

// RecoverIfMatch implements state.SharedCredentialHealthStore. The durable
// identity and active status are checked against the local registry, which
// already mirrors configuration; Redis checks the health generation.
func (health *CredentialHealth) RecoverIfMatch(
	ctx context.Context,
	ref state.CredentialRef,
	cooldownUntil *time.Time,
) (state.SharedHealthResult, error) {
	if !health.registry.CredentialMatchesRef(ref) {
		return state.SharedHealthResult{}, nil
	}
	cooldown := ""
	if cooldownUntil != nil {
		cooldown = formatHealthMS(*cooldownUntil)
	}
	return health.apply(ctx, ref, healthOp{"recover_if_match", []string{
		strconv.FormatUint(ref.FailureGeneration, 10), cooldown,
	}})
}

// ClearCooldownIfMatch implements state.SharedCredentialHealthStore.
func (health *CredentialHealth) ClearCooldownIfMatch(
	ctx context.Context,
	ref state.CredentialRef,
	expected time.Time,
) (state.SharedHealthResult, error) {
	if expected.IsZero() {
		return state.SharedHealthResult{}, nil
	}
	return health.apply(ctx, ref, healthOp{"clear_cooldown_if_match", []string{formatHealthMS(expected)}})
}

// SetAuthState implements state.SharedCredentialHealthStore.
func (health *CredentialHealth) SetAuthState(
	ctx context.Context,
	ref state.CredentialRef,
	authState state.CredentialAuthState,
	secretVersion uint64,
) (state.SharedHealthResult, error) {
	if authState == "" {
		authState = state.CredentialAuthStateReady
	}
	return health.apply(ctx, ref, healthOp{"auth", []string{string(authState), strconv.FormatUint(secretVersion, 10)}})
}

// apply runs one script operation and mirrors the resulting record locally.
func (health *CredentialHealth) apply(
	ctx context.Context,
	ref state.CredentialRef,
	op healthOp,
) (state.SharedHealthResult, error) {
	if ref.ID == 0 {
		return state.SharedHealthResult{}, fmt.Errorf("credential health %s: credential id is required", op.name)
	}
	args := make([]any, 0, 7+len(op.args))
	args = append(args, op.name, health.origin, strconv.FormatUint(uint64(ref.ID), 10),
		strconv.FormatUint(ref.IdentityGeneration, 10), randomToken(),
		strconv.FormatInt(credentialHealthTTL.Milliseconds(), 10), health.channel())
	for _, arg := range op.args {
		args = append(args, arg)
	}
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	reply, err := runStrings(callCtx, health.client, credentialHealthScript, []string{health.key(ref.ID)}, args)
	if err != nil {
		return state.SharedHealthResult{}, fmt.Errorf("credential %d health %s: %w", ref.ID, op.name, err)
	}
	if len(reply) < 3 || (len(reply)-3)%2 != 0 {
		return state.SharedHealthResult{}, fmt.Errorf("credential %d health %s: malformed reply %q", ref.ID, op.name, reply)
	}
	fields := make(map[string]string, (len(reply)-3)/2)
	for index := 3; index < len(reply); index += 2 {
		fields[reply[index]] = reply[index+1]
	}
	record, err := decodeCredentialHealth(fields)
	if err != nil {
		return state.SharedHealthResult{}, fmt.Errorf("credential %d health %s: %w", ref.ID, op.name, err)
	}
	health.registry.ApplySharedHealth(ref.ID, record)
	return state.SharedHealthResult{
		Accepted: reply[0] == "1", Changed: reply[1] == "1", BecameBlacklisted: reply[2] == "1",
		FailureCount: record.FailureCount,
	}, nil
}

// Hydrate mirrors the current Redis record of every registered credential.
// It is the startup restore and the periodic repair for lost events.
func (health *CredentialHealth) Hydrate(ctx context.Context) error {
	targets := health.registry.SharedHealthTargets()
	for start := 0; start < len(targets); start += credentialHealthHydrateBatch {
		end := min(start+credentialHealthHydrateBatch, len(targets))
		if err := health.hydrateBatch(ctx, targets[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (health *CredentialHealth) hydrateBatch(ctx context.Context, targets []state.SharedHealthTarget) error {
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	pipeline := health.client.Pipeline()
	commands := make([]*redis.MapStringStringCmd, 0, len(targets))
	for _, target := range targets {
		commands = append(commands, pipeline.HGetAll(callCtx, health.key(target.ID)))
	}
	if _, err := pipeline.Exec(callCtx); err != nil {
		return fmt.Errorf("read credential health: %w", err)
	}
	for index, target := range targets {
		record, err := decodeCredentialHealth(commands[index].Val())
		if err != nil {
			return fmt.Errorf("read credential %d health: %w", target.ID, err)
		}
		health.registry.ApplySharedHealth(target.ID, record)
	}
	return nil
}

// Run keeps the mirror current until ctx is canceled: peer events are the
// fast path, and a reconciliation after every (re)subscription and on each
// interval repairs anything an event missed.
func (health *CredentialHealth) Run(ctx context.Context) {
	if health == nil {
		return
	}
	var subscriber sync.WaitGroup
	subscriber.Add(1)
	go func() {
		defer subscriber.Done()
		for {
			err := health.subscribe(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				logrus.WithError(err).WithField("event", "credential_health.reconcile_failed").
					Warn("credential health subscription failed; retrying")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(credentialHealthSubscribeRetry):
			}
		}
	}()
	defer subscriber.Wait()

	ticker := time.NewTicker(health.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			health.reconcile(ctx)
		}
	}
}

func (health *CredentialHealth) reconcile(ctx context.Context) {
	if err := health.Hydrate(ctx); err != nil && ctx.Err() == nil {
		logrus.WithError(err).WithField("event", "credential_health.reconcile_failed").
			Warn("credential health reconciliation failed")
	}
}

func (health *CredentialHealth) subscribe(ctx context.Context) error {
	pubsub := health.client.Subscribe(ctx, health.channel())
	defer func() { _ = pubsub.Close() }()
	if _, err := pubsub.Receive(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("subscribe credential health: %w", err)
	}
	// Events published before the subscription are covered by this pass.
	health.reconcile(ctx)
	messages := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case message, ok := <-messages:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("subscribe credential health: %w", redis.ErrClosed)
			}
			health.handle(message.Payload)
		}
	}
}

type credentialHealthEvent struct {
	Origin string            `json:"origin"`
	ID     uint              `json:"id"`
	State  map[string]string `json:"state"`
}

func (health *CredentialHealth) handle(payload string) {
	var event credentialHealthEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil || event.ID == 0 {
		logrus.WithError(err).WithField("event", "credential_health.event_invalid").
			Warn("ignoring malformed credential health event")
		return
	}
	if event.Origin == health.origin {
		return
	}
	record, err := decodeCredentialHealth(event.State)
	if err != nil {
		logrus.WithError(err).WithFields(logrus.Fields{
			"event": "credential_health.event_invalid", "credential_id": event.ID,
		}).Warn("ignoring malformed credential health event")
		return
	}
	health.registry.ApplySharedHealth(event.ID, record)
}

func (health *CredentialHealth) channel() string {
	return health.client.Key(credentialHealthEventsChannel)
}

func (health *CredentialHealth) key(credentialID uint) string {
	return health.client.Key(credentialHashTag(credentialID), "health")
}

// credentialHashTag keeps every per-credential key in one Redis Cluster slot.
func credentialHashTag(credentialID uint) string {
	return "{cred:" + strconv.FormatUint(uint64(credentialID), 10) + "}"
}

// decodeCredentialHealth converts a Hash into a mirror state; an empty Hash
// is the zero state with an empty epoch.
func decodeCredentialHealth(fields map[string]string) (state.SharedCredentialHealth, error) {
	if len(fields) == 0 {
		return state.SharedCredentialHealth{}, nil
	}
	var (
		record state.SharedCredentialHealth
		errs   []error
	)
	parseUint := func(field string) uint64 {
		value, exists := fields[field]
		if !exists {
			return 0
		}
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("field %s: %w", field, err))
		}
		return parsed
	}
	parseMS := func(field string) time.Time {
		value, exists := fields[field]
		if !exists {
			return time.Time{}
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("field %s: %w", field, err))
		}
		return parseHealthMS(parsed)
	}
	record.Epoch = fields["ep"]
	if record.Epoch == "" {
		return state.SharedCredentialHealth{}, fmt.Errorf("credential health record has no epoch")
	}
	record.Version = parseUint("ver")
	record.IdentityGeneration = parseUint("idg")
	record.CooldownUntil = parseMS("cd")
	record.Blacklisted = fields["bl"] == "1"
	record.FailureCount = int(parseUint("fc"))
	record.FailureGeneration = parseUint("fg")
	record.ModelCooldownGeneration = parseUint("mg")
	record.AuthState = state.CredentialAuthState(fields["auth"])
	record.AuthSecretVersion = parseUint("asv")
	for field := range fields {
		if model, isModel := strings.CutPrefix(field, "m:"); isModel {
			if record.ModelCooldowns == nil {
				record.ModelCooldowns = make(map[string]time.Time)
			}
			record.ModelCooldowns[model] = parseMS(field)
		}
	}
	return record, errors.Join(errs...)
}

// formatHealthMS encodes a deadline; the zero time is stored as "0", the
// same value an absent field reads as.
func formatHealthMS(value time.Time) string {
	if value.IsZero() {
		return "0"
	}
	return strconv.FormatInt(value.UnixMilli(), 10)
}

func parseHealthMS(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.UnixMilli(value)
}

func formatFlag(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func randomToken() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}
