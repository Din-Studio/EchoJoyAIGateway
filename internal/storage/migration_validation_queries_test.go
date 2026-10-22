package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpt-load/internal/storage/models"
)

type migrationQueryCounter struct {
	logger.Interface
	total int
}

func (counter *migrationQueryCounter) LogMode(logger.LogLevel) logger.Interface { return counter }

func (counter *migrationQueryCounter) Trace(context.Context, time.Time, func() (string, int64), error) {
	counter.total++
}

func TestMigrationValidationLimitsMetadataQueries(t *testing.T) {
	db := openEmptyTestDatabase(t)
	initialize := &migrationQueryCounter{Interface: logger.Discard}
	if err := AutoMigrate(db.Session(&gorm.Session{Logger: initialize})); err != nil {
		t.Fatalf("initialize schema: %v", err)
	}
	t.Logf("phase=initialize total=%d", initialize.total)
	if initialize.total > 1000 {
		t.Errorf("initialization executed %d SQL statements, want at most 1000", initialize.total)
	}

	restart := &migrationQueryCounter{Interface: logger.Discard}
	if err := AutoMigrate(db.Session(&gorm.Session{Logger: restart})); err != nil {
		t.Fatalf("validate completed schema: %v", err)
	}
	t.Logf("phase=restart total=%d", restart.total)
	if restart.total > 400 {
		t.Errorf("repeat startup executed %d SQL statements, want at most 400", restart.total)
	}
}

func TestMigrationValidationPropagatesMetadataReadFailure(t *testing.T) {
	db := openEmptyTestDatabase(t)
	inspection, err := newMigrationInspection(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	inspection.db = db.WithContext(ctx)
	err = inspection.validate(func(validationDB *gorm.DB) error {
		// 对“不存在”的判断可以通过，但元数据读取失败不能被当作不存在。
		_ = validationDB.Migrator().HasTable("groups")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("metadata read error = %v, want context cancellation", err)
	}
}

func TestMigrationValidationRetainsSafetyChecks(t *testing.T) {
	db := openEmptyTestDatabase(t)
	if err := AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Table("system_settings").Create(map[string]any{
		"key": "inject_usage_options", "value": "true", "updated_at_ms": 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := AutoMigrate(db); err == nil || !strings.Contains(err.Error(), "0008_remove_inject_usage_options") {
		t.Fatalf("retired system setting passed validation: %v", err)
	}
	if err := db.Where(&models.SystemSetting{Key: "inject_usage_options"}).Delete(&models.SystemSetting{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("groups").Create(map[string]any{
		"name": "retired-override", "channel_id": "openai", "params": "{}", "models": "[]",
		"overrides": `{"inject_usage_options":true}`, "created_at_ms": 1, "updated_at_ms": 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := AutoMigrate(db); err == nil || !strings.Contains(err.Error(), "0008_remove_inject_usage_options") {
		t.Fatalf("retired Group override passed validation: %v", err)
	}
	if err := db.Table("groups").Where(map[string]any{"name": "retired-override"}).Update("overrides", "{}").Error; err != nil {
		t.Fatal(err)
	}
	if err := AutoMigrate(db); err != nil {
		t.Fatalf("valid Group override failed validation: %v", err)
	}
	if err := db.Migrator().DropIndex("model_prices", "idx_model_prices_channel_model"); err != nil {
		t.Fatal(err)
	}
	if err := AutoMigrate(db); err == nil || !strings.Contains(err.Error(), "validate applied migration 0001_initial") {
		t.Fatalf("missing completed index passed validation: %v", err)
	}
	if db.Migrator().HasIndex("model_prices", "idx_model_prices_channel_model") {
		t.Fatal("validation recreated an index owned by a completed migration")
	}
}
