package dialect

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"gpt-load/internal/protocol"
)

const (
	maxOutputTimingKeys          = 4096
	maxOutputTimingKeyBytes      = 4096
	maxOutputTimingRetainedBytes = 256 << 10
)

// OutputTimingObserver 仅观察已交付的生成内容，不参与转发或重试决策。
type OutputTimingObserver struct {
	Protocol   protocol.Protocol
	seen       bool
	ended      bool
	overflowed bool
	keyBytes   int
	items      map[outputTimingKey]bool
}

type outputTimingKey struct {
	item  string
	part  string
	index string
}

func (observer *OutputTimingObserver) Observe(event StreamEvent) bool {
	if observer.ended {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(event.Payload), []byte("[DONE]")) {
		observer.ended = true
		return false
	}
	if !gjson.ValidBytes(event.Payload) {
		return false
	}
	object := gjson.ParseBytes(event.Payload)
	if !object.IsObject() {
		return false
	}
	name := event.Name
	if name == "" {
		name = outputString(object.Get("type"))
	}
	if name == "error" || producedContentValue([]byte(object.Get("error").Raw)) {
		observer.ended = true
		return false
	}
	produced := false
	switch observer.Protocol {
	case protocol.OpenAICompletions:
		for _, choice := range outputObjects(object.Get("choices")) {
			delta := outputObject(choice.Get("delta"))
			produced = produced || outputTexts(delta, "content", "reasoning_content", "reasoning", "refusal") ||
				outputTexts(outputObject(delta.Get("function_call")), "arguments")
			for _, call := range outputObjects(delta.Get("tool_calls")) {
				produced = produced || outputTexts(outputObject(call.Get("function")), "arguments")
			}
			for _, detail := range outputObjects(delta.Get("reasoning_details")) {
				produced = produced || outputTexts(detail, "text", "summary")
			}
		}
	case protocol.Anthropic:
		switch name {
		case "content_block_delta":
			produced = outputTexts(outputObject(object.Get("delta")), "text", "thinking", "partial_json")
		case "content_block_start":
			block := outputObject(object.Get("content_block"))
			produced = outputTexts(block, "text", "thinking")
			outputObject(block.Get("input")).ForEach(func(_, _ gjson.Result) bool {
				produced = true
				return false
			})
		case "message_stop":
			observer.ended = true
		}
	case protocol.Gemini:
		for _, candidate := range outputObjects(object.Get("candidates")) {
			for _, part := range outputObjects(outputObject(candidate.Get("content")).Get("parts")) {
				produced = produced || outputTexts(part, "text") ||
					outputObject(part.Get("functionCall")).Get("args").IsObject() ||
					outputTexts(outputObject(part.Get("executableCode")), "code")
			}
		}
	case protocol.OpenAIResponses:
		produced = observer.observeResponses(name, object)
	}
	observer.seen = observer.seen || produced
	return produced
}

// Overflowed 表示观测状态超限，调用方必须丢弃此前采样的时点。
func (observer *OutputTimingObserver) Overflowed() bool {
	return observer.overflowed
}

