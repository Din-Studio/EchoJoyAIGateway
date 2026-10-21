package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const ID0024 = "0024_client_model_overrides"

// 冻结本次迁移的表与字段，后续运行模型变化不能改写历史 DDL。
type clientModelOverride0024 struct {
	ModelHash   string `gorm:"type:varchar(64);primaryKey;not null"`
	ClientModel string `gorm:"type:text;not null"`
	Overrides   string `gorm:"type:text;not null"`
}

func (clientModelOverride0024) TableName() string { return "client_model_overrides" }

// Up0024 creates the per-client-model catalog overrides served to Codex clients.
func Up0024(db *gorm.DB) error {
	if !db.Migrator().HasTable(&clientModelOverride0024{}) {
		if err := db.Migrator().CreateTable(&clientModelOverride0024{}); err != nil {
			return fmt.Errorf("create client model overrides: %w", err)
		}
	}
	return Validate0024(db)
}

func Validate0024(db *gorm.DB) error {
	model := &clientModelOverride0024{}
	if !db.Migrator().HasTable(model) {
		return fmt.Errorf("validate client model overrides: table client_model_overrides is missing")
	}
	for _, name := range []string{"ModelHash", "ClientModel", "Overrides"} {
		if !db.Migrator().HasColumn(model, name) {
			return fmt.Errorf("validate client model overrides: column %s is missing", name)
		}
	}
	return nil
}
