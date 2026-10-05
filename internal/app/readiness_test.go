package app

import (
	"context"
	"testing"

	"gpt-load/internal/cluster"
	"gpt-load/internal/storage"
	"gpt-load/internal/testutil/clustertest"
	"gpt-load/internal/testutil/pgtest"
)

// testClusterClient connects an App to a miniredis server.
func testClusterClient(t *testing.T) *cluster.Client {
	t.Helper()
	_, client := clustertest.NewClient(t)
	return client
}

func TestReadinessProbeChecksDatabaseAndRedis(t *testing.T) {
	db, err := storage.Open(pgtest.NewDatabase(t))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	server, client := clustertest.NewClient(t)
	probe := NewReadinessProbe(db, client)

	results := probe.Check(context.Background())
	if results["database"] != nil || results["redis"] != nil || len(results) != 2 {
		t.Fatalf("healthy Check() = %#v", results)
	}

	server.Close()
	results = probe.Check(context.Background())
	if results["database"] != nil || results["redis"] == nil {
		t.Fatalf("Check() with Redis down = %#v, want redis error only", results)
	}
}

func TestAppStopClosesClusterClient(t *testing.T) {
	_, client := clustertest.NewClient(t)
	application := NewApp(AppParams{ClusterClient: client})
	if err := application.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := client.Ping(context.Background()); err == nil {
		t.Fatal("cluster client still answers Ping after Stop()")
	}
}
