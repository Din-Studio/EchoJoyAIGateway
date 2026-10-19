package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/automodel"
	"gpt-load/internal/channel"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/pricing"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
	"gpt-load/internal/usage"
)

func cnySettings(rate string) config.Settings {
	return config.Settings{state.SettingCurrency: "CNY", state.SettingExchangeRate: rate}
}

var cnyAt71 = pricing.Currency{Code: pricing.CurrencyCNY, RateMicros: 7_100_000}

func TestRecorderSettlesAnswerAndDecisionCostInFrozenCurrency(t *testing.T) {
	recorder := newRequestRecorder(nil, "request", time.Unix(1, 0), 1, protocol.OpenAICompletions, time.Now)
	if recorder.currency != pricing.USDCurrency {
		t.Fatalf("default settlement currency = %#v, want USD", recorder.currency)
	}
	recorder.currency = cnyAt71
	recorder.usage.Pricing.EstimatedCostNanoUSD = 1
	recorder.autoDecision = &automodel.Decision{EstimatedCostNanoUSD: 4}
	// 答复与决策分别换算再相加（7.1→7、28.4→28），与日志两列本币金额之和一致；
	// 先合计再换算会得到 35.5→36。
	if got := recorder.estimatedCostNano(); got != 35 {
		t.Fatalf("estimatedCostNano() = %d, want 35", got)
	}
}

func TestHandlerFreezesSettlementCurrencyAtAdmission(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{{
		StatusCode: http.StatusOK, Header: make(http.Header), RequestWritten: true,
		Body:  []byte(`{"ok":true}`),
		Usage: usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1, Output: 1}},
	}}}
	sink := &recordingRequestLogSink{}
	engine, handler, manager, _ := newRequestLogHandlerTestRuntime(
		t, forwarder, &recordingAccessKeyRPMLimiter{}, sink, "sk-first",
	)
	rules := []accessquota.Rule{{ID: 902, Revision: 1, Kind: accessquota.KindTotal, Currency: pricing.CurrencyCNY, LimitNano: 1_000_000}}
	quota := useSharedAccessQuota(t, handler)
	table := mustGatewayTableWithClientPrices(t, []pricing.Rule{{
		Identity: pricing.Identity{ChannelID: "openai", ModelID: "gpt-4o"},
		Prices: pricing.Prices{
			Input:  pricing.Price{NanoUSDPerMillion: 600_000, Set: true},
			Output: pricing.Price{NanoUSDPerMillion: 600_000, Set: true},
		},
	}}, "gpt-4o")
	handler.priceTables = &mutableGatewayPriceTableProvider{table: table}
	input := gatewayAccessQuotaCompileInput(handler, rules)
	input.SystemSettings = cnySettings("7.1")
	if _, err := manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	// 请求进行中改汇率：本次请求仍按准入时的 7.1 结算。
	forwarder.onCall = func(int) {
		input.SystemSettings = cnySettings("7.3")
		if _, err := manager.Publish(input); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	request.Header.Set("Authorization", "Bearer gl-client")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	events := sink.snapshot()
	if len(events) != 1 || events[0].Currency != cnyAt71 || events[0].Usage.Pricing.EstimatedCostNanoUSD != 2 {
		t.Fatalf("events = %#v, want one request settled in CNY at 7.1", events)
	}
	// 2 nano USD × 7.1 = 14.2 → 14；按 7.3 会是 15，按 USD 会是 2。
	view, err := quota.View(t.Context(), manager.Current(), 1, time.Now())
	if err != nil || len(view.Rules) != 1 || view.Rules[0].UsedNano != 14 {
		t.Fatalf("quota = %#v, %v; want 14 nano CNY used", view, err)
	}
}

func TestWebsocketTurnFreezesSettlementCurrency(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var req struct {
			StreamID string `json:"stream_id"`
		}
		if conn.ReadJSON(&req) != nil {
			return
		}
		if conn.WriteMessage(websocket.TextMessage, websocketCompleted("resp_"+req.StreamID, req.StreamID)) != nil {
			return
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	h, engine, input := websocketTestHandler(t, upstream.URL+"/v1", channel.OpenAI)
	sink := &recordingRequestLogSink{}
	h.requestLogSink = sink
	h.limiter = &recordingAccessKeyRPMLimiter{}
	input.SystemSettings = cnySettings("7.1")
	input.AccessKeys[0].CostLimitRules = []accessquota.Rule{{ID: 1, Revision: 1, Kind: accessquota.KindTotal, Currency: pricing.CurrencyCNY, LimitNano: 1_000_000}}
	if _, err := h.manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	quota := useSharedAccessQuota(t, h)
	table := mustGatewayTableWithClientPrices(t, []pricing.Rule{{Identity: pricing.Identity{ChannelID: "openai", ModelID: "upstream"}, Prices: pricing.Prices{Input: pricing.Price{NanoUSDPerMillion: 1_000_000, Set: true}, Output: pricing.Price{NanoUSDPerMillion: 1_000_000, Set: true}}}}, "public")
	h.priceTables = &mutableGatewayPriceTableProvider{table: table}
	server := httptest.NewServer(engine)
	defer server.Close()
	conn := dialGatewayWebsocket(t, server.URL)
	_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","stream_id":%q,"model":"public","input":"hello"}`, "a")))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	events := waitWebsocketLogs(t, sink, 1)
	if events[0].Currency != cnyAt71 || events[0].Usage.Pricing.EstimatedCostNanoUSD != 3 {
		t.Fatalf("event = %+v, want one turn settled in CNY at 7.1", events[0])
	}
	// 3 nano USD × 7.1 = 21.3 → 21。
	view, err := quota.View(t.Context(), h.manager.Current(), 1, time.Now())
	if err != nil || len(view.Rules) != 1 || view.Rules[0].UsedNano != 21 {
		t.Fatalf("quota = %#v, %v; want 21 nano CNY used", view, err)
	}
}
