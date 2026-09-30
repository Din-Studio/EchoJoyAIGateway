package state

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpt-load/internal/automodel"
)

func TestResponseBindingsPreserveIdentityAndOriginalExpiry(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	bindings := NewResponseBindings()
	bindings.now = func() time.Time { return now }
	bindings.ttl = time.Hour
	ref := CredentialRef{ID: 1, GroupID: 2, IdentityGeneration: 3, Version: 1}
	if !recordBinding(t, bindings, 1, "opaque/id", ref) {
		t.Fatal("record failed")
	}
	now = now.Add(30 * time.Minute)
	ref.Version++
	if !recordBinding(t, bindings, 1, "opaque/id", ref) {
		t.Fatal("normal token refresh changed ownership")
	}
	ref.IdentityGeneration++
	if recordBinding(t, bindings, 1, "opaque/id", ref) {
		t.Fatal("another identity replaced existing ownership")
	}
	if saved, ok := lookupBinding(t, bindings, 1, "opaque/id"); !ok || saved.IdentityGeneration != 3 {
		t.Fatal("ownership changed after conflict")
	}
	if _, ok := lookupBinding(t, bindings, 2, "opaque/id"); ok {
		t.Fatal("binding crossed AccessKeys")
	}
	now = now.Add(30 * time.Minute)
	if _, ok := lookupBinding(t, bindings, 1, "opaque/id"); ok {
		t.Fatal("duplicate observation extended the original expiry")
	}
}

func TestResponseBindingsEvictOldestAndBoundIDMemory(t *testing.T) {
	bindings := NewResponseBindings()
	bindings.capacity = 2
	ref := CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}
	for _, id := range []string{"old", "new", "newest"} {
		if !recordBinding(t, bindings, 1, id, ref) {
			t.Fatal("record failed")
		}
	}
	if _, ok := lookupBinding(t, bindings, 1, "old"); ok {
		t.Fatal("capacity retained oldest entry")
	}
	for _, id := range []string{"new", "newest"} {
		if _, ok := lookupBinding(t, bindings, 1, id); !ok {
			t.Fatalf("recent binding %q was lost", id)
		}
	}
	bindings = NewResponseBindings()
	prefix := strings.Repeat("x", 4<<10)
	var firstID, lastID string
	for index := range maxResponseBindingIDBytes/(4<<10) + 1 {
		suffix := strconv.Itoa(index)
		lastID = prefix[len(suffix):] + suffix
		if index == 0 {
			firstID = lastID
		}
		if !recordBinding(t, bindings, 1, lastID, ref) {
			t.Fatal("valid ID was rejected before eviction")
		}
	}
	if _, ok := lookupBinding(t, bindings, 1, firstID); ok {
		t.Fatal("ID byte budget retained the oldest entry")
	}
	if _, ok := lookupBinding(t, bindings, 1, lastID); !ok {
		t.Fatal("ID byte budget lost the newest entry")
	}
}

func TestResponseBindingsRejectOversizedIDWithoutEvictingOtherAccessKeys(t *testing.T) {
	bindings := NewResponseBindings()
	ref := CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}
	maxID := strings.Repeat("x", 4<<10)
	if !recordBinding(t, bindings, 1, maxID, ref) {
		t.Fatal("ID at the single-record limit was rejected")
	}
	for _, size := range []int{len(maxID) + 1, 16 << 20} {
		if recordBinding(t, bindings, 2, strings.Repeat("y", size), ref) {
			t.Errorf("accepted oversized ID of %d bytes", size)
		}
		if _, ok := lookupBinding(t, bindings, 1, maxID); !ok {
			t.Error("oversized ID evicted another AccessKey's binding")
		}
	}
}

func TestResponseBindingsCheckpointSkipsOversizedIDs(t *testing.T) {
	bindings := NewResponseBindings()
	valid := ResponseBinding{AccessKeyID: 1, ResponseID: strings.Repeat("x", 4<<10),
		CredentialID: 1, GroupID: 1, IdentityGeneration: 1, ExpiresAt: time.Now().Add(time.Hour)}
	oversized := valid
	oversized.AccessKeyID = 2
	oversized.ResponseID += "x"
	if err := bindings.RestoreCheckpoint([]ResponseBinding{valid, oversized}); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupBinding(t, bindings, 1, valid.ResponseID); !ok {
		t.Fatal("restore discarded a valid binding")
	}
	if _, ok := lookupBinding(t, bindings, 2, oversized.ResponseID); ok {
		t.Fatal("restore accepted an oversized ID")
	}
}

func TestResponseBindingsConcurrentRegistrationIsConsistent(t *testing.T) {
	bindings := NewResponseBindings()
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for range 100 {
				if !recordBinding(t, bindings, 1, "same-response", CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}) {
					t.Error("idempotent concurrent record failed")
				}
				if _, ok := lookupBinding(t, bindings, 1, "same-response"); !ok {
					t.Error("registered response was lost")
				}
			}
		})
	}
	workers.Wait()
}

func TestResponseBindingsCheckpointKeepsExpiryAndCapacity(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	original := NewResponseBindings()
	original.now = func() time.Time { return now }
	original.ttl = time.Hour
	ref := CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}
	recordBinding(t, original, 1, "expired", ref)
	now = now.Add(30 * time.Minute)
	recordBinding(t, original, 1, "retained", ref)
	checkpoint := original.CaptureCheckpoint()
	now = now.Add(30 * time.Minute)
	restored := NewResponseBindings()
	restored.capacity = 1
	restored.now = func() time.Time { return now }
	if err := restored.RestoreCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupBinding(t, restored, 1, "expired"); ok {
		t.Fatal("expired checkpoint binding was restored")
	}
	if binding, ok := lookupBinding(t, restored, 1, "retained"); !ok || !binding.ExpiresAt.Equal(checkpoint[1].ExpiresAt) {
		t.Fatal("restore lost or extended retained binding")
	}
	now = now.Add(30 * time.Minute)
	if _, ok := lookupBinding(t, restored, 1, "retained"); ok {
		t.Fatal("restart renewed response TTL")
	}
}

func TestResponseBindingsRejectConflictingCheckpoint(t *testing.T) {
	bindings := NewResponseBindings()
	first := ResponseBinding{AccessKeyID: 1, ResponseID: "same", CredentialID: 1, GroupID: 1,
		IdentityGeneration: 1, ExpiresAt: time.Now().Add(time.Hour)}
	conflict := first
	conflict.CredentialID = 2
	if err := bindings.RestoreCheckpoint([]ResponseBinding{first, conflict}); err == nil {
		t.Fatal("ambiguous checkpoint was accepted")
	}
	if _, ok := lookupBinding(t, bindings, 1, "same"); ok {
		t.Fatal("conflicting checkpoint left routable ownership")
	}
}

func recordBinding(
	t *testing.T,
	bindings *ResponseBindings,
	accessKeyID uint,
	responseID string,
	ref CredentialRef,
	autoSelections ...*automodel.Selection,
) bool {
	t.Helper()
	recorded, err := bindings.Record(context.Background(), accessKeyID, responseID, ref, autoSelections...)
	if err != nil {
		t.Errorf("Record() error = %v", err)
	}
	return recorded
}

func lookupBinding(t *testing.T, bindings *ResponseBindings, accessKeyID uint, responseID string) (ResponseBinding, bool) {
	t.Helper()
	binding, found, err := bindings.Lookup(context.Background(), accessKeyID, responseID)
	if err != nil {
		t.Errorf("Lookup() error = %v", err)
	}
	return binding, found
}
