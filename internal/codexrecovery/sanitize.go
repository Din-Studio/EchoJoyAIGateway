package codexrecovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// ErrBodyTooLarge means the recovered JSON would exceed the transport limit.
var ErrBodyTooLarge = errors.New("recovered request body exceeds size limit")

// Removals counts the upstream-bound state stripped from a request body.
// The counts feed audit metadata; they are never sent upstream.
type Removals struct {
	ReasoningEncryptedContent      int
	FunctionOutputEncryptedContent int
	AgentMessageEncryptedContent   int
	ReasoningItem                  int
	AgentMessageItem               int
	HistoryItemID                  int
}

type sanitizeOptions struct {
	removeHistoryIDs     bool
	rejectItemReferences bool
	shouldStrip          func(string) bool
}

// Empty reports whether nothing was removed.
func (r Removals) Empty() bool {
	return r.ReasoningEncryptedContent == 0 &&
		r.FunctionOutputEncryptedContent == 0 &&
		r.AgentMessageEncryptedContent == 0 &&
		r.HistoryItemID == 0
}

// Total returns the number of removed encrypted entries and history IDs.
func (r Removals) Total() int {
	return r.ReasoningEncryptedContent +
		r.FunctionOutputEncryptedContent +
		r.AgentMessageEncryptedContent + r.HistoryItemID
}

// Sanitize rebuilds self-contained inline history after an upstream rejects
// continuation state. It removes replayable items' upstream IDs and opaque
// reasoning, tool output, and agent message encrypted_content.
// Compaction items and server-side references hold history that cannot be
// rebuilt here, so they are never removed.
//
// Message contents, tool call_id/name/arguments/output, and other resource
// identifiers are preserved. The caller keeps the original body when there
// is nothing to remove or the history requires server-side item references.
func Sanitize(body []byte, limit int64) ([]byte, Removals, bool, error) {
	return sanitizeBody(body, limit, sanitizeOptions{
		removeHistoryIDs:     true,
		rejectItemReferences: true,
		shouldStrip:          func(string) bool { return true },
	})
}

// StripEncryptedContentDigests removes only encrypted_content values whose
// digests were explicitly rejected for this session. Item IDs and valid
// account-bound state are preserved so a later turn does not need to repeat
// the upstream rejection before it can continue.
func StripEncryptedContentDigests(body []byte, digests map[string]struct{}, limit int64) ([]byte, Removals, bool, error) {
	if len(digests) == 0 {
		return nil, Removals{}, false, nil
	}
	return sanitizeBody(body, limit, sanitizeOptions{
		shouldStrip: func(value string) bool {
			_, rejected := digests[EncryptedContentDigest(value)]
			return rejected
		},
	})
}

// StripHistoryItemIDs removes IDs that were rejected as belonging to another
// upstream resource. Replayable items keep their readable content; a bare
// item_reference is kept because its history cannot be replayed without it.
func StripHistoryItemIDs(body []byte, itemIDs map[string]struct{}, limit int64) ([]byte, Removals, bool, error) {
	if len(itemIDs) == 0 {
		return nil, Removals{}, false, nil
	}
	object, ok := decodeJSONObject(body)
	if !ok {
		return nil, Removals{}, false, nil
	}
	inputItems, originalIsArray := responseInputItems(object["input"])
	if inputItems == nil {
		return nil, Removals{}, false, nil
	}

	var removals Removals
	kept := make([]any, 0, len(inputItems))
	for _, item := range inputItems {
		record, ok := item.(map[string]any)
		if !ok {
			kept = append(kept, item)
			continue
		}
		id, _ := record["id"].(string)
		if _, rejected := itemIDs[strings.TrimSpace(id)]; !rejected {
			kept = append(kept, item)
			continue
		}
		if !replayableHistoryItem(record) {
			kept = append(kept, item)
			continue
		}
		delete(record, "id")
		removals.HistoryItemID++
		kept = append(kept, record)
	}
	if removals.Empty() {
		return nil, Removals{}, false, nil
	}
	if originalIsArray {
		object["input"] = kept
	} else if len(kept) > 0 {
		object["input"] = kept[0]
	} else {
		object["input"] = []any{}
	}
	if encodedSize(object, limit) > limit {
		return nil, Removals{}, false, ErrBodyTooLarge
	}
	encoded, err := json.Marshal(object)
	return encoded, removals, err == nil, err
}

func sanitizeBody(body []byte, limit int64, options sanitizeOptions) ([]byte, Removals, bool, error) {
	object, ok := decodeJSONObject(body)
	if !ok {
		return nil, Removals{}, false, nil
	}
	inputItems, originalIsArray := responseInputItems(object["input"])
	if inputItems == nil {
		return nil, Removals{}, false, nil
	}
	if options.shouldStrip == nil {
		options.shouldStrip = func(string) bool { return true }
	}
	if options.rejectItemReferences {
		for _, item := range inputItems {
			if record, ok := item.(map[string]any); ok && record["type"] == "item_reference" {
				return nil, Removals{}, false, nil
			}
		}
	}

	var removals Removals
	kept := make([]any, 0, len(inputItems))
	for _, item := range inputItems {
		record, isObject := item.(map[string]any)
		if !isObject {
			kept = append(kept, item)
			continue
		}

		itemType, _ := record["type"].(string)
		if _, hasID := record["id"]; hasID && options.removeHistoryIDs && replayableHistoryItem(record) {
			delete(record, "id")
			removals.HistoryItemID++
		}
		if itemType == "reasoning" {
			if encrypted, isString := record["encrypted_content"].(string); isString && options.shouldStrip(encrypted) {
				delete(record, "encrypted_content")
				removals.ReasoningEncryptedContent++
				if isEmptyReasoningItem(record) {
					removals.ReasoningItem++
					continue
				}
				kept = append(kept, record)
				continue
			}
		}

		if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
			stripped := stripEncryptedContentItems(record["output"], options.shouldStrip)
			if stripped.changed {
				record["output"] = stripped.output
				removals.FunctionOutputEncryptedContent += stripped.removedCount
				kept = append(kept, record)
				continue
			}
		}

		if itemType == "agent_message" {
			stripped := stripEncryptedContentItems(record["content"], options.shouldStrip)
			if stripped.changed {
				removals.AgentMessageEncryptedContent += stripped.removedCount
				if contentArray, isArray := stripped.output.([]any); isArray && len(contentArray) == 0 {
					removals.AgentMessageItem++
					continue
				}
				record["content"] = stripped.output
				kept = append(kept, record)
				continue
			}
		}
		kept = append(kept, record)
	}

	if removals.Empty() {
		return nil, Removals{}, false, nil
	}
	if originalIsArray {
		object["input"] = kept
	} else if len(kept) > 0 {
		object["input"] = kept[0]
	} else {
		object["input"] = []any{}
	}
	if encodedSize(object, limit) > limit {
		return nil, Removals{}, false, ErrBodyTooLarge
	}
	encoded, err := json.Marshal(object)
	return encoded, removals, err == nil, err
}

