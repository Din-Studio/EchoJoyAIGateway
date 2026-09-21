package coordination

import (
	"testing"
)

// The window key is a cross-instance contract: two instances running different
// builds must count into the same sorted set, so a later change has to break
// this test rather than silently split one limit into two.
func TestAccessKeyRPMKeyNamesAreStable(t *testing.T) {
	if got := accessKeyRPMKey(42); got != "gw:rpm:{42}" {
		t.Errorf("accessKeyRPMKey(42) = %q, want %q", got, "gw:rpm:{42}")
	}
}

func TestAccessKeyRPMMembersAreUnique(t *testing.T) {
	limiter := NewAccessKeyRPM(&Client{}, "instance-a")
	peer := NewAccessKeyRPM(&Client{}, "instance-b")

	seen := make(map[string]struct{}, 32)
	for range 16 {
		for _, member := range []string{limiter.member(), peer.member()} {
			if _, exists := seen[member]; exists {
				t.Fatalf("member %q repeated; one request would go uncounted", member)
			}
			seen[member] = struct{}{}
		}
	}
}

func TestParseAccessKeyRPMReplyReadsBothVerdicts(t *testing.T) {
	admitted, _, err := parseAccessKeyRPMReply([]any{int64(1), "0"})
	if err != nil || !admitted {
		t.Fatalf("admitted reply = (%v, %v), want (true, nil)", admitted, err)
	}

	// Redis renders a sorted-set score as a double, and a millisecond
	// timestamp exceeds the range it prints in full decimal on every version.
	for _, score := range []string{"1750000000000", "1.75e+12"} {
		admitted, target, err := parseAccessKeyRPMReply([]any{int64(0), score})
		if err != nil {
			t.Fatalf("parseAccessKeyRPMReply(%q) error = %v", score, err)
		}
		if admitted || target != 1_750_000_000_000 {
			t.Fatalf("parseAccessKeyRPMReply(%q) = (%v, %d), want (false, 1750000000000)",
				score, admitted, target)
		}
	}
}

func TestParseAccessKeyRPMReplyRejectsUnexpectedShapes(t *testing.T) {
	for name, reply := range map[string][]any{
		"too short":     {int64(0)},
		"verdict type":  {"1", "0"},
		"score type":    {int64(0), int64(0)},
		"unparsable":    {int64(0), "not-a-score"},
		"empty rejects": {int64(0), ""},
	} {
		if _, _, err := parseAccessKeyRPMReply(reply); err == nil {
			t.Errorf("parseAccessKeyRPMReply(%s) error = nil, want a failure", name)
		}
	}
}
