package control

import (
	"context"
	"fmt"
	"sort"

	"gorm.io/gorm"

	"gpt-load/internal/accessquota"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
)

type normalizedAccessKeyCostLimitRule struct {
	ID            uint
	Kind          accessquota.Kind
	LimitNanoUSD  int64
	PeriodSeconds int64
}

// normalizeAccessKeyCostLimitRules validates the initial rules of a new
// AccessKey as one set.
func normalizeAccessKeyCostLimitRules(
	field OptionalAccessKeyCostLimitRules,
) ([]normalizedAccessKeyCostLimitRule, error) {
	if !field.Set {
		return []normalizedAccessKeyCostLimitRule{}, nil
	}
	result := make([]normalizedAccessKeyCostLimitRule, 0, len(field.Values))
	for _, input := range field.Values {
		rule, err := normalizeAccessKeyCostLimitRule(input)
		if err != nil {
			return nil, err
		}
		result = append(result, rule)
	}
	if err := validateAccessKeyCostLimitRuleSet(result); err != nil {
		return nil, err
	}
	sortNormalizedAccessKeyCostLimitRules(result)
	return result, nil
}

// normalizeAccessKeyCostLimitRule validates one rule definition on its own.
func normalizeAccessKeyCostLimitRule(input AccessKeyCostLimitRuleRequest) (normalizedAccessKeyCostLimitRule, error) {
	parsed, err := pricing.ParseUSD(input.LimitUSD)
	if err != nil || parsed <= 0 {
		return normalizedAccessKeyCostLimitRule{}, app_errors.ErrValidation
	}
	rule := normalizedAccessKeyCostLimitRule{
		Kind: input.Kind, LimitNanoUSD: int64(parsed), PeriodSeconds: input.PeriodSeconds,
	}
	switch rule.Kind {
	case accessquota.KindTotal:
		if rule.PeriodSeconds != 0 {
			return normalizedAccessKeyCostLimitRule{}, app_errors.ErrValidation
		}
	case accessquota.KindPeriodic:
		if rule.PeriodSeconds < accessquota.MinPeriodSeconds ||
			rule.PeriodSeconds > accessquota.MaxPeriodSeconds {
			return normalizedAccessKeyCostLimitRule{}, app_errors.ErrValidation
		}
	default:
		return normalizedAccessKeyCostLimitRule{}, app_errors.ErrValidation
	}
	return rule, nil
}

// validateAccessKeyCostLimitRuleSet enforces the per-key invariants: at most
// one total rule and at most MaxPeriodicRules periodic rules with distinct
// periods.
func validateAccessKeyCostLimitRuleSet(rules []normalizedAccessKeyCostLimitRule) error {
	totalCount := 0
	periods := make(map[int64]struct{})
	for _, rule := range rules {
		if rule.Kind == accessquota.KindTotal {
			totalCount++
			continue
		}
		if _, duplicate := periods[rule.PeriodSeconds]; duplicate {
			return app_errors.ErrValidation
		}
		periods[rule.PeriodSeconds] = struct{}{}
	}
	if totalCount > 1 || len(periods) > accessquota.MaxPeriodicRules {
		return app_errors.ErrValidation
	}
	return nil
}

func sortNormalizedAccessKeyCostLimitRules(rules []normalizedAccessKeyCostLimitRule) {
	sort.Slice(rules, func(i, j int) bool {
		left, right := rules[i], rules[j]
		if left.Kind != right.Kind {
			return left.Kind == accessquota.KindTotal
		}
		if left.PeriodSeconds != right.PeriodSeconds {
			return left.PeriodSeconds < right.PeriodSeconds
		}
		return left.ID < right.ID
	})
}

func createAccessKeyCostLimitRules(
	tx *gorm.DB,
	accessKeyID uint,
	rules []normalizedAccessKeyCostLimitRule,
) ([]models.AccessKeyCostLimitRule, error) {
	created := make([]models.AccessKeyCostLimitRule, 0, len(rules))
	for _, definition := range rules {
		if definition.ID != 0 {
			return nil, app_errors.ErrValidation
		}
		rule := models.AccessKeyCostLimitRule{
			AccessKeyID: accessKeyID, Kind: models.AccessKeyCostLimitKind(definition.Kind),
			LimitNanoUSD: definition.LimitNanoUSD, PeriodSeconds: definition.PeriodSeconds,
			RuleRevision: 1,
		}
		if err := tx.Create(&rule).Error; err != nil {
			return nil, app_errors.ParseDBError(err)
		}
		state := models.AccessKeyCostLimitState{
			RuleID: rule.ID, RuleRevision: rule.RuleRevision, SnapshotVersion: 1,
		}
		if err := tx.Create(&state).Error; err != nil {
			return nil, app_errors.ParseDBError(err)
		}
		created = append(created, rule)
	}
	return created, nil
}

