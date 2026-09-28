package state

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"gpt-load/internal/automodel"
)

const (
	DefaultResponseBindingTTL      = 30 * 24 * time.Hour
	DefaultResponseBindingCapacity = 100_000
	maxResponseIDBytes             = 4 << 10
	maxResponseBindingIDBytes      = 16 << 20
)

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

type responseBindingKey struct {
	accessKeyID uint
	responseID  string
}

// ResponseBindings 是有界的内存归属索引，不持有 DB、文件或软亲和配置。
type ResponseBindings struct {
	mu       sync.Mutex
	entries  map[responseBindingKey]*list.Element
	order    list.List
	idBytes  int
	capacity int
	ttl      time.Duration
	now      func() time.Time
}

func cloneBinding(value ResponseBinding) ResponseBinding {
	if value.AutoSelection != nil {
		copy := *value.AutoSelection
		copy.ParameterOverrides = append(json.RawMessage(nil), copy.ParameterOverrides...)
		value.AutoSelection = &copy
	}
	return value
}

func bindingBytes(value ResponseBinding) int {
	amount := len(value.ResponseID)
	if value.AutoSelection != nil {
		raw, _ := json.Marshal(value.AutoSelection)
		amount += len(raw)
	}
	return amount
}

func NewResponseBindings() *ResponseBindings {
	return &ResponseBindings{
		entries:  make(map[responseBindingKey]*list.Element),
		capacity: DefaultResponseBindingCapacity,
		ttl:      DefaultResponseBindingTTL,
		now:      time.Now,
	}
}

// Lookup returns the ownership recorded for one response. The in-process
// store never fails; the error exists for shared implementations.
func (bindings *ResponseBindings) Lookup(_ context.Context, accessKeyID uint, responseID string) (ResponseBinding, bool, error) {
	if bindings == nil {
		return ResponseBinding{}, false, nil
	}
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	element := bindings.entries[responseBindingKey{accessKeyID, responseID}]
	if element == nil {
		return ResponseBinding{}, false, nil
	}
	binding := element.Value.(ResponseBinding)
	if !binding.ExpiresAt.After(bindings.now()) {
		bindings.remove(element)
		return ResponseBinding{}, false, nil
	}
	return cloneBinding(binding), true, nil
}

// Record 在响应下发前登记；不同归属冲突时拒绝当前响应，不覆盖已有归属。
func (bindings *ResponseBindings) Record(
	_ context.Context,
	accessKeyID uint,
	responseID string,
	ref CredentialRef,
	autoSelections ...*automodel.Selection,
) (bool, error) {
	if bindings == nil {
		return false, nil
	}
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	now := bindings.now()
	bindings.expire(now)
	binding, valid := NewResponseBinding(accessKeyID, responseID, ref, firstAutoSelection(autoSelections), now, bindings.ttl)
	if !valid {
		return false, nil
	}
	return bindings.insert(binding), nil
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
		ref.IdentityGeneration == 0 || len(responseID) > maxResponseIDBytes {
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

func firstAutoSelection(autoSelections []*automodel.Selection) *automodel.Selection {
	if len(autoSelections) == 0 {
		return nil
	}
	return autoSelections[0]
}

func (bindings *ResponseBindings) insert(binding ResponseBinding) bool {
	key := responseBindingKey{binding.AccessKeyID, binding.ResponseID}
	if element := bindings.entries[key]; element != nil {
		return SameResponseOwner(element.Value.(ResponseBinding), binding)
	}
	if bindings.capacity <= 0 || bindings.ttl <= 0 {
		return false
	}
	amount := bindingBytes(binding)
	if amount > maxResponseBindingIDBytes {
		return false
	}
	for len(bindings.entries) >= bindings.capacity || bindings.idBytes+amount > maxResponseBindingIDBytes {
		bindings.remove(bindings.order.Front())
	}
	bindings.entries[key] = bindings.order.PushBack(cloneBinding(binding))
	bindings.idBytes += amount
	return true
}

func (bindings *ResponseBindings) CaptureCheckpoint() []ResponseBinding {
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	bindings.expire(bindings.now())
	checkpoint := make([]ResponseBinding, 0, len(bindings.entries))
	for element := bindings.order.Front(); element != nil; element = element.Next() {
		checkpoint = append(checkpoint, cloneBinding(element.Value.(ResponseBinding)))
	}
	return checkpoint
}

// RestoreCheckpoint 只恢复归属，不在这里复制路由、权限和健康判断。
func (bindings *ResponseBindings) RestoreCheckpoint(checkpoint []ResponseBinding) error {
	ordered := append([]ResponseBinding(nil), checkpoint...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ExpiresAt.Before(ordered[j].ExpiresAt) })
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	bindings.reset()
	now := bindings.now()
	for _, binding := range ordered {
		if binding.AccessKeyID == 0 || binding.ResponseID == "" || binding.CredentialID == 0 ||
			binding.GroupID == 0 || binding.IdentityGeneration == 0 || !binding.ExpiresAt.After(now) ||
			len(binding.ResponseID) > maxResponseIDBytes {
			continue
		}
		if !bindings.insert(binding) {
			bindings.reset()
			return fmt.Errorf("response checkpoint contains conflicting ownership")
		}
	}
	return nil
}

func (bindings *ResponseBindings) reset() {
	bindings.entries = make(map[responseBindingKey]*list.Element)
	bindings.order.Init()
	bindings.idBytes = 0
}

func (bindings *ResponseBindings) expire(now time.Time) {
	for element := bindings.order.Front(); element != nil; element = bindings.order.Front() {
		if element.Value.(ResponseBinding).ExpiresAt.After(now) {
			return
		}
		bindings.remove(element)
	}
}

func (bindings *ResponseBindings) remove(element *list.Element) {
	binding := element.Value.(ResponseBinding)
	delete(bindings.entries, responseBindingKey{binding.AccessKeyID, binding.ResponseID})
	bindings.idBytes -= bindingBytes(binding)
	bindings.order.Remove(element)
}
