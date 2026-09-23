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
	Complete(ctx context.Context, ticket accessquota.Ticket, costNanoUSD int64) (accessquota.CompletionResult, error)
}

// localAccessQuotaGate pins in-process quota decisions to the published
// snapshot the request was authorized against.
type localAccessQuotaGate struct {
	manager *state.Manager
	runtime *accessquota.Runtime
}

// NewLocalAccessQuotaGate returns nil when runtime is nil so callers never
// store a typed-nil implementation in the interface.
func NewLocalAccessQuotaGate(manager *state.Manager, runtime *accessquota.Runtime) AccessQuotaGate {
	if runtime == nil {
		return nil
	}
	return localAccessQuotaGate{manager: manager, runtime: runtime}
}

func (gate localAccessQuotaGate) Check(
	_ context.Context,
	snapshot *state.ConfigSnapshot,
	accessKeyID uint,
	now time.Time,
) (accessquota.Decision, error) {
	if snapshot == nil {
		return gate.runtime.Check(accessKeyID, now), nil
	}
	var decision accessquota.Decision
	current := gate.manager.WithCurrentSnapshotRead(func(currentSnapshot *state.ConfigSnapshot) bool {
		if currentSnapshot != snapshot {
			return false
		}
		decision = gate.runtime.Check(accessKeyID, now)
		return true
	})
	if !current {
		return decision, accessquota.ErrStaleRules
	}
	return decision, nil
}

func (gate localAccessQuotaGate) Admit(
	_ context.Context,
	snapshot *state.ConfigSnapshot,
	accessKeyID uint,
	now time.Time,
) (accessquota.Ticket, accessquota.Decision, error) {
	if snapshot == nil {
		ticket, decision := gate.runtime.Admit(accessKeyID, now)
		return ticket, decision, nil
	}
	var ticket accessquota.Ticket
	var decision accessquota.Decision
	current := gate.manager.WithCurrentSnapshotRead(func(currentSnapshot *state.ConfigSnapshot) bool {
		if currentSnapshot != snapshot {
			return false
		}
		ticket, decision = gate.runtime.Admit(accessKeyID, now)
		return true
	})
	if !current {
		return ticket, decision, accessquota.ErrStaleRules
	}
	return ticket, decision, nil
}

func (gate localAccessQuotaGate) Complete(
	_ context.Context,
	ticket accessquota.Ticket,
	costNanoUSD int64,
) (accessquota.CompletionResult, error) {
	return gate.runtime.Complete(ticket, costNanoUSD), nil
}

// limitStateFailureReason maps an AccessQuotaGate or AccessKeyRPMLimiter
// error to its client reason.
func limitStateFailureReason(err error) reason {
	if errors.Is(err, accessquota.ErrStaleRules) {
		return reasonConfigurationChanged
	}
	return reasonClusterStateUnavailable
}

func (handler *Handler) completeLimitStateFailure(
	context *gin.Context,
	recorder *requestRecorder,
	err error,
) {
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
	costNanoUSD int64,
) {
	completion, err := handler.accessQuota.Complete(context.WithoutCancel(ctx), ticket, costNanoUSD)
	if err != nil {
		utils.LogPlaneBestEffort(
			handler.logger,
			logrus.ErrorLevel,
			utils.LogPlaneData,
			logrus.Fields{
				"event":         "access_quota.complete_failed",
				"access_key_id": accessKeyID,
				"cost_nano_usd": costNanoUSD,
				"error":         err.Error(),
			},
			"Access key cost limit accounting failed",
		)
		return
	}
	handler.logAccessQuotaCompletionFault(accessKeyID, completion)
}
