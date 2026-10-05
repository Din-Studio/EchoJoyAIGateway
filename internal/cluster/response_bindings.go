package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"gpt-load/internal/automodel"
	"gpt-load/internal/state"
)

// ResponseBindings stores Responses ownership in Redis so any instance can
// continue a response created on another one. Each response is one key that
// expires after the configured TTL; the first recorded owner wins.
type ResponseBindings struct {
	client *Client
	ttl    time.Duration
	now    func() time.Time
}

// storedResponseBinding is the Redis value. The AccessKey and response ID are
// already part of the key, so they are not repeated.
type storedResponseBinding struct {
	GroupID            uint                 `json:"g"`
	CredentialID       uint                 `json:"c"`
	IdentityGeneration uint64               `json:"i"`
	ExpiresAtMS        int64                `json:"e"`
	AutoSelection      *automodel.Selection `json:"a,omitempty"`
}

// NewResponseBindings builds the shared Responses ownership index.
func NewResponseBindings(client *Client, ttl time.Duration) *ResponseBindings {
	return &ResponseBindings{client: client, ttl: ttl, now: time.Now}
}

// Lookup returns the ownership recorded for one response by any instance.
func (bindings *ResponseBindings) Lookup(
	ctx context.Context,
	accessKeyID uint,
	responseID string,
) (state.ResponseBinding, bool, error) {
	// Longer IDs can never be recorded; skip sending a client-sized key to Redis.
	if responseID == "" || len(responseID) > state.MaxResponseIDBytes {
		return state.ResponseBinding{}, false, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	raw, err := bindings.client.Get(callCtx, bindings.key(accessKeyID, responseID)).Result()
	if errors.Is(err, redis.Nil) {
		return state.ResponseBinding{}, false, nil
	}
	if err != nil {
		return state.ResponseBinding{}, false, fmt.Errorf("look up response ownership: %w", err)
	}
	binding, ok := decodeResponseBinding(accessKeyID, responseID, raw)
	if !ok {
		logrus.WithFields(logrus.Fields{
			"event": "response_binding.malformed", "access_key_id": accessKeyID,
		}).Warn("ignoring malformed shared response ownership")
		return state.ResponseBinding{}, false, nil
	}
	return binding, true, nil
}

// Record registers ownership before the response is delivered. A conflicting
// owner is rejected and never overwritten, matching the in-process index.
func (bindings *ResponseBindings) Record(
	ctx context.Context,
	accessKeyID uint,
	responseID string,
	ref state.CredentialRef,
	autoSelections ...*automodel.Selection,
) (bool, error) {
	var auto *automodel.Selection
	if len(autoSelections) > 0 {
		auto = autoSelections[0]
	}
	binding, valid := state.NewResponseBinding(accessKeyID, responseID, ref, auto, bindings.now(), bindings.ttl)
	if !valid {
		return false, nil
	}
	value, err := json.Marshal(storedResponseBinding{
		GroupID: binding.GroupID, CredentialID: binding.CredentialID,
		IdentityGeneration: binding.IdentityGeneration, ExpiresAtMS: binding.ExpiresAt.UnixMilli(),
		AutoSelection: binding.AutoSelection,
	})
	if err != nil {
		return false, fmt.Errorf("encode response ownership: %w", err)
	}
	key := bindings.key(accessKeyID, responseID)
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	// A second attempt covers an existing key that expired between SETNX and GET.
	for range 2 {
		created, err := bindings.client.SetNX(callCtx, key, value, bindings.ttl).Result()
		if err != nil {
			return false, fmt.Errorf("record response ownership: %w", err)
		}
		if created {
			return true, nil
		}
		raw, err := bindings.client.Get(callCtx, key).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read existing response ownership: %w", err)
		}
		existing, ok := decodeResponseBinding(accessKeyID, responseID, raw)
		return ok && state.SameResponseOwner(existing, binding), nil
	}
	return false, nil
}

func (bindings *ResponseBindings) key(accessKeyID uint, responseID string) string {
	return bindings.client.Key("rb", strconv.FormatUint(uint64(accessKeyID), 10), responseID)
}

func decodeResponseBinding(accessKeyID uint, responseID, raw string) (state.ResponseBinding, bool) {
	var stored storedResponseBinding
	if json.Unmarshal([]byte(raw), &stored) != nil || stored.GroupID == 0 || stored.CredentialID == 0 ||
		stored.IdentityGeneration == 0 {
		return state.ResponseBinding{}, false
	}
	return state.ResponseBinding{
		AutoSelection: stored.AutoSelection,
		AccessKeyID:   accessKeyID, ResponseID: responseID,
		GroupID: stored.GroupID, CredentialID: stored.CredentialID, IdentityGeneration: stored.IdentityGeneration,
		ExpiresAt: time.UnixMilli(stored.ExpiresAtMS),
	}, true
}
