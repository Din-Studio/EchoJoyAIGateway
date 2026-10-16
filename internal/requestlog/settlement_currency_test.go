package requestlog

import (
	"testing"
	"time"

	"gpt-load/internal/automodel"
	"gpt-load/internal/platform/redact"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/usage"
)

func pricedTestEvent(id string, currency pricing.Currency, costNanoUSD, decisionNanoUSD int64) models.RequestLog {
	event := testEvent(id)
	event.Currency = currency
	event.Usage.Result = usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 10, Output: 5}}
	event.Usage.Pricing.CostState = string(pricing.CostStatePriced)
	event.Usage.Pricing.PricingCompleteness = string(pricing.CompletenessComplete)
	event.Usage.Pricing.EstimatedCostNanoUSD = costNanoUSD
	if decisionNanoUSD > 0 {
		event.AutoDecision = &automodel.Decision{
			Selection: automodel.Selection{EntryID: "auto", EntryName: "auto", PresetID: "medium", TargetModel: "upstream-model"},
			Source:    "jev", Status: "selected", Provider: "openrouter", RequestedModel: "jev-latest",
			PromptVersion: automodel.PromptVersion, Called: true, CostState: "priced",
			PricingCompleteness: "complete", EstimatedCostNanoUSD: decisionNanoUSD,
		}
	}
	row, err := mapEvent(redact.New(), event)
	if err != nil {
		panic(err)
	}
	return row
}

type settledCostRow struct {
	Currency             string
	EstimatedCostNanoUSD int64
	EstimatedCostNano    int64
}

func TestSettlementCurrencyIsFrozenPerRequestAndAggregatedByCurrency(t *testing.T) {
	db := openRequestLogQueryDB(t)
	service := newRequestLogTestService(db)
	cny, err := pricing.NewCurrency(pricing.CurrencyCNY, 7_100_000)
	if err != nil {
		t.Fatal(err)
	}
	rows := []models.RequestLog{
		pricedTestEvent(aggregationRequestID(1), pricing.USDCurrency, 8_000_000_000, 0),
		pricedTestEvent(aggregationRequestID(2), cny, 1_000_000_000, 4),
	}
	// 答复与决策分别按冻结汇率取整：4 nano USD × 7.1 = 28.4 → 28。
	if rows[1].Currency != "CNY" || rows[1].ExchangeRateMicros != 7_100_000 ||
		rows[1].EstimatedCostNanoUSD != 1_000_000_000 || rows[1].EstimatedCostNano != 7_100_000_000 ||
		rows[1].DecisionCostNanoUSD != 4 || rows[1].DecisionCostNano != 28 {
		t.Fatalf("CNY row = %+v, want USD and CNY amounts frozen at 7.1", rows[1])
	}
	unsettled := testEvent(aggregationRequestID(3))
	unsettled.Currency = pricing.Currency{}
	if _, err := mapEvent(redact.New(), unsettled); err == nil {
		t.Fatal("mapEvent accepted a request without a settlement currency")
	}
	if err := service.writer.WriteBatch(t.Context(), rows); err != nil {
		t.Fatal(err)
	}

	// 两种币种落在同一小时桶、同一维度，聚合必须按币种分行。
	var stats []settledCostRow
	if err := db.Model(&models.UsageStat{}).Select("currency, estimated_cost_nano_usd, estimated_cost_nano").
		Order("currency ASC").Scan(&stats).Error; err != nil {
		t.Fatal(err)
	}
	wantStats := []settledCostRow{{"CNY", 1_000_000_000, 7_100_000_000}, {"USD", 8_000_000_000, 8_000_000_000}}
	if len(stats) != 2 || stats[0] != wantStats[0] || stats[1] != wantStats[1] {
		t.Fatalf("usage_stats = %+v, want %+v", stats, wantStats)
	}
	var decisions []settledCostRow
	if err := db.Model(&models.AutoDecisionUsageStat{}).Select("currency, estimated_cost_nano_usd, estimated_cost_nano").
		Scan(&decisions).Error; err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0] != (settledCostRow{"CNY", 4, 28}) {
		t.Fatalf("auto_decision_usage_stats = %+v, want one CNY row", decisions)
	}

	completedAt := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	report, err := service.QueryUsage(t.Context(), UsageQuery{
		FromMS: completedAt.Add(-time.Hour).UnixMilli(), ToMS: completedAt.Add(2 * time.Hour).UnixMilli(),
		Currency: pricing.CurrencyCNY,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.RequestCount != 2 || report.Summary.EstimatedCostNanoUSD != 9_000_000_004 ||
		report.Summary.EstimatedCostNano != 7_100_000_028 {
		t.Fatalf("summary = %+v, want both requests in USD and only the CNY request in CNY", report.Summary)
	}
	if len(report.OtherCurrencies) != 1 || report.OtherCurrencies[0] != pricing.CurrencyUSD {
		t.Fatalf("other currencies = %v, want [USD]", report.OtherCurrencies)
	}

	// 美元请求的本币金额 8e9 也超过下限，只有限定币种才能排除它。
	minimum := int64(7_000_000_000)
	page, err := service.List(t.Context(), ListQuery{Limit: 10, CostMinNano: &minimum, CostCurrency: pricing.CurrencyCNY})
	if err != nil || len(page.Items) != 1 || page.Items[0].Settlement != cny || page.Items[0].EstimatedCostNano != 7_100_000_000 {
		t.Fatalf("CNY cost filter = %+v, %v; want only the CNY request", page.Items, err)
	}
}
