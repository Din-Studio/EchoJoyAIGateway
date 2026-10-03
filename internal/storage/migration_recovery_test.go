package storage

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/platform/config"
	migrationfiles "gpt-load/internal/storage/migrations"
	"gpt-load/internal/testutil/pgtest"
)

func TestExternalDatabaseMySQLInterruptedBaselineRecovery(t *testing.T) {
	rawDSN := pgtest.DSN(t)
	parsed, err := url.Parse(rawDSN)
	if err != nil || !strings.EqualFold(parsed.Scheme, "mysql") {
		t.Skip("interrupted baseline recovery is specific to MySQL")
	}
	admin, err := OpenWithSource(rawDSN, config.DatabaseSourceExternal)
	if err != nil {
		t.Fatalf("open MySQL admin database: %v", err)
	}
	adminSQL, err := admin.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminSQL.Close() })

	models := migrationfiles.SchemaModels0001()
	for boundary := 0; boundary <= len(models); boundary++ {
		t.Run(fmt.Sprintf("boundary_%02d", boundary), func(t *testing.T) {
			databaseName := fmt.Sprintf("gpt_load_recovery_%d_%02d", time.Now().UnixNano(), boundary)
			if err := admin.Exec("CREATE DATABASE `" + databaseName + "`").Error; err != nil {
				t.Fatalf("create recovery database: %v", err)
			}
			t.Cleanup(func() {
				if dropErr := admin.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`").Error; dropErr != nil {
					t.Errorf("drop recovery database: %v", dropErr)
				}
			})

			recoveryURL := *parsed
			recoveryURL.Path = "/" + databaseName
			recoveryURL.RawPath = ""
			partial, err := OpenWithSource(recoveryURL.String(), config.DatabaseSourceExternal)
			if err != nil {
				t.Fatalf("open recovery database: %v", err)
			}
			if err := partial.AutoMigrate(&schemaMigration{}); err != nil {
				t.Fatalf("create recovery ledger: %v", err)
			}
			if err := partial.Create(&schemaMigration{ID: migrationResumeMarker(migrations[0].ID)}).Error; err != nil {
				t.Fatalf("record recovery marker: %v", err)
			}
			if boundary > 0 {
				if err := partial.AutoMigrate(models[:boundary]...); err != nil {
					t.Fatalf("create MySQL schema prefix %d: %v", boundary, err)
				}
			}
			partialSQL, err := partial.DB()
			if err != nil {
				t.Fatal(err)
			}
			if err := partialSQL.Close(); err != nil {
				t.Fatalf("close interrupted database: %v", err)
			}

			restarted, err := OpenWithSource(recoveryURL.String(), config.DatabaseSourceExternal)
			if err != nil {
				t.Fatalf("reopen recovery database: %v", err)
			}
			restartedSQL, err := restarted.DB()
			if err != nil {
				t.Fatal(err)
			}
			if err := AutoMigrate(restarted); err != nil {
				_ = restartedSQL.Close()
				t.Fatalf("resume MySQL schema prefix %d: %v", boundary, err)
			}
			assertInternalMigrationComplete(t, restarted, registeredMigrationIDs())
			if err := restartedSQL.Close(); err != nil {
				t.Fatalf("close recovered database: %v", err)
			}
		})
	}
	runExternalMySQLCostLimitRecovery(t, admin, parsed)
}

func TestExternalDatabaseIncrementalMigrations(t *testing.T) {
	rawDSN := pgtest.DSN(t)
	runParallelOnPostgres(t, rawDSN)
	db := openExternalIncrementalMigrationDatabase(t, rawDSN)
	if err := db.AutoMigrate(&schemaMigration{}); err != nil {
		t.Fatalf("create migration ledger: %v", err)
	}
	if err := migrations[0].Up(db); err != nil {
		t.Fatalf("create 0001 schema: %v", err)
	}
	if err := migrations[0].Validate(db); err != nil {
		t.Fatalf("validate 0001 schema: %v", err)
	}
	if err := db.Create(&schemaMigration{ID: migrations[0].ID}).Error; err != nil {
		t.Fatalf("record 0001 migration: %v", err)
	}

	if err := AutoMigrate(db); err != nil {
		t.Fatalf("apply pending incremental migrations: %v", err)
	}
	if err := AutoMigrate(db); err != nil {
		t.Fatalf("repeat migrated schema validation: %v", err)
	}
	assertInternalMigrationComplete(t, db, registeredMigrationIDs())
}

func TestExternalDatabaseMySQLRecoversObservationFreshnessCheckDrop(t *testing.T) {
	rawDSN := pgtest.DSN(t)
	parsed, err := url.Parse(rawDSN)
	if err != nil || !strings.EqualFold(parsed.Scheme, "mysql") {
		t.Skip("observation freshness recovery is specific to MySQL")
	}
	db := openExternalIncrementalMigrationDatabase(t, rawDSN)
	if err := db.AutoMigrate(&schemaMigration{}); err != nil {
		t.Fatalf("create migration ledger: %v", err)
	}
	for index := 0; index < 2; index++ {
		if err := migrations[index].Up(db); err != nil {
			t.Fatalf("apply migration %d: %v", index+1, err)
		}
		if err := migrations[index].Validate(db); err != nil {
			t.Fatalf("validate migration %d: %v", index+1, err)
		}
		if err := db.Create(&schemaMigration{ID: migrations[index].ID}).Error; err != nil {
			t.Fatalf("record migration %d: %v", index+1, err)
		}
	}
	if err := db.Create(&schemaMigration{ID: migrationResumeMarker(migrations[2].ID)}).Error; err != nil {
		t.Fatalf("record observation freshness recovery marker: %v", err)
	}
	if err := db.Exec(
		"ALTER TABLE `credential_observations` DROP CHECK `chk_credential_observation_fresh_until`",
	).Error; err != nil {
		t.Fatalf("drop observation freshness check: %v", err)
	}

	if err := AutoMigrate(db); err != nil {
		t.Fatalf("resume observation freshness removal: %v", err)
	}
	assertInternalMigrationComplete(t, db, registeredMigrationIDs())
}

func TestExternalDatabaseMySQLRecoversPartialErrorDecisionMigration(t *testing.T) {
	rawDSN := pgtest.DSN(t)
	parsed, err := url.Parse(rawDSN)
	if err != nil || !strings.EqualFold(parsed.Scheme, "mysql") {
		t.Skip("error decision recovery is specific to MySQL")
	}
	db := openExternalIncrementalMigrationDatabase(t, rawDSN)
	if err := db.AutoMigrate(&schemaMigration{}); err != nil {
		t.Fatalf("create migration ledger: %v", err)
	}
	for index := 0; index < 5; index++ {
		if err := migrations[index].Up(db); err != nil {
			t.Fatalf("apply migration %d: %v", index+1, err)
		}
		if err := migrations[index].Validate(db); err != nil {
			t.Fatalf("validate migration %d: %v", index+1, err)
		}
		if err := db.Create(&schemaMigration{ID: migrations[index].ID}).Error; err != nil {
			t.Fatalf("record migration %d: %v", index+1, err)
		}
	}
	if err := db.Create(&schemaMigration{ID: migrationResumeMarker(migrations[5].ID)}).Error; err != nil {
		t.Fatalf("record error decision recovery marker: %v", err)
	}
	if err := db.Exec(
		"ALTER TABLE `request_log_attempts` ADD COLUMN `failure_origin` varchar(16) NOT NULL DEFAULT ''",
	).Error; err != nil {
		t.Fatalf("create partial error decision schema: %v", err)
	}

	if err := AutoMigrate(db); err != nil {
		t.Fatalf("resume error decision migration: %v", err)
	}
	assertInternalMigrationComplete(t, db, registeredMigrationIDs())
}

func openExternalIncrementalMigrationDatabase(t *testing.T, rawDSN string) *gorm.DB {
	t.Helper()
	parsed, err := url.Parse(rawDSN)
	if err != nil {
		t.Fatalf("parse external database DSN: %v", err)
	}
	targetURL := *parsed
	switch strings.ToLower(parsed.Scheme) {
	case "mysql":
		admin, err := OpenWithSource(rawDSN, config.DatabaseSourceExternal)
		if err != nil {
			t.Fatalf("open external database admin connection: %v", err)
		}
		adminSQL, err := admin.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = adminSQL.Close() })
		name := fmt.Sprintf("gpt_load_cost_migration_%d", time.Now().UnixNano())
		if err := admin.Exec("CREATE DATABASE `" + name + "`").Error; err != nil {
			t.Fatalf("create MySQL migration database: %v", err)
		}
		t.Cleanup(func() {
			if dropErr := admin.Exec("DROP DATABASE IF EXISTS `" + name + "`").Error; dropErr != nil {
				t.Errorf("drop MySQL migration database: %v", dropErr)
			}
		})
		targetURL.Path = "/" + name
		targetURL.RawPath = ""
	case "postgres", "postgresql":
		// A database of its own also isolates PostgreSQL's per-database
		// migration advisory lock, so contracts can run in parallel.
		targetURL = mustParseURL(t, pgtest.NewEmptyDatabase(t))
	default:
		t.Fatalf("unsupported external driver %q", parsed.Scheme)
	}

	db, err := OpenWithSource(targetURL.String(), config.DatabaseSourceExternal)
	if err != nil {
		t.Fatalf("open isolated incremental migration database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// externalMigrationContract returns the database opener for one contract's
// scenarios. The contract and each of its scenarios run in parallel where
// runParallelOnPostgres allows it.
func externalMigrationContract(t *testing.T) func(*testing.T) *gorm.DB {
	t.Helper()
	dsn := pgtest.DSN(t)
	parallel := runParallelOnPostgres(t, dsn)
	return func(t *testing.T) *gorm.DB {
		t.Helper()
		if parallel {
			t.Parallel()
		}
		return openExternalIncrementalMigrationDatabase(t, dsn)
	}
}

// runParallelOnPostgres marks t parallel when dsn names PostgreSQL, where every
// incremental migration database has its own migration lock. MySQL's GET_LOCK
// is server-wide, so tests stay serial there.
func runParallelOnPostgres(t *testing.T, dsn string) bool {
	t.Helper()
	scheme := strings.ToLower(mustParseURL(t, dsn).Scheme)
	parallel := scheme == "postgres" || scheme == "postgresql"
	if parallel {
		t.Parallel()
	}
	return parallel
}

func mustParseURL(t *testing.T, raw string) url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse database DSN: %v", err)
	}
	return *parsed
}

func runExternalMySQLCostLimitRecovery(t *testing.T, admin *gorm.DB, parsed *url.URL) {
	t.Helper()
	models := migrationfiles.SchemaModels0002()
	for boundary := 0; boundary <= len(models); boundary++ {
		t.Run(fmt.Sprintf("access_key_cost_limits_boundary_%02d", boundary), func(t *testing.T) {
			databaseName := fmt.Sprintf("gpt_load_cost_recovery_%d_%02d", time.Now().UnixNano(), boundary)
			if err := admin.Exec("CREATE DATABASE `" + databaseName + "`").Error; err != nil {
				t.Fatalf("create recovery database: %v", err)
			}
			t.Cleanup(func() {
				if dropErr := admin.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`").Error; dropErr != nil {
					t.Errorf("drop recovery database: %v", dropErr)
				}
			})

			recoveryURL := *parsed
			recoveryURL.Path = "/" + databaseName
			recoveryURL.RawPath = ""
			partial, err := OpenWithSource(recoveryURL.String(), config.DatabaseSourceExternal)
			if err != nil {
				t.Fatalf("open recovery database: %v", err)
			}
			if err := partial.AutoMigrate(&schemaMigration{}); err != nil {
				t.Fatalf("create recovery ledger: %v", err)
			}
			if err := migrations[0].Up(partial); err != nil {
				t.Fatalf("create baseline schema: %v", err)
			}
			if err := migrations[0].Validate(partial); err != nil {
				t.Fatalf("validate baseline schema: %v", err)
			}
			if err := partial.Create(&schemaMigration{ID: migrations[0].ID}).Error; err != nil {
				t.Fatalf("record baseline migration: %v", err)
			}
			if err := partial.Create(&schemaMigration{ID: migrationResumeMarker(migrations[1].ID)}).Error; err != nil {
				t.Fatalf("record cost-limit recovery marker: %v", err)
			}
			if boundary > 0 {
				if err := partial.AutoMigrate(models[:boundary]...); err != nil {
					t.Fatalf("create MySQL cost-limit schema prefix %d: %v", boundary, err)
				}
			}
			partialSQL, err := partial.DB()
			if err != nil {
				t.Fatal(err)
			}
			if err := partialSQL.Close(); err != nil {
				t.Fatalf("close interrupted database: %v", err)
			}

			restarted, err := OpenWithSource(recoveryURL.String(), config.DatabaseSourceExternal)
			if err != nil {
				t.Fatalf("reopen recovery database: %v", err)
			}
			restartedSQL, err := restarted.DB()
			if err != nil {
				t.Fatal(err)
			}
			if err := AutoMigrate(restarted); err != nil {
				_ = restartedSQL.Close()
				t.Fatalf("resume MySQL cost-limit schema prefix %d: %v", boundary, err)
			}
			assertInternalMigrationComplete(t, restarted, registeredMigrationIDs())
			if err := restartedSQL.Close(); err != nil {
				t.Fatalf("close recovered database: %v", err)
			}
		})
	}
}

