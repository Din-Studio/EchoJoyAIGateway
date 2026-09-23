package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/channel"
	"gpt-load/internal/state"
)

type scriptedAccessQuotaGate struct {
	checkErr    error
	admitErr    error
	completeErr error
	completes   atomic.Int32
}

func (gate *scriptedAccessQuotaGate) Check(
	context.Context, *state.ConfigSnapshot, uint, time.Time,
) (accessquota.Decision, error) {
	return accessquota.Decision{Allowed: true, Recoverable: true}, gate.checkErr
}

func (gate *scriptedAccessQuotaGate) Admit(
	_ context.Context, _ *state.ConfigSnapshot, accessKeyID uint, _ time.Time,
) (accessquota.Ticket, accessquota.Decision, error) {
	ticket := accessquota.Ticket{AccessKeyID: accessKeyID, Rules: []accessquota.TicketRule{{RuleID: 1, RuleRevision: 1}}}
	return ticket, accessquota.Decision{Allowed: true, Recoverable: true}, gate.admitErr
}

func (gate *scriptedAccessQuotaGate) Complete(
	context.Context, accessquota.Ticket, int64,
) (accessquota.CompletionResult, error) {
	gate.completes.Add(1)
	return accessquota.CompletionResult{}, gate.completeErr
}

func TestHandlerFailsClosedWhenSharedLimitStateIsUnavailable(t *testing.T) {
	unavailable := errors.New("redis: connection refused")
	for _, test := range []struct {
		name       string
		gate       *scriptedAccessQuotaGate
		limiterErr error
		wantCode   string
	}{
		{name: "quota check unavailable", gate: &scriptedAccessQuotaGate{checkErr: unavailable}, wantCode: "cluster_state_unavailable"},
		{name: "quota check stale", gate: &scriptedAccessQuotaGate{checkErr: accessquota.ErrStaleRules}, wantCode: "configuration_changed"},
		{name: "quota admit unavailable", gate: &scriptedAccessQuotaGate{admitErr: unavailable}, wantCode: "cluster_state_unavailable"},
		{name: "quota admit stale", gate: &scriptedAccessQuotaGate{admitErr: accessquota.ErrStaleRules}, wantCode: "configuration_changed"},
		{name: "rpm unavailable", gate: &scriptedAccessQuotaGate{}, limiterErr: unavailable, wantCode: "cluster_state_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarder := &scriptedForwarder{results: []UpstreamResult{{
				StatusCode: http.StatusOK, Header: make(http.Header), Body: []byte(`{"ok":true}`), RequestWritten: true,
			}}}
			limiter := &recordingAccessKeyRPMLimiter{err: test.limiterErr}
			engine, handler, _, _ := newRequestLogHandlerTestRuntime(t, forwarder, limiter, &recordingRequestLogSink{}, "sk-first")
			handler.accessQuota = test.gate

			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
			request.Header.Set("Authorization", "Bearer gl-client")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			if response.Code != http.StatusServiceUnavailable ||
				!strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`) ||
				response.Header().Get("Retry-After") != "1" {
				t.Fatalf("response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}
			if len(forwarder.inputs) != 0 {
				t.Fatalf("forward calls = %d, want 0", len(forwarder.inputs))
			}
			if test.gate.completes.Load() != 0 {
				t.Fatal("Complete ran for a request that was never admitted")
			}
		})
	}
}

func TestHandlerKeepsResponseWhenQuotaCompletionFails(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{{
		StatusCode: http.StatusOK, Header: make(http.Header), Body: []byte(`{"ok":true}`), RequestWritten: true,
	}}}
	engine, handler, _, _ := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-first")
	gate := &scriptedAccessQuotaGate{completeErr: errors.New("redis: timeout")}
	handler.accessQuota = gate

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	request.Header.Set("Authorization", "Bearer gl-client")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusOK || gate.completes.Load() != 1 {
		t.Fatalf("response = %d completes=%d body=%s", response.Code, gate.completes.Load(), response.Body.String())
	}
}

func TestWebsocketTurnFailsClosedWhenSharedLimitStateIsUnavailable(t *testing.T) {
	unavailable := errors.New("redis: connection refused")
	for _, test := range []struct {
		name       string
		gate       *scriptedAccessQuotaGate
		limiterErr error
		wantCode   string
	}{
		{name: "quota check unavailable", gate: &scriptedAccessQuotaGate{checkErr: unavailable}, wantCode: "cluster_state_unavailable"},
		{name: "quota check stale", gate: &scriptedAccessQuotaGate{checkErr: accessquota.ErrStaleRules}, wantCode: "configuration_changed"},
		{name: "quota admit unavailable", gate: &scriptedAccessQuotaGate{admitErr: unavailable}, wantCode: "cluster_state_unavailable"},
		{name: "rpm unavailable", gate: &scriptedAccessQuotaGate{}, limiterErr: unavailable, wantCode: "cluster_state_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstreamTurns atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					upstreamTurns.Add(1)
				}
			}))
			defer upstream.Close()
			h, engine, _ := websocketTestHandler(t, upstream.URL+"/v1", channel.OpenAI)
			h.limiter = &recordingAccessKeyRPMLimiter{err: test.limiterErr}
			h.accessQuota = test.gate
			server := httptest.NewServer(engine)
			defer server.Close()
			conn := dialGatewayWebsocket(t, server.URL)

			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"public","input":"hello"}`)); err != nil {
				t.Fatal(err)
			}
			var rejected struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := conn.ReadJSON(&rejected); err != nil || rejected.Error.Code != test.wantCode {
				t.Fatalf("rejection = %+v err=%v, want %s", rejected, err, test.wantCode)
			}
			if upstreamTurns.Load() != 0 || test.gate.completes.Load() != 0 {
				t.Fatalf("upstream turns=%d completes=%d, want 0", upstreamTurns.Load(), test.gate.completes.Load())
			}
		})
	}
}
