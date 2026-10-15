package migrations_test

import (
	"testing"

	"gpt-load/internal/storage/migrations"
)

func TestAccessKeyNoteMigrationDefaultsExistingRowsToEmpty(t *testing.T) {
	t.Parallel()
	db := openInitialTestDatabase(t)
	if err := migrations.Up0001(db); err != nil {
		t.Fatalf("prepare previous schema: %v", err)
	}
	if err := migrations.Validate0020(db); err == nil {
		t.Fatal("missing note column accepted")
	}
	if err := db.Exec(`INSERT INTO access_keys (
		id, name, key_value, key_hash, key_suffix, status, rpm_limit, created_at_ms, updated_at_ms
	) VALUES (1, 'note migration', 'cipher', 'hash', 'abcd', 'active', 0, 1, 1)`).Error; err != nil {
		t.Fatalf("create access key: %v", err)
	}

	for range 2 {
		if err := migrations.Up0020(db); err != nil {
			t.Fatalf("Up0020() error = %v", err)
		}
	}
	var note string
	if err := db.Table("access_keys").Select("note").Where("id = ?", 1).Scan(&note).Error; err != nil {
		t.Fatalf("read note: %v", err)
	}
	if note != "" {
		t.Fatalf("existing note = %q, want empty", note)
	}
}