// responseInputItems normalizes the Responses `input` field to a slice. It
// mirrors `Array.isArray(body.input) ? body.input : [body.input]`, and reports
// whether the caller supplied an array so the shape can be restored.
func responseInputItems(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case map[string]any:
		return []any{typed}, false
	default:
		return nil, false
	}
}

// Only known, self-contained input items can be replayed without their
// upstream identity. Unknown extensions and resource-only items keep IDs.
func replayableHistoryItem(item map[string]any) bool {
	hasString := func(key string) bool {
		_, ok := item[key].(string)
		return ok
	}
	hasContent := func(key string) bool {
		switch value := item[key].(type) {
		case string:
			return value != ""
		case []any:
			return len(value) > 0
		}
		return false
	}
	switch item["type"] {
	case "message":
		return hasString("role") && hasContent("content")
	case "reasoning":
		return hasContent("summary") || hasContent("content") || hasContent("encrypted_content")
	case "function_call":
		return hasString("call_id") && hasString("name") && hasString("arguments")
	case "custom_tool_call":
		return hasString("call_id") && hasString("name") && hasString("input")
	case "function_call_output", "custom_tool_call_output":
		_, hasOutput := item["output"]
		return hasString("call_id") && hasOutput
	case "agent_message":
		return hasString("author") && hasString("recipient") && hasContent("content")
	}
	return false
}

type strippedItems struct {
	output       any
	changed      bool
	removedCount int
}

func stripEncryptedContentItems(value any, shouldStrip func(string) bool) strippedItems {
	switch typed := value.(type) {
	case []any:
		removedCount := 0
		output := make([]any, 0, len(typed))
		for _, item := range typed {
			if encryptedContentItemMatches(item, shouldStrip) {
				removedCount++
				continue
			}
			output = append(output, item)
		}
		return strippedItems{output: output, changed: removedCount > 0, removedCount: removedCount}
	case map[string]any:
		if encryptedContentItemMatches(typed, shouldStrip) {
			return strippedItems{output: []any{}, changed: true, removedCount: 1}
		}
	}
	return strippedItems{output: value, changed: false}
}

func encryptedContentItemMatches(value any, shouldStrip func(string) bool) bool {
	record, isObject := value.(map[string]any)
	if !isObject {
		return false
	}
	itemType, _ := record["type"].(string)
	encrypted, encryptedIsString := record["encrypted_content"].(string)
	return itemType == "encrypted_content" && encryptedIsString && shouldStrip(encrypted)
}

// isEmptyReasoningItem reports a reasoning item that carries nothing but
// framing fields once its encrypted content is removed.
func isEmptyReasoningItem(item map[string]any) bool {
	for key, value := range item {
		switch key {
		case "type", "id", "status":
			continue
		}
		switch typed := value.(type) {
		case nil:
			continue
		case []any:
			if len(typed) == 0 {
				continue
			}
		case string:
			if strings.TrimSpace(typed) == "" {
				continue
			}
		}
		return false
	}
	return true
}

func decodeJSONObject(body []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	object, ok := parsed.(map[string]any)
	return object, ok
}

// encodedSize measures encoding/json's output without allocating its escaped
// strings or complete output buffer. Decoding supplies only these JSON types.
func encodedSize(value any, limit int64) int64 {
	var size int64
	switch value := value.(type) {
	case nil:
		return 4
	case bool:
		if value {
			return 4
		}
		return 5
	case json.Number:
		return int64(len(value))
	case string:
		size = 2
		for len(value) > 0 && size <= limit {
			r, n := utf8.DecodeRuneInString(value)
			value = value[n:]
			switch {
			case r == utf8.RuneError && n == 1:
				size += 3
			case r == '<', r == '>', r == '&', r == '\u2028', r == '\u2029':
				size += 6
			case r == '\\', r == '"', r == '\b', r == '\f', r == '\n', r == '\r', r == '\t':
				size += 2
			case r < 0x20:
				size += 6
			default:
				size += int64(n)
			}
		}
	case []any:
		size = 2
		for i, child := range value {
			if i > 0 {
				size++
			}
			size += encodedSize(child, limit-size)
			if size > limit {
				break
			}
		}
	case map[string]any:
		size = 2
		first := true
		for key, child := range value {
			if !first {
				size++
			}
			first = false
			size += encodedSize(key, limit-size) + 1
			if size > limit {
				break
			}
			size += encodedSize(child, limit-size)
			if size > limit {
				break
			}
		}
	}
	return size
}
