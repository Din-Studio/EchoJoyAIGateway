package control

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/jev"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/pricing"
	"gpt-load/internal/requestaudit"
	"gpt-load/internal/requestlog"
)

// 分批审查的每次调用按冻结汇率各自换算，不把整条请求的护栏费用重复记到每一批。
func TestRequestAuditCallsSettleEachBatchSeparately(t *testing.T) {
	cny, err := pricing.NewCurrency(pricing.CurrencyCNY, 7_000_000)
	if err != nil {
		t.Fatal(err)
	}
	record := requestlog.Record{Settlement: cny, AuditCostNano: 21, RequestAudit: &requestaudit.Result{Status: "passed", Calls: []jev.Observation{
		{Called: true, EstimatedCostNanoUSD: 1}, {Called: true, EstimatedCostNanoUSD: 2}, {Reason: "no_candidate"},
	}}}
	calls := mapRequestAudit(record).Calls
	if calls[0].EstimatedCostNano != "7" || calls[1].EstimatedCostNano != "14" || calls[2].EstimatedCostNano != "0" {
		t.Fatalf("per-batch settled costs = %+v", calls)
	}
}

func TestRequestAuditLogQueryValidation(t *testing.T) {
	for _, status := range []string{"warned", "blocked", "incomplete"} {
		query, err := parseRequestLogQuery("audit_status=" + status + "&audit_rule=Private%20data")
		if err != nil || query.AuditStatus != status || query.AuditRule != "Private data" {
			t.Fatalf("guardrail query was rejected: %+v, %v", query, err)
		}
	}
	for _, raw := range []string{
		"audit_status=success", "audit_status=warned&audit_status=blocked",
		"audit_rule=a&audit_rule=b", "audit_rule=%0A", "audit_rule=%20", "audit_rule=" + strings.Repeat("x", 257),
	} {
		if _, err := parseRequestLogQuery(raw); err == nil {
			t.Fatalf("invalid guardrail query accepted: %s", raw)
		}
	}
}

func TestRequestAuditLogFiltersKeepAccessKeyScope(t *testing.T) {
	initControlI18n(t)
	fixture := newServiceFixture(t)
	key, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "guardrail log viewer"})
	if err != nil {
		t.Fatal(err)
	}
	reader := &recordingRequestLogReader{}
	fixture.service.requestLogs = reader
	engine := gin.New()
	newTestServer(t, &config.Config{AuthKey: "test-auth-key"}, fixture.service).RegisterRoutes(engine)
	response := performRequestLogRequest(engine, key.Key, "audit_status=warned&audit_rule=Privacy")
	if response.Code != http.StatusOK || len(reader.queries) != 1 {
		t.Fatalf("guardrail filters failed: %d %s", response.Code, response.Body)
	}
	query := reader.queries[0]
	if query.AccessKeyID == nil || *query.AccessKeyID != key.ID || query.AuditStatus != "warned" || query.AuditRule != "Privacy" {
		t.Fatalf("guardrail filters lost access-key scope: %+v", query)
	}
}