func (observer *OutputTimingObserver) observeResponses(name string, object gjson.Result) bool {
	switch name {
	case "response.completed", "response.done", "response.incomplete":
		// 完整快照仅在没有增量输出时提供首响；不能把重复快照当成末 token。
		observer.ended = true
		response := outputObject(object.Get("response"))
		if !observer.seen && outputString(response.Get("status")) != "failed" {
			for _, item := range outputObjects(response.Get("output")) {
				if timingOutputItem(item) {
					return true
				}
			}
		}
		return false
	case "response.failed":
		observer.ended = true
		return false
	}

	key := outputTimingKey{item: object.Get("output_index").Raw}
	if key.item == "" {
		key.item = outputString(object.Get("item_id"))
		if key.item == "" {
			key.item = outputString(outputObject(object.Get("item")).Get("id"))
		}
	}
	switch {
	case strings.HasPrefix(name, "response.reasoning_summary_"):
		key.part, key.index = "summary", object.Get("summary_index").Raw
	case strings.HasPrefix(name, "response.output_text."), strings.HasPrefix(name, "response.reasoning_text."),
		strings.HasPrefix(name, "response.refusal."), strings.HasPrefix(name, "response.content_part."):
		key.part, key.index = "content", object.Get("content_index").Raw
	}
	if key.part != "" && key.index == "" {
		key.index = "0"
	}

	produced, delta := false, false
	switch name {
	case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta",
		"response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta",
		"response.code_interpreter_call_code.delta":
		produced, delta = outputTexts(object, "delta"), true
	case "response.output_item.added", "response.output_item.done":
		produced = timingOutputItem(outputObject(object.Get("item")))
	case "response.content_part.added", "response.content_part.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		produced = outputTexts(outputObject(object.Get("part")), "text", "refusal")
	case "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done":
		produced = outputTexts(object, "text")
	case "response.refusal.done":
		produced = outputTexts(object, "refusal")
	case "response.function_call_arguments.done":
		produced = outputTexts(object, "arguments")
	case "response.custom_tool_call_input.done":
		produced = outputTexts(object, "input")
	}
	if !produced || (!delta && observer.items[key]) {
		return false
	}
	// 片段独立去重，条目标记只用于排除其外层重复快照。
	if !observer.rememberOutput(key) || !observer.rememberOutput(outputTimingKey{item: key.item}) {
		return false
	}
	if name == "response.output_item.added" || name == "response.output_item.done" {
		item := outputObject(object.Get("item"))
		for _, field := range []string{"content", "summary"} {
			for index, part := range outputObjects(item.Get(field)) {
				if outputTexts(part, "text", "refusal") &&
					!observer.rememberOutput(outputTimingKey{item: key.item, part: field, index: strconv.Itoa(index)}) {
					return false
				}
			}
		}
	}
	return true
}

func (observer *OutputTimingObserver) rememberOutput(key outputTimingKey) bool {
	if observer.items[key] {
		return true
	}
	size := len(key.item) + len(key.part) + len(key.index)
	if size > maxOutputTimingKeyBytes || len(observer.items) >= maxOutputTimingKeys ||
		observer.keyBytes+size > maxOutputTimingRetainedBytes {
		observer.items = nil
		observer.keyBytes = 0
		observer.overflowed, observer.ended = true, true
		return false
	}
	if observer.items == nil {
		observer.items = make(map[outputTimingKey]bool)
	}
	// gjson strings can reference the whole event. Retain only bounded key bytes.
	key.item, key.index = strings.Clone(key.item), strings.Clone(key.index)
	observer.items[key] = true
	observer.keyBytes += size
	return true
}

// Only selected fields are read; no per-event JSON object decoding.
func outputObject(value gjson.Result) gjson.Result {
	if !value.IsObject() {
		return gjson.Result{}
	}
	return value
}

func outputObjects(value gjson.Result) []gjson.Result {
	if !value.IsArray() {
		return nil
	}
	objects := value.Array()
	for _, object := range objects {
		if !object.IsObject() && object.Type != gjson.Null {
			return nil
		}
	}
	return objects
}

func outputString(value gjson.Result) string {
	if value.Type != gjson.String {
		return ""
	}
	return value.Str
}

func outputTexts(object gjson.Result, fields ...string) bool {
	for _, field := range fields {
		if outputString(object.Get(field)) != "" {
			return true
		}
	}
	return false
}

func timingOutputItem(item gjson.Result) bool {
	if outputTexts(item, "arguments", "input") {
		return true
	}
	for _, field := range []string{"content", "summary"} {
		for _, part := range outputObjects(item.Get(field)) {
			if outputTexts(part, "text", "refusal") {
				return true
			}
		}
	}
	return false
}
