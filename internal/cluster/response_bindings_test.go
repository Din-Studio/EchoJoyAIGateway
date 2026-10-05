package cluster

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/automodel"
	"gpt-load/internal/state"
)

type responseBindingStore interface {
	Lookup(ctx context.Context, accessKeyID uint, responseID string) (state.ResponseBinding, bool, error)
	Record(ctx context.Context, accessKeyID uint, responseID string, ref state.CredentialRef, auto ...*automodel.Selection) (bool, error)
}

func TestResponseBindingsMatchInProcessContract(t *testing.T) {
	_, client := newTestClient(t)
	stores := map[string]responseBindingStore{
		"in-process": state.NewResponseBindings(),
		"redis":      NewResponseBindings(client, time.Hour),
	}
	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ref := state.CredentialRef{ID: 1, GroupID: 2, IdentityGeneration: 3, Version: 9}
			selection := &automodel.Selection{
				EntryID: "entry", EntryName: "auto", PresetID: "high", TargetModel: "model",
				ParameterOverrides: json.RawMessage(`[]`), ConfigFingerprint: "entry-fingerprint", TaskFingerprint: "task",
			}
			record := func(accessKeyID uint, responseID string, ref state.CredentialRef, auto ...*automodel.Selection) bool {
				t.Helper()
				recorded, err := store.Record(ctx, accessKeyID, responseID, ref, auto...)
				if err != nil {
					t.Fatalf("Record() error = %v", err)
				}
				return recorded
			}
			lookup := func(accessKeyID uint, responseID string) (state.ResponseBinding, bool) {
				t.Helper()
				binding, found, err := store.Lookup(ctx, accessKeyID, responseID)
				if err != nil {
					t.Fatalf("Lookup() error = %v", err)
				}
				return binding, found
			}

			if !record(1, "opaque/id", ref, selection) {
				t.Fatal("first record rejected")
			}
			selection.TargetModel = "mutated after record"
			binding, found := lookup(1, "opaque/id")
			if !found || binding.AccessKeyID != 1 || binding.ResponseID != "opaque/id" || binding.GroupID != 2 ||
				binding.CredentialID != 1 || binding.IdentityGeneration != 3 || binding.ExpiresAt.IsZero() ||
				binding.AutoSelection == nil || binding.AutoSelection.TargetModel != "model" ||
				binding.AutoSelection.TaskFingerprint != "task" || string(binding.AutoSelection.ParameterOverrides) != "[]" {
				t.Fatalf("Lookup() = %+v, %t", binding, found)
			}
			selection.TargetModel = "model"
			if !record(1, "opaque/id", ref, selection) {
				t.Fatal("same owner rejected on repeat")
			}
			if record(1, "opaque/id", state.CredentialRef{ID: 5, GroupID: 2, IdentityGeneration: 6}, selection) {
				t.Fatal("conflicting credential accepted")
			}
			if record(1, "opaque/id", ref) {
				t.Fatal("conflicting auto selection accepted")
			}
			if binding, _ := lookup(1, "opaque/id"); binding.CredentialID != 1 {
				t.Fatalf("conflict overwrote ownership: %+v", binding)
			}
			if _, found := lookup(2, "opaque/id"); found {
				t.Fatal("another AccessKey resolved the response")
			}
			if !record(2, "opaque/id", state.CredentialRef{ID: 5, GroupID: 2, IdentityGeneration: 6}) {
				t.Fatal("another AccessKey could not own the same response ID")
			}
			for _, invalid := range []struct {
				accessKeyID uint
				responseID  string
				ref         state.CredentialRef
			}{
				{0, "id", ref},
				{1, "", ref},
				{1, "id", state.CredentialRef{GroupID: 2, IdentityGeneration: 3}},
				{1, "id", state.CredentialRef{ID: 1, IdentityGeneration: 3}},
				{1, "id", state.CredentialRef{ID: 1, GroupID: 2}},
				{1, strings.Repeat("x", state.MaxResponseIDBytes+1), ref},
			} {
				if record(invalid.accessKeyID, invalid.responseID, invalid.ref) {
					t.Fatalf("invalid record accepted: %+v", invalid)
				}
			}
			if _, found := lookup(1, "unknown"); found {
				t.Fatal("unknown response resolved")
			}
		})
	}
}

func TestResponseBindingsExpireAfterConfiguredTTL(t *testing.T) {
	server, client := newTestClient(t)
	bindings := NewResponseBindings(client, 2*time.Hour)
	ctx := context.Background()
	ref := state.CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}
	if recorded, err := bindings.Record(ctx, 1, "resp", ref); err != nil || !recorded {
		t.Fatalf("Record() = %t, %v", recorded, err)
	}
	if ttl := server.TTL("gl:rb:1:resp"); ttl != 2*time.Hour {
		t.Fatalf("key TTL = %s, want 2h", ttl)
	}
	server.FastForward(2*time.Hour - time.Second)
	if _, found, err := bindings.Lookup(ctx, 1, "resp"); err != nil || !found {
		t.Fatalf("Lookup() before expiry = %t, %v", found, err)
	}
	server.FastForward(time.Second)
	if _, found, err := bindings.Lookup(ctx, 1, "resp"); err != nil || found {
		t.Fatalf("Lookup() after expiry = %t, %v", found, err)
	}
	if recorded, err := bindings.Record(ctx, 1, "resp", state.CredentialRef{ID: 2, GroupID: 1, IdentityGeneration: 2}); err != nil || !recorded {
		t.Fatalf("Record() after expiry = %t, %v", recorded, err)
	}
}

func TestResponseBindingsIgnoreMalformedValue(t *testing.T) {
	server, client := newTestClient(t)
	bindings := NewResponseBindings(client, time.Hour)
	if err := server.Set("gl:rb:1:resp", "not-json"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := bindings.Lookup(context.Background(), 1, "resp"); err != nil || found {
		t.Fatalf("Lookup() = %t, %v, want miss", found, err)
	}
	ref := state.CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}
	if recorded, err := bindings.Record(context.Background(), 1, "resp", ref); err != nil || recorded {
		t.Fatalf("Record() over malformed value = %t, %v, want rejected", recorded, err)
	}
}

func TestResponseBindingsReturnErrorsWhenRedisIsUnavailable(t *testing.T) {
	server, client := newTestClient(t)
	bindings := NewResponseBindings(client, time.Hour)
	server.Close()
	ctx := context.Background()
	// An ID too long to ever be recorded resolves as a miss without Redis.
	if _, found, err := bindings.Lookup(ctx, 1, strings.Repeat("x", state.MaxResponseIDBytes+1)); err != nil || found {
		t.Fatalf("oversized Lookup() = %t, %v, want miss without a Redis call", found, err)
	}
	if _, _, err := bindings.Lookup(ctx, 1, "resp"); err == nil {
		t.Fatal("Lookup() error = nil with Redis down")
	}
	if _, err := bindings.Record(ctx, 1, "resp", state.CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}); err == nil {
		t.Fatal("Record() error = nil with Redis down")
	}
}
