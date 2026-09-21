package control

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/platform/utils"
	"gpt-load/internal/state"
)

// credentialHealthPollInterval bounds how long a lost doorbell message can
// hide a peer's health decision. The success criterion for propagation is two
// seconds, and the poll is one small read, so the fallback is set to meet that
// criterion on its own rather than relying on Pub/Sub being healthy.
const credentialHealthPollInterval = 2 * time.Second

// CredentialHealthCoordinator is the control plane's view of shared credential
// health. The registry stays the thing request routing reads; this only keeps
// every instance's registry saying the same thing.
type CredentialHealthCoordinator interface {
	Publish(context.Context, []state.CredentialHealth) (int64, error)
	Changed(context.Context, int64) ([]state.CredentialHealth, int64, error)
	Subscribe(context.Context) (<-chan struct{}, func())
}

// runCredentialHealthWatch is both halves of health replication in one loop:
// it pushes what this instance decided and pulls what its peers decided.
//
// One loop rather than two because the two halves are not independent: a push
// makes peers ring this instance's doorbell, so ordering them here is what
// keeps a burst of local decisions from arriving before the peers that caused
// it. Ordering is not what keeps the loop correct — the window between the
// drain and the read back is two round trips wide, so a record always arrives
// after decisions it could not have seen. The registry merges rather than
// overwrites, which is what makes the arrival order stop mattering.
//
// Failures leave this instance on its own health decisions, which are still
// correct for the traffic it is serving; they are just not yet shared. The
// next wakeup retries, and because both directions carry whole current values
// rather than deltas, a retry needs no recovery of what was missed.
func (runtime *Runtime) runCredentialHealthWatch(ctx context.Context, ticker runtimeTicker) {
	defer ticker.Stop()
	if ctx.Err() != nil {
		return
	}
	doorbell, release := runtime.credentialHealth.Subscribe(ctx)
	defer release()

	// Starting from zero pulls every credential's current health, which is how
	// an instance that just started learns the cooldowns its peers decided
	// while it was down.
	//
	// Reading before the first publish rather than after it, because the reset
	// counter a record carries only means anything next to the fleet's. An
	// instance that published first would stamp its decisions with the zero
	// every credential starts at, and every peer that had seen a reset would
	// correctly ignore them as older than its own.
	applied := runtime.applyCredentialHealth(ctx, 0)
	local := make(chan struct{}, 1)
	runtime.registry.SetHealthChangeNotifier(func() {
		select {
		case local <- struct{}{}:
		default:
		}
	})
	defer runtime.registry.SetHealthChangeNotifier(nil)

	for {
		select {
		case <-ctx.Done():
			return
		case <-local:
		case _, open := <-doorbell:
			if !open {
				doorbell = nil
				logCredentialHealthEvent(logrus.WarnLevel, "credential_health.watch_degraded", nil,
					"Credential health doorbell closed; falling back to polling")
			}
		case <-ticker.C():
		}
		if ctx.Err() != nil {
			return
		}
		runtime.publishCredentialHealth(ctx)
		applied = runtime.applyCredentialHealth(ctx, applied)
	}
}

// publishCredentialHealth hands this instance's decisions to its peers.
//
// A failed publish drops the drained changes rather than queuing them. The
// registry already holds the current state, and the next local decision about
// the same credential republishes it; queueing would mean replaying a cooldown
// that has since been cleared.
func (runtime *Runtime) publishCredentialHealth(ctx context.Context) {
	changes := runtime.registry.DrainHealthChanges()
	if len(changes) == 0 {
		return
	}
	sequence, err := runtime.credentialHealth.Publish(ctx, changes)
	if err != nil {
		logCredentialHealthEvent(logrus.WarnLevel, "credential_health.publish_failed", err,
			"Credential health could not be shared with other instances")
		return
	}
	logCredentialHealthEvent(logrus.DebugLevel, "credential_health.published", nil,
		"Credential health shared", logrus.Fields{
			"credentials": len(changes), "sequence": sequence,
		})
}

// applyCredentialHealth adopts peers' decisions and returns the sequence this
// instance has consumed. The sequence advances only on success, so a failed
// read is retried from the same point instead of skipping changes.
//
// This instance's own changes come back through here too, because skipping
// them would require knowing which entries were ours and peers' writes
// interleave with them. Letting them through costs nothing: an echo carries
// reasons this instance already holds, and merging a reason twice is the same
// as merging it once.
//
// The sequence can also come back lower than it went in, which means the
// shared store restarted its counter. Adopting the lower value re-reads every
// credential; keeping the old one would leave this instance deaf to its peers
// for the rest of its life.
func (runtime *Runtime) applyCredentialHealth(ctx context.Context, applied int64) int64 {
	changes, resume, err := runtime.credentialHealth.Changed(ctx, applied)
	if err != nil {
		logCredentialHealthEvent(logrus.WarnLevel, "credential_health.watch_degraded", err,
			"Credential health read failed")
		return applied
	}
	if resume < applied {
		logCredentialHealthEvent(logrus.WarnLevel, "credential_health.watch_degraded", nil,
			"Shared credential health restarted its sequence; re-reading every credential",
			logrus.Fields{"applied": applied, "resume": resume})
	}
	adopted := 0
	for _, change := range changes {
		if runtime.registry.ApplyRemoteHealth(change) {
			adopted++
		}
	}
	if adopted > 0 {
		logCredentialHealthEvent(logrus.InfoLevel, "credential_health.applied", nil,
			"Credential health from other instances applied", logrus.Fields{
				"credentials": adopted, "sequence": resume,
			})
	}
	return resume
}

func logCredentialHealthEvent(
	level logrus.Level,
	event string,
	err error,
	message string,
	extra ...logrus.Fields,
) {
	fields := logrus.Fields{"event": event}
	if err != nil {
		fields["error"] = err.Error()
	}
	for _, additional := range extra {
		for key, value := range additional {
			fields[key] = value
		}
	}
	utils.LogPlaneBestEffort(
		logrus.StandardLogger(), level, utils.LogPlaneControl, fields, message,
	)
}
