package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"gpt-load/internal/channel"
	"gpt-load/internal/dialect"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/pricing"
	"gpt-load/internal/scheduler"
	"gpt-load/internal/state"
	"gpt-load/internal/usage"
)

func TestAdvisorFrozenDualPricing(t *testing.T) {
	h, _, _ := newHandlerForTest(t, &scriptedForwarder{})
	price := func(n pricing.NanoUSD) pricing.Price { return pricing.Price{NanoUSDPerMillion: n, Set: true} }
	table, err := pricing.NewTableWithClientRules(
		[]pricing.Rule{
			{Identity: pricing.Identity{ChannelID: "anthropic", ModelID: "executor"}, Prices: pricing.Prices{Input: price(10_000_000)}},
			{Identity: pricing.Identity{ChannelID: "anthropic", ModelID: "advisor"}, Prices: pricing.Prices{Input: price(20_000_000)}}},
		[]pricing.ClientRule{
			{ClientModel: "public", Prices: pricing.Prices{Input: price(1_000_000)}},
			{ClientModel: "assistant", Prices: pricing.Prices{Input: price(2_000_000)}}})
	if err != nil {
		t.Fatal(err)
	}
	h.priceTables = &mutableGatewayPriceTableProvider{table: table}
	input := state.CompileInput{ChannelRegistry: channel.NewRegistry(), Groups: []state.GroupConfig{{ID: 1, Name: "native", ChannelID: channel.Anthropic, ConnectionType: "api_key", Params: json.RawMessage("{}"), Enabled: true, Models: []state.ModelConfig{{ID: "executor", Alias: "public"}, {ID: "advisor", Alias: "assistant"}}}}}
	groupMultiplier, _ := pricing.ParsePriceMultiplier("2")
	keyMultiplier, _ := pricing.ParsePriceMultiplier("3")
	snapshot, err := state.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	group := snapshot.Groups[1]
	group.PriceMultiplier = groupMultiplier
	model := "executor"
	frozen := h.freezeAttemptPricing(scheduler.Selection{Group: group, GroupID: 1, ChannelID: channel.Anthropic, UpstreamModelID: &model}, dialect.RequestMetadata{ObserveUsage: true}, true, state.AccessKeyView{PriceMultiplier: keyMultiplier})
	// Publishing a new alias and price table must not affect this attempt.
	input.Groups[0].Models[1].Alias = "changed"
	if _, err = h.manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	h.priceTables = &mutableGatewayPriceTableProvider{}
	result := usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 10}, Advisor: &usage.AdvisorUsage{Calls: []usage.ModelTokens{{Model: "advisor", Tokens: usage.Tokens{UncachedInput: 10}}}}}
	observed := quoteFrozenAttempt(frozen, "public", result, pricing.ModeStandard)
	if observed.EstimatedCostNanoUSD != 90 || observed.UpstreamCostNanoUSD != 600 {
		t.Fatalf("%+v", observed)
	}
	var charge, cost pricing.Receipt
	if err = json.Unmarshal([]byte(observed.ReceiptJSON), &charge); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(observed.UpstreamReceiptJSON), &cost); err != nil {
		t.Fatal(err)
	}
	if charge.SchemaVersion != 9 || cost.SchemaVersion != 8 || charge.LineItems[1].ModelID != "assistant" || cost.LineItems[1].ModelID != "advisor" || result.Advisor.Calls[0].Model != "advisor" {
		t.Fatalf("charge=%+v cost=%+v result=%+v", charge, cost, result)
	}
	if pricing.ValidateReceipt(charge) != nil || pricing.ValidateReceipt(cost) != nil {
		t.Fatal("invalid receipt")
	}
	// A single authorized alias is deterministic regardless of group ordering.
	for _, reverse := range []bool{false, true} {
		models := []state.ModelConfig{{ID: "advisor", Alias: "hidden"}, {ID: "advisor", Alias: "assistant"}}
		if reverse {
			models[0], models[1] = models[1], models[0]
		}
		frozen.models = models
		frozen.modelFilter = map[string]struct{}{"assistant": {}}
		got := quoteFrozenAttempt(frozen, "public", result, pricing.ModeStandard)
		if got.EstimatedCostNanoUSD != 90 || !strings.Contains(got.ReceiptJSON, `"model_id":"assistant"`) {
			t.Fatalf("authorized alias=%+v", got)
		}
		frozen.modelFilter = nil
		got = quoteFrozenAttempt(frozen, "public", result, pricing.ModeStandard)
		if got.CostState != "unpriced" || got.UpstreamCostNanoUSD != 600 {
			t.Fatalf("ambiguous aliases=%+v", got)
		}
	}
	frozen.models = nil
	observed = quoteFrozenAttempt(frozen, "public", result, pricing.ModeStandard)
	if observed.CostState != "unpriced" || observed.ReceiptJSON != "" || observed.UpstreamCostNanoUSD != 600 {
		t.Fatalf("unmapped advisor=%+v", observed)
	}
}

