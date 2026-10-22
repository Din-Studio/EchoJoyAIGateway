package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/automodel"
	"gpt-load/internal/channel"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
	"gpt-load/internal/telemetry"
	"gpt-load/internal/usage"
)

// 这些测试逐一验证统一定价的核心机制：去掉或写错其中任何一个，对应测试就会失败。

func clientModelPricingResult() UpstreamResult {
	return UpstreamResult{
		StatusCode: http.StatusOK, Header: make(http.Header), Body: []byte(`{"ok":true}`), RequestWritten: true,
		Usage: usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1_000_000}},
	}
}

func clientModelPricingTable(t *testing.T, upstreamModel string, clientRules ...pricing.ClientRule) *pricing.Table {
	t.Helper()
	table, err := pricing.NewTableWithClientRules([]pricing.Rule{{
		Identity: pricing.Identity{ChannelID: string(channel.OpenAI), ModelID: upstreamModel},
		Prices:   pricing.Prices{Input: pricing.Price{NanoUSDPerMillion: 1_000_000_000, Set: true}},
	}}, clientRules)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func clientPrice(model string, nanoUSDPerMillion pricing.NanoUSD) pricing.ClientRule {
	return pricing.ClientRule{ClientModel: model, Prices: pricing.Prices{Input: pricing.Price{NanoUSDPerMillion: nanoUSDPerMillion, Set: true}}}
}

func serveClientModelRequest(t *testing.T, engine *gin.Engine, body string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gl-client")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

// T1：同一个客户端模型走倍率不同的分组，费用不变，上游费用随分组倍率变化。
func TestClientModelChargeDoesNotDependOnTheRoutedGroup(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{clientModelPricingResult(), clientModelPricingResult()}}
	sink := &recordingRequestLogSink{}
	engine, handler, manager, _ := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, sink, "sk-first")
	handler.priceTables = &mutableGatewayPriceTableProvider{table: clientModelPricingTable(t, "gpt-4o", clientPrice("gpt-4o", 3_000_000_000))}
	input := gatewayAccessQuotaCompileInput(handler, nil)
	for _, groupMultiplier := range []string{"1", "2"} {
		setGatewayPriceMultipliers(t, &input, groupMultiplier, "1")
		if _, err := manager.Publish(input); err != nil {
			t.Fatal(err)
		}
		serveClientModelRequest(t, engine, `{"model":"gpt-4o"}`)
	}
	events := sink.snapshot()
	if len(events) != 2 {
		t.Fatalf("events = %#v", events)
	}
	for index, wantUpstream := range []int64{1_000_000_000, 2_000_000_000} {
		pricingObservation := events[index].Usage.Pricing
		if pricingObservation.EstimatedCostNanoUSD != 3_000_000_000 || pricingObservation.UpstreamCostNanoUSD != wantUpstream {
			t.Fatalf("request %d pricing = %#v, want charge 3e9 and upstream cost %d", index, pricingObservation, wantUpstream)
		}
	}
}

// T2：只有上游价格、没有统一价时，费用按未定价处理，不回退到上游价格。
func TestClientModelWithoutUnifiedPriceIsUnpricedWhileUpstreamCostIsPriced(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{clientModelPricingResult()}}
	sink := &recordingRequestLogSink{}
	engine, handler, _, _ := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, sink, "sk-first")
	handler.priceTables = &mutableGatewayPriceTableProvider{table: clientModelPricingTable(t, "gpt-4o")}
	serveClientModelRequest(t, engine, `{"model":"gpt-4o"}`)
	events := sink.snapshot()
	if len(events) != 1 || withoutPricingReceipt(events[0].Usage.Pricing) != (telemetry.PricingObservation{
		ClientModel: "gpt-4o", UpstreamModel: "gpt-4o",
		CostState: "unpriced", PricingCompleteness: "unavailable",
		UpstreamCostState: "priced", UpstreamPricingCompleteness: "complete", UpstreamCostNanoUSD: 1_000_000_000,
	}) || events[0].Usage.Pricing.ReceiptJSON != "" {
		t.Fatalf("events = %#v", events)
	}
}

