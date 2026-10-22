package requestlog

import (
	"cmp"
	"fmt"
	"slices"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gpt-load/internal/platform/epochms"
	"gpt-load/internal/storage/models"
)

// writeAutoDecisionUsage 把自动决策与护栏两类 JEV 调用聚合进 auto_decision_usage_stats。
// 一个请求的未定价或部分定价只计一次：答复、决策、护栏依次认领。
func writeAutoDecisionUsage(tx *gorm.DB, rows []models.RequestLog) error {
	stats := []models.AutoDecisionUsageStat{}
	for _, row := range rows {
		bucket := row.CompletedAtMS - row.CompletedAtMS%epochms.MillisecondsPerHour
		answerUnpriced := row.CostState == "unpriced" && (row.UsageState == "complete" || row.UsageState == "partial")
		if row.DecisionModel != "" && row.DecisionPricingCompleteness != "not_applicable" {
			stat := models.AutoDecisionUsageStat{
				BucketStartMS: bucket, AccessKeyID: row.AccessKeyID, GroupID: row.DecisionGroupID,
				ChannelID: row.DecisionChannelID, CredentialID: row.DecisionCredentialID,
				Model: row.DecisionModel, Currency: row.Currency,
				EstimatedCostNanoUSD: row.DecisionCostNanoUSD, EstimatedCostNano: row.DecisionCostNano,
			}
			if row.DecisionPricingCompleteness == "unavailable" && !answerUnpriced {
				stat.UnpricedRequestCount = 1
			}
			if row.DecisionPricingCompleteness == "partial" && row.PricingCompleteness != "partial" {
				stat.PricingPartialCount = 1
			}
			stats = append(stats, stat)
		}
		if row.AuditModel != "" && row.AuditPricingCompleteness != "not_applicable" {
			stat := models.AutoDecisionUsageStat{
				BucketStartMS: bucket, AccessKeyID: row.AccessKeyID, GroupID: row.AuditGroupID,
				ChannelID: row.AuditChannelID, CredentialID: row.AuditCredentialID,
				Model: row.AuditModel, Currency: row.Currency,
				EstimatedCostNanoUSD: row.AuditCostNanoUSD, EstimatedCostNano: row.AuditCostNano,
			}
			if row.AuditPricingCompleteness == "unavailable" && row.DecisionPricingCompleteness != "unavailable" && !answerUnpriced {
				stat.UnpricedRequestCount = 1
			}
			if row.AuditPricingCompleteness == "partial" && row.DecisionPricingCompleteness != "partial" && row.PricingCompleteness != "partial" {
				stat.PricingPartialCount = 1
			}
			stats = append(stats, stat)
		}
	}
	// 按主键排序，多实例并发写同一批键时行锁顺序一致。
	slices.SortFunc(stats, func(left, right models.AutoDecisionUsageStat) int {
		return cmp.Or(
			cmp.Compare(left.BucketStartMS, right.BucketStartMS), cmp.Compare(left.AccessKeyID, right.AccessKeyID),
			cmp.Compare(left.GroupID, right.GroupID), cmp.Compare(left.ChannelID, right.ChannelID),
			cmp.Compare(left.CredentialID, right.CredentialID), cmp.Compare(left.Model, right.Model),
			cmp.Compare(left.Currency, right.Currency),
		)
	})
	for _, stat := range stats {
		updates := incrementAssignments(map[string]int64{"estimated_cost_nano_usd": stat.EstimatedCostNanoUSD, "estimated_cost_nano": stat.EstimatedCostNano, "unpriced_request_count": stat.UnpricedRequestCount, "pricing_partial_count": stat.PricingPartialCount})
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{
			{Name: "bucket_start_ms"}, {Name: "access_key_id"}, {Name: "group_id"},
			{Name: "channel_id"}, {Name: "credential_id"}, {Name: "model"}, {Name: "currency"},
		}, DoUpdates: clause.Assignments(updates)}).Create(&stat).Error; err != nil {
			return fmt.Errorf("save automatic decision usage: %w", err)
		}
	}
	return nil
}

func decisionUsageScope(db *gorm.DB, input UsageQuery, groupIDs ...uint) *gorm.DB {
	scope := db.Session(&gorm.Session{NewDB: true}).Model(&models.AutoDecisionUsageStat{}).Where("bucket_start_ms >= ? AND bucket_start_ms < ?", input.FromMS, input.ToMS)
	if len(groupIDs) > 0 {
		scope = scope.Where("group_id IN ?", groupIDs)
	}
	if input.GroupID != nil {
		scope = scope.Where("group_id = ?", *input.GroupID)
	}
	if input.ChannelID != "" {
		scope = scope.Where("channel_id = ?", input.ChannelID)
	}
	if input.CredentialID != nil {
		scope = scope.Where("credential_id = ?", *input.CredentialID)
	}
	if input.AccessKeyID != nil {
		scope = scope.Where("access_key_id = ?", *input.AccessKeyID)
	}
	if input.UpstreamModel != "" {
		scope = scope.Where("model = ?", input.UpstreamModel)
	}
	return scope
}

