package coordination

import (
	"crypto/rand"
	"strings"
	"testing"
)

// Lease key names are a cross-instance contract: two instances running
// different builds must agree on them, so a later KeyPrefix change has to
// break this test rather than drift silently.
func TestLeaseKeyNamesAreStable(t *testing.T) {
	for task, want := range map[string]string{
		"requestlog_retention":     "gw:lease:requestlog_retention",
		"credential_stage_cleanup": "gw:lease:credential_stage_cleanup",
		"operation_compaction":     "gw:lease:operation_compaction",
	} {
		if got := leaseKey(task); got != want {
			t.Errorf("leaseKey(%q) = %q, want %q", task, got, want)
		}
	}
}

func TestLeaseHolderIsRandomHex(t *testing.T) {
	first, err := newLeaseHolder(rand.Reader)
	if err != nil {
		t.Fatalf("newLeaseHolder() error = %v", err)
	}
	if len(first) != 32 || strings.Trim(first, "0123456789abcdef") != "" {
		t.Fatalf("holder = %q, want 32 lowercase hex characters", first)
	}
	second, err := newLeaseHolder(rand.Reader)
	if err != nil {
		t.Fatalf("newLeaseHolder() error = %v", err)
	}
	if first == second {
		t.Fatal("two lease holders share an identity; instances would be indistinguishable")
	}
}

func TestLeaseHolderFailsWithoutRandomness(t *testing.T) {
	if _, err := newLeaseHolder(strings.NewReader("too short")); err == nil {
		t.Fatal("newLeaseHolder() error = nil, want a failure for an exhausted random source")
	}
}
