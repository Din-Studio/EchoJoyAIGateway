package coordination

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"gpt-load/internal/state"
)

// credentialHealthTag keeps the store, its change index and its sequence on
// one slot, because every script here reads at least two of them together.
const credentialHealthTag = "health"

// CredentialHealth carries locally decided credential health to the other
// instances, so an upstream that rate-limited one instance stops being picked
// by all of them.
//
// The design is a replicated log of latest values, not a read-through cache.
// Credential selection happens under the registry's lock on every request and
// cannot afford a round trip; instead each instance keeps its own registry as
// the thing selection reads, and this type's only job is to make every
// registry agree. That is why a lost Pub/Sub message costs latency and not
// correctness: the sequence index is the real channel, and the doorbell only
// says "read it now".
//
// Health entries are never deleted. One JSON payload per credential that has
// ever been unhealthy is bounded by the credential count, and a deletion here
// could only ever lose a change a lagging instance has not applied yet.
type CredentialHealth struct {
	client   *Client
	storeKey string
	indexKey string
	sequence string
	channel  string
	timeout  time.Duration
}

// publishCredentialHealth stamps each change with the next sequence number and
// records it, atomically so a reader can never see an index entry whose
// payload has not landed yet.
//
// The doorbell payload is the highest sequence written, for human
// troubleshooting only; receivers re-read the index rather than trust it.
var publishCredentialHealth = redis.NewScript(`
local sequence = 0
for index = 1, #ARGV, 2 do
  sequence = redis.call('INCR', KEYS[3])
  redis.call('HSET', KEYS[1], ARGV[index], ARGV[index + 1])
  redis.call('ZADD', KEYS[2], sequence, ARGV[index])
end
redis.call('PUBLISH', KEYS[4], sequence)
return sequence
`)

// readCredentialHealth returns every change after a sequence number together
// with the sequence each carries, so the caller can resume from exactly where
// it stopped. Reading the index and the payloads in one script is what keeps a
// change from being skipped by a write that lands between the two reads.
//
// The first element of the reply is the sequence the read actually resumed
// from, which is the caller's cursor except when the store has restarted its
// counter. A store that lost its data issues sequence 1 again, so a cursor
// above anything it has ever issued would match nothing for the rest of this
// process's life. Comparing against the counter inside the script costs no
// round trip and reconverges by re-reading everything, which is what the
// configuration version watch does when its counter falls back.
var readCredentialHealth = redis.NewScript(`
local after = tonumber(ARGV[1])
local issued = tonumber(redis.call('GET', KEYS[3]) or '0')
if issued < after then
  after = 0
end
local changed = redis.call('ZRANGEBYSCORE', KEYS[2], '(' .. after, '+inf', 'WITHSCORES')
local out = {tostring(after)}
for index = 1, #changed, 2 do
  local payload = redis.call('HGET', KEYS[1], changed[index])
  if payload then
    out[#out + 1] = changed[index]
    out[#out + 1] = changed[index + 1]
    out[#out + 1] = payload
  end
end
return out
`)

// credentialHealthPayload is the wire form of one credential's health. Times
// are milliseconds rather than RFC 3339 so two instances with different
// locales or clock formatting still produce comparable bytes.
//
// The payload carries reasons to avoid a credential and the reset counter that
// orders them, and nothing else. An older build's payload decodes with
// ResetGen zero, which is the generation every credential starts at, so a
// rolling upgrade merges as it always did.
type credentialHealthPayload struct {
	GroupID            uint             `json:"group_id"`
	IdentityGeneration uint64           `json:"identity_generation"`
	ResetGen           uint64           `json:"reset_gen,omitempty"`
	CooldownUntilMS    int64            `json:"cooldown_until_ms,omitempty"`
	Blacklisted        bool             `json:"blacklisted,omitempty"`
	ModelCooldowns     map[string]int64 `json:"model_cooldowns,omitempty"`
}

// NewCredentialHealth binds shared credential health to a Redis client.
func NewCredentialHealth(client *Client) *CredentialHealth {
	return &CredentialHealth{
		client:   client,
		storeKey: Key("cred", hashTag(credentialHealthTag), "store"),
		indexKey: Key("cred", hashTag(credentialHealthTag), "index"),
		sequence: Key("cred", hashTag(credentialHealthTag), "sequence"),
		channel:  Key("cred"),
		timeout:  5 * time.Second,
	}
}

// Publish tells the other instances what this one decided. It returns the
// highest sequence number it wrote, which exists for logging: the caller must
// not use it to skip its own changes on the way back in, because peers'
// changes can interleave with them.
func (health *CredentialHealth) Publish(ctx context.Context, changes []state.CredentialHealth) (int64, error) {
	if len(changes) == 0 {
		return 0, nil
	}
	arguments := make([]any, 0, len(changes)*2)
	for _, change := range changes {
		payload, err := json.Marshal(encodeCredentialHealth(change))
		if err != nil {
			return 0, fmt.Errorf("encode credential health: %w", err)
		}
		arguments = append(arguments,
			strconv.FormatUint(uint64(change.CredentialID), 10), payload)
	}

	ctx, cancel := context.WithTimeout(ctx, health.timeout)
	defer cancel()
	sequence, err := publishCredentialHealth.Run(ctx, health.client.Redis(),
		[]string{health.storeKey, health.indexKey, health.sequence, health.channel},
		arguments...,
	).Int64()
	if err != nil {
		return 0, fmt.Errorf("publish credential health: %w", err)
	}
	return sequence, nil
}

