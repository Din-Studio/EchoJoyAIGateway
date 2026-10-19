// Package ly connects the gateway to the Lingying gateway (灵影网关), which owns
// the billing accounts of its users' personal API keys. Those keys are mirrored
// into this gateway with the name "ly:<key_id>".
//
// The package is the only place that knows the Lingying billing callback
// contract (docs/lingying-billing-callback-plan.md):
//
//   - Admit checks the account balance before a billable request is forwarded.
//   - Record stores the finished request's usage report in a durable outbox.
//   - Run delivers the outbox to Lingying, retrying with the same request_id.
package ly

import (
	"database/sql"
	"time"

	"gpt-load/internal/platform/config"
)

// keyNamePrefix marks access keys mirrored from Lingying.
const keyNamePrefix = "ly:"

// Service implements admission, usage recording and outbox delivery.
type Service struct {
	client *client // nil when Lingying billing is not configured
	outbox outbox
	now    func() time.Time
}

// New builds the service. db holds the outbox table. Without a configured
// endpoint, Lingying keys are rejected (fail closed).
func New(cfg config.LingyingBillingConfig, db *sql.DB) *Service {
	service := &Service{outbox: outbox{db: db}, now: time.Now}
	if cfg.BaseURL != "" && cfg.Token != "" {
		service.client = newClient(cfg.BaseURL, cfg.Token)
	}
	return service
}
