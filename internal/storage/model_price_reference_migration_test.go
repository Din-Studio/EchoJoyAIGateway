package storage

import (
	"fmt"
	"strings"
	"testing"

	"gorm.io/gorm"
)

func TestModelPriceReferenceMigrationContract(t *testing.T) {
	const tiersJSON = `[{"threshold_tokens":200000,"input_price_nano_usd_per_million_tokens":456}]`
	const modesJSON = `{"fast":{"prices":{"input_price_nano_usd_per_million_tokens":789},"context_tiers":[]}}`
	for _, scenario := range []string{"fresh", "upgrade", "interrupted_column", "interrupted_index", "interrupted_complete", "partial_column", "partial_index", "partial_complete"} {
		t.Run(scenario, func(t *testing.T) {
			if len(migrations) != 33 {
				t.Fatal("model price reference migration is missing")
			}
			db := openEmptyTestDatabase(t)
			var removed, existing struct{ ID uint }
			if scenario != "fresh" {
				if err := applyMigrationRegistry(db, migrations[:32]); err != nil {
					t.Fatal(err)
				}
				if err := db.Table("model_prices").Create(map[string]any{
					"channel_id": "openai", "model_id": "existing", "is_manual": true,
					"input_price_nano_usd_per_million_tokens":       123,
					"output_price_nano_usd_per_million_tokens":      234,
					"cache_read_price_nano_usd_per_million_tokens":  345,
					"cache_write_price_nano_usd_per_million_tokens": 456,
					"context_price_tiers":                           tiersJSON, "mode_price_schedules": modesJSON,
					"created_at_ms": 1, "updated_at_ms": 1,
				}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Table("model_prices").Where("model_id = ?", "existing").Take(&existing).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Table("model_prices").Create(map[string]any{
					"channel_id": "openai", "model_id": "removed-before-migration", "is_manual": true,
					"created_at_ms": 1, "updated_at_ms": 1,
				}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Table("model_prices").Where("model_id = ?", "removed-before-migration").Take(&removed).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Exec("DELETE FROM model_prices WHERE id = ?", removed.ID).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "interrupted_column" || scenario == "interrupted_index" || scenario == "interrupted_complete" {
				entry := migrations[32]
				up := entry.Up
				entry.Up = func(tx *gorm.DB) error {
					if scenario == "interrupted_column" || scenario == "interrupted_index" {
						if err := tx.Exec("ALTER TABLE model_prices ADD COLUMN source_price_id BIGINT NULL").Error; err != nil {
							return err
						}
						if scenario == "interrupted_index" {
							if err := tx.Exec("CREATE INDEX idx_model_prices_source ON model_prices (source_price_id)").Error; err != nil {
								return err
							}
						}
					} else if err := up(tx); err != nil {
						return err
					}
					return fmt.Errorf("interrupt model price reference DDL")
				}
				registry := append(append([]migration(nil), migrations[:32]...), entry)
				if err := applyMigrationRegistry(db, registry); err == nil {
					t.Fatal("expected migration interruption")
				}
				if db.Migrator().HasColumn("model_prices", "source_price_id") {
					t.Fatal("interrupted PostgreSQL DDL did not roll back")
				}
				var count int64
				if err := db.Table(migrationLedgerTable).Where("id = ?", migrations[32].ID).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("interrupted migration ledger count=%d error=%v", count, err)
				}
			}
			// Committed partial DDL without a ledger entry must also be recoverable.
			if scenario == "partial_complete" {
				if err := migrations[32].Up(db); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "partial_column" || scenario == "partial_index" {
				if err := db.Exec("ALTER TABLE model_prices ADD COLUMN source_price_id BIGINT NULL").Error; err != nil {
					t.Fatal(err)
				}
				if scenario == "partial_index" {
					if err := db.Exec("CREATE INDEX idx_model_prices_source ON model_prices (source_price_id)").Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			for range 2 {
				if err := AutoMigrate(db); err != nil {
					t.Fatal(err)
				}
			}
			// Up itself is idempotent, independently of the completed ledger.
			for range 2 {
				if err := migrations[32].Up(db); err != nil {
					t.Fatal(err)
				}
			}
			assertInternalMigrationComplete(t, db, registeredMigrationIDs())
			if !db.Migrator().HasColumn("model_prices", "source_price_id") {
				t.Fatal("model price reference column is missing")
			}
			for _, name := range []string{"idx_model_prices_channel_model", "idx_model_prices_source"} {
				if !db.Migrator().HasIndex("model_prices", name) {
					t.Fatalf("model price index %s was lost", name)
				}
			}
			var parent struct{ ID uint }
			if err := db.Table("model_prices").Create(map[string]any{"channel_id": "openai", "model_id": "reference-parent", "is_manual": true, "created_at_ms": 1, "updated_at_ms": 1}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Table("model_prices").Where("model_id = ?", "reference-parent").Take(&parent).Error; err != nil {
				t.Fatal(err)
			}
			if parent.ID <= removed.ID {
				t.Fatal("migration reused a deleted model price ID")
			}
			if err := db.Table("model_prices").Create(map[string]any{"channel_id": "openai", "model_id": "reference-parent", "is_manual": true, "created_at_ms": 1, "updated_at_ms": 1}).Error; err == nil {
				t.Fatal("migration lost model price identity uniqueness")
			}
			if err := db.Table("model_prices").Create(map[string]any{"channel_id": "openai", "model_id": "reference-child", "source_price_id": parent.ID, "is_manual": false, "created_at_ms": 1, "updated_at_ms": 1}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("DELETE FROM model_prices WHERE id = ?", parent.ID).Error; err == nil {
				t.Fatal("source deletion was not restricted")
			}
			if err := db.Table("model_prices").Create(map[string]any{"channel_id": "openai", "model_id": "missing-source", "source_price_id": parent.ID + 1000, "is_manual": false, "created_at_ms": 1, "updated_at_ms": 1}).Error; err == nil {
				t.Fatal("missing source was accepted")
			}
			if err := AutoMigrate(db); err != nil {
				t.Fatalf("restart with references: %v", err)
			}
			var child struct{ SourcePriceID *uint }
			if err := db.Table("model_prices").Where("model_id = ?", "reference-child").Take(&child).Error; err != nil || child.SourcePriceID == nil || *child.SourcePriceID != parent.ID {
				t.Fatalf("reference changed on restart: %+v, error=%v", child, err)
			}
			if scenario != "fresh" {
				var row struct {
					ID                                     uint
					SourcePriceID                          *uint
					IsManual                               bool
					InputPriceNanoUSDPerMillionTokens      int64
					OutputPriceNanoUSDPerMillionTokens     int64
					CacheReadPriceNanoUSDPerMillionTokens  int64
					CacheWritePriceNanoUSDPerMillionTokens int64
					ContextPriceTiers                      string
					ModePriceSchedules                     string
					CreatedAtMS                            int64
					UpdatedAtMS                            int64
				}
				if err := db.Table("model_prices").Where("model_id = ?", "existing").Take(&row).Error; err != nil {
					t.Fatal(err)
				}
				if row.ID != existing.ID || row.SourcePriceID != nil || !row.IsManual || row.InputPriceNanoUSDPerMillionTokens != 123 ||
					row.OutputPriceNanoUSDPerMillionTokens != 234 || row.CacheReadPriceNanoUSDPerMillionTokens != 345 ||
					row.CacheWritePriceNanoUSDPerMillionTokens != 456 || row.ContextPriceTiers != tiersJSON ||
					row.ModePriceSchedules != modesJSON || row.CreatedAtMS != 1 || row.UpdatedAtMS != 1 {
					t.Fatalf("migration changed existing price: %#v", row)
				}
			}
		})
	}
}

// Completed migrations validate schema semantics, rather than repairing drift
// or accepting a same-named index/foreign key with different behavior.
func TestModelPriceReferenceMigrationRejectsSchemaDrift(t *testing.T) {
	for _, test := range []struct {
		name string
		ddl  []string
	}{
		{"missing_index", []string{"DROP INDEX idx_model_prices_source"}},
		{"wrong_index_column", []string{"DROP INDEX idx_model_prices_source", "CREATE INDEX idx_model_prices_source ON model_prices (id)"}},
		{"partial_index", []string{"DROP INDEX idx_model_prices_source", "CREATE INDEX idx_model_prices_source ON model_prices (source_price_id) WHERE source_price_id IS NOT NULL"}},
		{"unique_index", []string{"DROP INDEX idx_model_prices_source", "CREATE UNIQUE INDEX idx_model_prices_source ON model_prices (source_price_id)"}},
		{"missing_foreign_key", []string{"ALTER TABLE model_prices DROP CONSTRAINT fk_model_prices_source_price"}},
		{"cascade_foreign_key", []string{"ALTER TABLE model_prices DROP CONSTRAINT fk_model_prices_source_price", "ALTER TABLE model_prices ADD CONSTRAINT fk_model_prices_source_price FOREIGN KEY (source_price_id) REFERENCES model_prices (id) ON DELETE CASCADE"}},
		{"no_action_foreign_key", []string{"ALTER TABLE model_prices DROP CONSTRAINT fk_model_prices_source_price", "ALTER TABLE model_prices ADD CONSTRAINT fk_model_prices_source_price FOREIGN KEY (source_price_id) REFERENCES model_prices (id)"}},
		{"wrong_foreign_table", []string{"ALTER TABLE model_prices DROP CONSTRAINT fk_model_prices_source_price", "ALTER TABLE model_prices ADD CONSTRAINT fk_model_prices_source_price FOREIGN KEY (source_price_id) REFERENCES groups (id) ON DELETE RESTRICT"}},
		{"wrong_foreign_column", []string{"ALTER TABLE model_prices DROP CONSTRAINT fk_model_prices_source_price", "ALTER TABLE model_prices ADD CONSTRAINT fk_model_prices_source_price FOREIGN KEY (id) REFERENCES model_prices (id) ON DELETE RESTRICT"}},
		{"unvalidated_foreign_key", []string{"ALTER TABLE model_prices DROP CONSTRAINT fk_model_prices_source_price", "ALTER TABLE model_prices ADD CONSTRAINT fk_model_prices_source_price FOREIGN KEY (source_price_id) REFERENCES model_prices (id) ON DELETE RESTRICT NOT VALID"}},
		{"wrong_column_type", []string{"ALTER TABLE model_prices ALTER COLUMN source_price_id TYPE INTEGER"}},
		{"not_nullable", []string{"ALTER TABLE model_prices ALTER COLUMN source_price_id SET NOT NULL"}},
		{"non_null_default", []string{"ALTER TABLE model_prices ALTER COLUMN source_price_id SET DEFAULT 1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openEmptyTestDatabase(t)
			if err := AutoMigrate(db); err != nil {
				t.Fatal(err)
			}
			for _, ddl := range test.ddl {
				if err := db.Exec(ddl).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := AutoMigrate(db); err == nil || !strings.Contains(err.Error(), "validate applied migration 0033_model_price_reference") {
				t.Fatalf("schema drift passed migration validation: %v", err)
			}
		})
	}
}

func TestModelPriceReferenceMigrationRejectsInvalidPartialSchema(t *testing.T) {
	for _, column := range []string{"TEXT", "INTEGER", "BIGINT NOT NULL", "BIGINT DEFAULT 1"} {
		t.Run(column, func(t *testing.T) {
			db := openEmptyTestDatabase(t)
			if err := applyMigrationRegistry(db, migrations[:32]); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("ALTER TABLE model_prices ADD COLUMN source_price_id " + column).Error; err != nil {
				t.Fatal(err)
			}
			if err := AutoMigrate(db); err == nil {
				t.Fatal("invalid partial schema passed migration")
			}
			var count int64
			if err := db.Table(migrationLedgerTable).Where("id = ?", migrations[32].ID).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("invalid migration ledger count=%d error=%v", count, err)
			}
		})
	}
}
