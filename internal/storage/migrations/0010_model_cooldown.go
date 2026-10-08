package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ID0010 = "0010_model_cooldown"
const modelCooldownTable0010 = "request_log_attempts"
const modelCooldownEffect0010 = "chk_request_log_attempt_effect"
const modelCooldownDeadline0010 = "chk_request_log_attempt_cooldown"
const modelCooldownEffectExpression0010 = "effect IN ('','none','cooldown_credential','cooldown_model','record_credential_failure','skip_group') AND (effect <> 'cooldown_model' OR cooldown_until_ms IS NOT NULL)"

// Up0010 只扩展日志合同；运行态模型冷却仍由 Registry 与检查点管理。
func Up0010(db *gorm.DB) error {
	if err := ValidateRecoverable0010(db); err != nil {
		return err
	}
	if Validate0010(db) == nil {
		return nil
	}
	if !db.Migrator().HasColumn(modelCooldownTable0010, "cooldown_until_ms") {
		if err := db.Exec("ALTER TABLE request_log_attempts ADD COLUMN cooldown_until_ms BIGINT NULL CONSTRAINT chk_request_log_attempt_cooldown CHECK (cooldown_until_ms IS NULL OR cooldown_until_ms >= 0)").Error; err != nil {
			return err
		}
	}
	// 同一条 DDL 内替换 effect 约束，事务失败时不会留下缺约束的中间态。
	if err := db.Exec("ALTER TABLE request_log_attempts DROP CONSTRAINT chk_request_log_attempt_effect, ADD CONSTRAINT chk_request_log_attempt_effect CHECK (" + modelCooldownEffectExpression0010 + ")").Error; err != nil {
		return err
	}
	return Validate0010(db)
}

// ValidateRecoverable0010 是 Up0010 与 Validate0010 的前置校验：已存在的冷却列必须是可空整数并带约束。
func ValidateRecoverable0010(db *gorm.DB) error {
	if !db.Migrator().HasTable(modelCooldownTable0010) {
		return fmt.Errorf("model cooldown attempts table is missing")
	}
	columns, err := db.Migrator().ColumnTypes(modelCooldownTable0010)
	if err != nil {
		return err
	}
	hasColumn := false
	for _, column := range columns {
		if column.Name() != "cooldown_until_ms" {
			continue
		}
		hasColumn = true
		if !strings.Contains(strings.ToLower(column.DatabaseTypeName()), "int") {
			return fmt.Errorf("model cooldown deadline must be integer")
		}
		if nullable, known := column.Nullable(); !known || !nullable {
			return fmt.Errorf("model cooldown deadline must be nullable")
		}
	}
	if hasColumn && !db.Migrator().HasConstraint(modelCooldownTable0010, modelCooldownDeadline0010) {
		return fmt.Errorf("model cooldown deadline constraint is missing")
	}
	if !db.Migrator().HasConstraint(modelCooldownTable0010, modelCooldownEffect0010) {
		return fmt.Errorf("attempt effect constraint is missing")
	}
	return nil
}

func Validate0010(db *gorm.DB) error {
	if err := ValidateRecoverable0010(db); err != nil {
		return err
	}
	if !db.Migrator().HasColumn(modelCooldownTable0010, "cooldown_until_ms") {
		return fmt.Errorf("model cooldown deadline column is missing")
	}
	definition, err := modelCooldownConstraint0010(db, modelCooldownEffect0010)
	if err != nil {
		return err
	}
	if !strings.Contains(definition, "cooldown_model") || !strings.Contains(strings.ToLower(definition), "is not null") {
		return fmt.Errorf("model cooldown effect constraint is invalid")
	}
	deadline, err := modelCooldownConstraint0010(db, modelCooldownDeadline0010)
	if err != nil {
		return err
	}
	normalized := strings.NewReplacer(" ", "", "\n", "", "`", "", `"`, "", "(", "", ")", "", "::bigint", "").Replace(strings.ToLower(deadline))
	if !strings.Contains(normalized, "cooldown_until_ms>=0") {
		return fmt.Errorf("model cooldown deadline bounds are invalid")
	}
	return Validate0006(db)
}

func modelCooldownConstraint0010(db *gorm.DB, name string) (string, error) {
	var definition string
	if err := db.Raw("SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = ? AND conrelid = ?::regclass", name, modelCooldownTable0010).Scan(&definition).Error; err != nil {
		return "", err
	}
	if definition == "" {
		return "", fmt.Errorf("model cooldown constraint %q is missing", name)
	}
	return definition, nil
}
