package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const ID0026 = "0026_access_key_concurrency"

// 冻结本次迁移的字段，后续运行模型变化不能改写历史 DDL。
type accessKeyConcurrency0026 struct {
	ConcurrencyLimit *int64 `gorm:"type:bigint;check:chk_access_key_concurrency,concurrency_limit >= 0 AND concurrency_limit <= 9007199254740991"`
}

func (accessKeyConcurrency0026) TableName() string { return "access_keys" }

var accessKeyConcurrencyColumns0026 = columnSet0021{
	model:  &accessKeyConcurrency0026{},
	fields: []string{"ConcurrencyLimit"},
	checks: []string{"chk_access_key_concurrency"},
}

// Up0026 adds the per-key concurrency limit; NULL inherits the default
// setting and 0 is unlimited.
func Up0026(db *gorm.DB) error {
	if err := addColumnSet0021(db, accessKeyConcurrencyColumns0026); err != nil {
		return err
	}
	return Validate0026(db)
}

func Validate0026(db *gorm.DB) error {
	set := accessKeyConcurrencyColumns0026
	if !db.Migrator().HasColumn(set.model, "ConcurrencyLimit") {
		return fmt.Errorf("validate access key concurrency: column access_keys.concurrency_limit is missing")
	}
	if !db.Migrator().HasConstraint(set.model, "chk_access_key_concurrency") {
		return fmt.Errorf("validate access key concurrency: constraint chk_access_key_concurrency is missing")
	}
	return nil
}