// Changed returns every health change recorded after a sequence number, and
// the sequence the caller should resume from. Starting from zero returns
// everything, which is how an instance that just started learns the cooldowns
// its peers decided while it was down.
//
// The returned sequence can be lower than the one passed in. That means the
// store restarted its counter and this read re-hydrated from the beginning;
// the caller has to adopt it rather than keep its own, or it stays deaf.
func (health *CredentialHealth) Changed(
	ctx context.Context,
	after int64,
) ([]state.CredentialHealth, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, health.timeout)
	defer cancel()
	reply, err := readCredentialHealth.Run(ctx, health.client.Redis(),
		[]string{health.storeKey, health.indexKey, health.sequence}, after,
	).Slice()
	if err != nil {
		return nil, after, fmt.Errorf("read credential health: %w", err)
	}
	return parseCredentialHealthReply(reply, after)
}

// Subscribe turns the health channel into payload-free wakeups, with the same
// contract as the configuration doorbell: a closed channel means "fall back to
// polling", never "stop".
func (health *CredentialHealth) Subscribe(ctx context.Context) (<-chan struct{}, func()) {
	subscription := health.client.Redis().Subscribe(ctx, health.channel)
	messages := subscription.Channel()
	doorbell := make(chan struct{}, 1)
	done := make(chan struct{})

	go func() {
		defer close(doorbell)
		for {
			select {
			case <-done:
				return
			case _, ok := <-messages:
				if !ok {
					return
				}
				select {
				case doorbell <- struct{}{}:
				default:
				}
			}
		}
	}()

	var once sync.Once
	return doorbell, func() {
		once.Do(func() {
			close(done)
			_ = subscription.Close()
		})
	}
}

// parseCredentialHealthReply reads the script's flat (id, sequence, payload)
// triples. The credential id comes from the index member rather than the
// payload: one identity in the reply cannot then disagree with another.
// The reply leads with the sequence the read resumed from, so resume starts
// there rather than at the caller's cursor: keeping the caller's cursor is
// exactly what would make a restarted counter permanent.
func parseCredentialHealthReply(reply []any, after int64) ([]state.CredentialHealth, int64, error) {
	if len(reply) == 0 || len(reply)%3 != 1 {
		return nil, after, fmt.Errorf("read credential health: unexpected reply shape")
	}
	resume, err := parseHealthSequence(reply[0])
	if err != nil {
		return nil, after, err
	}
	changes := make([]state.CredentialHealth, 0, len(reply)/3)
	for index := 1; index < len(reply); index += 3 {
		member, ok := reply[index].(string)
		if !ok {
			return nil, after, fmt.Errorf("read credential health: unexpected id type")
		}
		credentialID, err := strconv.ParseUint(member, 10, 64)
		if err != nil {
			return nil, after, fmt.Errorf("read credential health: parse id: %w", err)
		}
		sequence, err := parseHealthSequence(reply[index+1])
		if err != nil {
			return nil, after, err
		}
		payload, ok := reply[index+2].(string)
		if !ok {
			return nil, after, fmt.Errorf("read credential health: unexpected payload type")
		}
		change, err := decodeCredentialHealth(uint(credentialID), payload)
		if err != nil {
			return nil, after, err
		}
		changes = append(changes, change)
		if sequence > resume {
			resume = sequence
		}
	}
	return changes, resume, nil
}

func encodeCredentialHealth(change state.CredentialHealth) credentialHealthPayload {
	payload := credentialHealthPayload{
		GroupID:            change.GroupID,
		IdentityGeneration: change.IdentityGeneration,
		ResetGen:           change.ResetGen,
		Blacklisted:        change.Blacklisted,
	}
	if !change.CooldownUntil.IsZero() {
		payload.CooldownUntilMS = change.CooldownUntil.UnixMilli()
	}
	if len(change.ModelCooldowns) > 0 {
		payload.ModelCooldowns = make(map[string]int64, len(change.ModelCooldowns))
		for model, until := range change.ModelCooldowns {
			payload.ModelCooldowns[model] = until.UnixMilli()
		}
	}
	return payload
}

func decodeCredentialHealth(credentialID uint, encoded string) (state.CredentialHealth, error) {
	var payload credentialHealthPayload
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		return state.CredentialHealth{}, fmt.Errorf("decode credential health: %w", err)
	}
	change := state.CredentialHealth{
		CredentialID:       credentialID,
		GroupID:            payload.GroupID,
		IdentityGeneration: payload.IdentityGeneration,
		ResetGen:           payload.ResetGen,
		Blacklisted:        payload.Blacklisted,
	}
	if payload.CooldownUntilMS != 0 {
		change.CooldownUntil = time.UnixMilli(payload.CooldownUntilMS)
	}
	if len(payload.ModelCooldowns) > 0 {
		change.ModelCooldowns = make(map[string]time.Time, len(payload.ModelCooldowns))
		for model, until := range payload.ModelCooldowns {
			change.ModelCooldowns[model] = time.UnixMilli(until)
		}
	}
	return change, nil
}

func parseHealthSequence(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		// Sorted-set scores come back as doubles rendered to text.
		parsed, err := strconv.ParseFloat(typed, 64)
		if err != nil {
			return 0, fmt.Errorf("read credential health: parse sequence: %w", err)
		}
		return int64(parsed), nil
	default:
		return 0, fmt.Errorf("read credential health: unexpected sequence type")
	}
}
