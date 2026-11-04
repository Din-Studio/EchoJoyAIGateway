package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ID0027 = "0027_request_log_output_timing"

// Frozen migration model: historical logs keep NULL timing, without backfill.
type requestLogOutputTiming0027 struct {
	FirstOutputMs *int64 `gorm:"column:first_output_ms;type:bigint;check:chk_request_log_first_output,first_output_ms >= 0"`
	LastOutputMs  *int64 `gorm:"column:last_output_ms;type:bigint;check:chk_request_log_last_output,last_output_ms >= 0"`
}

func (requestLogOutputTiming0027) TableName() string { return "request_logs" }

func Up0027(db *gorm.DB) error {
	if err := addColumnSet0021(db, columnSet0021{
		model:  &requestLogOutputTiming0027{},
		fields: []string{"FirstOutputMs", "LastOutputMs"},
		checks: []string{"chk_request_log_first_output", "chk_request_log_last_output"},
	}); err != nil {
		return err
	}
	return Validate0027(db)
}

func Validate0027(db *gorm.DB) error {
	columns, err := db.Migrator().ColumnTypes("request_logs")
	if err != nil {
		return err
	}
	for _, name := range []string{"first_output", "last_output"} {
		found := false
		for _, column := range columns {
			if column.Name() != name+"_ms" {
				continue
			}
			found = true
			if !strings.Contains(strings.ToLower(column.DatabaseTypeName()), "int") {
				return fmt.Errorf("%s_ms must be integer", name)
			}
			if nullable, known := column.Nullable(); !known || !nullable {
				return fmt.Errorf("%s_ms must be nullable", name)
			}
			if !db.Migrator().HasConstraint("request_logs", "chk_request_log_"+name) {
				return fmt.Errorf("%s constraint is missing", name)
			}
		}
		if !found {
			return fmt.Errorf("%s_ms is missing", name)
		}
	}
	return nil
}
