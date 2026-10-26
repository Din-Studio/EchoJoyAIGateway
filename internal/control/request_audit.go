package control

import (
	"strconv"

	"gpt-load/internal/requestaudit"
	"gpt-load/internal/requestlog"
)

type requestAuditResponse struct {
	Status   string                 `json:"status"`
	Reason   string                 `json:"reason,omitempty"`
	Findings []requestaudit.Finding `json:"findings"`
	Calls    []auditCallResponse    `json:"calls"`
}

// auditCallResponse 与自动决策一致：*_nano 以请求冻结的币种计，*_nano_usd 以美元计。
// 分批审查会有多次调用，每次按请求冻结的汇率各自换算。
type auditCallResponse struct {
	Model                string `json:"model"`
	GroupName            string `json:"group_name"`
	Called               bool   `json:"called"`
	EstimatedCostNanoUSD string `json:"estimated_cost_nano_usd"`
	EstimatedCostNano    string `json:"estimated_cost_nano"`
	CostState            string `json:"cost_state"`
	PricingCompleteness  string `json:"pricing_completeness"`
}

func mapRequestAudit(record requestlog.Record) *requestAuditResponse {
	value := record.RequestAudit
	if value == nil {
		return nil
	}
	response := &requestAuditResponse{Status: value.Status, Reason: value.Reason, Findings: value.Findings, Calls: []auditCallResponse{}}
	for _, call := range value.Calls {
		settled := int64(0)
		if call.Called {
			settled = record.Settlement.Settle(call.EstimatedCostNanoUSD)
		}
		response.Calls = append(response.Calls, auditCallResponse{
			Model: call.UpstreamModel, GroupName: call.GroupName, Called: call.Called,
			EstimatedCostNanoUSD: strconv.FormatInt(call.EstimatedCostNanoUSD, 10),
			EstimatedCostNano:    strconv.FormatInt(settled, 10),
			CostState:            call.CostState, PricingCompleteness: call.PricingCompleteness,
		})
	}
	return response
}
