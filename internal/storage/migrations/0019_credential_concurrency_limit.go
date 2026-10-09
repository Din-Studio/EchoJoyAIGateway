package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const ID0019 = "0019_credential_concurrency_limit"

const credentialConcurrencyLimitCheck0019 = "chk_credential_concurrency_limit"

// 冻结本次迁移的字段，后续运行模型变化不能改写历史 DDL。
type credentialConcurrencyLimit0019 struct {
	ConcurrencyLimit int `gorm:"not null;default:0;check:chk_credential_concurrency_limit,concurrency_limit >= 0"`
}

func (credentialConcurrencyLimit0019) TableName() string { return "credentials" }

// Up0019 adds a per-credential concurrency limit; existing rows default to 0 (unlimited).
func Up0019(db *gorm.DB) error {
	model := &credentialConcurrencyLimit0019{}
	if !db.Migrator().HasColumn(model, "ConcurrencyLimit") {
		if err := db.Migrator().AddColumn(model, "ConcurrencyLimit"); err != nil {
			return fmt.Errorf("add credentials.concurrency_limit: %w", err)
		}
	}
	if !db.Migrator().HasConstraint(model, credentialConcurrencyLimitCheck0019) {
		if err := db.Migrator().CreateConstraint(model, credentialConcurrencyLimitCheck0019); err != nil {
			return fmt.Errorf("add %s: %w", credentialConcurrencyLimitCheck0019, err)
		}
	}
	return Validate0019(db)
}

func Validate0019(db *gorm.DB) error {
	model := &credentialConcurrencyLimit0019{}
	if !db.Migrator().HasColumn(model, "ConcurrencyLimit") {
		return fmt.Errorf("validate credential concurrency limit: column credentials.concurrency_limit is missing")
	}
	if !db.Migrator().HasConstraint(model, credentialConcurrencyLimitCheck0019) {
		return fmt.Errorf("validate credential concurrency limit: constraint %s is missing", credentialConcurrencyLimitCheck0019)
	}
	return nil
}
