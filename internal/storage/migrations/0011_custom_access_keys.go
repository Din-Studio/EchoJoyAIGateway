package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ID0011 = "0011_custom_access_keys"
const accessKeySuffixCheck0011 = "chk_access_key_suffix"
const accessKeySuffixExpression0011 = "length(key_suffix) = 4"

// Up0011 保留四位尾号字段，允许自定义字符及短密钥的全遮罩。
func Up0011(db *gorm.DB) error {
	if err := ValidateRecoverable0011(db); err != nil {
		return err
	}
	if Validate0011(db) == nil {
		return nil
	}
	// 同一条 DDL 内替换尾号约束，事务失败时不会留下缺约束的中间态。
	if err := db.Exec("ALTER TABLE access_keys DROP CONSTRAINT chk_access_key_suffix, ADD CONSTRAINT chk_access_key_suffix CHECK (" + accessKeySuffixExpression0011 + ")").Error; err != nil {
		return err
	}
	return Validate0011(db)
}

func ValidateRecoverable0011(db *gorm.DB) error {
	if !db.Migrator().HasTable("access_keys") || !db.Migrator().HasColumn("access_keys", "key_suffix") || !db.Migrator().HasConstraint("access_keys", accessKeySuffixCheck0011) {
		return fmt.Errorf("custom access key suffix schema is incomplete")
	}
	return nil
}

func Validate0011(db *gorm.DB) error {
	if err := ValidateRecoverable0011(db); err != nil {
		return err
	}
	definition, err := accessKeySuffixConstraint0011(db)
	if err != nil {
		return err
	}
	normalized := strings.NewReplacer(" ", "", "\n", "", "\t", "", "`", "", `"`, "", "(", "", ")", "", "::text", "").Replace(strings.ToLower(definition))
	if normalized != "lengthkey_suffix=4" && normalized != "checklengthkey_suffix=4" {
		return fmt.Errorf("custom access key suffix constraint is invalid")
	}
	return nil
}

func accessKeySuffixConstraint0011(db *gorm.DB) (string, error) {
	var definition string
	err := db.Raw("SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = ? AND conrelid = 'access_keys'::regclass", accessKeySuffixCheck0011).Scan(&definition).Error
	return definition, err
}
