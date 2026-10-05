package state

import (
	"bytes"
	"encoding/json"
	"time"

	"gpt-load/internal/automodel"
)

// MaxResponseIDBytes bounds a response ID that can own a binding.
const MaxResponseIDBytes = 4 << 10

// ResponseBinding 只保存响应归属；可用性仍由当前路由和凭据运行态决定。
type ResponseBinding struct {
	AutoSelection      *automodel.Selection `json:"auto_selection,omitempty"`
	AccessKeyID        uint                 `json:"access_key_id"`
	ResponseID         string               `json:"response_id"`
	GroupID            uint                 `json:"group_id"`
	CredentialID       uint                 `json:"credential_id"`
	IdentityGeneration uint64               `json:"identity_generation"`
	ExpiresAt          time.Time            `json:"expires_at"`
}

// NewResponseBinding validates and builds one ownership record that expires
// ttl after now. The auto selection is deep-copied so later caller mutations
// cannot change recorded ownership.
func NewResponseBinding(
	accessKeyID uint,
	responseID string,
	ref CredentialRef,
	autoSelection *automodel.Selection,
	now time.Time,
	ttl time.Duration,
) (ResponseBinding, bool) {
	if accessKeyID == 0 || responseID == "" || ref.ID == 0 || ref.GroupID == 0 ||
		ref.IdentityGeneration == 0 || len(responseID) > MaxResponseIDBytes {
		return ResponseBinding{}, false
	}
	var auto *automodel.Selection
	if autoSelection != nil {
		raw, _ := json.Marshal(autoSelection)
		auto = new(automodel.Selection)
		_ = json.Unmarshal(raw, auto)
	}
	return ResponseBinding{
		AutoSelection: auto,
		AccessKeyID:   accessKeyID, ResponseID: responseID,
		GroupID: ref.GroupID, CredentialID: ref.ID, IdentityGeneration: ref.IdentityGeneration,
		ExpiresAt: now.Add(ttl),
	}, true
}

// SameResponseOwner reports whether two records assign a response to the
// same credential identity and automatic model selection.
func SameResponseOwner(existing, incoming ResponseBinding) bool {
	existingAuto, _ := json.Marshal(existing.AutoSelection)
	incomingAuto, _ := json.Marshal(incoming.AutoSelection)
	return existing.GroupID == incoming.GroupID && existing.CredentialID == incoming.CredentialID &&
		existing.IdentityGeneration == incoming.IdentityGeneration && bytes.Equal(existingAuto, incomingAuto)
}
