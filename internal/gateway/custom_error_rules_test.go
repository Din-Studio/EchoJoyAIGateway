package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/channel"
	"gpt-load/internal/cluster"
	"gpt-load/internal/execution"
	"gpt-load/internal/health"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/state"
	"gpt-load/internal/testutil/clustertest"
)

func TestHandlerCustomErrorRulesRespectOverridesAndBlacklistThreshold(t *testing.T) {
	rule := []health.ErrorRule{{StatusCodes: []int{403}, Keywords: []string{"欠费"}, Retry: health.RetryNone, Effect: health.EffectRecordCredentialFailure}}
	for _, test := range []struct {
		name        string
		overrides   config.Settings
		blacklisted bool
		ruleID      string
	}{
		{name: "global", blacklisted: true, ruleID: "custom.global.1"},
		{name: "empty override", overrides: config.Settings{state.SettingErrorRules: []any{}}, ruleID: "fallback.upstream_response"},
		{name: "group override", overrides: config.Settings{state.SettingErrorRules: []health.ErrorRule{{StatusCodes: []int{403}, Retry: health.RetryNone, Effect: health.EffectNone}}}, ruleID: "custom.group.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := customGatewayFailure(403)
			forwarder := &scriptedForwarder{results: []UpstreamResult{failure, failure}}
			handler, manager, registry := newHandlerForTest(t, forwarder, "test-error-rule-key")
			publishHandlerPolicySettings(t, handler, manager, 1, config.Settings{state.SettingErrorRules: rule, state.SettingBlacklistThreshold: 2}, test.overrides)
			sink := &recordingRequestLogSink{}
			handler.requestLogSink = sink
			engine := gin.New()
			bindGatewayRoutesForTest(t, engine, handler)
			for attempt := 0; attempt < 2; attempt++ {
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
				request.Header.Set("Authorization", "Bearer gl-client")
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				if response.Code != 403 || (attempt == 0 && len(registry.BlacklistedCredentials()) != 0) {
					t.Fatalf("attempt=%d status=%d blacklist=%+v", attempt, response.Code, registry.BlacklistedCredentials())
				}
			}
			if got := len(registry.BlacklistedCredentials()) != 0; got != test.blacklisted {
				t.Fatalf("blacklisted=%t, want %t", got, test.blacklisted)
			}
			logs := sink.snapshot()
			if len(logs) != 2 || len(logs[0].Attempts) != 1 || logs[0].Attempts[0].RuleID != test.ruleID {
				t.Fatalf("logs=%+v", logs)
			}
		})
	}
}

func TestHandlerCustomErrorRulesCoolDownAndRetryExistingStreamError(t *testing.T) {
	failure := customGatewayFailure(200)
	failure.ProviderErrorBeforeCommit = true
	failure.ExecutionError.Kind = execution.ErrorKindProvider
	forwarder := &scriptedForwarder{streamResults: []UpstreamResult{failure,
		{StatusCode: 200, RequestWritten: true, Committed: true},
	}}
	handler, manager, registry := newHandlerForTest(t, forwarder, "test-error-first", "test-error-second")
	publishHandlerPolicySettings(t, handler, manager, 2, config.Settings{state.SettingErrorRules: []health.ErrorRule{{
		StatusCodes: []int{200}, Keywords: []string{"欠费"}, Retry: health.RetryNextCandidate,
		Effect: health.EffectCooldownCredential, CooldownSeconds: 120,
	}}}, nil)
	sink := &recordingRequestLogSink{}
	handler.requestLogSink = sink
	engine := gin.New()
	bindGatewayRoutesForTest(t, engine, handler)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o","stream":true}`))
	request.Header.Set("Authorization", "Bearer gl-client")
	engine.ServeHTTP(httptest.NewRecorder(), request)
	logs := sink.snapshot()
	if len(forwarder.streamInputs) != 2 || len(logs) != 1 || len(logs[0].Attempts) != 2 {
		t.Fatalf("attempts=%d logs=%+v", len(forwarder.streamInputs), logs)
	}
	until, exists := registry.CredentialCooldownUntil(logs[0].Attempts[0].CredentialID)
	if !exists || !until.After(handler.now()) {
		t.Fatal("the first credential did not enter cooldown")
	}
	if logs[0].Attempts[0].RuleID != "custom.global.1" || !logs[0].Attempts[0].WillRetry {
		t.Fatalf("first attempt=%+v", logs[0].Attempts[0])
	}
}

