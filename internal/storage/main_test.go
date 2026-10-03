package storage

import (
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	pgtest.Run(m, AutoMigrate)
}

// openEmptyTestDatabase opens an isolated empty PostgreSQL database for tests
// that drive the migration registry themselves.
func openEmptyTestDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := Open(pgtest.NewEmptyDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}
