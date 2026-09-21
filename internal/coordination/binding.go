package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"gpt-load/internal/automodel"
	"gpt-load/internal/state"
)

// ResponseBindings is the cross-instance index of which credential owns a
// response id, so a continuation reaching any instance resolves the same way
// the instance that created the response would have resolved it.
//
// The write semantics are the in-memory index's, word for word: absent means
// insert, identical means accept, different means reject without overwriting
// (internal/state/response_bindings.go). Ownership is never corrected by a
// later response, only established by the first one.
//
// Expiry belongs to the key's PX, which is why the encoded payload carries no
// expiry of its own. Two instances recording the same ownership at different
// instants therefore produce identical bytes, and a difference in timing can
// never be mistaken for a difference in ownership.
//
// Every operation is a single key. There is no hash tag convention here and
// later phases should not assume one.
type ResponseBindings struct {
	client *Client
	ttl    time.Duration
}

// recordResponseBinding is the three-way decision the in-memory insert makes,
// executed atomically so a concurrent recorder on another instance cannot read
// "absent" and write over the ownership that was established in between.
//
// The identical branch deliberately leaves the TTL alone: ownership expires a
// fixed time after it was established, and re-recording it is not use.
var recordResponseBinding = redis.NewScript(`
local existing = redis.call('GET', KEYS[1])
if not existing then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end
if existing == ARGV[1] then return 1 end
return 0
`)

// lookupResponseBinding reads the payload together with the remaining TTL so
// the caller can be handed the absolute expiry its contract promises. Reading
// them separately could straddle an expiry and report a live binding with a
// lapsed deadline.
var lookupResponseBinding = redis.NewScript(`
local value = redis.call('GET', KEYS[1])
if not value then return nil end
return {value, redis.call('PTTL', KEYS[1])}
`)

// NewResponseBindings binds the shared ownership index to a Redis client. The
// lifetime is the in-memory index's compile-time constant rather than a
// parameter: a second way to set it would be a second source of truth.
func NewResponseBindings(client *Client) *ResponseBindings {
	return &ResponseBindings{client: client, ttl: state.DefaultResponseBindingTTL}
}

// Record establishes ownership of responseID before the response reaches the
// client, reporting whether this response may be delivered. False means a
// different owner already holds the id; an error means the answer is unknown
// and the caller must fail closed rather than guess.
func (bindings *ResponseBindings) Record(
	ctx context.Context,
	accessKeyID uint,
	responseID string,
	ref state.CredentialRef,
	auto *automodel.Selection,
) (bool, error) {
	if accessKeyID == 0 || responseID == "" || ref.ID == 0 || ref.GroupID == 0 ||
		ref.IdentityGeneration == 0 || len(responseID) > state.MaxResponseIDBytes {
		return false, nil
	}
	// ref.Version is deliberately absent: a rotated token is the same
	// credential and must keep the ownership it already established.
	payload, err := encodeResponseBinding(state.ResponseBinding{
		AutoSelection: auto,
		AccessKeyID:   accessKeyID, ResponseID: responseID,
		GroupID: ref.GroupID, CredentialID: ref.ID, IdentityGeneration: ref.IdentityGeneration,
	})
	if err != nil {
		return false, err
	}
	recorded, err := recordResponseBinding.Run(
		ctx, bindings.client.Redis(),
		[]string{responseBindingKey(accessKeyID, responseID)},
		payload, bindings.ttl.Milliseconds(),
	).Int64()
	if err != nil {
		return false, fmt.Errorf("record response binding: %w", err)
	}
	return recorded == 1, nil
}

// Lookup resolves who owns responseID. A missing key is an ordinary miss; a
// failure is reported as one so a coordination outage cannot be served to the
// client as "this response id does not exist".
func (bindings *ResponseBindings) Lookup(
	ctx context.Context,
	accessKeyID uint,
	responseID string,
) (state.ResponseBinding, bool, error) {
	if accessKeyID == 0 || responseID == "" || len(responseID) > state.MaxResponseIDBytes {
		return state.ResponseBinding{}, false, nil
	}
	found, err := lookupResponseBinding.Run(
		ctx, bindings.client.Redis(), []string{responseBindingKey(accessKeyID, responseID)},
	).Slice()
	if errors.Is(err, redis.Nil) {
		return state.ResponseBinding{}, false, nil
	}
	if err != nil {
		return state.ResponseBinding{}, false, fmt.Errorf("read response binding: %w", err)
	}
	if len(found) != 2 {
		return state.ResponseBinding{}, false, fmt.Errorf("read response binding: unexpected reply shape")
	}
	payload, ok := found[0].(string)
	if !ok {
		return state.ResponseBinding{}, false, fmt.Errorf("read response binding: unexpected payload type")
	}
	remaining, ok := found[1].(int64)
	if !ok {
		return state.ResponseBinding{}, false, fmt.Errorf("read response binding: unexpected ttl type")
	}
	var binding state.ResponseBinding
	if err := json.Unmarshal([]byte(payload), &binding); err != nil {
		return state.ResponseBinding{}, false, fmt.Errorf("decode response binding: %w", err)
	}
	// The payload carries no expiry, so rebuild the absolute deadline callers
	// read from the TTL that owns it.
	binding.ExpiresAt = time.Now().Add(time.Duration(remaining) * time.Millisecond)
	return binding, true, nil
}

// encodeResponseBinding produces the bytes ownership is compared by. Equality
// is byte equality, exactly as the in-memory index compares its own encoding,
// so any field that can differ between two recordings of the same ownership
// must be left out rather than normalized here.
func encodeResponseBinding(binding state.ResponseBinding) ([]byte, error) {
	binding.ExpiresAt = time.Time{}
	payload, err := json.Marshal(binding)
	if err != nil {
		return nil, fmt.Errorf("encode response binding: %w", err)
	}
	return payload, nil
}

func responseBindingKey(accessKeyID uint, responseID string) string {
	return Key("binding", strconv.FormatUint(uint64(accessKeyID), 10), responseID)
}
