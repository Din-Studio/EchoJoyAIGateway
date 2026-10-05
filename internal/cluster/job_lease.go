package cluster

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"
)

const minJobClaim = time.Second

// JobLease lets one instance claim a periodic background job for most of a
// period, so every instance can keep its own ticker while the cluster runs
// the job about once per period. Every job it gates is safe to run twice;
// the claim only avoids duplicate work.
type JobLease struct {
	client *Client
}

// NewJobLease builds the periodic job claim.
func NewJobLease(client *Client) *JobLease {
	return &JobLease{client: client}
}

// RunOncePerPeriod runs fn when this instance claims job for nine tenths of
// period, leaving the next tick of the same instance room to claim it again.
// A failed run releases the claim so any instance's next tick retries; a
// successful run keeps it until it expires. A Redis error skips this round.
// It reports whether fn ran.
func (lease *JobLease) RunOncePerPeriod(
	ctx context.Context,
	job string,
	period time.Duration,
	fn func(context.Context) error,
) bool {
	claim := max(period*9/10, minJobClaim)
	key := lease.client.Key("lease", job)
	token := lease.client.InstanceID() + ":" + randomToken()
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	claimed, err := lease.client.SetNX(callCtx, key, token, claim).Result()
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"event": "cluster.job_claim_failed", "job": job,
			}).Warn("background job claim failed; skipping this round")
		}
		return false
	}
	if !claimed {
		logrus.WithFields(logrus.Fields{
			"event": "cluster.job_skipped", "job": job,
		}).Debug("background job is claimed by another instance")
		return false
	}
	if err := fn(ctx); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshLeaseReleaseWait)
		defer cancel()
		if err := releaseLeaseScript.Run(releaseCtx, lease.client, []string{key}, token).Err(); err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"event": "cluster.job_release_failed", "job": job,
			}).Warn("background job claim release failed; it expires on its own")
		}
	}
	return true
}
