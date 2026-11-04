package gateway

import (
	"net/http"
	"testing"
	"time"

	"gpt-load/internal/health"
	"gpt-load/internal/integration/ly"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
)

// HTTP and WebSocket turns share this admission hook and recorder. Billing
// must retain the response timing origin even when cleanup delays emission.
func TestExternalBillingKeepsResponseStartAcrossCleanup(t *testing.T) {
	for _, value := range []protocol.Protocol{protocol.OpenAICompletions, protocol.OpenAIResponses, protocol.Anthropic} {
		t.Run(string(value), func(t *testing.T) {
			start := time.Unix(1000, 900_000_000)
			now := start.Add(-time.Second)
			sink := &recordingRequestLogSink{}
			recorder := newRequestRecorder(sink, fixedRequestID, now, 1, value, func() time.Time { return now })
			recorder.setClientModel("model")
			billing := &fakeExternalBilling{decision: billedAdmission}
			handler := &Handler{billing: billing}
			if failure := handler.admitExternalBilling(t.Context(), recorder, state.AccessKeyView{Name: "ly:k1"},
				ly.Credential{APIKey: "test"}, http.Header{}, recorder.currency); failure != nil {
				t.Fatal(failure)
			}
			now = start
			recorder.startTiming()
			recorder.usage.Pricing.EstimatedCostNanoUSD = 12_345
			now = start.Add(200 * time.Millisecond)
			recorder.completeResponse(UpstreamResult{StatusCode: http.StatusOK}, health.Decision{}, "model", -1)
			wantCharge := recorder.estimatedCostNano()
			now = now.Add(3 * time.Second)
			recorder.emit()
			recorder.emit()
			_, records := billing.snapshot()
			if len(records) != 1 {
				t.Fatalf("billing records = %d, want one", len(records))
			}
			got := records[0]
			if !got.startedAt.Equal(start) || got.event.DurationMs != 200 || got.chargeNano != wantCharge {
				t.Fatalf("start=%v duration=%d charge=%d, want %v/200/%d", got.startedAt, got.event.DurationMs, got.chargeNano, start, wantCharge)
			}
			if !got.event.CompletedAt.Equal(now) || !sink.snapshot()[0].CompletedAt.Equal(now) {
				t.Fatal("billing adaptation changed the request log emission timestamp")
			}
		})
	}
}
