package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const ID0024 = "0024_auto_decision_attribution"

var autoDecisionLogColumns0024 = []string{"DecisionGroupID", "DecisionChannelID", "DecisionCredentialID"}

var autoDecisionUsageColumns0024 = []string{"GroupID", "ChannelID", "CredentialID"}

// 冻结本次迁移的字段，后续运行模型变化不能改写历史 DDL。
type autoLog0024 struct {
	DecisionGroupID      uint   `gorm:"not null;default:0"`
	DecisionChannelID    string `gorm:"type:varchar(64);not null;default:''"`
	DecisionCredentialID uint   `gorm:"not null;default:0"`
}

func (autoLog0024) TableName() string { return "request_logs" }

type autoUsage0024 struct {
	GroupID      uint   `gorm:"not null;default:0"`
	ChannelID    string `gorm:"type:varchar(64);not null;default:''"`
	CredentialID uint   `gorm:"not null;default:0"`
}

func (autoUsage0024) TableName() string { return "auto_decision_usage_stats" }

// Up0024 把决策实际走到的分组、渠道和凭据记入请求日志，并加入决策用量的主键；
// 已有用量行的这三列取默认值，历史合计不变。
func Up0024(db *gorm.DB) error {
	for _, name := range autoDecisionLogColumns0024 {
		if !db.Migrator().HasColumn(&autoLog0024{}, name) {
			if err := db.Migrator().AddColumn(&autoLog0024{}, name); err != nil {
				return fmt.Errorf("add automatic decision attribution column %s: %w", name, err)
			}
		}
	}
	for _, name := range autoDecisionUsageColumns0024 {
		if !db.Migrator().HasColumn(&autoUsage0024{}, name) {
			if err := db.Migrator().AddColumn(&autoUsage0024{}, name); err != nil {
				return fmt.Errorf("add automatic decision usage column %s: %w", name, err)
			}
		}
	}
	if err := db.Exec(`ALTER TABLE auto_decision_usage_stats
		DROP CONSTRAINT IF EXISTS auto_decision_usage_stats_pkey,
		ADD CONSTRAINT auto_decision_usage_stats_pkey
			PRIMARY KEY (bucket_start_ms, access_key_id, group_id, channel_id, credential_id, model, currency)`).Error; err != nil {
		return fmt.Errorf("extend automatic decision usage primary key: %w", err)
	}
	return Validate0024(db)
}

func Validate0024(db *gorm.DB) error {
	for _, name := range autoDecisionLogColumns0024 {
		if !db.Migrator().HasColumn(&autoLog0024{}, name) {
			return fmt.Errorf("automatic decision attribution log column %s missing", name)
		}
	}
	for _, name := range autoDecisionUsageColumns0024 {
		if !db.Migrator().HasColumn(&autoUsage0024{}, name) {
			return fmt.Errorf("automatic decision usage column %s missing", name)
		}
	}
	var keyColumns int64
	if err := db.Raw(`SELECT count(*) FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = 'auto_decision_usage_stats'::regclass AND i.indisprimary`).Scan(&keyColumns).Error; err != nil {
		return fmt.Errorf("read automatic decision usage primary key: %w", err)
	}
	if keyColumns != 7 {
		return fmt.Errorf("automatic decision usage primary key has %d columns, want 7", keyColumns)
	}
	return nil
}
