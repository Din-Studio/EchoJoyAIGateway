package loader_test

import (
	"testing"

	"gpt-load/internal/storage"
	"gpt-load/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	pgtest.Run(m, storage.AutoMigrate)
}
