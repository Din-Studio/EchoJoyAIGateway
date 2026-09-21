package coordination

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gpt-load/internal/automodel"
	"gpt-load/internal/state"
)

// TestExternalRedisResponseBindingRecordsOwnershipOnce is opt-in so ordinary
// unit tests stay hermetic. It proves the shared index reproduces the
// in-memory index's write semantics against a live Redis.
func TestExternalRedisResponseBindingRecordsOwnershipOnce(t *testing.T) {
	bindings, key := externalResponseBindings(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	owner := state.CredentialRef{ID: 3, GroupID: 2, Version: 1, IdentityGeneration: 4}
	auto := &automodel.Selection{
		EntryID: "auto-probe", PresetID: "balanced", TargetModel: "gpt-4o",
		ParameterOverrides: json.RawMessage(`[]`), ConfigRevision: 7,
	}
	recorded, err := bindings.Record(ctx, 1, key, owner, auto)
	if err != nil || !recorded {
		t.Fatalf("Record() = %t, %v; want true, nil", recorded, err)
	}
	established, err := bindings.client.Redis().PTTL(ctx, responseBindingKey(1, key)).Result()
	if err != nil {
		t.Fatalf("PTTL() error = %v", err)
	}
	if established <= 0 || established > state.DefaultResponseBindingTTL {
		t.Fatalf("PTTL() = %v, want a positive value within the 30-day lifetime", established)
	}

	// Re-recording the same ownership is accepted and is not use: the
	// lifetime runs from when ownership was established.
	time.Sleep(20 * time.Millisecond)
	rotated := owner
	rotated.Version = 99
	repeated, err := bindings.Record(ctx, 1, key, rotated, auto)
	if err != nil || !repeated {
		t.Fatalf("Record() after a token rotation = %t, %v; want true, nil", repeated, err)
	}
	refreshed, err := bindings.client.Redis().PTTL(ctx, responseBindingKey(1, key)).Result()
	if err != nil {
		t.Fatalf("PTTL() error = %v", err)
	}
	if refreshed >= established {
		t.Fatalf("PTTL() = %v after re-recording, want no extension beyond %v", refreshed, established)
	}

	for name, conflicting := range map[string]struct {
		ref  state.CredentialRef
		auto *automodel.Selection
	}{
		"other credential": {ref: state.CredentialRef{ID: 9, GroupID: 2, IdentityGeneration: 4}, auto: auto},
		"other group":      {ref: state.CredentialRef{ID: 3, GroupID: 9, IdentityGeneration: 4}, auto: auto},
		"other identity":   {ref: state.CredentialRef{ID: 3, GroupID: 2, IdentityGeneration: 9}, auto: auto},
		"other preset": {ref: owner, auto: &automodel.Selection{
			EntryID: "auto-probe", PresetID: "strong", TargetModel: "gpt-4.1",
			ParameterOverrides: json.RawMessage(`[]`), ConfigRevision: 7,
		}},
		"no preset": {ref: owner, auto: nil},
	} {
		t.Run(name, func(t *testing.T) {
			recorded, err := bindings.Record(ctx, 1, key, conflicting.ref, conflicting.auto)
			if err != nil || recorded {
				t.Fatalf("Record() = %t, %v; want false, nil", recorded, err)
			}
		})
	}

	binding, found, err := bindings.Lookup(ctx, 1, key)
	if err != nil || !found {
		t.Fatalf("Lookup() = %t, %v; want the established ownership", found, err)
	}
	if binding.CredentialID != owner.ID || binding.GroupID != owner.GroupID ||
		binding.IdentityGeneration != owner.IdentityGeneration || binding.AccessKeyID != 1 ||
		binding.ResponseID != key || binding.AutoSelection == nil ||
		binding.AutoSelection.TargetModel != "gpt-4o" {
		t.Fatalf("Lookup() = %#v, want the ownership a conflict could not overwrite", binding)
	}
	if !binding.ExpiresAt.After(time.Now()) {
		t.Fatalf("ExpiresAt = %v, want a deadline in the future", binding.ExpiresAt)
	}
}

func TestExternalRedisResponseBindingLookupMissesAndFailsClosed(t *testing.T) {
	bindings, key := externalResponseBindings(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	binding, found, err := bindings.Lookup(ctx, 1, key)
	if found || err != nil || binding != (state.ResponseBinding{}) {
		t.Fatalf("Lookup() = %#v, %t, %v; want an ordinary miss", binding, found, err)
	}

	// A payload this build cannot read is a coordination failure, not a miss:
	// reporting it as absent would tell the client its id does not exist.
	if err := bindings.client.Redis().Set(
		ctx, responseBindingKey(1, key), "{not json", time.Minute,
	).Err(); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if _, found, err := bindings.Lookup(ctx, 1, key); found || err == nil {
		t.Fatalf("Lookup() = %t, %v; want an error for an undecodable payload", found, err)
	}
}

// externalResponseBindings opens a live Redis and returns a response id
// namespaced to this run so parallel CI jobs cannot observe each other's
// ownership.
func externalResponseBindings(t *testing.T) (*ResponseBindings, string) {
	t.Helper()
	client := externalLeaseClient(t)
	bindings := NewResponseBindings(client)
	responseID := fmt.Sprintf("resp_test-%d-%s", time.Now().UnixNano(), t.Name())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Redis().Del(ctx, responseBindingKey(1, responseID)).Err(); err != nil {
			t.Errorf("delete test binding key: %v", err)
		}
	})
	return bindings, responseID
}
