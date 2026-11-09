package bifrost

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/health"
	"gpt-load/internal/protocol"
)

func TestConvertedResponsesStreamPreservesErrorStatusSource(t *testing.T) {
	for _, eventType := range []string{"error", "response.failed"} {
		for _, status := range []int{0, 401, 429} {
			for _, late := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/status=%d/late=%t", eventType, status, late), func(t *testing.T) {
					payload := `"error":{"message":"request refused"}`
					if eventType == "response.failed" {
						payload = `"response":{"id":"resp_1","status":"failed",` + payload + `}`
					}
					if status != 0 {
						payload += fmt.Sprintf(`,"status_code":%d`, status)
					}
					payload = fmt.Sprintf(`{"type":%q,%s}`, eventType, payload)
					delivered := make(chan struct{})
					var once sync.Once
					var calls atomic.Int64
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if r.URL.Path != "/v1/responses" {
							t.Errorf("path = %s", r.URL.Path)
						}
						_, _ = io.Copy(io.Discard, r.Body)
						w.Header().Set("Content-Type", "text/event-stream")
						if late {
							_, _ = io.WriteString(w, strings.Split(openAIResponsesStreamFixture, "event: response.completed")[0])
							w.(http.Flusher).Flush()
							select {
							case <-delivered:
							case <-r.Context().Done():
								return
							}
						}
						_, _ = io.WriteString(w, "data: "+payload+"\n\n"+
							`data: {"type":"response.output_text.delta","sequence_number":9,"output_index":0,"content_index":0,"item_id":"msg_1","delta":"MUST_NOT_FORWARD"}`+"\n\ndata: [DONE]\n\n")
					}))
					defer server.Close()
					runtime := newProtocolTestRuntime(t, testRuntimeOptions{allowPrivateNetwork: true, openAIBaseURL: server.URL})
					spec := convertedSpec(channel.OpenAI, protocol.Anthropic, execution.OperationChatCompletion, "/v1/messages",
						[]byte(`{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					events := 0
					result := runtime.ExecuteStream(ctx, spec, func(event execution.StreamEvent) error {
						events++
						if strings.Contains(string(event.Data), "MUST_NOT_FORWARD") {
							t.Error("data after terminal error")
						}
						if strings.Contains(string(event.Data), "hello") {
							once.Do(func() { close(delivered) })
						}
						return nil
					})
					if err := result.Validate(); err != nil || result.Error == nil {
						t.Fatalf("result=%+v validation=%v", result, err)
					}
					wantStatus, wantEvidenceStatus := status, status
					hint, effect, retry := execution.FailureHint(""), health.EffectNone, health.RetryNone
					switch status {
					case 401:
						hint, effect, retry = execution.FailureHintInvalidCredential, health.EffectRecordCredentialFailure, health.RetryNextCandidate
					case 429:
						hint, effect, retry = execution.FailureHintRateLimited, health.EffectCooldownModel, health.RetryNextCandidate
					}
					if late {
						wantStatus, wantEvidenceStatus, retry = http.StatusOK, 0, health.RetryNone
					}
					if result.UpstreamProtocol != protocol.OpenAIResponses || result.ResponseStarted != (late || status != 0) ||
						result.StatusCode != wantStatus || result.Error.StatusCode != wantEvidenceStatus || result.Error.Hint != hint {
						t.Fatalf("result=%+v evidence=%+v", result, result.Error)
					}
					if status == 0 && result.Error.Kind != execution.ErrorKindProvider {
						t.Fatalf("inferred HTTP error: %+v", result.Error)
					}
					decision := health.JudgeExecution(health.ExecutionAttempt{
						DispatchState: result.DispatchState, StatusCode: result.StatusCode, Header: result.Header,
						Evidence: result.Error, DownstreamCommitted: late,
					}, health.DecisionContext{Method: spec.Method, Operation: spec.Operation})
					if decision.Effect != effect || decision.Retry != retry {
						t.Fatalf("decision=%+v want=%s/%s", decision, effect, retry)
					}
					if calls.Load() != 1 || (!late && events != 0) || (late && events < 2) {
						t.Fatalf("calls=%d events=%d", calls.Load(), events)
					}
				})
			}
		}
	}
}
