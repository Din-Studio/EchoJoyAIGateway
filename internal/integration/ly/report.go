package ly

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/protocol"
	"gpt-load/internal/telemetry"
)

const enqueueTimeout = 5 * time.Second

// usageReport is the body of POST /internal/aigw/usage. prices is omitted:
// Lingying only displays it, and reconciliation joins on request_id.
type usageReport struct {
	RequestID    string      `json:"request_id"`
	UserID       string      `json:"user_id"`
	KeyID        string      `json:"key_id"`
	ProjectID    *string     `json:"project_id"`
	Model        string      `json:"model"`
	Upstream     string      `json:"upstream"`
	Protocol     string      `json:"protocol"`
	Status       string      `json:"status"`
	ErrorCode    string      `json:"error_code"`
	ErrorMessage string      `json:"error_message"`
	Usage        usageTokens `json:"usage"`
	Cost         string      `json:"cost"`
	StartedAt    string      `json:"started_at"`
	DurationMs   int64       `json:"duration_ms"`
}

type usageTokens struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

var reportProtocols = map[protocol.Protocol]string{
	protocol.OpenAICompletions: "openai",
	protocol.Anthropic:         "anthropic",
	protocol.OpenAIResponses:   "responses",
}

// Record stores the usage report of a request admitted by Admit. chargeNano is
// the request's charge in nano CNY, the same amount its cost limits consume.
// A report that cannot be stored is logged in full for manual replay.
func (s *Service) Record(admission *Admission, event telemetry.RequestEvent, chargeNano int64) {
	if admission == nil {
		return
	}
	payload, err := json.Marshal(newUsageReport(admission, event, chargeNano))
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), enqueueTimeout)
		defer cancel()
		err = s.outbox.enqueue(ctx, event.RequestID, payload, s.now())
	}
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"component": "ly_billing", "request_id": event.RequestID, "payload": string(payload),
		}).WithError(err).Error("Lingying usage report was not stored")
	}
}

func newUsageReport(admission *Admission, event telemetry.RequestEvent, chargeNano int64) usageReport {
	var projectID *string
	if admission.ProjectID != "" {
		projectID = &admission.ProjectID
	}
	status := "failed"
	if event.Status == telemetry.RequestStatusSuccess {
		status = "success"
	}
	tokens := event.Usage.Result.Tokens
	started := event.CompletedAt.Add(-time.Duration(event.DurationMs) * time.Millisecond)
	return usageReport{
		RequestID:    event.RequestID,
		UserID:       admission.UserID,
		KeyID:        admission.KeyID,
		ProjectID:    projectID,
		Model:        event.ClientModel,
		Upstream:     event.UpstreamModel,
		Protocol:     reportProtocols[event.Protocol],
		Status:       status,
		ErrorCode:    event.ErrorCode,
		ErrorMessage: event.ErrorSummary,
		Usage: usageTokens{
			InputTokens:      tokens.UncachedInput,
			OutputTokens:     tokens.Output,
			CacheReadTokens:  tokens.CacheRead,
			CacheWriteTokens: tokens.CacheWrite5M + tokens.CacheWrite1H + tokens.CacheWriteUnknown,
		},
		Cost:       formatYuan(chargeNano),
		StartedAt:  started.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		DurationMs: event.DurationMs,
	}
}

// formatYuan renders nano yuan as a decimal string with at most six decimal
// places, rounding half up like the request log display (logMoney in
// web/src/frontends/modern/features/logs/log-display.ts), so Lingying charges
// the amount the gateway shows.
func formatYuan(nano int64) string {
	if nano <= 0 {
		return "0"
	}
	micros := nano/1000 + (nano%1000+500)/1000
	whole := strconv.FormatInt(micros/1_000_000, 10)
	fraction := strings.TrimRight(strconv.FormatInt(1_000_000+micros%1_000_000, 10)[1:], "0")
	if fraction == "" {
		return whole
	}
	return whole + "." + fraction
}
