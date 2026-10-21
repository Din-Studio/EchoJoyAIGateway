package storage

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"gorm.io/gorm"
)

func TestExternalClientModelOverrideMigrationContract(t *testing.T) {
	open := externalMigrationContract(t)
	t.Run("upgrade", func(t *testing.T) { testClientModelOverrideUpgrade(t, open(t)) })
}

func testClientModelOverrideUpgrade(t *testing.T, db *gorm.DB) {
	if err := applyMigrationRegistry(db, migrations[:23]); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := AutoMigrate(db); err != nil {
			t.Fatal(err)
		}
	}
	modelNames := []string{"Model", "model", "模型🚀", "é", "é", strings.Repeat("模型", 600)}
	for _, name := range modelNames {
		identity := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
		row := map[string]any{"model_hash": identity, "client_model": name, "overrides": `{"display_name":"Custom"}`}
		if err := db.Table("client_model_overrides").Create(row).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Table("client_model_overrides").Create(row).Error; err == nil {
			t.Fatal("duplicate model identity accepted")
		}
		var saved struct{ ClientModel, Overrides string }
		if err := db.Table("client_model_overrides").Where("model_hash = ?", identity).Take(&saved).Error; err != nil {
			t.Fatal(err)
		}
		if saved.ClientModel != name || saved.Overrides != row["overrides"] {
			t.Fatalf("model identity or overrides changed: %+v", saved)
		}
	}
	if err := AutoMigrate(db); err != nil {
		t.Fatalf("repeat with persisted overrides: %v", err)
	}
	var count int64
	if err := db.Table("client_model_overrides").Count(&count).Error; err != nil || count != int64(len(modelNames)) {
		t.Fatalf("persisted overrides count = %d, error = %v", count, err)
	}
}
