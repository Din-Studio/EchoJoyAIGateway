package control

import (
	"context"
	"fmt"
	"sort"

	"gorm.io/gorm"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage/models"
)

// reloadCommittedConfig re-reads the persisted configuration committed by any
// cluster instance and applies it locally while preserving runtime state:
// credential health (cooldowns, blacklist, failure counts, auth state, model
// cooldowns) survives through ReconcileGroup, access-key quota counters
// survive through the snapshot reconciler, and the soft-affinity cache is only
// invalidated when the compiled snapshot actually changed. It returns the
// cluster revision that was applied.
func (s *Service) reloadCommittedConfig(ctx context.Context) (uint64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	credentialIDs, err := s.allKnownCredentialIDs(ctx)
	if err != nil {
		return 0, err
	}
	var revision uint64
	var resultErr error
	apply := func() {
		revision, resultErr = s.reloadCommittedConfigLocked(ctx)
	}
	if err := s.doCredentialMutations(credentialIDs, apply); err != nil {
		return 0, err
	}
	return revision, resultErr
}

// allKnownCredentialIDs unions persisted and registry credential IDs so the
// mutation stripes of credentials deleted by a peer are held as well.
func (s *Service) allKnownCredentialIDs(ctx context.Context) ([]uint, error) {
	var persisted []uint
	if err := s.db.WithContext(ctx).Model(&models.Credential{}).
		Order("id ASC").Pluck("id", &persisted).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	seen := make(map[uint]struct{}, len(persisted))
	ids := make([]uint, 0, len(persisted))
	for _, id := range persisted {
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if s.registrySnapshot != nil {
		for _, view := range s.registrySnapshot() {
			if _, exists := seen[view.ID]; !exists {
				seen[view.ID] = struct{}{}
				ids = append(ids, view.ID)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (s *Service) reloadCommittedConfigLocked(ctx context.Context) (uint64, error) {
	var (
		input      state.CompileInput
		entries    []state.CredentialEntry
		priceTable *pricing.Table
		revision   uint64
	)
	err := s.withReadSnapshot(ctx, func(tx *gorm.DB) error {
		var err error
		input, err = stateloader.BuildCompileInputWithProxy(
			ctx, tx, s.encryption, s.environmentProxy, s.channelRegistry,
		)
		if err != nil {
			return fmt.Errorf("reload committed configuration: %w", err)
		}
		entries, err = stateloader.BuildCredentialEntriesWithProxy(ctx, tx, s.encryption)
		if err != nil {
			return fmt.Errorf("reload committed credentials: %w", err)
		}
		priceTable, err = loadPriceTable(ctx, tx)
		if err != nil {
			return fmt.Errorf("reload committed prices: %w", err)
		}
		revision, err = readClusterConfigRevision(ctx, tx)
		return err
	})
	if err != nil {
		return 0, err
	}

	s.priceRuntime.Publish(priceTable)

	byGroup := make(map[uint][]state.CredentialEntry, len(input.Groups))
	for _, group := range input.Groups {
		byGroup[group.ID] = nil
	}
	for _, entry := range entries {
		byGroup[entry.GroupID] = append(byGroup[entry.GroupID], entry)
	}
	if s.registrySnapshot != nil {
		for _, view := range s.registrySnapshot() {
			if _, exists := byGroup[view.GroupID]; !exists {
				byGroup[view.GroupID] = nil
			}
		}
	}
	groupIDs := make([]uint, 0, len(byGroup))
	for groupID := range byGroup {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })
	for _, groupID := range groupIDs {
		if _, err := s.reconcileRegistryGroup(groupID, byGroup[groupID]); err != nil {
			return 0, fmt.Errorf("reconcile committed group %d credentials: %w", groupID, err)
		}
	}
	if err := s.restoreCredentialQuotaObservations(ctx); err != nil {
		return 0, fmt.Errorf("restore committed credential quota observations: %w", err)
	}

	matches, err := s.manager.Matches(input)
	if err != nil {
		return 0, fmt.Errorf("compile committed configuration: %w", err)
	}
	if !matches {
		if _, err := s.manager.Publish(input); err != nil {
			return 0, fmt.Errorf("publish committed configuration: %w", err)
		}
	}
	return revision, nil
}
