package control

import (
	"gpt-load/internal/storage/models"
	"gpt-load/internal/subscription/providers/codex"
)

// 仅补充展示；不制造额度观测成功、观测时间或任何可用额度。
func withCredentialPlan(current *CredentialObservationResponse, plan string) *CredentialObservationResponse {
	if plan == "" || current != nil && current.Snapshot != nil && current.Snapshot.Plan.Name != "" {
		return current
	}
	summary := codex.NormalizePlan(plan)
	if summary.Name == "" {
		return current
	}
	hint := CredentialObservationSnapshot{
		Plan:         ObservationPlanSummary{Name: summary.Name, Level: string(summary.Level)},
		QuotaWindows: []ObservationQuotaWindow{},
	}
	result := CredentialObservationResponse{State: string(models.CredentialObservationUnavailable)}
	if current != nil {
		result = *current
	}
	if result.Snapshot == nil {
		result.Snapshot = &hint
	} else {
		snapshot := *result.Snapshot
		snapshot.Plan = hint.Plan
		result.Snapshot = &snapshot
	}
	return &result
}
