package dialect

import (
	"bytes"

	"github.com/tidwall/gjson"

	"gpt-load/internal/protocol"
)

// FirstOutputObserver 仅供自动选模观察首次交付的生成内容，不参与日志速度或调度决策。
type FirstOutputObserver struct {
	Protocol protocol.Protocol
	ended    bool
}

func (observer *FirstOutputObserver) Observe(event StreamEvent) bool {
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
	observer.ended = observer.ended || produced
	return produced
}

func (observer *FirstOutputObserver) observeResponses(name string, object gjson.Result) bool {
	switch name {
	case "response.completed", "response.done", "response.incomplete":
		// 没有增量输出时，完整快照也可以提供首次生成内容。
		observer.ended = true
		response := outputObject(object.Get("response"))
		if outputString(response.Get("status")) != "failed" {
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

	produced := false
	switch name {
	case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta",
		"response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta",
		"response.code_interpreter_call_code.delta":
		produced = outputTexts(object, "delta")
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
	return produced
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
