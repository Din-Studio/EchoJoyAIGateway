package control

import (
	"context"
	"fmt"
	"time"

	"gpt-load/internal/accessquota"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/state"
)

type runtimeObservation struct {
	observedAt time.Time
	snapshot   *state.ConfigSnapshot
	keys       []state.CredentialRuntimeView
}

type runtimeHealthObservation struct {
	runtimeObservation
	credentialCiphertexts map[uint]string
	accessQuotaViews      map[uint]accessquota.View
}

func (service *Service) captureRuntimeObservation() (runtimeObservation, error) {
	if service == nil || service.manager == nil || service.registry == nil ||
		service.now == nil {
		return runtimeObservation{}, fmt.Errorf(
			"capture runtime observation: %w",
			app_errors.ErrInternalServer,
		)
	}
	service.writeMu.RLock()
	snapshot := service.manager.Current()
	if snapshot == nil {
		service.writeMu.RUnlock()
		return runtimeObservation{}, fmt.Errorf(
			"capture runtime observation: Snapshot is nil: %w",
			app_errors.ErrInternalServer,
		)
	}
	keys := service.registry.Snapshot()
	observedAt := service.now().UTC()
	service.writeMu.RUnlock()

	for _, key := range keys {
		if _, exists := snapshot.GroupCatalog[key.GroupID]; !exists {
			return runtimeObservation{}, fmt.Errorf(
				"capture runtime observation: key %d group %d missing from catalog: %w",
				key.ID,
				key.GroupID,
				app_errors.ErrInternalServer,
			)
		}
	}
	return runtimeObservation{
		observedAt: observedAt,
		snapshot:   snapshot,
		keys:       keys,
	}, nil
}

// accessQuotaView reads one AccessKey's cost-limit state from the shared
// cluster store, or from the in-process runtime in single-instance mode. ok is
// false when neither exists. Callers must not hold writeMu: the cluster store
// performs network I/O.
func (service *Service) accessQuotaView(
	ctx context.Context,
	snapshot *state.ConfigSnapshot,
	accessKeyID uint,
	now time.Time,
) (accessquota.View, bool, error) {
	switch {
	case service.clusterQuota != nil:
		view, err := service.clusterQuota.View(ctx, snapshot, accessKeyID, now)
		if err != nil {
			return accessquota.View{}, false, fmt.Errorf(
				"read access key %d cost limit state: %v: %w", accessKeyID, err, app_errors.ErrInternalServer,
			)
		}
		return view, true, nil
	case service.accessQuota != nil:
		return service.accessQuota.Snapshot(accessKeyID, now), true, nil
	}
	return accessquota.View{}, false, nil
}

func (service *Service) captureRuntimeHealthObservation() (
	runtimeHealthObservation,
	error,
) {
	observation, err := service.captureRuntimeHealthState()
	if err != nil || (service.accessQuota == nil && service.clusterQuota == nil) {
		return observation, err
	}
	views := make(map[uint]accessquota.View, len(observation.snapshot.AccessKeysByID))
	for accessKeyID := range observation.snapshot.AccessKeysByID {
		view, _, err := service.accessQuotaView(context.Background(), observation.snapshot, accessKeyID, observation.observedAt)
		if err != nil {
			return runtimeHealthObservation{}, err
		}
		views[accessKeyID] = view
	}
	observation.accessQuotaViews = views
	return observation, nil
}

func (service *Service) captureRuntimeHealthState() (
	runtimeHealthObservation,
	error,
) {
	if service == nil || service.manager == nil || service.registry == nil ||
		service.now == nil {
		return runtimeHealthObservation{}, fmt.Errorf(
			"capture runtime health observation: %w",
			app_errors.ErrInternalServer,
		)
	}

	service.writeMu.RLock()
	defer service.writeMu.RUnlock()

	snapshot := service.manager.Current()
	if snapshot == nil {
		return runtimeHealthObservation{}, fmt.Errorf(
			"capture runtime health observation: Snapshot is nil: %w",
			app_errors.ErrInternalServer,
		)
	}
	keys := service.registry.Snapshot()
	observedAt := service.now().UTC()
	credentialCiphertexts := make(map[uint]string)
	for _, key := range keys {
		group, exists := snapshot.GroupCatalog[key.GroupID]
		if !exists {
			return runtimeHealthObservation{}, fmt.Errorf(
				"capture runtime health observation: key %d group %d missing from catalog: %w",
				key.ID,
				key.GroupID,
				app_errors.ErrInternalServer,
			)
		}
		bucket := classifyHealthKey(group, key, observedAt)
		if bucket == healthBucketDisabled {
			continue
		}
		ciphertext, exists := service.registry.EncryptedCredentialData(key.ID)
		if !exists || ciphertext == "" {
			return runtimeHealthObservation{}, fmt.Errorf(
				"capture runtime health observation: key %d ciphertext unavailable: %w",
				key.ID,
				app_errors.ErrInternalServer,
			)
		}
		credentialCiphertexts[key.ID] = ciphertext
	}

	return runtimeHealthObservation{
		runtimeObservation: runtimeObservation{
			observedAt: observedAt,
			snapshot:   snapshot,
			keys:       keys,
		},
		credentialCiphertexts: credentialCiphertexts,
	}, nil
}
