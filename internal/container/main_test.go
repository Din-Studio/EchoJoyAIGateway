package container

import (
	"testing"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/storage"
	"gpt-load/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	pgtest.Run(m, storage.AutoMigrate)
}

// setStartupEnv sets every variable startup requires: a fresh PostgreSQL
// database, both keys, and a miniredis server.
func setStartupEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("DATABASE_DSN", pgtest.NewEmptyDatabase(t))
	t.Setenv("AUTH_KEY", "test-auth-key")
	t.Setenv("ENCRYPTION_KEY", "test-master-key-long")
	t.Setenv("REDIS_ADDRS", miniredis.RunT(t).Addr())
}
