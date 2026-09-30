package storage

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

func TestGroupUsageIndexSupportsPageWindowLookup(t *testing.T) {
	db := openEmptyTestDatabase(t)
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	var plan []string
	if err := db.Transaction(func(tx *gorm.DB) error {
		// An empty table is cheapest to scan sequentially; disable that path so
		// the plan shows which index the lookup can use.
		if err := tx.Exec("SET LOCAL enable_seqscan = off").Error; err != nil {
			return err
		}
		return tx.Raw("EXPLAIN SELECT SUM(output_tokens) FROM request_logs WHERE group_id IN (1,2) AND completed_at_ms >= 1000 AND completed_at_ms < 2000 AND attempt_count > 0").Scan(&plan).Error
	}); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	var indexCondition string
	for _, line := range plan {
		if strings.Contains(line, "Index Cond:") {
			indexCondition = line
		}
	}
	if !strings.Contains(text, "idx_request_logs_group_completed") ||
		!strings.Contains(indexCondition, "group_id") || !strings.Contains(indexCondition, "completed_at_ms") {
		t.Fatalf("current-page lookup must use group and time together:\n%s", text)
	}
	if err := applyMigrations(db); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
}
