package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gpt-load/internal/channel"
	"gpt-load/internal/dialect"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
)

// Uses the real Bifrost runtime, an HTTP loopback server, miniredis and an
// in-memory log sink. No database or external provider is used.
func TestConvertedFirstErrorRetainsSanitizedHeaderSummary(t *testing.T) {
	const key = "e2e888-upstream-secret"
	const headerSecret = "e2e888-header-secret"
	const message = "Selected model is at capacity. Please try a different model. " + key + " " + headerSecret
	for _, test := range []struct {
		name             string
		typed, outerType bool
	}{
		{name: "message_only"},
		{name: "typed_without_outer_type", typed: true},
		{name: "typed_with_outer_type", typed: true, outerType: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("X-Diagnostic-Secret") != headerSecret {
					t.Errorf("unexpected upstream request: path=%s headers=%v", r.URL.Path, r.Header)
				}
				if !strings.Contains(string(body), `"messages"`) || !strings.Contains(string(body), `"stream":true`) {
					t.Errorf("not converted streaming Chat: %s", body)
				}
				errorObject := map[string]any{"message": message}
				if test.typed {
					errorObject["type"] = "server_error"
					errorObject["code"] = "server_is_overloaded"
				}
				payload := map[string]any{"error": errorObject}
				if test.outerType {
					payload["type"] = "error"
				}
				encoded, _ := json.Marshal(payload)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
			}))
			defer upstream.Close()
			sink := &recordingRequestLogSink{}
			engine, handler, manager, _ := newRequestLogHandlerTestRuntime(t, newTestExecutionForwarder(t), &recordingAccessKeyRPMLimiter{}, sink, key)
			handler.dialects = dialect.NewSet(dialect.NewOpenAIResponses())
			params, _ := json.Marshal(map[string]string{"base_url": upstream.URL + "/v1"})
			_, err := manager.Publish(state.CompileInput{
				ChannelRegistry: channel.NewRegistry(),
				Groups: []state.GroupConfig{{ConnectionType: "api_key", ID: 1, Name: "compatible", ChannelID: channel.OpenAICompatible, Params: params, Models: []state.ModelConfig{{ID: "upstream-model", Alias: "public"}}, Enabled: true,
					Settings: config.Settings{state.SettingHeaderRules: map[string]any{"set": map[string]any{"X-Diagnostic-Secret": headerSecret}}},
				}},
				Credentials: []state.CredentialConfig{{ID: 1, GroupID: 1, Status: state.CredentialStatusActive, Version: 1, IdentityGeneration: 1, Fingerprint: "credential-1"}},
				AccessKeys:  []state.AccessKeyConfig{{ID: 1, Name: "client", KeyHash: handler.encryption.Hash("gl-client"), Status: state.AccessKeyStatusActive}},
			})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public","input":"hi","stream":true}`))
			request.Header.Set("Authorization", "Bearer gl-client")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			events := sink.snapshot()
			if calls.Load() != 1 || len(events) != 1 || len(events[0].Attempts) != 1 {
				t.Fatalf("calls=%d response=%d %s events=%+v", calls.Load(), response.Code, response.Body, events)
			}
			event, attempt := events[0], events[0].Attempts[0]
			if attempt.RouteMode != execution.RouteConverted || attempt.UpstreamProtocol != protocol.OpenAICompletions {
				t.Fatalf("wrong route: %+v", attempt)
			}
			t.Logf("client_status=%d attempt_status=%d response_started=%t error_code=%s attempt_code=%s summary=%q attempt_summary=%q", response.Code, attempt.StatusCode, attempt.ResponseStarted, event.ErrorCode, attempt.ErrorCode, event.ErrorSummary, attempt.ErrorSummary)
			for name, summary := range map[string]string{"request": event.ErrorSummary, "attempt": attempt.ErrorSummary} {
				if !strings.Contains(summary, "Selected model is at capacity.") {
					t.Errorf("%s lost provider message: %q", name, summary)
				}
				if !strings.Contains(summary, "[REDACTED]") {
					t.Errorf("%s missing redaction: %q", name, summary)
				}
				for _, secret := range []string{key, headerSecret} {
					if strings.Contains(summary, secret) {
						t.Errorf("%s summary leaked %q: %q", name, secret, summary)
					}
				}
			}
		})
	}
}
