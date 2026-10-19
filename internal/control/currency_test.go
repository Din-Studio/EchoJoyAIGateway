package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
)

func updateCurrencySettings(t *testing.T, fixture serviceFixture, settings string) (SettingsResponse, error) {
	t.Helper()
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settings), &values); err != nil {
		t.Fatal(err)
	}
	return fixture.service.UpdateSettings(t.Context(), SettingsUpdateRequest{Settings: values})
}

func TestCurrencySwitchConvertsCostLimitsAndResetsUsage(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	created, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{
		Name: "limited",
		CostLimitRules: OptionalAccessKeyCostLimitRules{Set: true, Values: []AccessKeyCostLimitRuleRequest{
			{Kind: accessquota.KindTotal, Limit: "10"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&models.AccessKeyCostLimitState{}).
		Where("rule_id = ?", created.CostLimitRules[0].ID).
		Update("used_nano", int64(3_000_000_000)).Error; err != nil {
		t.Fatal(err)
	}

	response, err := updateCurrencySettings(t, fixture, `{"currency":"CNY","exchange_rate":"7.1"}`)
	if err != nil {
		t.Fatalf("switch to CNY: %v", err)
	}
	if response.Values.Currency != pricing.CurrencyCNY || response.Values.ExchangeRate == nil || *response.Values.ExchangeRate != "7.1" {
		t.Fatalf("settings currency = %s %v", response.Values.Currency, response.Values.ExchangeRate)
	}
	var rule models.AccessKeyCostLimitRule
	if err := fixture.db.Take(&rule, created.CostLimitRules[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	var state models.AccessKeyCostLimitState
	if err := fixture.db.Take(&state, rule.ID).Error; err != nil {
		t.Fatal(err)
	}
	if rule.Currency != "CNY" || rule.LimitNano != 71_000_000_000 || rule.RuleRevision != 2 ||
		state.RuleRevision != 2 || state.UsedNano != 0 {
		t.Fatalf("after switch rule = %+v state = %+v, want ¥71 limit on a fresh revision", rule, state)
	}

	// 只改汇率不改币种时，限额和已用额度都保持本币不变。
	if err := fixture.db.Model(&models.AccessKeyCostLimitState{}).
		Where("rule_id = ?", rule.ID).Update("used_nano", int64(5)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := updateCurrencySettings(t, fixture, `{"exchange_rate":"7.3"}`); err != nil {
		t.Fatalf("change rate: %v", err)
	}
	if err := fixture.db.Take(&rule, rule.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Take(&state, rule.ID).Error; err != nil {
		t.Fatal(err)
	}
	if rule.LimitNano != 71_000_000_000 || rule.RuleRevision != 2 || state.UsedNano != 5 {
		t.Fatalf("after rate change rule = %+v state = %+v, want limit and usage unchanged", rule, state)
	}

	// 新建的规则以当前系统货币计。
	second, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{
		Name: "second",
		CostLimitRules: OptionalAccessKeyCostLimitRules{Set: true, Values: []AccessKeyCostLimitRuleRequest{
			{Kind: accessquota.KindTotal, Limit: "100"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := second.CostLimitRules[0]; got.Currency != pricing.CurrencyCNY || got.Limit != "100" {
		t.Fatalf("new rule = %+v, want ¥100", got)
	}
}

func TestCurrencySettingsRejectInvalidCombinationWithoutWriting(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	for _, settings := range []string{
		`{"currency":"CNY"}`,
		`{"exchange_rate":"7.1"}`,
		`{"currency":"EUR","exchange_rate":"1.1"}`,
	} {
		if _, err := updateCurrencySettings(t, fixture, settings); err == nil {
			t.Fatalf("%s accepted", settings)
		}
	}
	var count int64
	if err := fixture.db.Model(&models.SystemSetting{}).
		Where("key IN ?", []string{"currency", "exchange_rate"}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected currency settings left %d rows", count)
	}
}

func TestAutoModelPricesUseSystemCurrencyAtTheBoundary(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	if _, err := updateCurrencySettings(t, fixture, `{"currency":"CNY","exchange_rate":"7"}`); err != nil {
		t.Fatal(err)
	}
	response, err := updateCurrencySettings(t, fixture, `{"auto_model":{"enabled":false,"provider":"typesafe","model":"jev-latest","api_key":"","timeout_seconds":2,"input_price":"0.7","output_price":"0","models":[]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if response.Values.AutoModel.InputPrice != "0.7" {
		t.Fatalf("auto model input price = %q, want ¥0.7", response.Values.AutoModel.InputPrice)
	}
	if got := fixture.service.manager.Current().AutoModels.Config().InputPrice; got != "0.1" {
		t.Fatalf("stored decision input price = %q, want $0.1", got)
	}
}

func TestModelPriceHTTPShowsAndAcceptsPricesInSystemCurrency(t *testing.T) {
	t.Parallel()
	fixture, engine, row := newModelPriceHTTPFixture(t, true)
	if _, err := updateCurrencySettings(t, fixture, `{"currency":"CNY","exchange_rate":"7"}`); err != nil {
		t.Fatal(err)
	}
	list := serveModelPriceHTTPRequest(engine, http.MethodGet,
		"/api/model-prices?usage=all&status=all&page=1&page_size=20", "", authTestKey)
	if list.Code != http.StatusOK {
		t.Fatalf("GET /api/model-prices status = %d: %s", list.Code, list.Body.String())
	}
	items, _ := decodeModelPriceHTTPData(t, list)["items"].([]any)
	item, _ := items[0].(map[string]any)
	prices, _ := item["prices"].(map[string]any)
	// 存储为 $2.5，按汇率 7 显示为 ¥17.5。
	if item["currency"] != "CNY" || prices["input"] != "17.5" {
		t.Fatalf("listed price = %v %v, want CNY 17.5", item["currency"], item["prices"])
	}

	update := serveModelPriceHTTPRequest(engine, http.MethodPut,
		fmt.Sprintf("/api/model-prices/%d", row.ID), modelPriceHTTPUpdateBody(`"7"`, "false"), authTestKey)
	if update.Code != http.StatusOK {
		t.Fatalf("PUT /api/model-prices/:id status = %d: %s", update.Code, update.Body.String())
	}
	updated, _ := decodeModelPriceHTTPData(t, update)["prices"].(map[string]any)
	var stored models.ModelPrice
	if err := fixture.db.Take(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if updated["input"] != "7" || stored.InputPriceNanoUSDPerMillionTokens == nil ||
		*stored.InputPriceNanoUSDPerMillionTokens != 1_000_000_000 {
		t.Fatalf("updated price = %v, stored = %v; want ¥7 shown and $1 stored", updated, stored.InputPriceNanoUSDPerMillionTokens)
	}
}

// ¥30000 ÷ 7.3 不能整除成 nano USD；读回时仍应显示为输入的 30000，而不是带舍入尾数。
func TestModelPriceHTTPReadsBackPricesEnteredInSystemCurrency(t *testing.T) {
	t.Parallel()
	fixture, engine, row := newModelPriceHTTPFixture(t, true)
	if _, err := updateCurrencySettings(t, fixture, `{"currency":"CNY","exchange_rate":"7.3"}`); err != nil {
		t.Fatal(err)
	}
	update := serveModelPriceHTTPRequest(engine, http.MethodPut,
		fmt.Sprintf("/api/model-prices/%d", row.ID), modelPriceHTTPUpdateBody(`"30000"`, "false"), authTestKey)
	if update.Code != http.StatusOK {
		t.Fatalf("PUT /api/model-prices/:id status = %d: %s", update.Code, update.Body.String())
	}
	updated, _ := decodeModelPriceHTTPData(t, update)["prices"].(map[string]any)
	var stored models.ModelPrice
	if err := fixture.db.Take(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if updated["input"] != "30000" || stored.InputPriceNanoUSDPerMillionTokens == nil ||
		*stored.InputPriceNanoUSDPerMillionTokens != 4_109_589_041_096 {
		t.Fatalf("updated price = %v, stored = %v; want ¥30000 shown", updated, stored.InputPriceNanoUSDPerMillionTokens)
	}
}
