package coordination

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"gpt-load/internal/automodel"
	"gpt-load/internal/state"
)

// The binding key is a cross-instance contract and an operator's way to look
// up one response id by hand, so a later KeyPrefix change has to break this
// test rather than drift silently.
func TestResponseBindingKeyNamesAreStable(t *testing.T) {
	for name, test := range map[string]struct {
		accessKeyID uint
		responseID  string
		want        string
	}{
		"ordinary": {accessKeyID: 7, responseID: "resp_abc", want: "gw:binding:7:resp_abc"},
		"id with separators": {
			accessKeyID: 1, responseID: "resp:with:colons", want: "gw:binding:1:resp:with:colons",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := responseBindingKey(test.accessKeyID, test.responseID); got != test.want {
				t.Errorf("responseBindingKey() = %q, want %q", got, test.want)
			}
		})
	}
}

// Every rejection the in-memory index makes has to be made here too, and made
// before Redis is involved: a malformed recording is not a coordination
// question. A nil client proves no request was attempted.
func TestResponseBindingRecordRejectsWithoutReachingRedis(t *testing.T) {
	bindings := &ResponseBindings{ttl: state.DefaultResponseBindingTTL}
	valid := state.CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}
	for name, test := range map[string]struct {
		accessKeyID uint
		responseID  string
		ref         state.CredentialRef
	}{
		"no access key":          {accessKeyID: 0, responseID: "resp", ref: valid},
		"no response id":         {accessKeyID: 1, responseID: "", ref: valid},
		"no credential":          {accessKeyID: 1, responseID: "resp", ref: state.CredentialRef{GroupID: 1, IdentityGeneration: 1}},
		"no group":               {accessKeyID: 1, responseID: "resp", ref: state.CredentialRef{ID: 1, IdentityGeneration: 1}},
		"no identity generation": {accessKeyID: 1, responseID: "resp", ref: state.CredentialRef{ID: 1, GroupID: 1}},
		"oversized response id": {
			accessKeyID: 1, responseID: string(make([]byte, state.MaxResponseIDBytes+1)), ref: valid,
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorded, err := bindings.Record(t.Context(), test.accessKeyID, test.responseID, test.ref, nil)
			if recorded || err != nil {
				t.Fatalf("Record() = %t, %v; want false, nil", recorded, err)
			}
		})
	}
}

func TestResponseBindingLookupRejectsWithoutReachingRedis(t *testing.T) {
	bindings := &ResponseBindings{ttl: state.DefaultResponseBindingTTL}
	for name, responseID := range map[string]string{
		"no response id":        "",
		"oversized response id": string(make([]byte, state.MaxResponseIDBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			binding, found, err := bindings.Lookup(t.Context(), 1, responseID)
			if found || err != nil || binding != (state.ResponseBinding{}) {
				t.Fatalf("Lookup() = %#v, %t, %v; want a zero miss", binding, found, err)
			}
		})
	}
}

// Ownership is compared as bytes, so anything that can differ between two
// recordings of the same ownership must not reach the payload.
func TestResponseBindingPayloadIgnoresExpiryVersionAndWhitespace(t *testing.T) {
	selection := func(overrides string) *automodel.Selection {
		return &automodel.Selection{
			EntryID: "auto-probe", EntryName: "auto-probe", PresetID: "balanced",
			PresetName: "balanced", TargetModel: "gpt-4o",
			ParameterOverrides: json.RawMessage(overrides), ConfigRevision: 3,
		}
	}
	binding := func(mutate func(*state.ResponseBinding)) []byte {
		t.Helper()
		value := state.ResponseBinding{
			AutoSelection: selection(`[{"set":{"temperature":0}}]`),
			AccessKeyID:   1, ResponseID: "resp_abc",
			GroupID: 2, CredentialID: 3, IdentityGeneration: 4,
		}
		mutate(&value)
		payload, err := encodeResponseBinding(value)
		if err != nil {
			t.Fatalf("encodeResponseBinding() error = %v", err)
		}
		return payload
	}

	base := binding(func(*state.ResponseBinding) {})
	for name, mutate := range map[string]func(*state.ResponseBinding){
		"repeated encoding": func(*state.ResponseBinding) {},
		"different expiry":  func(value *state.ResponseBinding) { value.ExpiresAt = time.Now() },
		"override whitespace": func(value *state.ResponseBinding) {
			value.AutoSelection = selection("[ { \"set\" : { \"temperature\" : 0 } } ]")
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := binding(mutate); !bytes.Equal(got, base) {
				t.Fatalf("payload = %s, want the unchanged %s", got, base)
			}
		})
	}

	for name, mutate := range map[string]func(*state.ResponseBinding){
		"different credential": func(value *state.ResponseBinding) { value.CredentialID = 9 },
		"different group":      func(value *state.ResponseBinding) { value.GroupID = 9 },
		"different identity":   func(value *state.ResponseBinding) { value.IdentityGeneration = 9 },
		"different preset": func(value *state.ResponseBinding) {
			value.AutoSelection = selection(`[{"set":{"temperature":1}}]`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := binding(mutate); bytes.Equal(got, base) {
				t.Fatalf("payload = %s, want a different ownership to encode differently", got)
			}
		})
	}
}

// The lifetime is the in-memory index's constant, not a second knob.
func TestResponseBindingUsesTheSharedTTL(t *testing.T) {
	if ttl := NewResponseBindings(nil).ttl; ttl != state.DefaultResponseBindingTTL {
		t.Fatalf("ttl = %v, want the in-memory index's %v", ttl, state.DefaultResponseBindingTTL)
	}
}
