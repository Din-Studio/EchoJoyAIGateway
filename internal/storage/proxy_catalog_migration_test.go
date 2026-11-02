package storage

import (
	"errors"
	"reflect"
	"testing"

	"gorm.io/gorm"

	migrationfiles "gpt-load/internal/storage/migrations"
	"gpt-load/internal/storage/models"
)

func TestProxyCatalogMigrationContract(t *testing.T) {
	for _, scenario := range []string{"fresh", "upgrade", "existing_table", "missing_indexes", "interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			db := openEmptyTestDatabase(t)
			if len(migrations) != 28 || migrations[27].ID != migrationfiles.ID0028 {
				t.Fatal("proxy catalog must be dev migration 0028")
			}
			if scenario != "fresh" {
				if err := applyMigrationRegistry(db, migrations[:27]); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "existing_table" || scenario == "missing_indexes" {
				if err := migrationfiles.Up0028(db); err != nil {
					t.Fatal(err)
				}
				if scenario == "missing_indexes" {
					for _, name := range []string{"ux_proxies_fingerprint", "idx_proxies_enabled"} {
						if err := db.Migrator().DropIndex(&models.Proxy{}, name); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if scenario == "interrupted" {
				entry := migrations[27]
				entry.Up = func(tx *gorm.DB) error {
					if err := migrationfiles.Up0028(tx); err != nil {
						return err
					}
					return errors.New("interrupt proxy migration")
				}
				registry := append(append([]migration(nil), migrations[:27]...), entry)
				if err := applyMigrationRegistry(db, registry); err == nil {
					t.Fatal("expected interrupted migration")
				}
				if db.Migrator().HasTable("proxies") {
					t.Fatal("interrupted DDL was not rolled back")
				}
			}
			for range 2 {
				if err := AutoMigrate(db); err != nil {
					t.Fatal(err)
				}
				if err := migrationfiles.Validate0028(db); err != nil {
					t.Fatal(err)
				}
				var ids []string
				if err := db.Table("schema_migrations").Order("id ASC").Pluck("id", &ids).Error; err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(ids, registeredMigrationIDs()) {
					t.Fatalf("migration ledger = %v", ids)
				}
			}
		})
	}
}

func TestProxyCatalogMigrationRejectsIncompatibleSchema(t *testing.T) {
	for _, ddl := range []string{
		"DROP INDEX ux_proxies_fingerprint; CREATE INDEX ux_proxies_fingerprint ON proxies (fingerprint)",
		"DROP INDEX idx_proxies_enabled; CREATE INDEX idx_proxies_enabled ON proxies (name)",
		"ALTER TABLE proxies DROP COLUMN config",
		"ALTER TABLE proxies ALTER COLUMN config DROP NOT NULL",
		"ALTER TABLE proxies ALTER COLUMN enabled DROP DEFAULT; ALTER TABLE proxies ALTER COLUMN enabled TYPE text USING enabled::text",
	} {
		t.Run(ddl, func(t *testing.T) {
			db := openEmptyTestDatabase(t)
			if err := applyMigrationRegistry(db, migrations[:27]); err != nil {
				t.Fatal(err)
			}
			if err := migrationfiles.Up0028(db); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec(ddl).Error; err != nil {
				t.Fatal(err)
			}
			if err := AutoMigrate(db); err == nil {
				t.Fatal("migration accepted incompatible proxy schema")
			}
		})
	}
}

func TestProxyCatalogMigrationDoesNotReuseDeletedIDs(t *testing.T) {
	db := openEmptyTestDatabase(t)
	if err := AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	row := models.Proxy{Name: "first", Config: "opaque-ciphertext", Fingerprint: "identity", Enabled: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&row).Error; err != nil {
		t.Fatal(err)
	}
	replacement := models.Proxy{Name: "replacement", Config: row.Config, Fingerprint: row.Fingerprint, Enabled: true}
	if err := db.Create(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	if replacement.ID == row.ID {
		t.Fatal("deleted proxy ID was reused")
	}
}
