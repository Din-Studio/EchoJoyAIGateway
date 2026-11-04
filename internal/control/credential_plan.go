package control

import "gpt-load/internal/storage/models"

// 仅补充展示；不制造额度观测成功、观测时间或任何可用额度。
func withCredentialPlan(current *CredentialObservationResponse, plan ObservationPlanSummary) *CredentialObservationResponse {
	if plan.Name == "" || current != nil && current.Snapshot != nil && current.Snapshot.Plan.Name != "" {
		return current
	}
	hint := CredentialObservationSnapshot{
		Plan:         plan,
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
