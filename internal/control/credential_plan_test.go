package control

import "testing"

func TestCredentialPlanHintDoesNotInventObservation(t *testing.T) {
	hint := withCredentialPlan(nil, ObservationPlanSummary{Name: "Pro 20x"})
	if hint == nil || hint.Snapshot.Plan.Name != "Pro 20x" || hint.State != "unavailable" || hint.ObservedAtMS != nil || len(hint.Snapshot.QuotaWindows) != 0 {
		t.Fatalf("unexpected hint: %+v", hint)
	}
	current := &CredentialObservationResponse{State: "success", Snapshot: &CredentialObservationSnapshot{Plan: ObservationPlanSummary{Name: "Plus"}}}
	if withCredentialPlan(current, ObservationPlanSummary{Name: "Pro 20x"}) != current {
		t.Fatal("token overwrote live observation")
	}
	if withCredentialPlan(nil, ObservationPlanSummary{}) != nil {
		t.Fatal("missing plan fabricated data")
	}
	empty := &CredentialObservationResponse{State: "unavailable", Snapshot: &CredentialObservationSnapshot{}}
	result := withCredentialPlan(empty, ObservationPlanSummary{Name: "Team"})
	if result.Snapshot.Plan.Name != "Team" || empty.Snapshot.Plan.Name != "" {
		t.Fatal("shared observation mutated")
	}
}
