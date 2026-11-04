package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ID0030 = "0030_request_log_client_ip"
const requestLogIPIndex0030 = "idx_request_logs_ip_completed_id"

type requestLog0030 struct {
	ClientIP      *string `gorm:"column:client_ip;type:varchar(45);index:idx_request_logs_ip_completed_id,priority:1"`
	CompletedAtMS int64   `gorm:"column:completed_at_ms;index:idx_request_logs_ip_completed_id,priority:2,sort:desc"`
	ID            string  `gorm:"column:id;index:idx_request_logs_ip_completed_id,priority:3,sort:desc"`
}

func (requestLog0030) TableName() string { return "request_logs" }

// Up0030 adds the nullable client IP and exact-search index without backfilling logs.
func Up0030(db *gorm.DB) error {
	if !db.Migrator().HasTable(&requestLog0030{}) {
		return fmt.Errorf("request log client IP: request_logs table is missing")
	}
	if !db.Migrator().HasColumn(&requestLog0030{}, "client_ip") {
		if err := db.Migrator().AddColumn(&requestLog0030{}, "ClientIP"); err != nil {
			return fmt.Errorf("add request log client IP: %w", err)
		}
	}
	if !db.Migrator().HasIndex(&requestLog0030{}, requestLogIPIndex0030) {
		if err := db.Migrator().CreateIndex(&requestLog0030{}, requestLogIPIndex0030); err != nil {
			return fmt.Errorf("create request log client IP index: %w", err)
		}
	}
	return Validate0030(db)
}

func Validate0030(db *gorm.DB) error {
	columnTypes, err := db.Migrator().ColumnTypes("request_logs")
	if err != nil {
		return err
	}
	found := false
	for _, column := range columnTypes {
		if column.Name() != "client_ip" {
			continue
		}
		kind := strings.ToLower(column.DatabaseTypeName())
		if kind != "varchar" && kind != "character varying" {
			return fmt.Errorf("client_ip must be varchar(45)")
		}
		if length, known := column.Length(); !known || length != 45 {
			return fmt.Errorf("client_ip must have length 45")
		}
		if nullable, known := column.Nullable(); !known || !nullable {
			return fmt.Errorf("client_ip must be nullable")
		}
		found = true
		break
	}
	if !found {
		return fmt.Errorf("client_ip column is missing")
	}
	// GORM's PostgreSQL GetIndexes omits column ordering and sort direction.
	var columns []struct {
		Name       string
		Descending bool
		IsUnique   bool
		IsValid    bool
	}
	if err := db.Raw(`
		SELECT attribute.attname AS name,
			(definition.indoption[key.position - 1] & 1) <> 0 AS descending,
			definition.indisunique AS is_unique, definition.indisvalid AS is_valid
		FROM pg_index AS definition
		JOIN pg_class AS table_relation ON table_relation.oid = definition.indrelid
		JOIN pg_namespace AS namespace ON namespace.oid = table_relation.relnamespace
		JOIN pg_class AS index_relation ON index_relation.oid = definition.indexrelid
		CROSS JOIN LATERAL unnest(definition.indkey) WITH ORDINALITY AS key(attnum, position)
		JOIN pg_attribute AS attribute ON attribute.attrelid = table_relation.oid
			AND attribute.attnum = key.attnum
		WHERE namespace.nspname = current_schema() AND table_relation.relname = 'request_logs'
			AND index_relation.relname = ?
		ORDER BY key.position`, requestLogIPIndex0030).Scan(&columns).Error; err != nil {
		return fmt.Errorf("read request log client IP index: %w", err)
	}
	want := []struct {
		name string
		desc bool
	}{
		{name: "client_ip"},
		{name: "completed_at_ms", desc: true},
		{name: "id", desc: true},
	}
	if len(columns) != len(want) {
		return fmt.Errorf("request log client IP index is missing or has an unexpected definition")
	}
	for position, column := range columns {
		if column.Name != want[position].name || column.Descending != want[position].desc || column.IsUnique || !column.IsValid {
			return fmt.Errorf("request log client IP index has an unexpected definition")
		}
	}
	return nil
}