func (s *Service) ResetAccessKeyCostLimitRules(
	ctx context.Context,
	accessKeyID uint,
	ruleIDs []uint,
) error {
	if accessKeyID == 0 || len(ruleIDs) == 0 || len(ruleIDs) > accessquota.MaxPeriodicRules+1 {
		return app_errors.ErrBadRequest
	}
	seen := make(map[uint]struct{}, len(ruleIDs))
	for _, ruleID := range ruleIDs {
		if ruleID == 0 {
			return app_errors.ErrValidation
		}
		if _, duplicate := seen[ruleID]; duplicate {
			return app_errors.ErrValidation
		}
		seen[ruleID] = struct{}{}
	}

	_, err := s.writeConfig(ctx, func(tx *gorm.DB) error {
		var accessKey models.AccessKey
		if err := tx.Select("id").First(&accessKey, accessKeyID).Error; err != nil {
			return app_errors.ParseDBError(err)
		}
		var rules []models.AccessKeyCostLimitRule
		if err := tx.Where("access_key_id = ? AND id IN ?", accessKeyID, ruleIDs).
			Order("id ASC").Find(&rules).Error; err != nil {
			return app_errors.ParseDBError(err)
		}
		if len(rules) != len(ruleIDs) {
			return app_errors.ErrValidation
		}
		for _, rule := range rules {
			if rule.RuleRevision == ^uint64(0) {
				return fmt.Errorf(
					"advance access key cost limit rule %d revision: %w",
					rule.ID,
					app_errors.ErrInternalServer,
				)
			}
			nextRevision := rule.RuleRevision + 1
			result := tx.Model(&models.AccessKeyCostLimitRule{}).
				Where("id = ? AND access_key_id = ? AND rule_revision = ?", rule.ID, accessKeyID, rule.RuleRevision).
				Update("rule_revision", nextRevision)
			if result.Error != nil {
				return app_errors.ParseDBError(result.Error)
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("reset access key cost limit rule %d: %w", rule.ID, app_errors.ErrInternalServer)
			}
			if err := resetAccessKeyCostLimitRuleState(
				tx,
				rule.ID,
				rule.RuleRevision,
				nextRevision,
			); err != nil {
				return err
			}
		}
		return nil
	}, nil)
	return err
}