const decisionUsageProjection = `bucket_start_ms, group_id, access_key_id, model,
	0 AS request_count, 0 AS success_count, 0 AS failure_count,
	0 AS uncached_input_tokens, 0 AS cache_read_tokens, 0 AS cache_write_5m_tokens,
	0 AS cache_write_1h_tokens, 0 AS cache_write_unknown_tokens, 0 AS output_tokens,
	estimated_cost_nano_usd, currency, estimated_cost_nano, 0 AS usage_missing_count, 0 AS partial_count,
	unpriced_request_count, pricing_partial_count, channel_id, credential_id`

const decisionRequestProjection = `completed_at_ms - completed_at_ms % ? AS bucket_start_ms,
	decision_group_id AS group_id, access_key_id, decision_model AS model,
	0 AS request_count, 0 AS success_count, 0 AS failure_count,
	0 AS uncached_input_tokens, 0 AS cache_read_tokens, 0 AS cache_write_5m_tokens,
	0 AS cache_write_1h_tokens, 0 AS cache_write_unknown_tokens, 0 AS output_tokens,
	decision_cost_nano_usd AS estimated_cost_nano_usd, currency, decision_cost_nano AS estimated_cost_nano,
	0 AS usage_missing_count, 0 AS partial_count,
	CASE WHEN decision_pricing_completeness = 'unavailable' AND NOT (cost_state = 'unpriced' AND usage_state IN ('complete','partial')) THEN 1 ELSE 0 END AS unpriced_request_count,
	CASE WHEN decision_pricing_completeness = 'partial' AND pricing_completeness <> 'partial' THEN 1 ELSE 0 END AS pricing_partial_count,
	? + 0 AS bucket_alignment_ms`

func decisionRequestScope(db *gorm.DB, input UsageQuery, groupIDs ...uint) *gorm.DB {
	scope := db.Session(&gorm.Session{NewDB: true}).Model(&models.RequestLog{}).
		Where("completed_at_ms >= ? AND completed_at_ms < ? AND decision_model <> '' AND decision_pricing_completeness <> 'not_applicable'", input.FromMS, input.ToMS)
	if len(groupIDs) > 0 {
		scope = scope.Where("decision_group_id IN ?", groupIDs)
	}
	if input.GroupID != nil {
		scope = scope.Where("decision_group_id = ?", *input.GroupID)
	}
	if input.ChannelID != "" {
		scope = scope.Where("decision_channel_id = ?", input.ChannelID)
	}
	if input.CredentialID != nil {
		scope = scope.Where("decision_credential_id = ?", *input.CredentialID)
	}
	if input.AccessKeyID != nil {
		scope = scope.Where("access_key_id = ?", *input.AccessKeyID)
	}
	if input.UpstreamModel != "" {
		scope = scope.Where("decision_model = ?", input.UpstreamModel)
	}
	return scope.Select(decisionRequestProjection, UsageFiveMinuteBucketMS, UsageFiveMinuteBucketMS)
}

const auditRequestProjection = `completed_at_ms - completed_at_ms % ? AS bucket_start_ms,
	audit_group_id AS group_id, access_key_id, audit_model AS model,
	0 AS request_count, 0 AS success_count, 0 AS failure_count,
	0 AS uncached_input_tokens, 0 AS cache_read_tokens, 0 AS cache_write_5m_tokens,
	0 AS cache_write_1h_tokens, 0 AS cache_write_unknown_tokens, 0 AS output_tokens,
	audit_cost_nano_usd AS estimated_cost_nano_usd, currency, audit_cost_nano AS estimated_cost_nano,
	0 AS usage_missing_count, 0 AS partial_count,
	CASE WHEN audit_pricing_completeness = 'unavailable' AND decision_pricing_completeness <> 'unavailable' AND NOT (cost_state = 'unpriced' AND usage_state IN ('complete','partial')) THEN 1 ELSE 0 END AS unpriced_request_count,
	CASE WHEN audit_pricing_completeness = 'partial' AND decision_pricing_completeness <> 'partial' AND pricing_completeness <> 'partial' THEN 1 ELSE 0 END AS pricing_partial_count,
	? + 0 AS bucket_alignment_ms`

// auditRequestScope 是护栏 JEV 调用在 5 分钟报表中的投影，与 decisionRequestScope 对称。
func auditRequestScope(db *gorm.DB, input UsageQuery, groupIDs ...uint) *gorm.DB {
	scope := db.Session(&gorm.Session{NewDB: true}).Model(&models.RequestLog{}).
		Where("completed_at_ms >= ? AND completed_at_ms < ? AND audit_model <> '' AND audit_pricing_completeness <> 'not_applicable'", input.FromMS, input.ToMS)
	if len(groupIDs) > 0 {
		scope = scope.Where("audit_group_id IN ?", groupIDs)
	}
	if input.GroupID != nil {
		scope = scope.Where("audit_group_id = ?", *input.GroupID)
	}
	if input.ChannelID != "" {
		scope = scope.Where("audit_channel_id = ?", input.ChannelID)
	}
	if input.CredentialID != nil {
		scope = scope.Where("audit_credential_id = ?", *input.CredentialID)
	}
	if input.AccessKeyID != nil {
		scope = scope.Where("access_key_id = ?", *input.AccessKeyID)
	}
	if input.UpstreamModel != "" {
		scope = scope.Where("audit_model = ?", input.UpstreamModel)
	}
	return scope.Select(auditRequestProjection, UsageFiveMinuteBucketMS, UsageFiveMinuteBucketMS)
}
