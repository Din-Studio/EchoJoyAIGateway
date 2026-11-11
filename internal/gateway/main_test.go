package gateway

import (
	"gpt-load/internal/storage"
	"gpt-load/internal/testutil/pgtest"
	"testing"
)

func TestMain(m *testing.M) { pgtest.Run(m, storage.AutoMigrate) }
