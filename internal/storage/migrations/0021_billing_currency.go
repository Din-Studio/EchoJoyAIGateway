package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ID0021 = "0021_billing_currency"

// 冻结本次迁移的字段，后续运行模型变化不能改写历史 DDL。
type requestLogCurrency0021 struct {
	Currency           string `gorm:"type:varchar(3);not null;default:'USD';check:chk_request_log_currency,currency ~ '^[A-Z]{3}$'"`
	ExchangeRateMicros int64  `gorm:"column:exchange_rate_micros;not null;default:1000000;check:chk_request_log_exchange_rate,exchange_rate_micros > 0"`
	EstimatedCostNano  int64  `gorm:"column:estimated_cost_nano;not null;default:0;check:chk_request_log_cost_local,estimated_cost_nano >= 0"`
	DecisionCostNano   int64  `gorm:"column:decision_cost_nano;not null;default:0;check:chk_request_log_decision_cost_local,decision_cost_nano >= 0"`
}

func (requestLogCurrency0021) TableName() string { return "request_logs" }

type usageStatCurrency0021 struct {
	Currency          string `gorm:"type:varchar(3);not null;default:'USD';check:chk_usage_stat_currency,currency ~ '^[A-Z]{3}$'"`
	EstimatedCostNano int64  `gorm:"column:estimated_cost_nano;not null;default:0;check:chk_usage_stat_cost_local,estimated_cost_nano >= 0"`
}

func (usageStatCurrency0021) TableName() string { return "usage_stats" }

type autoUsageCurrency0021 struct {
	Currency          string `gorm:"type:varchar(3);not null;default:'USD';check:chk_auto_decision_usage_currency,currency ~ '^[A-Z]{3}$'"`
	EstimatedCostNano int64  `gorm:"column:estimated_cost_nano;not null;default:0;check:chk_auto_decision_usage_cost_local,estimated_cost_nano >= 0"`
}

func (autoUsageCurrency0021) TableName() string { return "auto_decision_usage_stats" }

type costLimitRuleCurrency0021 struct {
	Currency string `gorm:"type:varchar(3);not null;default:'USD';check:chk_ak_cost_rule_currency,currency ~ '^[A-Z]{3}$'"`
}

func (costLimitRuleCurrency0021) TableName() string { return "access_key_cost_limit_rules" }

type columnSet0021 struct {
	model    any
	fields   []string
	checks   []string
	backfill string
}

// 新增列、检查约束和已有行的回填；已有行均为美元。
var columnSets0021 = []columnSet0021{
	{
		model:    &requestLogCurrency0021{},
		fields:   []string{"Currency", "ExchangeRateMicros", "EstimatedCostNano", "DecisionCostNano"},
		checks:   []string{"chk_request_log_currency", "chk_request_log_exchange_rate", "chk_request_log_cost_local", "chk_request_log_decision_cost_local"},
		backfill: "UPDATE request_logs SET estimated_cost_nano = estimated_cost_nano_usd, decision_cost_nano = decision_cost_nano_usd",
	},
	{
		model:    &usageStatCurrency0021{},
		fields:   []string{"Currency", "EstimatedCostNano"},
		checks:   []string{"chk_usage_stat_currency", "chk_usage_stat_cost_local"},
		backfill: "UPDATE usage_stats SET estimated_cost_nano = estimated_cost_nano_usd",
	},
	{
		model:    &autoUsageCurrency0021{},
		fields:   []string{"Currency", "EstimatedCostNano"},
		checks:   []string{"chk_auto_decision_usage_currency", "chk_auto_decision_usage_cost_local"},
		backfill: "UPDATE auto_decision_usage_stats SET estimated_cost_nano = estimated_cost_nano_usd",
	},
	{
		model:  &costLimitRuleCurrency0021{},
		fields: []string{"Currency"},
		checks: []string{"chk_ak_cost_rule_currency"},
	},
}

type columnRename0021 struct {
	table string
	from  string
	to    string
}

// 限额金额改为规则自身币种的 nano 单位，列名去掉 usd。
var columnRenames0021 = []columnRename0021{
	{table: "access_key_cost_limit_rules", from: "limit_nano_usd", to: "limit_nano"},
	{table: "access_key_cost_limit_states", from: "used_nano_usd", to: "used_nano"},
}

const (
	usageStatsIdentity0021 = "CREATE UNIQUE INDEX idx_usage_stats_identity ON usage_stats " +
		"(bucket_start_ms, access_key_id, channel_id, group_id, credential_id, model, currency)"
	autoUsagePrimaryKey0021 = "auto_decision_usage_stats_pkey"
)

