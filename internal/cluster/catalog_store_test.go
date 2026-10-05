package cluster

import (
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestCatalogStoreSharesDocumentAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	writer := NewCatalogStore(newClientForServer(t, server, "node-a"))
	reader := NewCatalogStore(newClientForServer(t, server, "node-b"))

	if _, err := reader.FetchedAt(t.Context()); !errors.Is(err, ErrCatalogMissing) {
		t.Fatalf("FetchedAt() on empty store error = %v, want ErrCatalogMissing", err)
	}
	if _, err := reader.Load(t.Context()); !errors.Is(err, ErrCatalogMissing) {
		t.Fatalf("Load() on empty store error = %v, want ErrCatalogMissing", err)
	}

	if err := writer.Store(t.Context(), []byte(`{"version":1}`), 1_754_180_400_123); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	fetchedAt, err := reader.FetchedAt(t.Context())
	if err != nil || fetchedAt != 1_754_180_400_123 {
		t.Fatalf("FetchedAt() = %d, %v", fetchedAt, err)
	}
	document, err := reader.Load(t.Context())
	if err != nil || string(document) != `{"version":1}` {
		t.Fatalf("Load() = %q, %v", document, err)
	}
	if !server.Exists("gl:catalog") {
		t.Fatal("catalog key gl:catalog is missing")
	}
}

func TestCatalogStoreReportsRedisErrors(t *testing.T) {
	server := miniredis.RunT(t)
	store := NewCatalogStore(newClientForServer(t, server, "node-a"))
	server.Close()
	if err := store.Store(t.Context(), []byte("{}"), 1); err == nil {
		t.Fatal("Store() with Redis down succeeded")
	}
	if _, err := store.FetchedAt(t.Context()); err == nil || errors.Is(err, ErrCatalogMissing) {
		t.Fatalf("FetchedAt() with Redis down error = %v", err)
	}
}
