package bifrost

import (
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"gpt-load/internal/execution"
)

// SDK cancellation can return before the worker publishes its billed terminal.
// Only immutable usage evidence crosses this attempt-local, one-slot handoff.
type cancellationUsageKey struct{}

type cancellationUsageCapture struct {
	stream   bool
	evidence chan *execution.UsageEvidence
}

const cancellationUsageWait = 100 * time.Millisecond

func captureCancellationUsage(ctx *schemas.BifrostContext, stream bool) <-chan *execution.UsageEvidence {
	capture := cancellationUsageCapture{stream: stream, evidence: make(chan *execution.UsageEvidence, 1)}
	ctx.SetValue(cancellationUsageKey{}, capture)
	return capture.evidence
}

func retainedCancellationUsage(failure *execution.ErrorEvidence, previous *execution.UsageEvidence, capture <-chan *execution.UsageEvidence) *execution.UsageEvidence {
	if failure == nil || (failure.Kind != execution.ErrorKindCanceled && failure.Kind != execution.ErrorKindTimeout) {
		return previous
	}
	timer := time.NewTimer(cancellationUsageWait)
	defer timer.Stop()
	select {
	case evidence := <-capture:
		return evidence
	case <-timer.C:
		return previous
	}
}

// One usage hook owns both Anthropic raw-event capture and cancellation evidence.
type usageCaptureHook struct{}

func (usageCaptureHook) GetName() string { return "gpt-load-usage" }
func (usageCaptureHook) Cleanup() error  { return nil }
func (usageCaptureHook) PreRequestHook(_ *schemas.BifrostContext, _ *schemas.BifrostRequest) error {
	return nil
}
func (usageCaptureHook) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}
func (usageCaptureHook) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, failure *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if enabled, _ := ctx.Value(anthropicUsageCaptureKey).(bool); enabled && resp != nil && resp.ResponsesStreamResponse != nil {
		ctx.Root().SetValue(schemas.BifrostContextKeyIntegrationType, "anthropic")
	}
	if capture, ok := ctx.Value(cancellationUsageKey{}).(cancellationUsageCapture); ok {
		var evidence *execution.UsageEvidence
		if failure != nil {
			evidence = errorUsageEvidence(failure, nil)
		} else if resp != nil {
			if resp.ChatResponse != nil && (!capture.stream || sdkStreamEndedNormally(ctx)) {
				evidence, _ = usageEvidenceFromResponses(resp.ChatResponse.Usage.ToResponsesResponseUsage())
			} else if resp.ResponsesResponse != nil {
				evidence, _ = usageEvidenceFromResponses(resp.ResponsesResponse.Usage)
			} else if response := resp.ResponsesStreamResponse; response != nil && response.Response != nil {
				switch response.Type {
				case schemas.ResponsesStreamResponseTypeCompleted, schemas.ResponsesStreamResponseTypeIncomplete, schemas.ResponsesStreamResponseTypeFailed:
					evidence, _ = usageEvidenceFromResponses(response.Response.Usage)
				}
			}
		}
		if evidence != nil {
			select {
			case capture.evidence <- evidence:
			default:
			}
		}
	}
	return resp, failure, nil
}