func resetAccessKeyCostLimitRuleState(
	tx *gorm.DB,
	ruleID uint,
	currentRevision uint64,
	nextRevision uint64,
) error {
	result := tx.Model(&models.AccessKeyCostLimitState{}).
		Where("rule_id = ? AND rule_revision = ?", ruleID, currentRevision).
		Updates(map[string]any{
			"rule_revision":        nextRevision,
			"used_nano_usd":        int64(0),
			"window_started_at_ms": nil,
			"window_ends_at_ms":    nil,
			"window_generation":    uint64(0),
			"snapshot_version":     uint64(1),
		})
	if result.Error != nil {
		return app_errors.ParseDBError(result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("reset access key cost limit rule %d state: %w", ruleID, app_errors.ErrInternalServer)
	}
	return nil
}

func loadAccessKeyCostLimitRuleRows(
	db *gorm.DB,
	accessKeyID uint,
) ([]models.AccessKeyCostLimitRule, error) {
	var rows []models.AccessKeyCostLimitRule
	if err := db.Where("access_key_id = ?", accessKeyID).
		Order("CASE WHEN kind = 'total' THEN 0 ELSE 1 END ASC, period_seconds ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	return rows, nil
}

func mapAccessKeyCostLimitRules(rows []models.AccessKeyCostLimitRule) []AccessKeyCostLimitRule {
	result := make([]AccessKeyCostLimitRule, 0, len(rows))
	for _, row := range rows {
		result = append(result, AccessKeyCostLimitRule{
			ID: row.ID, Kind: accessquota.Kind(row.Kind),
			LimitUSD:      pricing.FormatUSD(pricing.NanoUSD(row.LimitNanoUSD)),
			PeriodSeconds: row.PeriodSeconds,
		})
	}
	return result
}

func costLimitRuleRequestsForDigest(
	rules []normalizedAccessKeyCostLimitRule,
) []AccessKeyCostLimitRuleRequest {
	result := make([]AccessKeyCostLimitRuleRequest, 0, len(rules))
	for _, rule := range rules {
		result = append(result, AccessKeyCostLimitRuleRequest{
			Kind: rule.Kind, LimitUSD: pricing.FormatUSD(pricing.NanoUSD(rule.LimitNanoUSD)),
			PeriodSeconds: rule.PeriodSeconds,
		})
	}
	return result
}

func mapAccessKeyCostLimitStatus(view accessquota.View) AccessKeyCostLimitStatus {
	result := AccessKeyCostLimitStatus{
		ObservedAtMS:      view.ObservedAtMS,
		Allowed:           view.Allowed,
		Recoverable:       view.Recoverable,
		NextAvailableAtMS: cloneCostLimitMilliseconds(view.NextAvailableAtMS),
		Rules:             make([]AccessKeyCostLimitRuleStatus, 0, len(view.Rules)),
	}
	for _, rule := range view.Rules {
		result.Rules = append(result.Rules, AccessKeyCostLimitRuleStatus{
			ID: rule.ID, Kind: rule.Kind,
			LimitUSD:     pricing.FormatUSD(pricing.NanoUSD(rule.LimitNanoUSD)),
			UsedUSD:      pricing.FormatUSD(pricing.NanoUSD(rule.UsedNanoUSD)),
			RemainingUSD: pricing.FormatUSD(pricing.NanoUSD(rule.RemainingNanoUSD)),
			Status:       rule.Status, PeriodSeconds: rule.PeriodSeconds,
			WindowStartedAtMS: cloneCostLimitMilliseconds(rule.WindowStartedAtMS),
			WindowEndsAtMS:    cloneCostLimitMilliseconds(rule.WindowEndsAtMS),
		})
	}
	return result
}

func costLimitDefinitionsFromStatus(status AccessKeyCostLimitStatus) []AccessKeyCostLimitRule {
	rules := make([]AccessKeyCostLimitRule, 0, len(status.Rules))
	for _, rule := range status.Rules {
		rules = append(rules, AccessKeyCostLimitRule{
			ID: rule.ID, Kind: rule.Kind, LimitUSD: rule.LimitUSD,
			PeriodSeconds: rule.PeriodSeconds,
		})
	}
	return rules
}

func blockingCostLimitRuleStatuses(status AccessKeyCostLimitStatus) []AccessKeyCostLimitRuleStatus {
	result := make([]AccessKeyCostLimitRuleStatus, 0)
	for _, rule := range status.Rules {
		if rule.Status == accessquota.RuleStatusExhausted {
			result = append(result, rule)
		}
	}
	return result
}

func cloneCostLimitMilliseconds(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// CreateAccessKeyCostLimitRule adds one rule to an AccessKey. A rule starts
// at revision 1 with no recorded usage.
func (s *Service) CreateAccessKeyCostLimitRule(
	ctx context.Context,
	accessKeyID uint,
	request AccessKeyCostLimitRuleRequest,
) (AccessKeyMetadata, error) {
	definition, err := normalizeAccessKeyCostLimitRule(request)
	if err != nil {
		return AccessKeyMetadata{}, err
	}
	return s.writeAccessKeyCostLimitRule(ctx, accessKeyID, func(tx *gorm.DB, current []normalizedAccessKeyCostLimitRule) error {
		if err := validateAccessKeyCostLimitRuleSet(append(current, definition)); err != nil {
			return err
		}
		_, err := createAccessKeyCostLimitRules(tx, accessKeyID, []normalizedAccessKeyCostLimitRule{definition})
		return err
	})
}

// UpdateAccessKeyCostLimitRule replaces one rule's definition. The kind is
// immutable. Changing the period starts a new revision with no usage;
// changing only the limit keeps the recorded usage.
func (s *Service) UpdateAccessKeyCostLimitRule(
	ctx context.Context,
	accessKeyID uint,
	ruleID uint,
	request AccessKeyCostLimitRuleRequest,
) (AccessKeyMetadata, error) {
	if ruleID == 0 {
		return AccessKeyMetadata{}, app_errors.ErrBadRequest
	}
	definition, err := normalizeAccessKeyCostLimitRule(request)
	if err != nil {
		return AccessKeyMetadata{}, err
	}
	return s.writeAccessKeyCostLimitRule(ctx, accessKeyID, func(tx *gorm.DB, current []normalizedAccessKeyCostLimitRule) error {
		var existing models.AccessKeyCostLimitRule
		if err := tx.Where("id = ? AND access_key_id = ?", ruleID, accessKeyID).Take(&existing).Error; err != nil {
			return app_errors.ParseDBError(err)
		}
		if accessquota.Kind(existing.Kind) != definition.Kind {
			return app_errors.ErrValidation
		}
		desired := make([]normalizedAccessKeyCostLimitRule, 0, len(current))
		for _, rule := range current {
			if rule.ID == ruleID {
				rule = definition
			}
			desired = append(desired, rule)
		}
		if err := validateAccessKeyCostLimitRuleSet(desired); err != nil {
			return err
		}
		updates := map[string]any{"limit_nano_usd": definition.LimitNanoUSD}
		periodChanged := existing.PeriodSeconds != definition.PeriodSeconds
		if periodChanged {
			if existing.RuleRevision == ^uint64(0) {
				return fmt.Errorf("advance access key cost limit rule %d revision: %w", ruleID, app_errors.ErrInternalServer)
			}
			updates["period_seconds"] = definition.PeriodSeconds
			updates["rule_revision"] = existing.RuleRevision + 1
		}
		if err := tx.Model(&models.AccessKeyCostLimitRule{}).
			Where("id = ? AND access_key_id = ?", ruleID, accessKeyID).
			Updates(updates).Error; err != nil {
			return app_errors.ParseDBError(err)
		}
		if periodChanged {
			return resetAccessKeyCostLimitRuleState(tx, ruleID, existing.RuleRevision, existing.RuleRevision+1)
		}
		return nil
	})
}

// DeleteAccessKeyCostLimitRule removes one rule and its usage state.
func (s *Service) DeleteAccessKeyCostLimitRule(
	ctx context.Context,
	accessKeyID uint,
	ruleID uint,
) (AccessKeyMetadata, error) {
	if ruleID == 0 {
		return AccessKeyMetadata{}, app_errors.ErrBadRequest
	}
	return s.writeAccessKeyCostLimitRule(ctx, accessKeyID, func(tx *gorm.DB, _ []normalizedAccessKeyCostLimitRule) error {
		result := tx.Where("id = ? AND access_key_id = ?", ruleID, accessKeyID).
			Delete(&models.AccessKeyCostLimitRule{})
		if result.Error != nil {
			return app_errors.ParseDBError(result.Error)
		}
		if result.RowsAffected == 0 {
			return app_errors.ErrResourceNotFound
		}
		return nil
	})
}

// writeAccessKeyCostLimitRule runs one rule mutation inside a published
// configuration write, handing it the AccessKey's current rules, and returns
// the AccessKey as committed.
func (s *Service) writeAccessKeyCostLimitRule(
	ctx context.Context,
	accessKeyID uint,
	mutate func(tx *gorm.DB, current []normalizedAccessKeyCostLimitRule) error,
) (AccessKeyMetadata, error) {
	if accessKeyID == 0 {
		return AccessKeyMetadata{}, app_errors.ErrBadRequest
	}
	var result AccessKeyMetadata
	_, err := s.writeConfig(ctx, func(tx *gorm.DB) error {
		if err := tx.Select("id").Take(&models.AccessKey{}, accessKeyID).Error; err != nil {
			return app_errors.ParseDBError(err)
		}
		rows, err := loadAccessKeyCostLimitRuleRows(tx, accessKeyID)
		if err != nil {
			return err
		}
		current := make([]normalizedAccessKeyCostLimitRule, 0, len(rows))
		for _, row := range rows {
			current = append(current, normalizedAccessKeyCostLimitRule{
				ID: row.ID, Kind: accessquota.Kind(row.Kind),
				LimitNanoUSD: row.LimitNanoUSD, PeriodSeconds: row.PeriodSeconds,
			})
		}
		if err := mutate(tx, current); err != nil {
			return err
		}
		result, err = loadAccessKeyMetadata(tx, accessKeyID)
		return err
	}, nil)
	if err != nil {
		return AccessKeyMetadata{}, err
	}
	return result, nil
}
