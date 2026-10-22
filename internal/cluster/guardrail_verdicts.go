package cluster

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// GuardrailVerdicts shares guardrail review evidence across instances. Keys
// are irreversible fingerprints and values are rule probabilities; every entry
// keeps the fixed TTL it was written with, and reads never extend it. One
// AccessKey's entries share a hash tag so a lookup is a single MGET.
type GuardrailVerdicts struct {
	client *Client
	ttl    time.Duration
}

// NewGuardrailVerdicts builds the shared guardrail evidence store whose
// entries live for ttl.
func NewGuardrailVerdicts(client *Client, ttl time.Duration) *GuardrailVerdicts {
	return &GuardrailVerdicts{client: client, ttl: ttl}
}

// Lookup returns the stored probability of every key that is still live.
func (store *GuardrailVerdicts) Lookup(ctx context.Context, accessKeyID uint, keys [][32]byte) (map[[32]byte]float64, error) {
	found := map[[32]byte]float64{}
	if len(keys) == 0 {
		return found, nil
	}
	names := make([]string, len(keys))
	for index, key := range keys {
		names[index] = store.key(accessKeyID, key)
	}
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	values, err := store.client.MGet(callCtx, names...).Result()
	if err != nil {
		return nil, fmt.Errorf("look up guardrail verdicts: %w", err)
	}
	for index, value := range values {
		text, ok := value.(string)
		if !ok {
			continue
		}
		probability, err := strconv.ParseFloat(text, 64)
		if err != nil {
			continue
		}
		found[keys[index]] = probability
	}
	return found, nil
}

// Store writes every entry with the store TTL in one pipeline.
func (store *GuardrailVerdicts) Store(ctx context.Context, accessKeyID uint, entries map[[32]byte]float64) error {
	if len(entries) == 0 {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	pipe := store.client.Pipeline()
	for key, probability := range entries {
		pipe.Set(callCtx, store.key(accessKeyID, key), strconv.FormatFloat(probability, 'g', -1, 64), store.ttl)
	}
	if _, err := pipe.Exec(callCtx); err != nil {
		return fmt.Errorf("store guardrail verdicts: %w", err)
	}
	return nil
}

func (store *GuardrailVerdicts) key(accessKeyID uint, key [32]byte) string {
	return store.client.Key("guard", accessKeyHashTag(accessKeyID), hex.EncodeToString(key[:]))
}