// T3：auto 请求按选中的目标模型收费，不按 auto 别名收费。
func TestAutoModelIsChargedByItsTargetModel(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{clientModelPricingResult()}}
	handler, manager, _ := newHandlerForTest(t, forwarder, "key-a")
	autoConfig := automodel.DefaultConfig()
	autoConfig.Enabled, autoConfig.Model = true, "jev-router"
	autoConfig.Models = []automodel.Entry{{ID: "auto-one", Name: "auto-one", Enabled: true, Fallback: "only", Presets: []automodel.Preset{{
		ID: "only", Name: "only", Description: "All permitted work", Model: "gpt-4o", ParameterOverrides: json.RawMessage(`[]`),
	}}}}
	if _, err := manager.Publish(state.CompileInput{AutoModel: &autoConfig, Jev: testSharedJev(autoConfig), ChannelRegistry: channel.NewRegistry(),
		Groups: []state.GroupConfig{
			{ID: 1, Name: "openai", ChannelID: channel.OpenAI, ConnectionType: "api_key", Params: json.RawMessage(`{}`), Models: []state.ModelConfig{{ID: "gpt-4o"}}, Enabled: true},
			{ID: 2, Name: "jev", ChannelID: channel.Jev, ConnectionType: "api_key", Params: json.RawMessage(`{}`), Models: []state.ModelConfig{{ID: "jev-latest", Alias: "jev-router"}}, Enabled: true},
		},
		Credentials: []state.CredentialConfig{{ID: 1, GroupID: 1, Status: state.CredentialStatusActive, Version: 1, IdentityGeneration: 1, Fingerprint: "credential-1"}},
		AccessKeys:  []state.AccessKeyConfig{{ID: 1, Name: "client", KeyHash: handler.encryption.Hash("gl-client"), Status: state.AccessKeyStatusActive}},
	}); err != nil {
		t.Fatal(err)
	}
	handler.priceTables = &mutableGatewayPriceTableProvider{table: clientModelPricingTable(t, "gpt-4o",
		clientPrice("gpt-4o", 4_000_000_000), clientPrice("auto-one", 9_000_000_000))}
	sink := &recordingRequestLogSink{}
	handler.requestLogSink = sink
	engine := gin.New()
	bindGatewayRoutesForTest(t, engine, handler)
	serveClientModelRequest(t, engine, `{"model":"auto-one","messages":[{"role":"user","content":"task"}]}`)

	events := sink.snapshot()
	if len(events) != 1 || events[0].ClientModel != "auto-one" || events[0].Usage.Pricing.ClientModel != "gpt-4o" ||
		events[0].Usage.Pricing.EstimatedCostNanoUSD != 4_000_000_000 {
		t.Fatalf("events = %#v", events)
	}
	var receipt pricing.Receipt
	if err := json.Unmarshal([]byte(events[0].Usage.Pricing.ReceiptJSON), &receipt); err != nil ||
		receipt.Rule != (pricing.ReceiptRule{ClientModel: "gpt-4o"}) {
		t.Fatalf("charge receipt = %#v, %v", receipt, err)
	}
}

// T4：分组配置参数覆盖后，单次尝试的元数据不再带模型名，费用仍按客户端模型计价。
func TestParameterOverrideGroupStillChargesTheClientModel(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{clientModelPricingResult()}}
	sink := &recordingRequestLogSink{}
	engine, handler, manager, _ := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, sink, "sk-first")
	input := gatewayAccessQuotaCompileInput(handler, nil)
	input.Groups[0].Models = []state.ModelConfig{{ID: "provider-model", Alias: "public"}}
	input.Groups[0].Settings = config.Settings{"parameter_overrides": []any{map[string]any{
		"match": map[string]any{"protocol": "openai-completions"}, "set": map[string]any{"temperature": 0},
	}}}
	if _, err := manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	handler.priceTables = &mutableGatewayPriceTableProvider{table: clientModelPricingTable(t, "provider-model", clientPrice("public", 2_000_000_000))}
	serveClientModelRequest(t, engine, `{"model":"public"}`)

	if len(forwarder.inputs) != 1 || !strings.Contains(string(forwarder.inputs[0].Request.Body), `"temperature":0`) {
		t.Fatalf("parameter override was not applied: %#v", forwarder.inputs)
	}
	events := sink.snapshot()
	if len(events) != 1 || events[0].Usage.Pricing.ClientModel != "public" ||
		events[0].Usage.Pricing.EstimatedCostNanoUSD != 2_000_000_000 || events[0].Usage.Pricing.UpstreamCostNanoUSD != 1_000_000_000 {
		t.Fatalf("events = %#v", events)
	}
}