func TestAdvisorHTTPOverrideCannotBypassAccess(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			forwarder := &scriptedForwarder{results: []UpstreamResult{{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"type":"message","content":[]}`)}}}
			h, m, r := newHandlerForTest(t, forwarder)
			in := gatewayAccessQuotaCompileInput(h, nil)
			in.Groups[0].ChannelID = channel.Anthropic
			in.Groups[0].Models = []state.ModelConfig{{ID: "executor"}, {ID: "advisor", Alias: "assistant"}}
			in.Groups[0].Settings = config.Settings{state.SettingParameterOverrides: []any{map[string]any{"set": map[string]any{"tools": []any{map[string]any{"type": "advisor_20260301", "model": "advisor"}}}}}}
			in.AccessKeys[0].Filters.Models = map[string]struct{}{"executor": {}}
			if allowed {
				in.AccessKeys[0].Filters.Models["assistant"] = struct{}{}
			}
			if _, err := m.Publish(in); err != nil {
				t.Fatal(err)
			}
			if err := r.ReplaceCredentials([]state.CredentialEntry{testCredentialEntry(t, h.encryption, 1, 1, "synthetic-advisor")}); err != nil {
				t.Fatal(err)
			}
			h.dialects = dialect.NewSet(dialect.NewAnthropic())
			c, w := prepareAuthenticatedGatewayRequest(t, h, http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"executor","max_tokens":8,"messages":[{"role":"user","content":"test"}]}`))
			h.Handle(c)
			want := 0
			if allowed {
				want = 1
			}
			if len(forwarder.inputs) != want || (allowed && w.Code != 200) || (!allowed && w.Code < 400) {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, len(forwarder.inputs), w.Body.String())
			}
		})
	}
}

func TestAdvisorWebsocketAccessOnFirstAndBoundTurns(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprint(override), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					if _, _, err = conn.ReadMessage(); err != nil {
						return
					}
					n := calls.Add(1)
					_ = conn.WriteMessage(websocket.TextMessage, websocketCompleted(fmt.Sprintf("resp_%d", n), ""))
				}
			}))
			defer upstream.Close()
			h, engine, in := websocketTestHandler(t, upstream.URL, channel.OpenAI)
			in.Groups[0].Models = append(in.Groups[0].Models, state.ModelConfig{ID: "advisor", Alias: "assistant"})
			in.AccessKeys[0].Filters.Models = map[string]struct{}{"public": {}, "assistant": {}}
			if override {
				in.Groups[0].Settings = config.Settings{state.SettingParameterOverrides: []any{map[string]any{"set": map[string]any{"tools": []any{map[string]any{"type": "advisor", "model": "advisor"}}}}}}
			}
			if _, err := h.manager.Publish(in); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(engine)
			defer server.Close()
			conn := dialGatewayWebsocket(t, server.URL)
			defer conn.Close()
			body := `{"type":"response.create","model":"public","input":"hello","tools":[{"type":"advisor","model":"advisor"}]}`
			if override {
				body = `{"type":"response.create","model":"public","input":"hello"}`
			}
			send := func(want string) {
				t.Helper()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
					t.Fatal(err)
				}
				_, data, err := conn.ReadMessage()
				if err != nil || !strings.Contains(string(data), want) {
					t.Fatalf("data=%s err=%v want=%s", data, err, want)
				}
			}
			send("response.completed")
			delete(in.AccessKeys[0].Filters.Models, "assistant")
			if _, err := h.manager.Publish(in); err != nil {
				t.Fatal(err)
			}
			send("advisor_model_unavailable")
			if calls.Load() != 1 {
				t.Fatalf("bound calls=%d", calls.Load())
			}
			conn.Close()
			conn = dialGatewayWebsocket(t, server.URL)
			defer conn.Close()
			send("advisor_model_unavailable")
			if calls.Load() != 1 {
				t.Fatalf("first calls=%d", calls.Load())
			}
		})
	}
}
