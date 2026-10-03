package pgtest_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/storage"
	"gpt-load/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	pgtest.Run(m, storage.AutoMigrate)
}

func openDatabase(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func TestNewDatabaseClonesIsolatedMigratedDatabases(t *testing.T) {
	t.Parallel()
	first := openDatabase(t, pgtest.NewDatabase(t))
	second := openDatabase(t, pgtest.NewDatabase(t))
	for _, db := range []*gorm.DB{first, second} {
		if !db.Migrator().HasTable("groups") {
			t.Fatal("cloned database has no groups table")
		}
	}

	if err := first.Exec("CREATE TABLE pgtest_marker (id integer)").Error; err != nil {
		t.Fatalf("create marker table: %v", err)
	}
	if second.Migrator().HasTable("pgtest_marker") {
		t.Fatal("second database sees a table created in the first")
	}
}

func TestNewEmptyDatabaseHasNoSchema(t *testing.T) {
	t.Parallel()
	db := openDatabase(t, pgtest.NewEmptyDatabase(t))
	if db.Migrator().HasTable("groups") {
		t.Fatal("empty database has a groups table")
	}
}

func TestFailOnCommitRejectsTheTransactionAtCommit(t *testing.T) {
	t.Parallel()
	db := openDatabase(t, pgtest.NewEmptyDatabase(t))
	if err := db.Exec("CREATE TABLE items (id integer PRIMARY KEY)").Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	pgtest.FailOnCommit(t, db, "items", "commit rejected")

	var statementErr error
	err := db.Transaction(func(tx *gorm.DB) error {
		statementErr = tx.Exec("INSERT INTO items (id) VALUES (1)").Error
		return statementErr
	})
	if statementErr != nil {
		t.Fatalf("statement failed before COMMIT: %v", statementErr)
	}
	if err == nil || !strings.Contains(err.Error(), "commit rejected") {
		t.Fatalf("Transaction() error = %v, want commit rejected", err)
	}
	var count int64
	if err := db.Raw("SELECT COUNT(*) FROM items").Scan(&count).Error; err != nil {
		t.Fatalf("count items: %v", err)
	}
	if count != 0 {
		t.Fatalf("items count = %d, want 0 after rejected COMMIT", count)
	}
}

func TestFailOnAndDropConstraints(t *testing.T) {
	t.Parallel()
	db := openDatabase(t, pgtest.NewEmptyDatabase(t))
	if err := db.Exec("CREATE TABLE items (id integer PRIMARY KEY, name text CHECK (name <> ''))").Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	pgtest.DropConstraints(t, db, "items", 'c')
	if err := db.Exec("INSERT INTO items (id, name) VALUES (1, '')").Error; err != nil {
		t.Fatalf("insert after dropping check: %v", err)
	}

	pgtest.FailOn(t, db, "items", "INSERT", "NEW.name = 'bad'", "bad name")
	if err := db.Exec("INSERT INTO items (id, name) VALUES (2, 'good')").Error; err != nil {
		t.Fatalf("insert not matching WHEN: %v", err)
	}
	err := db.Exec("INSERT INTO items (id, name) VALUES (3, 'bad')").Error
	if err == nil || !strings.Contains(err.Error(), "bad name") {
		t.Fatalf("insert matching WHEN error = %v, want bad name", err)
	}
}

func TestDSNFailsWhenUnset(t *testing.T) {
	if os.Getenv("PGTEST_DSN_CHILD") == "1" {
		pgtest.DSN(t)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestDSNFailsWhenUnset$")
	command.Env = append(withoutEnv(os.Environ(), pgtest.DSNEnv), "PGTEST_DSN_CHILD=1")
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child error = %v, want a failing exit; output:\n%s", err, output)
	}
	if !strings.Contains(string(output), pgtest.DSNEnv+" is not set") ||
		!strings.Contains(string(output), "make test-deps") {
		t.Fatalf("child output does not explain the missing DSN:\n%s", output)
	}
}

func withoutEnv(environment []string, name string) []string {
	kept := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, name+"=") {
			kept = append(kept, entry)
		}
	}
	return kept
}