// Up0021 records each cost in the settlement currency of the request beside
// its USD amount, keys usage aggregates by currency, and denominates
// AccessKey cost limits in the rule's own currency. Existing rows are USD.
func Up0021(db *gorm.DB) error {
	for _, set := range columnSets0021 {
		if err := addColumnSet0021(db, set); err != nil {
			return err
		}
	}
	if !usageStatsIdentityHasCurrency0021(db) {
		if err := db.Exec("DROP INDEX IF EXISTS idx_usage_stats_identity").Error; err != nil {
			return fmt.Errorf("drop usage_stats identity: %w", err)
		}
		if err := db.Exec(usageStatsIdentity0021).Error; err != nil {
			return fmt.Errorf("create usage_stats identity with currency: %w", err)
		}
	}
	if !autoUsagePrimaryKeyHasCurrency0021(db) {
		if err := db.Exec("ALTER TABLE auto_decision_usage_stats DROP CONSTRAINT " + autoUsagePrimaryKey0021 +
			", ADD CONSTRAINT " + autoUsagePrimaryKey0021 +
			" PRIMARY KEY (bucket_start_ms, access_key_id, model, currency)").Error; err != nil {
			return fmt.Errorf("rebuild auto_decision_usage_stats primary key: %w", err)
		}
	}
	for _, rename := range columnRenames0021 {
		if !db.Migrator().HasColumn(rename.table, rename.from) {
			continue
		}
		if err := db.Migrator().RenameColumn(rename.table, rename.from, rename.to); err != nil {
			return fmt.Errorf("rename %s.%s: %w", rename.table, rename.from, err)
		}
	}
	return Validate0021(db)
}

func addColumnSet0021(db *gorm.DB, set columnSet0021) error {
	added := false
	for _, field := range set.fields {
		if db.Migrator().HasColumn(set.model, field) {
			continue
		}
		if err := db.Migrator().AddColumn(set.model, field); err != nil {
			return fmt.Errorf("add %T.%s: %w", set.model, field, err)
		}
		added = true
	}
	if added && set.backfill != "" {
		if err := db.Exec(set.backfill).Error; err != nil {
			return fmt.Errorf("backfill %T: %w", set.model, err)
		}
	}
	for _, check := range set.checks {
		if db.Migrator().HasConstraint(set.model, check) {
			continue
		}
		if err := db.Migrator().CreateConstraint(set.model, check); err != nil {
			return fmt.Errorf("create %s: %w", check, err)
		}
	}
	return nil
}

func usageStatsIdentityHasCurrency0021(db *gorm.DB) bool {
	var definition string
	db.Raw("SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'idx_usage_stats_identity'").
		Scan(&definition)
	return strings.Contains(definition, "currency")
}

func autoUsagePrimaryKeyHasCurrency0021(db *gorm.DB) bool {
	var count int64
	db.Raw(`SELECT count(*) FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = 'auto_decision_usage_stats'::regclass AND i.indisprimary AND a.attname = 'currency'`).
		Scan(&count)
	return count == 1
}

func Validate0021(db *gorm.DB) error {
	for _, set := range columnSets0021 {
		for _, field := range set.fields {
			if !db.Migrator().HasColumn(set.model, field) {
				return fmt.Errorf("validate billing currency: %T.%s is missing", set.model, field)
			}
		}
		for _, check := range set.checks {
			if !db.Migrator().HasConstraint(set.model, check) {
				return fmt.Errorf("validate billing currency: constraint %s is missing", check)
			}
		}
	}
	for _, rename := range columnRenames0021 {
		if !db.Migrator().HasColumn(rename.table, rename.to) || db.Migrator().HasColumn(rename.table, rename.from) {
			return fmt.Errorf("validate billing currency: %s.%s was not renamed to %s", rename.table, rename.from, rename.to)
		}
	}
	if !usageStatsIdentityHasCurrency0021(db) {
		return fmt.Errorf("validate billing currency: usage_stats identity does not include currency")
	}
	if !autoUsagePrimaryKeyHasCurrency0021(db) {
		return fmt.Errorf("validate billing currency: auto_decision_usage_stats primary key does not include currency")
	}
	return nil
}

// ValidateCurrent0002 accepts the 0002 schema before and after 0021 renamed
// its amount columns.
func ValidateCurrent0002(db *gorm.DB) error {
	renamed := make(map[string]string, len(columnRenames0021))
	for _, rename := range columnRenames0021 {
		if !db.Migrator().HasColumn(rename.table, rename.from) {
			renamed[rename.table] = rename.from
		}
	}
	return validateSchema0002(db, renamed)
}