func assertInternalMigrationComplete(t *testing.T, db *gorm.DB, wantIDs []string) {
	t.Helper()
	for _, table := range migrationfiles.TableNames0001() {
		if !db.Migrator().HasTable(table) {
			t.Errorf("table %q is missing", table)
		}
	}
	if len(wantIDs) >= 2 {
		for _, table := range migrationfiles.TableNames0002() {
			if !db.Migrator().HasTable(table) {
				t.Errorf("table %q is missing", table)
			}
		}
	}
	if len(wantIDs) >= 3 && db.Migrator().HasColumn("credential_observations", "fresh_until_ms") {
		t.Error("credential_observations.fresh_until_ms remains after migration 0003")
	}
	if len(wantIDs) >= 4 && !db.Migrator().HasIndex("usage_stats", "idx_usage_stats_group_bucket") {
		t.Error("usage_stats group activity index is missing after migration 0004")
	}
	if len(wantIDs) >= 6 {
		for _, column := range []string{
			"failure_origin", "failure_scope", "retry_directive", "effect", "rule_id",
		} {
			if !db.Migrator().HasColumn("request_log_attempts", column) {
				t.Errorf("request_log_attempts.%s is missing after migration 0006", column)
			}
		}
	}
	if len(wantIDs) >= 7 && !db.Migrator().HasColumn("access_keys", "expires_at_ms") {
		t.Error("access_keys.expires_at_ms is missing after migration 0007")
	}
	var ids []string
	if err := db.Table(migrationLedgerTable).Order("id").Pluck("id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(wantIDs) {
		t.Fatalf("migration IDs = %v, want %v", ids, wantIDs)
	}
	for index := range wantIDs {
		if ids[index] != wantIDs[index] {
			t.Fatalf("migration IDs = %v, want %v", ids, wantIDs)
		}
	}
}

func registeredMigrationIDs() []string {
	result := make([]string, 0, len(migrations))
	for _, entry := range migrations {
		result = append(result, entry.ID)
	}
	return result
}
