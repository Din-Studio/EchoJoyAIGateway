package ly

import (
	"testing"
	"time"

	"gpt-load/internal/protocol"
	"gpt-load/internal/telemetry"
	"gpt-load/internal/usage"
)

func TestFormatYuanRoundsHalfUpToSixDecimals(t *testing.T) {
	for nano, want := range map[int64]string{
		0:                         "0",
		-5:                        "0",
		499:                       "0",
		500:                       "0.000001",
		1_000:                     "0.000001",
		1_499:                     "0.000001",
		481_004:                   "0.000481", // the request log shows ¥0.000481
		12_345_000:                "0.012345",
		12_345_678:                "0.012346",
		2_000_000_000:             "2",
		10_500_000_000:            "10.5",
		9_223_372_036_854_775_807: "9223372036.854776",
	} {
		if got := formatYuan(nano); got != want {
			t.Errorf("formatYuan(%d) = %q, want %q", nano, got, want)
		}
	}
}

func TestNewUsageReportMapsTheRequestEvent(t *testing.T) {
	completed := time.Date(2026, 10, 19, 10, 0, 1, 623_000_000, time.UTC)
	event := telemetry.RequestEvent{
		RequestID:     "req-1",
		CompletedAt:   completed,
		DurationMs:    1500,
		Protocol:      protocol.Anthropic,
		ClientModel:   "claude-sonnet",
		UpstreamModel: "anthropic/claude-sonnet-4.5",
		Status:        telemetry.RequestStatusIncomplete,
		ErrorCode:     "stream_interrupted",
		ErrorSummary:  "client went away",
		Usage: telemetry.UsageObservation{Result: usage.Result{Tokens: usage.Tokens{
			UncachedInput: 1000, Output: 500, CacheRead: 70,
			CacheWrite5M: 10, CacheWrite1H: 20, CacheWriteUnknown: 3,
		}}},
	}

	got := newUsageReport(&Admission{UserID: "u1", KeyID: "k1"}, event, 12_345_678)

	want := usageReport{
		RequestID: "req-1", UserID: "u1", KeyID: "k1", Model: "claude-sonnet",
		Upstream: "anthropic/claude-sonnet-4.5", Protocol: "anthropic", Status: "failed",
		ErrorCode: "stream_interrupted", ErrorMessage: "client went away",
		Usage: usageTokens{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 70, CacheWriteTokens: 33},
		Cost:  "0.012346", StartedAt: "2026-10-19T10:00:00.123Z", DurationMs: 1500,
	}
	if got.ProjectID != nil {
		t.Fatalf("ProjectID = %q, want null without X-Project-Id", *got.ProjectID)
	}
	if got != want {
		t.Fatalf("newUsageReport() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestNewUsageReportStatusAndProtocol(t *testing.T) {
	for _, test := range []struct {
		status   telemetry.RequestStatus
		protocol protocol.Protocol
		want     [2]string
	}{
		{telemetry.RequestStatusSuccess, protocol.OpenAICompletions, [2]string{"success", "openai"}},
		{telemetry.RequestStatusError, protocol.OpenAIResponses, [2]string{"failed", "responses"}},
		{telemetry.RequestStatusCanceled, protocol.Anthropic, [2]string{"failed", "anthropic"}},
	} {
		report := newUsageReport(&Admission{UserID: "u1", ProjectID: "p1"},
			telemetry.RequestEvent{Status: test.status, Protocol: test.protocol}, 0)
		if [2]string{report.Status, report.Protocol} != test.want || report.ProjectID == nil || *report.ProjectID != "p1" {
			t.Errorf("status %s protocol %s -> %q %q project %v, want %v",
				test.status, test.protocol, report.Status, report.Protocol, report.ProjectID, test.want)
		}
	}
}
