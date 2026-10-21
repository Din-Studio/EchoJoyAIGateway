package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/platform/utils"
	"gpt-load/internal/state"
)

// AccessQuotaGate owns AccessKey cost-limit admission for the data plane.
// snapshot is the configuration the request was authorized against; it is nil
// when the key had no cost-limit rules at authorization time. Implementations
// return accessquota.ErrStaleRules when snapshot no longer matches the quota
// state, and any other error when the state is unavailable.
type AccessQuotaGate interface {
	Check(ctx context.Context, snapshot *state.ConfigSnapshot, accessKeyID uint, now time.Time) (accessquota.Decision, error)
	Admit(ctx context.Context, snapshot *state.ConfigSnapshot, accessKeyID uint, now time.Time) (accessquota.Ticket, accessquota.Decision, error)
	Complete(ctx context.Context, ticket accessquota.Ticket, costNano int64) (accessquota.CompletionResult, error)
}

// limitStateFailureReason maps an AccessQuotaGate or AccessKeyRPMLimiter
// error to its client reason.
func limitStateFailureReason(err error) reason {
	if errors.Is(err, accessquota.ErrStaleRules) {
		return reasonConfigurationChanged
	}
	return reasonClusterStateUnavailable
}

// completeLimitStateFailure rejects a request whose shared limit state could
// not be read. operation names the failed call; the log keeps the underlying
// error, which tells a pool wait, a dial, and a slow reply apart.
func (handler *Handler) completeLimitStateFailure(
	context *gin.Context,
	recorder *requestRecorder,
	operation string,
	err error,
) {
	if !errors.Is(err, accessquota.ErrStaleRules) {
		utils.LogPlaneBestEffort(
			handler.logger,
			logrus.WarnLevel,
			utils.LogPlaneData,
			logrus.Fields{
				"event":     "limit_state.unavailable",
				"operation": operation,
				"error":     err.Error(),
			},
			"Shared limit state unavailable",
		)
	}
	context.Writer.Header().Set("Retry-After", "1")
	handler.completeReason(context, recorder, limitStateFailureReason(err))
}

// completeAccessQuota records the settled request cost. It runs after the
// response, so the request context is detached from cancellation and a
// failure only loses accounting for this request.
func (handler *Handler) completeAccessQuota(
	ctx context.Context,
	accessKeyID uint,
	ticket accessquota.Ticket,
	costNano int64,
) {
	completion, err := handler.accessQuota.Complete(context.WithoutCancel(ctx), ticket, costNano)
	if err != nil {
		utils.LogPlaneBestEffort(
			handler.logger,
			logrus.ErrorLevel,
			utils.LogPlaneData,
			logrus.Fields{
				"event":         "access_quota.complete_failed",
				"access_key_id": accessKeyID,
				"cost_nano":     costNano,
				"error":         err.Error(),
			},
			"Access key cost limit accounting failed",
		)
		return
	}
	handler.logAccessQuotaCompletionFault(accessKeyID, completion)
}
