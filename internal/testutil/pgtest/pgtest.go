// Package pgtest owns the PostgreSQL database lifecycle for tests. Each test
// process migrates one template database lazily; every test clones it with
// CREATE DATABASE ... TEMPLATE and drops the clone on cleanup. Opening business
// connections stays on the production path (storage.Open).
package pgtest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	// DSNEnv names the PostgreSQL server every database-backed test uses.
	DSNEnv = "GPT_LOAD_DATABASE_TEST_DSN"
	// RedisAddrEnv names the Redis server cluster-mode tests use.
	RedisAddrEnv = "GPT_LOAD_REDIS_TEST_ADDR"

	missingDependencyHint = "; run `make test-deps` and export it (see CONTRIBUTING.md)"

	// sessionExitWait bounds how long cleanup waits for closed connections'
	// backends before dropping the database.
	sessionExitWait = 100 * time.Millisecond
)

var (
	migrate func(*gorm.DB) error
	running bool

	templateOnce sync.Once
	templateName string
	templateErr  error

	adminOnce sync.Once
	admin     *gorm.DB
	adminErr  error
)

// Run executes the package tests with migrate as the schema of NewDatabase
// clones, then drops this process's template database. Call it from TestMain.
// migrate may be nil when the package only uses NewEmptyDatabase.
func Run(m *testing.M, migrateSchema func(*gorm.DB) error) {
	migrate = migrateSchema
	running = true
	code := m.Run()
	if templateName != "" {
		if err := dropDatabase(templateName); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: drop template database %s: %v\n", templateName, err)
			if code == 0 {
				code = 1
			}
		}
	}
	if admin != nil {
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	os.Exit(code)
}

// DSN returns the configured test database DSN and fails the test when it is
// missing. External contract tests use this PostgreSQL server as-is.
func DSN(t testing.TB) string {
	t.Helper()
	return requireEnv(t, DSNEnv)
}

// RedisAddr returns the configured Redis address list and fails the test when
// it is missing.
func RedisAddr(t testing.TB) string {
	t.Helper()
	return requireEnv(t, RedisAddrEnv)
}

// NewDatabase creates an isolated database cloned from the migrated template
// and returns its DSN. The database is dropped when the test ends.
func NewDatabase(t testing.TB) string {
	t.Helper()
	requireRun(t)
	if migrate == nil {
		t.Fatal("pgtest.Run was given no migration; use NewEmptyDatabase")
	}
	baseDSN := DSN(t)
	templateOnce.Do(func() {
		templateName, templateErr = createTemplate(baseDSN)
	})
	if templateErr != nil {
		t.Fatalf("create migrated PostgreSQL test template: %v", templateErr)
	}
	return createDatabase(t, baseDSN, "TEMPLATE "+quoteIdentifier(templateName)+" STRATEGY FILE_COPY")
}

// NewEmptyDatabase creates an isolated database without any schema and returns
// its DSN. The database is dropped when the test ends.
func NewEmptyDatabase(t testing.TB) string {
	t.Helper()
	requireRun(t)
	return createDatabase(t, DSN(t), "")
}

func requireEnv(t testing.TB, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is not set%s", name, missingDependencyHint)
	}
	return value
}

func requireRun(t testing.TB) {
	t.Helper()
	if !running {
		t.Fatal("call pgtest.Run from TestMain")
	}
}

func createDatabase(t testing.TB, baseDSN, clause string) string {
	t.Helper()
	name := "gpt_load_t_" + randomSuffix()
	if err := adminExec(baseDSN, "CREATE DATABASE "+quoteIdentifier(name)+" "+clause); err != nil {
		t.Fatalf("create PostgreSQL test database: %v", err)
	}
	t.Cleanup(func() {
		waitForSessionsToExit(name)
		if err := adminExec(baseDSN, "DROP DATABASE IF EXISTS "+quoteIdentifier(name)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop PostgreSQL test database %s: %v", name, err)
		}
	})
	dsn, err := withDatabase(baseDSN, name)
	if err != nil {
		t.Fatalf("build PostgreSQL test DSN: %v", err)
	}
	return dsn
}

// waitForSessionsToExit gives the backends of just-closed pools a moment to
// exit. DROP DATABASE ... WITH (FORCE) sleeps 100ms whenever a session is still
// alive, which would otherwise dominate every short test. Sessions a test left
// open are still terminated by FORCE once the wait gives up.
func waitForSessionsToExit(name string) {
	deadline := time.Now().Add(sessionExitWait)
	for time.Now().Before(deadline) {
		var sessions int64
		if err := admin.Raw("SELECT count(*) FROM pg_stat_activity WHERE datname = ?", name).
			Scan(&sessions).Error; err != nil || sessions == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func createTemplate(baseDSN string) (string, error) {
	name := "gpt_load_tpl_" + randomSuffix()
	if err := adminExec(baseDSN, "CREATE DATABASE "+quoteIdentifier(name)); err != nil {
		return "", err
	}
	dsn, err := withDatabase(baseDSN, name)
	if err == nil {
		err = withConnection(dsn, migrate)
	}
	if err != nil {
		_ = adminExec(baseDSN, "DROP DATABASE IF EXISTS "+quoteIdentifier(name)+" WITH (FORCE)")
		return "", fmt.Errorf("migrate template: %w", err)
	}
	return name, nil
}

func dropDatabase(name string) error {
	baseDSN := strings.TrimSpace(os.Getenv(DSNEnv))
	return adminExec(baseDSN, "DROP DATABASE IF EXISTS "+quoteIdentifier(name)+" WITH (FORCE)")
}

// adminExec runs statement on the process-wide pool connected to the base
// database, which is never a template.
func adminExec(dsn, statement string) error {
	adminOnce.Do(func() {
		admin, adminErr = openConnection(dsn)
	})
	if adminErr != nil {
		return adminErr
	}
	return admin.Exec(statement).Error
}

// withConnection runs fn on a short-lived connection and closes it, because a
// template database cannot be cloned while any session is connected to it.
func withConnection(dsn string, fn func(*gorm.DB) error) error {
	db, err := openConnection(dsn)
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()
	return fn(db)
}

// openConnection matches storage's PostgreSQL dialector configuration.
func openConnection(dsn string) (*gorm.DB, error) {
	return gorm.Open(gormpostgres.New(gormpostgres.Config{
		DSN:                  dsn,
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
	})
}

// withDatabase replaces only the database path, keeping the DSN's query.
func withDatabase(dsn, name string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", fmt.Errorf("%s must be a postgres:// URL", DSNEnv)
	}
	parsed.Path = "/" + name
	parsed.RawPath = ""
	return parsed.String(), nil
}

func randomSuffix() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		panic(fmt.Sprintf("pgtest: read random database suffix: %v", err))
	}
	return hex.EncodeToString(buffer)
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
