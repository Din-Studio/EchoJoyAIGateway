package gateway

import (
	"context"
	"net/http"
	"strings"

	"gpt-load/internal/integration/ly"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
	"gpt-load/internal/telemetry"
)

// ExternalBilling admits requests on endpoints marked ExternalBilling against
// an external billing account and records their usage once they finish.
// *ly.Service implements it.
type ExternalBilling interface {
	Admit(context.Context, ly.AdmitInput) ly.Decision
	Record(admission *ly.Admission, event telemetry.RequestEvent, chargeNano int64)
}

const projectIDHeader = "X-Project-Id"

var (
	reasonInsufficientBalance = reason{
		Status:  http.StatusPaymentRequired,
		Code:    "insufficient_balance",
		Message: "Insufficient balance.",
	}
	reasonBillingUnavailable = reason{
		Status:  http.StatusServiceUnavailable,
		Code:    "billing_unavailable",
		Message: "Billing service is unavailable.",
	}
)

// admitExternalBilling returns the rejection for a request its billing
// account does not allow, or nil. An allowed billed request leaves a hook on
// the recorder so emit records its usage. The recorder's client model must be
// set: a billed request is priced by its model, so one without a model is
// rejected rather than reported unpriceable.
func (handler *Handler) admitExternalBilling(
	ctx context.Context,
	recorder *requestRecorder,
	key state.AccessKeyView,
	credential ly.Credential,
	header http.Header,
	currency pricing.Currency,
) *reason {
	if handler.billing == nil {
		return nil
	}
	decision := handler.billing.Admit(ctx, ly.AdmitInput{
		KeyName:    key.Name,
		Credential: credential,
		ProjectID:  strings.TrimSpace(header.Get(projectIDHeader)),
		Currency:   currency.Code,
	})
	var rejection reason
	switch decision.Verdict {
	case ly.Allow:
		if decision.Admission == nil {
			return nil
		}
		if recorder.clientModel == "" {
			rejection = reasonInvalidProtocolRequest
			return &rejection
		}
		billing, admission := handler.billing, decision.Admission
		recorder.recordBilling = func(event telemetry.RequestEvent, chargeNano int64) {
			billing.Record(admission, event, chargeNano)
		}
		return nil
	case ly.InvalidKey:
		rejection = reasonInvalidAccessKey
	case ly.Unusable:
		rejection = reason{
			Status:  http.StatusForbidden,
			Code:    "billing_account_unusable",
			Message: "Billing account cannot be used: " + decision.Reason + ".",
		}
	case ly.Insufficient:
		rejection = reasonInsufficientBalance
	default:
		rejection = reasonBillingUnavailable
	}
	return &rejection
}
