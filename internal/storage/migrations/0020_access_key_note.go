package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const ID0020 = "0020_access_key_note"

// 冻结本次迁移的字段，后续运行模型变化不能改写历史 DDL。
type accessKeyNote0020 struct {
	Note string `gorm:"type:text;not null;default:''"`
}

func (accessKeyNote0020) TableName() string { return "access_keys" }

// Up0020 adds an operator note to access keys; existing rows default to an empty note.
func Up0020(db *gorm.DB) error {
	model := &accessKeyNote0020{}
	if !db.Migrator().HasColumn(model, "Note") {
		if err := db.Migrator().AddColumn(model, "Note"); err != nil {
			return fmt.Errorf("add access_keys.note: %w", err)
		}
	}
	return Validate0020(db)
}

func Validate0020(db *gorm.DB) error {
	if !db.Migrator().HasColumn(&accessKeyNote0020{}, "Note") {
		return fmt.Errorf("validate access key note: column access_keys.note is missing")
	}
	return nil
}
