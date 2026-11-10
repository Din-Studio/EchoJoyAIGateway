package bifrost

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/usage"
)

func TestCancellationRetainsRealDeferredToolUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, timeout := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/timeout=%t", stream, timeout), func(t *testing.T) {
				var calls atomic.Int32
				reached, release := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 2 {
						close(reached)
						<-release
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					body := `{"responseId":"first","modelVersion":"gemini-test","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"bifrost_tool_search","args":{"query":"select:lookup"}}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":12}}`
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, body)
					}
				}))
				defer server.Close()
				defer close(release)
				runtime := newProtocolTestRuntime(t, testRuntimeOptions{allowPrivateNetwork: true, geminiBaseURL: server.URL + "/v1beta"})
				spec := convertedSpec(channel.Gemini, protocol.OpenAIResponses, execution.OperationResponsesCreate, "/v1/responses", []byte(fmt.Sprintf(`{"model":"client-model","input":"hello","stream":%t,"store":false,"tools":[{"type":"function","name":"lookup","defer_loading":true,"parameters":{"type":"object","properties":{}}}]}`, stream)))
				ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
				defer cancel()
				if !timeout {
					go func() {
						select {
						case <-reached:
							cancel()
						case <-ctx.Done():
						}
					}()
				}
				var evidence *execution.UsageEvidence
				var failure *execution.ErrorEvidence
				if stream {
					result := runtime.ExecuteStream(ctx, spec, func(execution.StreamEvent) error { return nil })
					evidence, failure = result.Usage, result.Error
				} else {
					result := runtime.Execute(ctx, spec)
					evidence, failure = result.Usage, result.Error
				}
				wantKind := execution.ErrorKindCanceled
				if timeout {
					wantKind = execution.ErrorKindTimeout
				}
				want := usage.Result{State: usage.StatePartial, Tokens: usage.Tokens{UncachedInput: 6, CacheRead: 4, Output: 2}}
				if calls.Load() != 2 || failure == nil || failure.Kind != wantKind || evidence == nil || evidence.Normalized != want {
					t.Fatalf("calls=%d usage=%+v failure=%+v, want %s/%+v", calls.Load(), evidence, failure, wantKind, want)
				}
			})
		}
	}
}

func TestCancellationRetainsCorrectedAnthropicUsage(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", timeout), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_usage","type":"message","role":"assistant","model":"test-model","content":[],"usage":{"input_tokens":1200,"output_tokens":0}}}`+"\n\nevent: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":100,"cache_read_input_tokens":900,"output_tokens":10}}`+"\n\n")
				w.(http.Flusher).Flush()
				if !timeout {
					timer := time.AfterFunc(100*time.Millisecond, cancel)
					defer timer.Stop()
				}
				<-release
			}))
			defer server.Close()
			defer close(release)
			runtime := newProtocolTestRuntime(t, testRuntimeOptions{allowPrivateNetwork: true, anthropicBaseURL: server.URL})
			spec := convertedSpec(channel.Anthropic, protocol.OpenAIResponses, execution.OperationResponsesCreate, "/v1/responses", []byte(`{"model":"client-model","input":"hello","stream":true,"store":false}`))
			result := runtime.ExecuteStream(ctx, spec, func(execution.StreamEvent) error { return nil })
			want := usage.Tokens{UncachedInput: 100, CacheRead: 900, Output: 10}
			wantKind := execution.ErrorKindCanceled
			if timeout {
				wantKind = execution.ErrorKindTimeout
			}
			if result.Error == nil || result.Error.Kind != wantKind || result.Usage == nil || result.Usage.Normalized.Tokens != want {
				t.Fatalf("usage=%+v error=%+v, want %+v", result.Usage, result.Error, want)
			}
		})
	}
}

func TestCancellationRetainsUnconsumedSuccessfulStreamTerminal(t *testing.T) {
	for _, chat := range []bool{false, true} {
		t.Run(fmt.Sprintf("chat=%t", chat), func(t *testing.T) {
			ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
			capture := captureCancellationUsage(ctx, true)
			response := &schemas.BifrostResponse{}
			if chat {
				response.ChatResponse = &schemas.BifrostChatResponse{Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}}
			} else {
				response.ResponsesStreamResponse = &schemas.BifrostResponsesStreamResponse{
					Type:     schemas.ResponsesStreamResponseTypeInProgress,
					Response: &schemas.BifrostResponsesResponse{Usage: &schemas.ResponsesResponseUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}},
				}
			}
			_, _, _ = (usageCaptureHook{}).PostLLMHook(ctx, response, nil)
			select {
			case <-capture:
				t.Fatal("intermediate usage occupied the terminal handoff")
			default:
			}
			ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
			if !chat {
				response.ResponsesStreamResponse.Type = schemas.ResponsesStreamResponseTypeCompleted
			}
			_, _, _ = (usageCaptureHook{}).PostLLMHook(ctx, response, nil)
			evidence := retainedCancellationUsage(&execution.ErrorEvidence{Kind: execution.ErrorKindCanceled}, nil, capture)
			want := usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 10, Output: 2}}
			if evidence == nil || evidence.Normalized != want {
				t.Fatalf("terminal usage=%+v, want %+v", evidence, want)
			}
		})
	}
}

func TestCancellationUsageHandoffIsBoundedAndOwnsEvidence(t *testing.T) {
	ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	capture := captureCancellationUsage(ctx, false)
	failure := &schemas.BifrostError{ExtraFields: schemas.BifrostErrorExtraFields{BilledUsage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}}}
	_, _, _ = (usageCaptureHook{}).PostLLMHook(ctx, nil, failure)
	failure.ExtraFields.BilledUsage.PromptTokens = 999
	evidence := retainedCancellationUsage(&execution.ErrorEvidence{Kind: execution.ErrorKindCanceled}, nil, capture)
	if evidence == nil || evidence.Normalized.Tokens.UncachedInput != 10 {
		t.Fatalf("capture aliases SDK usage: %+v", evidence)
	}
	start := time.Now()
	if got := retainedCancellationUsage(&execution.ErrorEvidence{Kind: execution.ErrorKindTimeout}, evidence, capture); got != evidence {
		t.Fatalf("missing terminal replaced known usage: %+v", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("unbounded terminal wait: %s", elapsed)
	}
	// A late worker must not block after the gateway has returned.
	for range 2 {
		_, _, _ = (usageCaptureHook{}).PostLLMHook(ctx, nil, failure)
	}
}
