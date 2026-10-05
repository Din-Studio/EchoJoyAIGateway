package gateway

import (
	"context"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/affinity"
	"gpt-load/internal/platform/utils"
	"gpt-load/internal/protocol"
	"gpt-load/internal/scheduler"
	"gpt-load/internal/state"
	"gpt-load/internal/telemetry"
)

// AffinityStore remembers soft credential preferences shared through Redis.
// They are advisory, so a failing store only removes the preference.
type AffinityStore interface {
	Lookup(ctx context.Context, policy affinity.Policy, key affinity.Key) (affinity.Observation, error)
	RecordSuccess(
		ctx context.Context,
		policy affinity.Policy,
		key affinity.Key,
		observed affinity.Observation,
		target affinity.Target,
	) (bool, error)
}

type requestAffinity struct {
	key                   affinity.Key
	policy                affinity.Policy
	observation           affinity.Observation
	preferredCredentialID uint
	continuityKey         string
	kind                  string
}

func (handler *Handler) resolveRequestAffinity(
	ctx context.Context,
	snapshot *state.ConfigSnapshot,
	accessKeyID uint,
	clientProtocol protocol.Protocol,
	prefix []byte,
	allowedCredentialRefs map[uint]state.CredentialRef,
	promptCacheKey string,
) requestAffinity {
	if handler == nil || snapshot == nil {
		return requestAffinity{}
	}
	key := affinity.DeriveKey(
		handler.encryption,
		accessKeyID,
		clientProtocol,
		prefix,
	)
	// 执行层私有 replay scope 仍由提示词派生，不把客户端缓存分组当作会话身份。
	result := requestAffinity{continuityKey: string(key), kind: telemetry.AffinityPromptPrefix}
	if promptCacheKey != "" {
		key = affinity.DerivePromptCacheKey(handler.encryption, accessKeyID, clientProtocol, promptCacheKey)
		result.kind = telemetry.AffinityPromptCacheKey
	}
	policy := affinity.Policy{
		Revision: snapshot.Revision,
		Capacity: snapshot.Settings.AffinityCapacity,
		TTL:      snapshot.Settings.AffinityTTL,
	}
	if handler.affinity == nil || !key.Valid() || !policy.Valid() {
		return result
	}
	observation, err := handler.affinity.Lookup(ctx, policy, key)
	if err != nil {
		utils.LogPlaneBestEffort(handler.logger, logrus.WarnLevel, utils.LogPlaneData,
			logrus.Fields{"event": "affinity.redis_unavailable", "op": "lookup", "error": err.Error()},
			"Shared soft affinity is unavailable; scheduling without a preference")
		return result
	}
	resolved := result
	resolved.key = key
	resolved.policy = policy
	resolved.observation = observation
	if !observation.Found() {
		return resolved
	}
	target := observation.Target
	group, exists := snapshot.Groups[target.GroupID]
	if !exists || !group.AffinityEnabled {
		return resolved
	}
	ref, allowed := allowedCredentialRefs[target.CredentialID]
	if !allowed || ref.GroupID != target.GroupID ||
		ref.IdentityGeneration != target.IdentityGeneration {
		return resolved
	}
	resolved.preferredCredentialID = target.CredentialID
	return resolved
}

func (handler *Handler) recordAffinitySuccess(
	request requestAffinity,
	selection scheduler.Selection,
	ref state.CredentialRef,
) {
	if handler == nil || handler.affinity == nil || !request.key.Valid() ||
		!selection.Group.AffinityEnabled {
		return
	}
	// 响应已完成；客户端断开不应阻止学习，调用由共享存储自身的超时约束。
	_, err := handler.affinity.RecordSuccess(
		context.Background(),
		request.policy,
		request.key,
		request.observation,
		affinity.Target{
			GroupID: selection.GroupID, CredentialID: selection.CredentialID,
			IdentityGeneration: ref.IdentityGeneration,
		},
	)
	if err != nil {
		utils.LogPlaneBestEffort(handler.logger, logrus.WarnLevel, utils.LogPlaneData,
			logrus.Fields{"event": "affinity.redis_unavailable", "op": "record",
				"credential_id": selection.CredentialID, "error": err.Error()},
			"Shared soft affinity is unavailable; the success was not learned")
	}
}
