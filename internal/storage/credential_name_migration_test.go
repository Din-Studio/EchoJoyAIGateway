package storage

import (
	"fmt"
	"testing"

	"gorm.io/gorm"
)

func TestCredentialNameMigrationContract(t *testing.T) {

	for _, scenario := range []string{"fresh", "upgrade", "interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			db := openEmptyTestDatabase(t)
			if len(migrations) < 29 {
				t.Fatal("credential name migration is missing")
			}
			if scenario != "fresh" {
				if err := applyMigrationRegistry(db, migrations[:28]); err != nil {
					t.Fatal(err)
				}
				if err := db.Table("groups").Create(map[string]any{"id": 1, "name": "existing", "channel_id": "openai", "params": "{}", "models": "[]", "created_at_ms": 1, "updated_at_ms": 1}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Table("credentials").Create(map[string]any{"id": 1, "group_id": 1, "data": "cipher", "fingerprint": "fingerprint", "identity_fingerprint": "identity", "created_at_ms": 1, "updated_at_ms": 1}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "interrupted" {
				entry := migrations[28]
				up := entry.Up
				entry.Up = func(tx *gorm.DB) error {
					if err := up(tx); err != nil {
						return err
					}
					return fmt.Errorf("interrupt credential name DDL")
				}
				registry := append(append([]migration(nil), migrations[:28]...), entry)
				if err := applyMigrationRegistry(db, registry); err == nil {
					t.Fatal("expected migration interruption")
				}
				if db.Migrator().HasColumn("credentials", "name") {
					t.Fatal("interrupted migration did not roll back name column")
				}
			}
			if err := AutoMigrate(db); err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasColumn("credentials", "name") {
				t.Fatal("credential name column is missing")
			}
			if scenario != "fresh" {
				var row struct {
					Name, Data, Fingerprint, IdentityFingerprint string
					SecretVersion                                uint64
				}
				if err := db.Table("credentials").Where("id = 1").Take(&row).Error; err != nil {
					t.Fatal(err)
				}
				if row.Name != "" || row.Data != "cipher" || row.Fingerprint != "fingerprint" || row.IdentityFingerprint != "identity" || row.SecretVersion != 1 {
					t.Fatal("migration changed existing credential")
				}
				if err := db.Table("credentials").Where("id = 1").Update("name", "生产账号").Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := AutoMigrate(db); err != nil {
				t.Fatal(err)
			}
			if scenario != "fresh" {
				var name string
				if err := db.Table("credentials").Where("id = 1").Pluck("name", &name).Error; err != nil || name != "生产账号" {
					t.Fatalf("repeat migration changed alias: %q, %v", name, err)
				}
			}
		})
	}
}
