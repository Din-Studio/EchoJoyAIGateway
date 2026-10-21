package ly

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

// pollInterval is how long the delivery loop sleeps once nothing is due.
const pollInterval = time.Second

// Run delivers the outbox until ctx ends. Every replica runs it; claim keeps
// two replicas from delivering the same report at once, and Lingying's
// request_id idempotency keeps a repeated delivery from charging twice.
func (s *Service) Run(ctx context.Context) {
	if s.client == nil {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		for ctx.Err() == nil && s.deliverBatch(ctx) > 0 {
		}
		timer.Reset(pollInterval)
	}
}

// deliverBatch delivers up to deliveryBatchSize due reports in one request
// and returns how many it claimed. A replica therefore holds at most one
// delivery in flight, which bounds the transactions Lingying opens.
func (s *Service) deliverBatch(ctx context.Context) int {
	log := logrus.WithField("component", "ly_billing")
	reports, err := s.outbox.claim(ctx, s.now(), s.deliveryBatchSize)
	if err != nil {
		if ctx.Err() == nil {
			log.WithError(err).Warn("claim Lingying usage report failed")
		}
		return 0
	}
	if len(reports) == 0 {
		return 0
	}
	payloads := make([]string, len(reports))
	for index, report := range reports {
		payloads[index] = report.Payload
	}
	results, err := s.client.reportUsageBatch(ctx, payloads)
	if err != nil {
		log.WithFields(logrus.Fields{"reports": len(reports), "detail": err.Error()}).
			Warn("Lingying usage batch delivery failed; it will be retried")
		for _, report := range reports {
			warnIfUpdateFailed(ctx, report, s.outbox.fail(ctx, report.RequestID, err.Error()))
		}
		return len(reports)
	}
	byRequestID := make(map[string]batchResult, len(results))
	for _, result := range results {
		byRequestID[result.RequestID] = result
	}
	var accepted []string
	for _, report := range reports {
		result, found := byRequestID[report.RequestID]
		detail := fmt.Sprintf("HTTP %d: %s", result.Status, result.Error)
		switch {
		case found && result.Status == http.StatusOK:
			accepted = append(accepted, report.RequestID)
		case found && (result.Status == http.StatusBadRequest || result.Status == http.StatusUnprocessableEntity):
			reportLog(report).WithField("detail", detail).Error("Lingying rejected a usage report; it is parked and will not be retried")
			warnIfUpdateFailed(ctx, report, s.outbox.park(ctx, report.RequestID, detail))
		default:
			if !found {
				detail = "missing from the Lingying batch response"
			}
			reportLog(report).WithField("detail", detail).Warn("Lingying usage report delivery failed; it will be retried")
			warnIfUpdateFailed(ctx, report, s.outbox.fail(ctx, report.RequestID, detail))
		}
	}
	// Reports left after a failed delete are delivered again, and Lingying
	// answers them as duplicates.
	if err := s.outbox.remove(ctx, accepted); err != nil && ctx.Err() == nil {
		log.WithError(err).WithField("reports", len(accepted)).Warn("update Lingying usage report failed")
	}
	return len(reports)
}

func reportLog(report pendingReport) *logrus.Entry {
	return logrus.WithFields(logrus.Fields{
		"component": "ly_billing", "request_id": report.RequestID, "attempt": report.Attempts,
	})
}

// warnIfUpdateFailed logs a failure to record a report's delivery outcome.
func warnIfUpdateFailed(ctx context.Context, report pendingReport, err error) {
	if err != nil && ctx.Err() == nil {
		reportLog(report).WithError(err).Warn("update Lingying usage report failed")
	}
}
