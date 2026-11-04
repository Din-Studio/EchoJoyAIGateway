package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ID0029 = "0029_credential_names"

// Frozen display metadata; existing credentials keep an empty alias.
type credentialName0029 struct {
	Name string `gorm:"type:varchar(255);not null;default:''"`
}

func (credentialName0029) TableName() string { return "credentials" }

func Up0029(db *gorm.DB) error {
	if err := addColumnSet0021(db, columnSet0021{
		model:  &credentialName0029{},
		fields: []string{"Name"},
	}); err != nil {
		return err
	}
	return Validate0029(db)
}

func Validate0029(db *gorm.DB) error {
	columns, err := db.Migrator().ColumnTypes("credentials")
	if err != nil {
		return err
	}
	for _, column := range columns {
		if column.Name() != "name" {
			continue
		}
		if !strings.Contains(strings.ToLower(column.DatabaseTypeName()), "char") {
			return fmt.Errorf("credential name must be varchar")
		}
		if nullable, known := column.Nullable(); !known || nullable {
			return fmt.Errorf("credential name must not be nullable")
		}
		if length, known := column.Length(); known && length != 255 {
			return fmt.Errorf("credential name length must be 255")
		}
		value, known := column.DefaultValue()
		if !known || value != "" && value != "''" && !strings.HasPrefix(value, "''::") {
			return fmt.Errorf("credential name default must be empty")
		}
		return nil
	}
	return fmt.Errorf("credential name is missing")
}
