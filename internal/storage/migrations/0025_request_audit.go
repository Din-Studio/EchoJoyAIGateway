package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const ID0025 = "0025_request_audit"

// 冻结本次迁移的字段，后续运行模型变化不能改写历史 DDL。
type requestAudit0025 struct {
	RequestAudit             []byte `gorm:"type:json"`
	AuditModel               string `gorm:"type:varchar(512);not null;default:''"`
	AuditGroupID             uint   `gorm:"not null;default:0"`
	AuditChannelID           string `gorm:"type:varchar(64);not null;default:''"`
	AuditCredentialID        uint   `gorm:"not null;default:0"`
	AuditCostNanoUSD         int64  `gorm:"column:audit_cost_nano_usd;not null;default:0;check:chk_request_log_audit_cost,audit_cost_nano_usd >= 0"`
	AuditCostNano            int64  `gorm:"column:audit_cost_nano;not null;default:0;check:chk_request_log_audit_cost_local,audit_cost_nano >= 0"`
	AuditPricingCompleteness string `gorm:"type:varchar(32);not null;default:'not_applicable'"`
}

func (requestAudit0025) TableName() string { return "request_logs" }

var requestAuditColumns0025 = columnSet0021{
	model: &requestAudit0025{},
	fields: []string{
		"RequestAudit", "AuditModel", "AuditGroupID", "AuditChannelID", "AuditCredentialID",
		"AuditCostNanoUSD", "AuditCostNano", "AuditPricingCompleteness",
	},
	checks: []string{"chk_request_log_audit_cost", "chk_request_log_audit_cost_local"},
}

// Up0025 records guardrail review results on the request log. A request's
// guardrail JEV calls share one route, so their summed cost sits beside the
// automatic decision columns instead of in a separate table.
func Up0025(db *gorm.DB) error {
	if err := addColumnSet0021(db, requestAuditColumns0025); err != nil {
		return err
	}
	return Validate0025(db)
}

func Validate0025(db *gorm.DB) error {
	set := requestAuditColumns0025
	for _, field := range set.fields {
		if !db.Migrator().HasColumn(set.model, field) {
			return fmt.Errorf("validate request audit: request_logs.%s is missing", field)
		}
	}
	for _, check := range set.checks {
		if !db.Migrator().HasConstraint(set.model, check) {
			return fmt.Errorf("validate request audit: constraint %s is missing", check)
		}
	}
	return nil
}
