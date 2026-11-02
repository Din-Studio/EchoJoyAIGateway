package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ID0028 = "0028_proxy_catalog"

type proxy0028 struct {
	ID                 uint   `gorm:"primaryKey;autoIncrement"`
	Name               string `gorm:"type:varchar(255);not null"`
	Config             string `gorm:"type:text;not null"`
	Fingerprint        string `gorm:"type:varchar(128);not null;uniqueIndex:ux_proxies_fingerprint"`
	Enabled            bool   `gorm:"not null;default:true;index:idx_proxies_enabled"`
	LastTestSequence   int64  `gorm:"column:last_test_sequence;not null;default:0;check:chk_proxy_test_sequence,last_test_sequence >= 0"`
	LastTestURL        string `gorm:"column:last_test_url;type:varchar(2048);not null;default:''"`
	LastTestAtMS       *int64 `gorm:"column:last_test_at_ms"`
	LastTestDurationMS *int64 `gorm:"column:last_test_duration_ms"`
	LastTestStatusCode *int   `gorm:"column:last_test_status_code"`
	LastTestError      string `gorm:"type:varchar(64);not null;default:''"`
	CreatedAtMS        int64  `gorm:"column:created_at_ms;not null;autoCreateTime:milli"`
	UpdatedAtMS        int64  `gorm:"column:updated_at_ms;not null;autoUpdateTime:milli"`
}

func (proxy0028) TableName() string { return "proxies" }

// Up0028 creates storage only; control bootstrap converts encrypted legacy overrides.
func Up0028(db *gorm.DB) error {
	if err := validateProxyTable0028(db); err != nil {
		return err
	}
	if !db.Migrator().HasTable(&proxy0028{}) {
		if err := db.Migrator().CreateTable(&proxy0028{}); err != nil {
			return err
		}
	}
	for _, name := range []string{"ux_proxies_fingerprint", "idx_proxies_enabled"} {
		if !db.Migrator().HasIndex(&proxy0028{}, name) {
			if err := db.Migrator().CreateIndex(&proxy0028{}, name); err != nil {
				return err
			}
		}
	}
	if !db.Migrator().HasConstraint(&proxy0028{}, "chk_proxy_test_sequence") {
		if err := db.Migrator().CreateConstraint(&proxy0028{}, "chk_proxy_test_sequence"); err != nil {
			return err
		}
	}
	return Validate0028(db)
}

func validateProxyTable0028(db *gorm.DB) error {
	if !db.Migrator().HasTable(&proxy0028{}) {
		return nil
	}
	columns, err := db.Migrator().ColumnTypes(&proxy0028{})
	if err != nil {
		return err
	}
	statement := &gorm.Statement{DB: db}
	if err := statement.Parse(&proxy0028{}); err != nil {
		return err
	}
	found := make(map[string]bool, len(columns))
	for _, column := range columns {
		field := statement.Schema.LookUpField(column.Name())
		if field == nil {
			return fmt.Errorf("unexpected proxies column %s", column.Name())
		}
		found[column.Name()] = true
		typeName := strings.ToLower(column.DatabaseTypeName())
		if (field.DataType == "int" || field.DataType == "uint") && !strings.Contains(typeName, "int") {
			return fmt.Errorf("proxies column %s must be integer", column.Name())
		}
		if field.DataType == "string" && !strings.Contains(typeName, "char") && !strings.Contains(typeName, "text") {
			return fmt.Errorf("proxies column %s must be text", column.Name())
		}
		if field.DataType == "bool" && typeName != "bool" && typeName != "boolean" {
			return fmt.Errorf("proxies column %s must be boolean", column.Name())
		}
		if nullable, known := column.Nullable(); field.NotNull && known && nullable {
			return fmt.Errorf("proxies column %s must be non-null", column.Name())
		}
		if primary, known := column.PrimaryKey(); field.PrimaryKey && known && !primary {
			return fmt.Errorf("proxies column %s must be primary key", column.Name())
		}
	}
	for _, field := range statement.Schema.Fields {
		if !found[field.DBName] {
			return fmt.Errorf("incomplete proxies table: missing %s", field.DBName)
		}
	}
	indexes, err := db.Migrator().GetIndexes(&proxy0028{})
	if err != nil {
		return err
	}
	for _, index := range indexes {
		column := ""
		unique := false
		switch index.Name() {
		case "ux_proxies_fingerprint":
			column, unique = "fingerprint", true
		case "idx_proxies_enabled":
			column = "enabled"
		default:
			continue
		}
		actualUnique, known := index.Unique()
		if fields := index.Columns(); len(fields) != 1 || fields[0] != column || !known || actualUnique != unique {
			return fmt.Errorf("incompatible proxy index %s", index.Name())
		}
	}
	return nil
}

func Validate0028(db *gorm.DB) error {
	if !db.Migrator().HasConstraint(&proxy0028{}, "chk_proxy_test_sequence") {
		return fmt.Errorf("proxy test sequence constraint is missing")
	}
	if !db.Migrator().HasTable(&proxy0028{}) {
		return fmt.Errorf("proxies table is missing")
	}
	if err := validateProxyTable0028(db); err != nil {
		return err
	}
	for _, name := range []string{"ux_proxies_fingerprint", "idx_proxies_enabled"} {
		if !db.Migrator().HasIndex(&proxy0028{}, name) {
			return fmt.Errorf("proxy index %s is missing", name)
		}
	}
	return nil
}
