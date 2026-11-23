package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ID0033 = "0033_model_price_reference"

type modelPrice0033 struct {
	ID            uint            `gorm:"primaryKey;autoIncrement"`
	SourcePriceID *uint           `gorm:"index:idx_model_prices_source"`
	SourcePrice   *modelPrice0033 `gorm:"foreignKey:SourcePriceID;references:ID;constraint:OnDelete:RESTRICT"`
}

func (modelPrice0033) TableName() string { return "model_prices" }

// Up0033 保存模型间的价格关联；价格值仍在原列中持久化。
func Up0033(db *gorm.DB) error {
	if !db.Migrator().HasTable(&modelPrice0033{}) {
		return fmt.Errorf("model_prices table is missing")
	}
	if err := validateReferenceColumn0033(db, false); err != nil {
		return err
	}
	model := &modelPrice0033{}
	if !db.Migrator().HasColumn(model, "SourcePriceID") {
		if err := db.Migrator().AddColumn(model, "SourcePriceID"); err != nil {
			return fmt.Errorf("add model price source: %w", err)
		}
	}
	if !db.Migrator().HasIndex(model, "idx_model_prices_source") {
		if err := db.Migrator().CreateIndex(model, "idx_model_prices_source"); err != nil {
			return err
		}
	}
	if !db.Migrator().HasConstraint(model, "SourcePrice") {
		if err := db.Migrator().CreateConstraint(model, "SourcePrice"); err != nil {
			return err
		}
	}
	return Validate0033(db)
}

func Validate0033(db *gorm.DB) error {
	if err := validateReferenceColumn0033(db, true); err != nil {
		return err
	}
	// Names alone do not prove a single-column index or a RESTRICT self-reference.
	var valid struct {
		IndexOK      bool
		ForeignKeyOK bool
	}
	if err := db.Raw(`SELECT
		EXISTS (SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_am am ON am.oid = c.relam
			WHERE i.indrelid = 'model_prices'::regclass AND c.relname = 'idx_model_prices_source'
			AND am.amname = 'btree' AND i.indisvalid AND i.indisready
			AND NOT i.indisunique AND NOT i.indisprimary
			AND i.indnatts = 1 AND i.indkey::text = a.attnum::text
			AND i.indpred IS NULL AND i.indexprs IS NULL) AS index_ok,
		EXISTS (SELECT 1 FROM pg_constraint c
			WHERE c.conrelid = 'model_prices'::regclass AND c.confrelid = c.conrelid
			AND c.conname = 'fk_model_prices_source_price' AND c.contype = 'f'
			AND c.conkey = ARRAY[a.attnum] AND c.confkey = ARRAY[id.attnum]
			AND c.confdeltype = 'r' AND c.convalidated AND NOT c.condeferrable) AS foreign_key_ok
		FROM pg_attribute a JOIN pg_attribute id ON id.attrelid = a.attrelid AND id.attname = 'id'
		WHERE a.attrelid = 'model_prices'::regclass AND a.attname = 'source_price_id'`).Scan(&valid).Error; err != nil {
		return err
	}
	if !valid.IndexOK || !valid.ForeignKeyOK {
		return fmt.Errorf("invalid model price source index or RESTRICT self-reference")
	}
	return nil
}

func validateReferenceColumn0033(db *gorm.DB, required bool) error {
	columns, err := db.Migrator().ColumnTypes(&modelPrice0033{})
	if err != nil {
		return err
	}
	for _, column := range columns {
		if column.Name() != "source_price_id" {
			continue
		}
		if kind := strings.ToLower(column.DatabaseTypeName()); kind != "int8" && kind != "bigint" {
			return fmt.Errorf("model price source must be a bigint")
		}
		if nullable, known := column.Nullable(); !known || !nullable {
			return fmt.Errorf("model price source must be nullable")
		}
		if value, known := column.DefaultValue(); known && !strings.EqualFold(strings.TrimSpace(value), "null") {
			return fmt.Errorf("model price source must default to null")
		}
		return nil
	}
	if required {
		return fmt.Errorf("model price source column is missing")
	}
	return nil
}
