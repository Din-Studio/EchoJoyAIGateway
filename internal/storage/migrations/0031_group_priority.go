package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ID0031 = "0031_group_priority"

type group0031 struct {
	Priority int32 `gorm:"type:integer;not null;default:0"`
}

func (group0031) TableName() string { return "groups" }

// Up0031 旧分组统一使用 0，保留原有同档调度行为。
func Up0031(db *gorm.DB) error {
	if !db.Migrator().HasTable(&group0031{}) {
		return fmt.Errorf("groups table is missing")
	}
	if !db.Migrator().HasColumn(&group0031{}, "priority") {
		if err := db.Migrator().AddColumn(&group0031{}, "Priority"); err != nil {
			return fmt.Errorf("add group priority: %w", err)
		}
	}
	return Validate0031(db)
}

func Validate0031(db *gorm.DB) error {
	columns, err := db.Migrator().ColumnTypes(&group0031{})
	if err != nil {
		return err
	}
	for _, column := range columns {
		if column.Name() != "priority" {
			continue
		}
		if strings.ToLower(column.DatabaseTypeName()) != "int4" {
			return fmt.Errorf("group priority must be a signed 32-bit integer")
		}
		if nullable, known := column.Nullable(); !known || nullable {
			return fmt.Errorf("group priority must not be nullable")
		}
		value, known := column.DefaultValue()
		if !known || strings.Trim(strings.SplitN(value, "::", 2)[0], "'\"() ") != "0" {
			return fmt.Errorf("group priority default must be zero")
		}
		return nil
	}
	return fmt.Errorf("group priority is missing")
}