func TestCustomErrorRulesShareHealthAcrossHandlers(t *testing.T) {
	for _, effect := range []health.Effect{health.EffectRecordCredentialFailure, health.EffectCooldownCredential, health.EffectCooldownModel} {
		t.Run(string(effect), func(t *testing.T) {
			_, client := clustertest.NewClient(t)
			rule := health.ErrorRule{StatusCodes: []int{403}, Retry: health.RetryNone, Effect: effect}
			if effect != health.EffectRecordCredentialFailure {
				rule.CooldownSeconds = 120
			}
			var last *cluster.CredentialHealth
			for instance := 0; instance < 2; instance++ {
				forwarder := &scriptedForwarder{results: []UpstreamResult{customGatewayFailure(403)}}
				h, manager, registry := newHandlerForTest(t, forwarder, "shared-rule-key")
				publishHandlerPolicySettings(t, h, manager, 1, config.Settings{state.SettingErrorRules: []health.ErrorRule{rule}, state.SettingBlacklistThreshold: 2}, nil)
				last = cluster.NewCredentialHealth(client, registry)
				h.sharedHealth = last
				// The second replica reads the first replica's cooldown without issuing another request.
				if instance == 1 && effect != health.EffectRecordCredentialFailure {
					break
				}
				engine := gin.New()
				bindGatewayRoutesForTest(t, engine, h)
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
				request.Header.Set("Authorization", "Bearer gl-client")
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				if response.Code != 403 {
					t.Fatalf("instance %d status=%d", instance, response.Code)
				}
			}
			shared, err := last.ReadHealth(t.Context(), []uint{1})
			if err != nil {
				t.Fatal(err)
			}
			view := shared[1]
			switch effect {
			case health.EffectRecordCredentialFailure:
				if view.FailureCount != 2 || !view.Blacklisted {
					t.Fatalf("failures not shared: %+v", view)
				}
			case health.EffectCooldownCredential:
				if !view.CooldownUntil.After(time.Now()) {
					t.Fatalf("cooldown not shared: %+v", view)
				}
			case health.EffectCooldownModel:
				if !view.ModelCooldowns["gpt-4o"].After(time.Now()) {
					t.Fatalf("model cooldown not shared: %+v", view)
				}
			}
		})
	}
}

func TestWebsocketCustomErrorRulesHandleRejectedHandshake(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"account billing unavailable"}}`))
	}))
	defer upstream.Close()
	h, engine, input := websocketTestHandler(t, upstream.URL, channel.OpenAI)
	input.SystemSettings = config.Settings{state.SettingErrorRules: []health.ErrorRule{{StatusCodes: []int{403}, Retry: health.RetryNone, Effect: health.EffectCooldownCredential, CooldownSeconds: 60}}}
	if _, err := h.manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	sink := &recordingRequestLogSink{}
	h.requestLogSink = sink
	server := httptest.NewServer(engine)
	defer server.Close()
	conn := dialGatewayWebsocket(t, server.URL)
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"public","input":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage()
	logs := waitWebsocketLogs(t, sink, 1)
	if len(logs[0].Attempts) != 1 || logs[0].Attempts[0].RuleID != "custom.global.1" {
		t.Fatalf("websocket attempts=%+v", logs[0].Attempts)
	}
	shared, err := h.sharedHealth.ReadHealth(t.Context(), []uint{1})
	if err != nil || !shared[1].CooldownUntil.After(time.Now()) {
		t.Fatalf("websocket rejection did not apply shared cooldown: %v", err)
	}
}

func customGatewayFailure(status int) UpstreamResult {
	return UpstreamResult{StatusCode: status, Header: make(http.Header), Body: []byte(`{"error":{"message":"账户欠费"}}`),
		RequestWritten: true, ExecutionError: &execution.ErrorEvidence{
			Kind: execution.ErrorKindHTTP, OriginHint: execution.ErrorOriginUpstream,
			StatusCode: status, Summary: "账户欠费",
		}}
}
