package storage

import (
	"fmt"
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/storage/models"
)

func TestExternalCustomAccessKeyMigrationContract(t *testing.T) {
	testCustomAccessKeyMigration(t, externalMigrationContract(t))
}

func testCustomAccessKeyMigration(t *testing.T, open func(*testing.T) *gorm.DB) {
	for _, scenario := range []string{"fresh", "upgrade", "interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			db := open(t)
			var key models.AccessKey
			var rule models.AccessKeyCostLimitRule
			var deletedID uint
			if scenario != "fresh" {
				if err := applyMigrationRegistry(db, migrations[:10]); err != nil {
					t.Fatal(err)
				}
				key = models.AccessKey{Name: "existing", KeyValue: "encrypted-test-value", KeyHash: "existing-hash", KeySuffix: "cafe", Status: "active", Filters: models.JSON(`{}`)}
				if err := db.Omit("KeyPrefix", "Note", "ConcurrencyLimit").Create(&key).Error; err != nil {
					t.Fatal(err)
				}
				// 0010 时限额列仍是 *_nano_usd，0021 才改名，所以这里写原始 SQL。
				if err := db.Raw(`INSERT INTO access_key_cost_limit_rules
					(access_key_id, kind, limit_nano_usd, period_seconds, rule_revision, created_at_ms, updated_at_ms)
					VALUES (?, 'total', 100, 0, 1, 1, 1) RETURNING id`, key.ID).Scan(&rule.ID).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Exec(`INSERT INTO access_key_cost_limit_states
					(rule_id, rule_revision, used_nano_usd, window_generation, snapshot_version, updated_at_ms)
					VALUES (?, 1, 17, 0, 1, 1)`, rule.ID).Error; err != nil {
					t.Fatal(err)
				}
				// 删除过的较大 ID 也不能因重建表而被再次分配。
				deleted := models.AccessKey{Name: "deleted", KeyValue: "cipher", KeyHash: "deleted-hash", KeySuffix: "dead", Status: "active", Filters: models.JSON(`{}`)}
				if err := db.Omit("KeyPrefix", "Note", "ConcurrencyLimit").Create(&deleted).Error; err != nil {
					t.Fatal(err)
				}
				deletedID = deleted.ID
				if err := db.Delete(&deleted).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "interrupted" && len(migrations) > 10 {
				entry := migrations[10]
				up := entry.Up
				entry.Up = func(tx *gorm.DB) error {
					if err := up(tx); err != nil {
						return err
					}
					return fmt.Errorf("interrupt after custom access key DDL")
				}
				registry := append(append([]migration(nil), migrations[:10]...), entry)
				if err := applyMigrationRegistry(db, registry); err == nil {
					t.Fatal("interruption succeeded")
				}
			}
			if err := AutoMigrate(db); err != nil {
				t.Fatal(err)
			}
			if err := AutoMigrate(db); err != nil {
				t.Fatal(err)
			}
			if scenario != "fresh" {
				var current models.AccessKey
				if err := db.Take(&current, key.ID).Error; err != nil {
					t.Fatal(err)
				}
				if current.KeyValue != key.KeyValue || current.KeyHash != key.KeyHash || current.KeySuffix != key.KeySuffix {
					t.Fatal("existing credential changed")
				}
				var currentRule models.AccessKeyCostLimitRule
				if err := db.Take(&currentRule, rule.ID).Error; err != nil {
					t.Fatal(err)
				}
				var state models.AccessKeyCostLimitState
				if err := db.Take(&state, rule.ID).Error; err != nil {
					t.Fatal(err)
				}
				if currentRule.LimitNano != 100 || state.UsedNano != 17 {
					t.Fatal("quota data changed")
				}
			}
			custom := models.AccessKey{Name: "custom", KeyValue: "encrypted-custom", KeyHash: "custom-hash", KeySuffix: "Z9-_", Status: "active", Filters: models.JSON(`{}`)}
			if err := db.Create(&custom).Error; err != nil {
				t.Fatalf("custom suffix rejected: %v", err)
			}
			if scenario != "fresh" && custom.ID <= deletedID {
				t.Fatal("migration reused a deleted access key ID")
			}
			if err := db.Model(&custom).Update("key_suffix", "****").Error; err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []string{"", "abc", "12345"} {
				if err := db.Model(&custom).Update("key_suffix", invalid).Error; err == nil {
					t.Fatal("invalid suffix length accepted")
				}
			}
		})
	}
}
