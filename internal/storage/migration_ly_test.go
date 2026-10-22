package storage

import (
	"reflect"
	"testing"

	migrationfiles "gpt-load/internal/storage/migrations"
)

func TestAutoMigrateAppliesTheLyTrackOnItsOwnLedger(t *testing.T) {
	db := openEmptyTestDatabase(t)
	for range 2 {
		if err := AutoMigrate(db); err != nil {
			t.Fatalf("AutoMigrate() error = %v", err)
		}
	}

	var lyIDs []string
	if err := db.Table(lyMigrationLedgerTable).Order("id").Pluck("id", &lyIDs).Error; err != nil {
		t.Fatalf("read %s: %v", lyMigrationLedgerTable, err)
	}
	if want := []string{migrationfiles.IDLy0001}; !reflect.DeepEqual(lyIDs, want) {
		t.Fatalf("%s = %v, want %v", lyMigrationLedgerTable, lyIDs, want)
	}
	var coreCount int64
	if err := db.Table(migrationLedgerTable).Count(&coreCount).Error; err != nil {
		t.Fatalf("count %s: %v", migrationLedgerTable, err)
	}
	if coreCount != int64(len(migrations)) {
		t.Fatalf("%s has %d rows, want only the %d core migrations", migrationLedgerTable, coreCount, len(migrations))
	}
	if !db.Migrator().HasTable("ly_usage_reports") {
		t.Fatal("ly_usage_reports table is missing")
	}
}
