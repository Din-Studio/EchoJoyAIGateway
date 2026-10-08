package storage

import (
	"testing"

	"gpt-load/internal/platform/config"
	"gpt-load/internal/testutil/pgtest"
)

func TestOpenConfiguredAppliesConfiguredPoolLimits(t *testing.T) {
	db, err := OpenConfigured(&config.Config{
		DatabaseDSN: pgtest.NewEmptyDatabase(t),
		DatabasePool: config.DatabasePoolConfig{
			MaxOpenConnections: 24,
			MaxIdleConnections: 12,
		},
	})
	if err != nil {
		t.Fatalf("OpenConfigured() error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	if !db.Config.TranslateError {
		t.Fatal("TranslateError = false, want true")
	}
	if got := sqlDB.Stats().MaxOpenConnections; got != 24 {
		t.Fatalf("MaxOpenConnections = %d, want 24", got)
	}
}
