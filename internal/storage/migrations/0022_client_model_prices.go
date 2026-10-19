package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const ID0022 = "0022_client_model_prices"

// 冻结本次迁移的表与字段，后续运行模型变化不能改写历史 DDL。
type clientModelPrice0022 struct {
	ID                                     uint   `gorm:"primaryKey;autoIncrement"`
	ClientModel                            string `gorm:"type:varchar(255);not null;uniqueIndex:idx_client_model_prices_client_model"`
	InputPriceNanoUSDPerMillionTokens      *int64 `gorm:"column:input_price_nano_usd_per_million_tokens;check:chk_client_model_price_input_nano,input_price_nano_usd_per_million_tokens IS NULL OR input_price_nano_usd_per_million_tokens >= 0"`
	OutputPriceNanoUSDPerMillionTokens     *int64 `gorm:"column:output_price_nano_usd_per_million_tokens;check:chk_client_model_price_output_nano,output_price_nano_usd_per_million_tokens IS NULL OR output_price_nano_usd_per_million_tokens >= 0"`
	CacheReadPriceNanoUSDPerMillionTokens  *int64 `gorm:"column:cache_read_price_nano_usd_per_million_tokens;check:chk_client_model_price_cache_read_nano,cache_read_price_nano_usd_per_million_tokens IS NULL OR cache_read_price_nano_usd_per_million_tokens >= 0"`
	CacheWritePriceNanoUSDPerMillionTokens *int64 `gorm:"column:cache_write_price_nano_usd_per_million_tokens;check:chk_client_model_price_cache_write_nano,cache_write_price_nano_usd_per_million_tokens IS NULL OR cache_write_price_nano_usd_per_million_tokens >= 0"`
	ContextPriceTiers                      []byte `gorm:"type:json"`
	ModePriceSchedules                     []byte `gorm:"column:mode_price_schedules;type:json"`
	CreatedAtMS                            int64  `gorm:"column:created_at_ms;not null;check:chk_client_model_price_created_at,created_at_ms >= 0"`
	UpdatedAtMS                            int64  `gorm:"column:updated_at_ms;not null;check:chk_client_model_price_updated_at,updated_at_ms >= 0"`
}

func (clientModelPrice0022) TableName() string { return "client_model_prices" }

type requestLogUpstreamCost0022 struct {
	UpstreamCostNanoUSD int64  `gorm:"column:upstream_cost_nano_usd;not null;default:0;check:chk_request_log_upstream_cost_nano,upstream_cost_nano_usd >= 0"`
	UpstreamCostState   string `gorm:"type:varchar(32);not null;default:'not_applicable'"`
	// 组合约束逐一列出合法的用量、状态与完整度组合，已覆盖各列的取值范围。
	UpstreamPricingCompleteness string `gorm:"type:varchar(32);not null;default:'not_applicable';check:chk_request_log_upstream_pricing_state,(usage_state = 'not_applicable' AND upstream_cost_state = 'not_applicable' AND upstream_pricing_completeness = 'not_applicable' AND upstream_cost_nano_usd = 0) OR (usage_state = 'missing' AND upstream_cost_state = 'unpriced' AND upstream_pricing_completeness = 'unavailable' AND upstream_cost_nano_usd = 0) OR (usage_state IN ('complete','partial') AND ((upstream_cost_state = 'unpriced' AND upstream_pricing_completeness = 'unavailable' AND upstream_cost_nano_usd = 0) OR (upstream_cost_state = 'priced' AND upstream_pricing_completeness IN ('complete','partial'))))"`
}

func (requestLogUpstreamCost0022) TableName() string { return "request_logs" }

type requestLogAttemptUpstreamReceipt0022 struct {
	UpstreamPricingReceipt []byte `gorm:"type:json"`
}

func (requestLogAttemptUpstreamReceipt0022) TableName() string { return "request_log_attempts" }

var (
	clientModelPriceChecks0022 = []string{
		"chk_client_model_price_input_nano", "chk_client_model_price_output_nano",
		"chk_client_model_price_cache_read_nano", "chk_client_model_price_cache_write_nano",
		"chk_client_model_price_created_at", "chk_client_model_price_updated_at",
	}
	// 新增列、检查约束和已有行的回填。之前的费用都按上游价格计算，
	// 所以已有行的上游费用直接取原费用，以满足状态组合约束。
	upstreamCostColumns0022 = columnSet0021{
		model:  &requestLogUpstreamCost0022{},
		fields: []string{"UpstreamCostNanoUSD", "UpstreamCostState", "UpstreamPricingCompleteness"},
		checks: []string{"chk_request_log_upstream_cost_nano", "chk_request_log_upstream_pricing_state"},
		backfill: "UPDATE request_logs SET upstream_cost_nano_usd = estimated_cost_nano_usd, " +
			"upstream_cost_state = cost_state, upstream_pricing_completeness = pricing_completeness",
	}
	upstreamReceiptColumns0022 = columnSet0021{
		model:  &requestLogAttemptUpstreamReceipt0022{},
		fields: []string{"UpstreamPricingReceipt"},
	}
)

// Up0022 adds unified client-model prices and records each request's upstream
// cost beside the client-model charge.
func Up0022(db *gorm.DB) error {
	if !db.Migrator().HasTable(&clientModelPrice0022{}) {
		if err := db.Migrator().CreateTable(&clientModelPrice0022{}); err != nil {
			return fmt.Errorf("create client model prices: %w", err)
		}
	}
	for _, set := range []columnSet0021{upstreamCostColumns0022, upstreamReceiptColumns0022} {
		if err := addColumnSet0021(db, set); err != nil {
			return err
		}
	}
	return Validate0022(db)
}

func Validate0022(db *gorm.DB) error {
	model := &clientModelPrice0022{}
	if !db.Migrator().HasTable(model) {
		return fmt.Errorf("validate client model prices: table is missing")
	}
	for _, column := range []string{
		"id", "client_model", "input_price_nano_usd_per_million_tokens", "output_price_nano_usd_per_million_tokens",
		"cache_read_price_nano_usd_per_million_tokens", "cache_write_price_nano_usd_per_million_tokens",
		"context_price_tiers", "mode_price_schedules", "created_at_ms", "updated_at_ms",
	} {
		if !db.Migrator().HasColumn(model, column) {
			return fmt.Errorf("validate client model prices: column %q is missing", column)
		}
	}
	if !db.Migrator().HasIndex(model, "idx_client_model_prices_client_model") {
		return fmt.Errorf("validate client model prices: unique client model index is missing")
	}
	for _, check := range clientModelPriceChecks0022 {
		if !db.Migrator().HasConstraint(model, check) {
			return fmt.Errorf("validate client model prices: constraint %s is missing", check)
		}
	}
	for _, set := range []columnSet0021{upstreamCostColumns0022, upstreamReceiptColumns0022} {
		for _, field := range set.fields {
			if !db.Migrator().HasColumn(set.model, field) {
				return fmt.Errorf("validate upstream cost: %T.%s is missing", set.model, field)
			}
		}
		for _, check := range set.checks {
			if !db.Migrator().HasConstraint(set.model, check) {
				return fmt.Errorf("validate upstream cost: constraint %s is missing", check)
			}
		}
	}
	return nil
}
