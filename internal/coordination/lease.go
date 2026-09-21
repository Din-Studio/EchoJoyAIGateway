package coordination

import (
	"context"
	"fmt"
	"time"
)

// Lease claims a period of a globally scheduled task, so that N instances
// running the same periodic sweep produce one execution per period instead of
// N. It is deliberately not a general mutex, and three properties say why:
//
//   - The key's TTL equals the task's period. What is claimed is the period,
//     not the execution; the claim expires when the next period begins.
//   - A finished execution does not release the claim. That is what keeps the
//     remaining instances out of the same period rather than letting them run
//     the moment the holder is done.
//   - There is no renewal and no fencing token. The sweeps this guards are
//     already idempotent and concurrency-safe at the database: window-bounded
//     deletes (internal/requestlog/retention.go), conditional updates on
//     compacted_at_ms (internal/control/operation_recovery.go) and on stage
//     status (internal/control/credential_stages.go). Fencing would mean
//     adding token columns or predicates to those writes to re-establish an
//     invariant the database already holds.
//
// The accepted cost is that a holder crashing mid-sweep loses that period; the
// next period is claimed normally by whichever instance ticks first.
//
// Claims are single-key operations. There is no hash tag convention here and
// later phases should not assume one.
type Lease struct {
	client *Client
	holder string
}

// NewLease binds periodic task claims to a Redis client. The holder identity
// is random per process: it exists so an operator can read which instance owns
// the current period, never to authorize a write.
//
// A failing random source is returned rather than swallowed. An instance that
// cannot identify itself cannot claim a period, and starting it in that state
// would silently restore the duplicated sweeps this type exists to remove.
func NewLease(client *Client) (*Lease, error) {
	holder, err := NewInstanceIdentity()
	if err != nil {
		return nil, err
	}
	return &Lease{client: client, holder: holder}, nil
}

// Holder reports this process's lease identity.
func (lease *Lease) Holder() string {
	return lease.holder
}

// Claim attempts to take the current period of task for this instance,
// reporting whether it may run the task now. The period doubles as the key's
// TTL, so the claim and the period it covers expire together.
func (lease *Lease) Claim(ctx context.Context, task string, period time.Duration) (bool, error) {
	claimed, err := lease.client.Redis().SetNX(ctx, leaseKey(task), lease.holder, period).Result()
	if err != nil {
		return false, fmt.Errorf("claim task lease: %w", err)
	}
	return claimed, nil
}

func leaseKey(task string) string {
	return Key("lease", task)
}
