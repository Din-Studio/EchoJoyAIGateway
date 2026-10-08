package dbtx

import (
	"context"
	"testing"

	"gpt-load/internal/storage"
	"gpt-load/internal/testutil/pgtest"
)

func TestBeginStatementForSelectsPostgreSQLIsolation(t *testing.T) {
	for _, test := range []struct {
		mode Mode
		want string
	}{
		{mode: Write, want: "BEGIN ISOLATION LEVEL READ COMMITTED"},
		{mode: ReadSnapshot, want: "BEGIN ISOLATION LEVEL REPEATABLE READ"},
	} {
		got, err := beginStatementFor(test.mode)
		if err != nil || got != test.want {
			t.Fatalf("beginStatementFor(%d) = %q/%v, want %q/nil", test.mode, got, err, test.want)
		}
	}
	if _, err := beginStatementFor(Mode(99)); err == nil {
		t.Fatal("beginStatementFor(99) error = nil, want unsupported mode error")
	}
}

func TestDiscardConnectionTreatsBadConnectionAsSuccessfulCleanup(t *testing.T) {
	db, err := storage.Open(pgtest.NewEmptyDatabase(t))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get database: %v", err)
	}
	defer sqlDB.Close()

	connection, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("get connection: %v", err)
	}
	if err := discardConnection("test", connection); err != nil {
		t.Fatalf("discardConnection() error = %v, want nil", err)
	}
}
