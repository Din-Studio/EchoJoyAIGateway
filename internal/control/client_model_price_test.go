package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/pricing"
	"gpt-load/internal/protocol"
	"gpt-load/internal/requestlog"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/telemetry"
	"gpt-load/internal/usage"
)

func clientModelPriceBody(clientModel, input string) string {
	return `{"client_model":` + clientModel + `,"input":` + input + `,"output":null,"cache_read":null,"cache_write":null,` +
		`"context_tiers":[],"mode_schedules":{},"confirm_unpriced":false}`
}

func quoteClientModel(fixture serviceFixture, clientModel string, mode pricing.Mode) pricing.Quote {
	quote, _ := fixture.priceRuntime.Load().QuoteClientModel(clientModel, usage.Result{
		State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1_000_000},
	}, mode, pricing.DefaultPriceMultiplier)
	return quote
}

func TestClientModelPriceHTTPCreatesReplacesAndPublishes(t *testing.T) {
	t.Parallel()
	fixture, engine, _ := newModelPriceHTTPFixture(t, true)
	createPriceTestGroup(t, fixture.db, models.Group{
		Name: "wire", Models: models.JSON(`[{"id":"gpt-wire"}]`), Overrides: models.JSON(`{}`), Enabled: true,
	})
	mustEnsureInitialPrices(t, fixture)

	created := serveModelPriceHTTPRequest(engine, http.MethodPut, "/api/client-model-prices", clientModelPriceBody(`"gpt-wire"`, `"3"`), authTestKey)
	if created.Code != http.StatusOK {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	data := decodeModelPriceHTTPData(t, created)
	if data["client_model"] != "gpt-wire" || data["currency"] != "USD" || data["prices"].(map[string]any)["input"] != "3" {
		t.Fatalf("created price = %#v", data)
	}
	if quote := quoteClientModel(fixture, "gpt-wire", pricing.ModeStandard); quote.EstimatedCostNanoUSD != 3_000_000_000 {
		t.Fatalf("published quote after create = %+v", quote)
	}

	body := `{"client_model":"gpt-wire","input":"4","output":null,"cache_read":null,"cache_write":null,"context_tiers":[],` +
		`"mode_schedules":{"fast":{"prices":{"input":"8","output":null,"cache_read":null,"cache_write":null},"context_tiers":[]}},"confirm_unpriced":false}`
	replaced := serveModelPriceHTTPRequest(engine, http.MethodPut, "/api/client-model-prices", body, authTestKey)
	if replaced.Code != http.StatusOK {
		t.Fatalf("replace = %d %s", replaced.Code, replaced.Body.String())
	}
	var rows []models.ClientModelPrice
	if err := fixture.db.Find(&rows).Error; err != nil || len(rows) != 1 || *rows[0].InputPriceNanoUSDPerMillionTokens != 4_000_000_000 {
		t.Fatalf("persisted client prices = %#v, %v", rows, err)
	}
	// 统一价没有模型目录可比对，任何合法模式都可以配置（决策 D）。
	if quote := quoteClientModel(fixture, "gpt-wire", pricing.ModeFast); quote.EstimatedCostNanoUSD != 8_000_000_000 {
		t.Fatalf("published fast quote after replace = %+v", quote)
	}

	list := serveModelPriceHTTPRequest(engine, http.MethodGet, "/api/models?group_status=all&page_size=100", "", authTestKey)
	var envelope struct {
		Data ProjectModelListResponse `json:"data"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &envelope); err != nil || len(envelope.Data.Items) != 1 {
		t.Fatalf("models = %d %s", list.Code, list.Body.String())
	}
	item := envelope.Data.Items[0]
	if item.Price == nil || *item.Price.Prices.Input != "4" || len(item.UpstreamModels) != 1 ||
		*item.UpstreamModels[0].Price.Prices.Input != "2.5" {
		t.Fatalf("model item = %#v", item)
	}
}

func TestClientModelPriceHTTPRejectsInvalidRequests(t *testing.T) {
	t.Parallel()
	fixture, engine, _ := newModelPriceHTTPFixture(t, false)
	if response := serveModelPriceHTTPRequest(engine, http.MethodPut, "/api/client-model-prices", clientModelPriceBody(`"gpt"`, `"1"`), ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", response.Code)
	}
	for name, body := range map[string]string{
		"no base price":    clientModelPriceBody(`"gpt"`, "null"),
		"marked unpriced":  `{"client_model":"gpt","input":"1","output":null,"cache_read":null,"cache_write":null,"context_tiers":[],"mode_schedules":{},"confirm_unpriced":true}`,
		"empty model":      clientModelPriceBody(`""`, `"1"`),
		"padded model":     clientModelPriceBody(`" gpt"`, `"1"`),
		"missing slots":    `{"client_model":"gpt","input":"1"}`,
		"unknown field":    `{"client_model":"gpt","input":"1","output":null,"cache_read":null,"cache_write":null,"context_tiers":[],"mode_schedules":{},"channel_id":"openai"}`,
		"negative price":   clientModelPriceBody(`"gpt"`, `"-1"`),
		"fast with tiers":  `{"client_model":"gpt","input":"1","output":null,"cache_read":null,"cache_write":null,"context_tiers":[],"mode_schedules":{"fast":{"prices":{"input":"2","output":null,"cache_read":null,"cache_write":null},"context_tiers":[{"threshold_tokens":1,"input":"3","output":null,"cache_read":null,"cache_write":null}]}}}`,
		"standard as mode": `{"client_model":"gpt","input":"1","output":null,"cache_read":null,"cache_write":null,"context_tiers":[],"mode_schedules":{"standard":{"prices":{"input":"2","output":null,"cache_read":null,"cache_write":null},"context_tiers":[]}}}`,
	} {
		if response := serveModelPriceHTTPRequest(engine, http.MethodPut, "/api/client-model-prices", body, authTestKey); response.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s, want 400", name, response.Code, response.Body.String())
		}
	}
	var count int64
	if err := fixture.db.Model(&models.ClientModelPrice{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rejected requests persisted %d client prices, %v", count, err)
	}
}

// T6：访问密钥范围看得到统一价，看不到上游价格的金额。
func TestProjectModelsHTTPHidesUpstreamAmountsFromAccessKeys(t *testing.T) {
	t.Parallel()
	initControlI18n(t)
	fixture := newServiceFixture(t)
	createPriceTestGroup(t, fixture.db, models.Group{
		Name: "openai", Models: models.JSON(`[{"id":"upstream-a","alias":"client-a"}]`),
		Overrides: models.JSON(`{}`), Enabled: true,
	})
	mustEnsureInitialPrices(t, fixture)
	upstreamInput := int64(1_234_000_000)
	if err := fixture.db.Model(&models.ModelPrice{}).Where("model_id = ?", "upstream-a").
		Update("input_price_nano_usd_per_million_tokens", upstreamInput).Error; err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	newTestServer(t, &config.Config{AuthKey: authTestKey}, fixture.service).RegisterRoutes(engine)
	if response := serveModelPriceHTTPRequest(engine, http.MethodPut, "/api/client-model-prices", clientModelPriceBody(`"client-a"`, `"5"`), authTestKey); response.Code != http.StatusOK {
		t.Fatalf("put client price = %d %s", response.Code, response.Body.String())
	}
	key, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "model viewer"})
	if err != nil {
		t.Fatal(err)
	}

	for _, viewer := range []struct {
		name          string
		authorization string
		wantUpstream  bool
	}{
		{"admin", "Bearer " + authTestKey, true},
		{"access key", "Bearer " + key.Key, false},
	} {
		recorder := serveAuthRequest(engine, "/api/models?group_status=all&page_size=100", "192.0.2.91:1234", viewer.authorization, nil)
		var envelope struct {
			Data ProjectModelListResponse `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || recorder.Code != http.StatusOK || len(envelope.Data.Items) != 1 {
			t.Fatalf("%s models = %d %s", viewer.name, recorder.Code, recorder.Body.String())
		}
		item := envelope.Data.Items[0]
		upstream := item.UpstreamModels[0].Price
		if item.Price == nil || *item.Price.Prices.Input != "5" || upstream.ChannelID == "" {
			t.Fatalf("%s model = %#v", viewer.name, item)
		}
		if hasAmount := upstream.Prices.Input != nil; hasAmount != viewer.wantUpstream {
			t.Fatalf("%s upstream amount visible = %v, want %v: %s", viewer.name, hasAmount, viewer.wantUpstream, recorder.Body.String())
		}
	}
}

func TestRequestLogsReturnUpstreamCostToAdministrators(t *testing.T) {
	t.Parallel()
	initControlI18n(t)
	fixture := newServiceFixture(t)
	cny, err := pricing.NewCurrency(pricing.CurrencyCNY, 7_100_000)
	if err != nil {
		t.Fatal(err)
	}
	record := requestlog.Record{
		RequestID:     "00000000-0000-4000-8000-000000000801",
		CompletedAtMS: time.Date(2026, time.October, 19, 8, 0, 0, 0, time.UTC).UnixMilli(),
		AccessKey:     requestlog.AccessKeyRef{ID: 1, Deleted: true},
		Protocol:      protocol.OpenAICompletions, Operation: execution.OperationChatCompletion,
		ClientModel: "client-model", UpstreamModel: "upstream-model",
		ModelConsistency: telemetry.ModelConsistencyNotApplicable,
		Status:           telemetry.RequestStatusSuccess, StatusCode: http.StatusOK, DurationMs: 10,
		GroupID: 9, ChannelID: channel.OpenAI, CredentialID: 3,
		UsageState: usage.StateComplete, CostState: pricing.CostStatePriced, PricingCompleteness: pricing.CompletenessComplete,
		UncachedInputTokens: 10, EstimatedCostNanoUSD: 50, EstimatedCostNano: 355, Settlement: cny,
		UpstreamCostState: pricing.CostStatePriced, UpstreamPricingCompleteness: pricing.CompletenessComplete, UpstreamCostNanoUSD: 20,
	}
	fixture.service.requestLogs = &recordingRequestLogReader{pages: []requestlog.Page{{Items: []requestlog.Record{record}}}}
	engine := gin.New()
	newTestServer(t, &config.Config{AuthKey: "test-auth-key"}, fixture.service).RegisterRoutes(engine)

	recorder := performRequestLogRequest(engine, "test-auth-key", "")
	var envelope struct {
		Data struct {
			Items []requestLogItemResponse `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || recorder.Code != http.StatusOK || len(envelope.Data.Items) != 1 {
		t.Fatalf("admin logs = %d %s", recorder.Code, recorder.Body.String())
	}
	// 20 nano USD × 7.1 = 142 nano CNY，按该请求冻结的汇率换算。
	if got := envelope.Data.Items[0].UpstreamCost; got == nil || *got != (requestLogUpstreamCostResponse{
		CostState: pricing.CostStatePriced, PricingCompleteness: pricing.CompletenessComplete, CostNanoUSD: "20", CostNano: "142",
	}) || envelope.Data.Items[0].EstimatedCostNanoUSD != "50" {
		t.Fatalf("admin upstream cost = %#v; body=%s", got, recorder.Body.String())
	}
}

// 日志详情把费用明细（按客户端模型）和上游明细（按渠道模型）原样返回给管理员；
// 费用明细没有渠道身份，不能按渠道注册表校验。
func TestRequestLogDetailReturnsChargeAndUpstreamReceipts(t *testing.T) {
	t.Parallel()
	identity := pricing.Identity{ChannelID: string(channel.OpenAI), ModelID: "upstream-model"}
	table, err := pricing.NewTableWithClientRules(
		[]pricing.Rule{{Identity: identity, Prices: pricing.Prices{Input: pricing.Price{NanoUSDPerMillion: 2_000_000_000, Set: true}}}},
		[]pricing.ClientRule{{ClientModel: "client-model", Prices: pricing.Prices{Input: pricing.Price{NanoUSDPerMillion: 5_000_000_000, Set: true}}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1000}}
	charge, chargeReceipt := table.QuoteClientModel("client-model", result, pricing.ModeStandard, pricing.DefaultPriceMultiplier)
	upstream, upstreamReceipt := table.QuoteForModeWithMultipliers(identity, result, pricing.ModeStandard, pricing.PriceMultipliers{
		Group: pricing.DefaultPriceMultiplier, AccessKey: pricing.DefaultPriceMultiplier,
	})
	const requestID = "00000000-0000-4000-8000-000000000802"
	reader := &recordingRequestLogReader{details: map[string]requestlog.Record{requestID: {
		RequestID: requestID, CompletedAtMS: 1_784_894_400_000,
		Protocol: protocol.OpenAICompletions, Operation: execution.OperationChatCompletion,
		ClientModel: "client-model", UpstreamModel: "upstream-model",
		Status: telemetry.RequestStatusSuccess, StatusCode: http.StatusOK, AttemptCount: 1,
		GroupID: 12, ChannelID: channel.OpenAI, CredentialID: 99,
		UsageState: usage.StateComplete, CostState: pricing.CostStatePriced, PricingCompleteness: pricing.CompletenessComplete,
		PricingMode: pricing.ModeStandard, UncachedInputTokens: 1000,
		EstimatedCostNanoUSD: int64(charge.EstimatedCostNanoUSD), EstimatedCostNano: int64(charge.EstimatedCostNanoUSD),
		Settlement:        pricing.Currency{Code: pricing.CurrencyUSD, RateMicros: 1_000_000},
		UpstreamCostState: pricing.CostStatePriced, UpstreamPricingCompleteness: pricing.CompletenessComplete,
		UpstreamCostNanoUSD: int64(upstream.EstimatedCostNanoUSD),
		Attempts: []requestlog.Attempt{{
			Sequence: 1, GroupID: 12, GroupName: "Primary", ChannelID: channel.OpenAI, CredentialID: 99,
			Operation: execution.OperationChatCompletion, RouteMode: channel.RouteNative, UpstreamModel: "upstream-model",
			DispatchState: execution.DispatchMaybeSent, ResponseStarted: true, UpstreamProtocol: protocol.OpenAICompletions,
			StatusCode: http.StatusOK, DurationMs: 10, FailureCategory: telemetry.FailureCategoryOK,
			Action: telemetry.ActionTerminate, Committed: true,
			PricingReceipt: chargeReceipt, UpstreamPricingReceipt: upstreamReceipt,
		}},
	}}}
	engine := newRequestLogTestEngine(t, reader)
	request := httptest.NewRequest(http.MethodGet, "/api/logs/"+requestID, nil)
	request.Header.Set("Authorization", "Bearer test-auth-key")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	type rule struct {
		ChannelID   string `json:"channel_id"`
		ModelID     string `json:"model_id"`
		ClientModel string `json:"client_model"`
	}
	var envelope struct {
		Data struct {
			Attempts []struct {
				PricingReceipt         struct{ Rule rule } `json:"pricing_receipt"`
				UpstreamPricingReceipt struct{ Rule rule } `json:"upstream_pricing_receipt"`
			} `json:"attempts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || recorder.Code != http.StatusOK || len(envelope.Data.Attempts) != 1 {
		t.Fatalf("log detail = %d %s", recorder.Code, recorder.Body.String())
	}
	attempt := envelope.Data.Attempts[0]
	if attempt.PricingReceipt.Rule != (rule{ClientModel: "client-model"}) ||
		attempt.UpstreamPricingReceipt.Rule != (rule{ChannelID: string(channel.OpenAI), ModelID: "upstream-model"}) {
		t.Fatalf("receipt rules = %+v; body=%s", attempt, recorder.Body.String())
	}
}
