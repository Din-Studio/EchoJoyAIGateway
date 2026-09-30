package app

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/storage"
)

func newTestClusterClient(t *testing.T) (*miniredis.Miniredis, *cluster.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	client, err := cluster.NewClient(&config.Config{Cluster: config.ClusterConfig{
		RedisAddrs: []string{server.Addr()}, RedisKeyPrefix: "gl", InstanceID: "node-test",
	}})
	if err != nil {
		t.Fatalf("cluster.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return server, client
}

func TestNewReadinessProbeIsNilWithoutClusterClient(t *testing.T) {
	if NewReadinessProbe(nil, nil) != nil {
		t.Fatal("NewReadinessProbe(nil client) must return nil")
	}
}

func TestReadinessProbeChecksDatabaseAndRedis(t *testing.T) {
	db, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	server, client := newTestClusterClient(t)
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
	_, client := newTestClusterClient(t)
	application := NewApp(AppParams{ClusterClient: client})
	if err := application.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := client.Ping(context.Background()); err == nil {
		t.Fatal("cluster client still answers Ping after Stop()")
	}
}
