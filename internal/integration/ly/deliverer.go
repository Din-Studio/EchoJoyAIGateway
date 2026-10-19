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
		for ctx.Err() == nil && s.deliverNext(ctx) {
		}
		timer.Reset(pollInterval)
	}
}

// deliverNext delivers one due report and reports whether there was one.
func (s *Service) deliverNext(ctx context.Context) bool {
	log := logrus.WithField("component", "ly_billing")
	report, found, err := s.outbox.claim(ctx, s.now())
	if err != nil {
		if ctx.Err() == nil {
			log.WithError(err).Warn("claim Lingying usage report failed")
		}
		return false
	}
	if !found {
		return false
	}
	log = log.WithFields(logrus.Fields{"request_id": report.RequestID, "attempt": report.Attempts})
	status, body, err := s.client.reportUsage(ctx, []byte(report.Payload))
	switch {
	case err == nil && status == http.StatusOK:
		err = s.outbox.remove(ctx, report.RequestID)
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		detail := fmt.Sprintf("HTTP %d: %s", status, body)
		log.WithField("detail", detail).Error("Lingying rejected a usage report; it is parked and will not be retried")
		err = s.outbox.park(ctx, report.RequestID, detail)
	default:
		detail := fmt.Sprintf("HTTP %d: %s", status, body)
		if err != nil {
			detail = err.Error()
		}
		log.WithField("detail", detail).Warn("Lingying usage report delivery failed; it will be retried")
		err = s.outbox.fail(ctx, report.RequestID, detail)
	}
	if err != nil && ctx.Err() == nil {
		log.WithError(err).Warn("update Lingying usage report failed")
	}
	return true
}
