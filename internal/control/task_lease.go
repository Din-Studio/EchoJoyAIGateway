package control

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/platform/utils"
)

// Task names are a cross-instance contract as well as the Redis key names an
// operator reads. Renaming one resets a period rather than renaming a concept,
// so these strings stay fixed.
const (
	taskRequestLogRetention    = "requestlog_retention"
	taskCredentialStageCleanup = "credential_stage_cleanup"
	taskOperationCompaction    = "operation_compaction"
)

// TaskLease is the control plane's view of claiming one period of a globally
// scheduled database sweep. It claims the period, not the execution: a claim
// outlives the sweep that took it, which is what keeps the other instances out
// of the same period. It is not a mutex and must not be used as one.
//
// Single-instance deployments leave it unset, and every claim then succeeds
// without reaching Redis.
type TaskLease interface {
	Claim(ctx context.Context, task string, period time.Duration) (bool, error)
	// Holder identifies this instance in the lease value, so a log line here
	// and a GET on the Redis key name the same process.
	Holder() string
}

// claimTask owns the whole claiming protocol: whether a sweep runs in this
// period, and what gets reported about it.
//
// A failed claim is treated as "not claimed". When it is unknown whether
// another instance is already sweeping, the cost of a duplicated sweep on the
// shared database is higher than the cost of cleaning up one period later, and
// the next period retries from scratch.
func claimTask(ctx context.Context, lease TaskLease, task string, period time.Duration) bool {
	if lease == nil {
		return true
	}
	claimed, err := lease.Claim(ctx, task, period)
	if err != nil {
		logTaskLeaseEvent(logrus.WarnLevel, logrus.Fields{
			"event": "task.claim_failed", "task": task, "error": err.Error(),
		}, "Task lease claim failed; skipping this period")
		return false
	}
	if !claimed {
		logTaskLeaseEvent(logrus.DebugLevel, logrus.Fields{
			"event": "task.lease_skipped", "task": task, "holder": lease.Holder(),
		}, "Task period already claimed by another instance")
		return false
	}
	logTaskLeaseEvent(logrus.InfoLevel, logrus.Fields{
		"event": "task.lease_claimed", "task": task, "holder": lease.Holder(),
		"period_ms": period.Milliseconds(),
	}, "Task period claimed")
	return true
}

// logTaskLeaseEvent emits a best-effort control-plane event. Claiming is never
// allowed to fail because logging did.
func logTaskLeaseEvent(level logrus.Level, fields logrus.Fields, message string) {
	utils.LogPlaneBestEffort(
		logrus.StandardLogger(), level, utils.LogPlaneControl, fields, message,
	)
}
