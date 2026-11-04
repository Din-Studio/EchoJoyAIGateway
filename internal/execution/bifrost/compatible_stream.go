package bifrost

import (
	"bytes"
	"net/http"
	"sort"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// #665：从完整 Chat chunk 转换，保留原 Responses 执行器的生命周期和客户端编码。
// SDK 修复后应连同专用入口删除；终态/用量/晚到错误的回归继续保留。
func (r *Runtime) compatibleResponsesStream(ctx *schemas.BifrostContext, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	chat, err := prepareCompatibleChatRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	// Chat SDK 会合成零用量尾块；原始事件用于区分“未返回用量”和“明确返回零”。
	ctx.SetValue(schemas.BifrostContextKeyAllowPerRequestRawOverride, true)
	ctx.SetValue(schemas.BifrostContextKeySendBackRawResponse, true)
	upstream, err := r.core.ChatCompletionStreamRequest(ctx, chat)
	if err != nil {
		err.ExtraFields.RawResponse = nil
		return nil, err
	}
	if upstream == nil {
		return nil, nil
	}
	output := make(chan *schemas.BifrostStreamChunk, 1)
	go func() {
		defer close(output)
		state := schemas.AcquireChatToResponsesStreamState()
		defer schemas.ReleaseChatToResponsesStreamState(state)
		var pending *schemas.BifrostResponsesStreamResponse
		var usage *schemas.BifrostLLMUsage
		var finalExtra schemas.BifrostResponseExtraFields
		usageSeen := false
		send := func(chunk *schemas.BifrostStreamChunk) bool {
			select {
			case output <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		emit := func(response *schemas.BifrostResponsesStreamResponse) bool {
			return send(&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: response})
		}
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, open := <-upstream:
				if !open {
					if !sdkStreamEndedNormally(ctx) || ctx.Err() != nil {
						return
					}
					if pending == nil {
						send(&schemas.BifrostStreamChunk{BifrostError: compatibleStreamError("Chat stream ended without a finish reason")})
						return
					}
					pending.Response.Usage = nil
					if usageSeen && usage != nil {
						pending.Response.Usage = usage.ToResponsesResponseUsage()
					}
					pending.ExtraFields = finalExtra
					pending.ExtraFields.RequestType = schemas.ResponsesStreamRequest
					emit(pending)
					return
				}
				if chunk == nil {
					send(&schemas.BifrostStreamChunk{BifrostError: compatibleStreamError("invalid Chat stream chunk")})
					return
				}
				if chunk.BifrostError != nil {
					chunk.BifrostError.ExtraFields.RawResponse = nil
					send(chunk)
					return
				}
				response := chunk.BifrostChatResponse
				if response == nil {
					send(&schemas.BifrostStreamChunk{BifrostError: compatibleStreamError("missing Chat stream response")})
					return
				}
				usageSeen = takeCompatibleUsagePresence(&response.ExtraFields.RawResponse) || usageSeen
				if response.Usage != nil {
					usage = response.Usage
					// 原始捕获可能被 SDK 的上限截断；正数累计值仍是明确的上游用量证据。
					usageSeen = usageSeen || usage.TotalTokens > 0 || usage.PromptTokens > 0 || usage.CompletionTokens > 0
				}
				finalExtra = response.ExtraFields
				if !compatibleToolIndexesKnown(response, state) {
					send(&schemas.BifrostStreamChunk{BifrostError: compatibleStreamError("Chat tool call has no ID or known index")})
					return
				}
				started := time.Now()
				events := chatToResponsesStreamEvents(response, state)
				orderCompatibleToolCompletions(events)
				schemas.AddStreamConvert(ctx, time.Since(started))
				for _, event := range events {
					if event.Type == schemas.ResponsesStreamResponseTypeCompleted || event.Type == schemas.ResponsesStreamResponseTypeIncomplete {
						pending = event
						continue
					}
					if !emit(event) {
						return
					}
				}
			}
		}
	}()
	return output, nil
}

// SDK 从 map 生成工具结束事件；Gemini 在此时才输出调用，必须保持原输出索引顺序。
func orderCompatibleToolCompletions(events []*schemas.BifrostResponsesStreamResponse) {
	toolCompletion := func(event *schemas.BifrostResponsesStreamResponse) bool {
		if event.OutputIndex == nil {
			return false
		}
		return event.Type == schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDone ||
			event.Type == schemas.ResponsesStreamResponseTypeOutputItemDone && event.Item != nil &&
				event.Item.Type != nil && *event.Item.Type == schemas.ResponsesMessageTypeFunctionCall
	}
	for start := 0; start < len(events); {
		if !toolCompletion(events[start]) {
			start++
			continue
		}
		end := start + 1
		for end < len(events) && toolCompletion(events[end]) {
			end++
		}
		group := events[start:end]
		sequence := group[0].SequenceNumber
		sort.SliceStable(group, func(i, j int) bool {
			return *group[i].OutputIndex < *group[j].OutputIndex
		})
		for index, event := range group {
			event.SequenceNumber = sequence + index
		}
		start = end
	}
}

func compatibleToolIndexesKnown(response *schemas.BifrostChatResponse, state *schemas.ChatToResponsesStreamState) bool {
	if len(response.Choices) == 0 || response.Choices[0].ChatStreamResponseChoice == nil || response.Choices[0].Delta == nil {
		return true
	}
	var introduced map[uint16]bool
	for _, call := range response.Choices[0].Delta.ToolCalls {
		if call.ID != nil && *call.ID != "" {
			if introduced == nil {
				introduced = make(map[uint16]bool)
			}
			introduced[call.Index] = true
		} else if !introduced[call.Index] && state.ToolCallIndexToID[call.Index] == "" {
			return false
		}
	}
	return true
}

// 仅提取存在性，立刻清除原始文本。SDK 可将多个 JSON 事件以空白连接在同一捕获值中。
func takeCompatibleUsagePresence(raw *any) bool {
	value := *raw
	*raw = nil
	if value == nil {
		return false
	}
	body, ok := rawRequestJSON(value)
	if !ok {
		return false
	}
	for body = bytes.TrimSpace(body); len(body) > 0; body = bytes.TrimSpace(body) {
		// JSON Lines 路径只取首个值，也支持空白连接的多个事件。
		event := gjson.GetBytes(body, "..0")
		// 缺失或被截断的捕获不是零用量证据；不解码完整事件。
		if !gjson.Valid(event.Raw) || (!event.IsObject() && event.Raw != "null") {
			return false
		}
		if usage := event.Get("usage"); usage.Exists() && usage.Type != gjson.Null {
			return true
		}
		body = body[len(event.Raw):]
	}
	return false
}

func compatibleStreamError(message string) *schemas.BifrostError {
	return &schemas.BifrostError{
		StatusCode: schemas.Ptr(http.StatusBadGateway),
		Error:      &schemas.ErrorField{Type: schemas.Ptr("invalid_response_error"), Message: message},
	}
}
