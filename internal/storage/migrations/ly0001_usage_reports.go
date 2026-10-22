package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const IDLy0001 = "ly0001_usage_reports"

// 冻结本次迁移的表与字段，后续运行模型变化不能改写历史 DDL。
// 每行是一条尚未被灵影网关接受的用量上报；next_attempt_at_ms 为空表示已停放、不再投递。
type lyUsageReportLy0001 struct {
	RequestID       string `gorm:"column:request_id;type:varchar(36);primaryKey"`
	Payload         string `gorm:"column:payload;type:jsonb;not null"`
	Attempts        int    `gorm:"column:attempts;not null;default:0;check:chk_ly_usage_report_attempts,attempts >= 0"`
	NextAttemptAtMS *int64 `gorm:"column:next_attempt_at_ms;index:idx_ly_usage_reports_due,where:next_attempt_at_ms IS NOT NULL"`
	LastError       string `gorm:"column:last_error;type:text;not null;default:''"`
}

func (lyUsageReportLy0001) TableName() string { return "ly_usage_reports" }

// UpLy0001 adds the outbox of usage reports owed to the Lingying gateway.
func UpLy0001(db *gorm.DB) error {
	if !db.Migrator().HasTable(&lyUsageReportLy0001{}) {
		if err := db.Migrator().CreateTable(&lyUsageReportLy0001{}); err != nil {
			return fmt.Errorf("create ly usage reports: %w", err)
		}
	}
	return ValidateLy0001(db)
}

func ValidateLy0001(db *gorm.DB) error {
	model := &lyUsageReportLy0001{}
	if !db.Migrator().HasTable(model) {
		return fmt.Errorf("validate ly usage reports: table is missing")
	}
	for _, column := range []string{"request_id", "payload", "attempts", "next_attempt_at_ms", "last_error"} {
		if !db.Migrator().HasColumn(model, column) {
			return fmt.Errorf("validate ly usage reports: column %q is missing", column)
		}
	}
	if !db.Migrator().HasIndex(model, "idx_ly_usage_reports_due") {
		return fmt.Errorf("validate ly usage reports: due index is missing")
	}
	if !db.Migrator().HasConstraint(model, "chk_ly_usage_report_attempts") {
		return fmt.Errorf("validate ly usage reports: constraint chk_ly_usage_report_attempts is missing")
	}
	return nil
}
