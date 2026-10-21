package ly

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

const (
	// maxBackoff caps the delay between deliveries of one report.
	maxBackoff = 5 * time.Minute
	// maxLastErrorBytes bounds the stored failure detail.
	maxLastErrorBytes = 1024
)

// outbox is the ly_usage_reports table (migration 0023). A row is a report not
// yet accepted by Lingying; next_attempt_at_ms NULL parks a rejected report.
// It uses database/sql because the data plane must not depend on the ORM.
type outbox struct {
	db *sql.DB
}

type pendingReport struct {
	RequestID string
	Payload   string
	Attempts  int
}

func (o outbox) enqueue(ctx context.Context, requestID string, payload []byte, now time.Time) error {
	_, err := o.db.ExecContext(ctx,
		`INSERT INTO ly_usage_reports (request_id, payload, attempts, next_attempt_at_ms, last_error)
		VALUES ($1, $2, 0, $3, '') ON CONFLICT (request_id) DO NOTHING`,
		requestID, string(payload), now.UnixMilli(),
	)
	return err
}

// claim takes up to limit due reports, oldest first, and in the same
// statement schedules their next attempt: reportTimeout plus an exponential
// backoff of 1s, 2s, 4s... up to maxBackoff. A replica that dies mid-delivery
// therefore leaves the reports due again later, and another replica cannot
// take them while the delivery may still be in flight.
func (o outbox) claim(ctx context.Context, now time.Time, limit int) ([]pendingReport, error) {
	rows, err := o.db.QueryContext(ctx,
		`UPDATE ly_usage_reports
		SET attempts = attempts + 1,
			next_attempt_at_ms = $1 + LEAST(1000 * POWER(2, LEAST(attempts, 20)), $2)::bigint
		WHERE request_id IN (
			SELECT request_id FROM ly_usage_reports
			WHERE next_attempt_at_ms <= $3
			ORDER BY next_attempt_at_ms
			LIMIT $4
			FOR UPDATE SKIP LOCKED)
		RETURNING request_id, payload, attempts`,
		now.Add(reportTimeout).UnixMilli(), maxBackoff.Milliseconds(), now.UnixMilli(), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reports []pendingReport
	for rows.Next() {
		var report pendingReport
		if err := rows.Scan(&report.RequestID, &report.Payload, &report.Attempts); err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

// remove deletes the reports Lingying accepted.
func (o outbox) remove(ctx context.Context, requestIDs []string) error {
	if len(requestIDs) == 0 {
		return nil
	}
	_, err := o.db.ExecContext(ctx, `DELETE FROM ly_usage_reports WHERE request_id = ANY($1)`, requestIDs)
	return err
}

// fail records why a delivery failed; its retry time was set by claim.
func (o outbox) fail(ctx context.Context, requestID, detail string) error {
	_, err := o.db.ExecContext(ctx,
		`UPDATE ly_usage_reports SET last_error = $1 WHERE request_id = $2`, truncate(detail), requestID,
	)
	return err
}

// park stops delivering a report Lingying rejected permanently.
func (o outbox) park(ctx context.Context, requestID, detail string) error {
	_, err := o.db.ExecContext(ctx,
		`UPDATE ly_usage_reports SET last_error = $1, next_attempt_at_ms = NULL WHERE request_id = $2`,
		truncate(detail), requestID,
	)
	return err
}

func truncate(detail string) string {
	if len(detail) > maxLastErrorBytes {
		// Drop a rune cut in half; PostgreSQL text must be valid UTF-8.
		return strings.ToValidUTF8(detail[:maxLastErrorBytes], "")
	}
	return detail
}
