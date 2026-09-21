package coordination

import (
	"testing"
)

// The store, its change index, its sequence and its doorbell are a
// cross-instance contract: two instances running different builds must name
// all four the same, so a later change has to break this test rather than
// leave one instance publishing where no one reads. A rename of one name
// alone is the worst case, because the read script skips an index entry whose
// payload it cannot find and reports no error.
func TestCredentialHealthKeyNamesAreStable(t *testing.T) {
	health := NewCredentialHealth(&Client{})
	for name, pair := range map[string][2]string{
		"store":    {health.storeKey, "gw:cred:{health}:store"},
		"index":    {health.indexKey, "gw:cred:{health}:index"},
		"sequence": {health.sequence, "gw:cred:{health}:sequence"},
		"channel":  {health.channel, "gw:cred"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s key = %q, want %q", name, pair[0], pair[1])
		}
	}
}

// The reply leads with the sequence the read resumed from. Carrying it back is
// what lets a restarted counter reconverge, so the parser must take it rather
// than the cursor it was called with.
func TestParseCredentialHealthReplyResumesFromTheScriptsFloor(t *testing.T) {
	payload := `{"group_id":9,"identity_generation":1,"blacklisted":true}`

	t.Run("normal read keeps the cursor moving forward", func(t *testing.T) {
		changes, resume, err := parseCredentialHealthReply(
			[]any{"7", "1", "12", payload}, 7,
		)
		if err != nil {
			t.Fatalf("parseCredentialHealthReply() error = %v", err)
		}
		if len(changes) != 1 || changes[0].CredentialID != 1 || !changes[0].Blacklisted {
			t.Fatalf("changes = %#v, want one blacklisted credential 1", changes)
		}
		if resume != 12 {
			t.Fatalf("resume = %d, want 12", resume)
		}
	})

	t.Run("restarted counter resumes from zero", func(t *testing.T) {
		// A store that lost its data issues sequence 1 again. Returning the
		// caller's 5000 here is what used to make the instance deaf for the
		// rest of its life.
		changes, resume, err := parseCredentialHealthReply([]any{"0"}, 5000)
		if err != nil {
			t.Fatalf("parseCredentialHealthReply() error = %v", err)
		}
		if len(changes) != 0 {
			t.Fatalf("changes = %#v, want none", changes)
		}
		if resume != 0 {
			t.Fatalf("resume = %d, want 0 so the next read re-hydrates", resume)
		}
	})

	t.Run("a reply without the floor is rejected", func(t *testing.T) {
		for name, reply := range map[string][]any{
			"empty":            {},
			"missing floor":    {"1", "12", payload},
			"truncated triple": {"0", "1", "12"},
		} {
			if _, _, err := parseCredentialHealthReply(reply, 7); err == nil {
				t.Errorf("parseCredentialHealthReply(%s) error = nil, want a shape error", name)
			}
		}
	})
}
