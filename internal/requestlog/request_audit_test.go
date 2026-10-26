package requestlog

import (
	"testing"

	"gpt-load/internal/automodel"
	"gpt-load/internal/jev"
	"gpt-load/internal/platform/redact"
	"gpt-load/internal/pricing"
	"gpt-load/internal/requestaudit"
	"gpt-load/internal/storage/models"
)

func guardrailCall(groupID uint, cost int64, completeness string) jev.Observation {
	return jev.Observation{Called: true, UpstreamModel: "jev-model", GroupID: groupID, GroupName: "jev", ChannelID: "jev", CredentialID: groupID, CostState: "priced", PricingCompleteness: completeness, EstimatedCostNanoUSD: cost}
}

func TestGuardrailCallIsMappedBesideDecisionInSettlementCurrency(t *testing.T) {
	event := testEvent("00000000-0000-4000-8000-000000000596")
	cny, err := pricing.NewCurrency(pricing.CurrencyCNY, 7_100_000)
	if err != nil {
		t.Fatal(err)
	}
	event.Currency = cny
	event.RequestAudit = &requestaudit.Result{Status: "warned", Findings: []requestaudit.Finding{{RuleID: "pii", Name: "PII", Action: requestaudit.ActionWarn}}, Calls: []jev.Observation{
		{Reason: "no_candidate", CostState: "not_applicable", PricingCompleteness: "not_applicable"},
		guardrailCall(9, 1_000_000, "complete"),
	}}
	row := mustMapEvent(t, redact.New(), event)
	if row.AuditModel != "jev-model" || row.AuditGroupID != 9 || row.AuditChannelID != "jev" || row.AuditCredentialID != 9 ||
		row.AuditCostNanoUSD != 1_000_000 || row.AuditCostNano != 7_100_000 || row.AuditPricingCompleteness != "complete" || len(row.RequestAudit) == 0 {
		t.Fatalf("guardrail call columns = %#v", row)
	}

	// 分批审查的后续调用并入同一行：费用求和，凭据归第一次调用，完整度不一致记为 partial。
	event.RequestAudit.Calls = append(event.RequestAudit.Calls, guardrailCall(10, 2_000_000, "complete"), guardrailCall(9, 1, "unavailable"))
	row = mustMapEvent(t, redact.New(), event)
	if row.AuditGroupID != 9 || row.AuditCredentialID != 9 || row.AuditCostNanoUSD != 3_000_001 || row.AuditCostNano != 21_300_007 ||
		row.AuditPricingCompleteness != "partial" {
		t.Fatalf("batched guardrail calls = %#v", row)
	}
}

func TestGuardrailFeeIsCountedOnceAndAttributedToItsRoute(t *testing.T) {
	db := openRequestLogQueryDB(t)
	event := testEvent("00000000-0000-4000-8000-000000000597")
	event.AutoDecision = &automodel.Decision{Called: true, UpstreamModel: "jev-model", GroupID: 8, ChannelID: "jev", CredentialID: 8, CostState: "priced", PricingCompleteness: "complete", EstimatedCostNanoUSD: 3}
	event.RequestAudit = &requestaudit.Result{Status: "passed", Findings: []requestaudit.Finding{}, Calls: []jev.Observation{guardrailCall(9, 5, "complete")}}
	row := mustMapEvent(t, redact.New(), event)
	writer := &gormBatchWriter{db: db}
	for range 2 {
		if err := writer.WriteBatch(t.Context(), []models.RequestLog{row}); err != nil {
			t.Fatal(err)
		}
	}
	answer := row.EstimatedCostNanoUSD
	service := newRequestLogTestService(db)
	// 2 秒窗口读 request_logs 投影，1 天窗口读小时聚合表；两侧口径一致。
	for _, span := range []int64{2000, 86_400_000} {
		query := UsageQuery{Currency: pricing.CurrencyUSD, FromMS: row.CompletedAtMS - 1000, ToMS: row.CompletedAtMS + span}
		report, err := service.QueryUsage(t.Context(), query)
		if err != nil || report.Summary.EstimatedCostNanoUSD != answer+8 || report.Summary.EstimatedCostNano != answer+8 || report.Summary.RequestCount != 1 {
			t.Fatalf("span %d combined usage = %#v, %v", span, report.Summary, err)
		}
		group := uint(9)
		query.GroupID = &group
		report, err = service.QueryUsage(t.Context(), query)
		if err != nil || report.Summary.EstimatedCostNanoUSD != 5 || report.Summary.RequestCount != 0 {
			t.Fatalf("span %d guardrail attribution = %#v, %v", span, report.Summary, err)
		}
	}
	page, err := service.List(t.Context(), ListQuery{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].TotalPricing.EstimatedCostNanoUSD != answer+8 ||
		page.Items[0].AuditCostNano != 5 || page.Items[0].RequestAudit == nil {
		t.Fatalf("guardrail log = %#v, %v", page, err)
	}
}

func TestUnpricedGuardrailIsCountedOnceAcrossSources(t *testing.T) {
	db := openRequestLogQueryDB(t)
	event := testEvent("00000000-0000-4000-8000-000000000598")
	call := guardrailCall(9, 0, "unavailable")
	call.CostState = "unpriced"
	event.RequestAudit = &requestaudit.Result{Status: "passed", Findings: []requestaudit.Finding{}, Calls: []jev.Observation{call}}
	row := mustMapEvent(t, redact.New(), event)
	if err := (&gormBatchWriter{db: db}).WriteBatch(t.Context(), []models.RequestLog{row}); err != nil {
		t.Fatal(err)
	}
	service := newRequestLogTestService(db)
	for _, span := range []int64{2000, 86_400_000} {
		report, err := service.QueryUsage(t.Context(), UsageQuery{Currency: pricing.CurrencyUSD, FromMS: row.CompletedAtMS - 1000, ToMS: row.CompletedAtMS + span})
		if err != nil || report.Summary.UnpricedRequestCount != 1 {
			t.Fatalf("span %d unpriced = %#v, %v", span, report.Summary, err)
		}
	}
}
