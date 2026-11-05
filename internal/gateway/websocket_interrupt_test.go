package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/channel"
	"gpt-load/internal/cluster"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/pricing"
	"gpt-load/internal/telemetry"
	"gpt-load/internal/testutil/clustertest"
	"gpt-load/internal/usage"
)

type interruptibleWebsocketFixture struct {
	websocketScriptSession
	interrupts     chan []byte
	afterInterrupt func(context.Context) error
}

func (s *interruptibleWebsocketFixture) Interrupt(ctx context.Context, payload []byte) error {
	select {
	case s.interrupts <- bytes.Clone(payload):
		if s.afterInterrupt != nil {
			return s.afterInterrupt(ctx)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestWebsocketInterruptBypassesActiveTurnAndContinues(t *testing.T) {
	interrupt := []byte(`{"type":"response.interrupt","response_id":"resp_1","mode":"discard_partial_items","extension":"preserve"}`)
	session := &interruptibleWebsocketFixture{interrupts: make(chan []byte, 1)}
	session.done = make(chan struct{})
	var turns, opens atomic.Int32
	session.turn = func(ctx context.Context, payload []byte, emit func(context.Context, []byte) error) execution.WebsocketResult {
		if turns.Add(1) == 1 {
			if err := emit(ctx, []byte(`{"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"upstream"}}`)); err != nil {
				return execution.WebsocketResult{}
			}
			select {
			case body := <-session.interrupts:
				if !bytes.Equal(body, interrupt) {
					t.Errorf("interrupt changed: %s", body)
				}
			case <-ctx.Done():
				return execution.WebsocketResult{}
			}
			_ = emit(ctx, []byte(`{"type":"response.incomplete","response":{"id":"resp_1","object":"response","status":"incomplete","model":"upstream","incomplete_details":{"reason":"interrupted"},"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`))
		} else {
			var request struct {
				Previous string `json:"previous_response_id"`
			}
			if json.Unmarshal(payload, &request) != nil || request.Previous != "resp_1" {
				t.Errorf("continuation lost: %s", payload)
			}
			_ = emit(ctx, websocketCompleted("resp_2", ""))
		}
		return execution.WebsocketResult{DispatchState: execution.DispatchMaybeSent}
	}
	h, engine, input := websocketTestHandler(t, "http://unused.invalid", channel.OpenAI)
	input.SystemSettings = cnySettings("7.1")
	input.SystemSettings["global_concurrency_limit"] = 1
	input.SystemSettings["default_access_key_concurrency_limit"] = 1
	input.Groups[0].Settings = config.Settings{"concurrency_limit": 1}
	input.AccessKeys[0].CostLimitRules = []accessquota.Rule{{ID: 1, Revision: 1, Kind: accessquota.KindTotal, Currency: pricing.CurrencyCNY, LimitNano: 1_000_000}}
	if _, err := h.manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	quota := useSharedAccessQuota(t, h)
	prices := mustGatewayTableWithClientPrices(t, []pricing.Rule{{Identity: pricing.Identity{ChannelID: "openai", ModelID: "upstream"}, Prices: pricing.Prices{
		Input: pricing.Price{NanoUSDPerMillion: 1_000_000, Set: true}, Output: pricing.Price{NanoUSDPerMillion: 1_000_000, Set: true},
	}}}, "public")
	h.priceTables = &mutableGatewayPriceTableProvider{table: prices}
	_, client := clustertest.NewClient(t)
	store := cluster.NewRPMStats(client)
	h.forwarder = websocketScriptForwarder{open: func(context.Context, ForwardInput) (execution.WebsocketSession, execution.WebsocketResult) {
		opens.Add(1)
		return &rpmObservedWebsocketSession{WebsocketSession: session, recorder: store, credentialID: 1}, execution.WebsocketResult{}
	}}
	sink := &recordingRequestLogSink{}
	h.requestLogSink = sink
	limiter := &recordingAccessKeyRPMLimiter{}
	h.limiter = limiter
	server := httptest.NewServer(engine)
	defer server.Close()
	conn := dialGatewayWebsocket(t, server.URL)
	defer conn.Close()
	read := func(want string) {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("expected %s while the connection remains usable: %v", want, err)
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(body, &event) != nil || event.Type != want {
			t.Fatalf("expected %s, got %s", want, body)
		}
	}
	if err := conn.WriteMessage(websocket.TextMessage, interrupt); err != nil {
		t.Fatal(err)
	}
	read("error")
	if opens.Load() != 0 {
		t.Fatal("idle interrupt opened an upstream session")
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"public","input":"question","store":false}`)); err != nil {
		t.Fatal(err)
	}
	read("response.created")
	if counts := concurrencySnapshot(t, h); counts.Global != 1 || counts.AccessKeys[1] != 1 || counts.Groups[1] != 1 {
		t.Fatalf("active turn slots=%+v, want one per limited scope", counts)
	}
	for _, invalid := range []string{
		`{"type":"response.interrupt"}`,
		`{"type":"response.interrupt","response_id":42}`,
		`{"type":"response.interrupt","response_id":"other_connection"}`,
		`{"type":"response.interrupt","response_id":"resp_1","stream_id":"other_lane"}`,
	} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(invalid)); err != nil {
			t.Fatal(err)
		}
		read("error")
	}
	if err := conn.WriteMessage(websocket.TextMessage, interrupt); err != nil {
		t.Fatal(err)
	}
	read("response.incomplete")
	waitWebsocketLogs(t, sink, 1)
	waitConcurrencyEmpty(t, h)
	if err := conn.WriteMessage(websocket.TextMessage, interrupt); err != nil {
		t.Fatal(err)
	}
	read("error")
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"public","previous_response_id":"resp_1","input":"answer","store":false}`)); err != nil {
		t.Fatal(err)
	}
	read("response.completed")
	events := waitWebsocketLogs(t, sink, 2)
	waitConcurrencyEmpty(t, h)
	view, err := quota.View(t.Context(), h.manager.Current(), 1, time.Now())
	if err != nil || len(view.Rules) != 1 || view.Rules[0].UsedNano != 42 {
		t.Fatalf("quota=%+v err=%v, want two turns charged 21 nano CNY each", view, err)
	}
	if opens.Load() != 1 || turns.Load() != 2 || len(events) != 2 {
		t.Fatalf("opens=%d turns=%d logs=%d", opens.Load(), turns.Load(), len(events))
	}
	if len(limiter.snapshot()) != 2 || events[0].Status != telemetry.RequestStatusSuccess || events[1].Status != telemetry.RequestStatusSuccess {
		t.Fatalf("RPM or terminal accounting changed: calls=%d statuses=%s/%s", len(limiter.snapshot()), events[0].Status, events[1].Status)
	}
	report, err := cluster.NewRPMStats(client).Report(t.Context(), cluster.RPMCredential, 1)
	if err != nil || report.Requests != 2 {
		t.Fatalf("shared credential RPM=%d err=%v, want two inference attempts", report.Requests, err)
	}
	for _, event := range events {
		if len(event.Attempts) != 1 || event.ClientModel != "public" {
			t.Fatalf("control frame was counted as inference: %+v", event)
		}
		attempt := event.Attempts[0]
		if event.ErrorCode != "" || event.ErrorSummary != "" || attempt.ErrorCode != "" || attempt.ErrorSummary != "" || attempt.FailureCategory != telemetry.FailureCategoryOK {
			t.Fatalf("successful interaction was logged as a failure: %+v", event)
		}
		if event.Usage.Result.State != usage.StateComplete || event.Usage.Result.Tokens.UncachedInput != 2 || event.Usage.Result.Tokens.Output != 1 {
			t.Fatalf("successful interruption changed usage: %+v", event.Usage.Result)
		}
		if event.Currency != cnyAt71 || event.Usage.Pricing.CostState != "priced" || event.Usage.Pricing.EstimatedCostNanoUSD != 3 {
			t.Fatalf("successful interruption changed pricing: %+v", event.Usage.Pricing)
		}
	}
}
