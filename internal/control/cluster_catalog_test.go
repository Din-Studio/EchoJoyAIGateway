package control

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/catalog"
	"gpt-load/internal/cluster"
	"gpt-load/internal/testutil/clustertest"
)

type clusterCatalogInstance struct {
	fixture     serviceFixture
	coordinator *CatalogSyncCoordinator
	applied     *atomic.Int32
}

// newClusterCatalogInstance assembles one instance the way the container does
// in cluster mode: bootstrapped from the shared catalog, storing to it, and
// claiming automatic syncs through the shared job lease.
func newClusterCatalogInstance(
	t *testing.T,
	server *miniredis.Miniredis,
	instanceID string,
	client catalogSyncClient,
) clusterCatalogInstance {
	t.Helper()
	redis := clustertest.Connect(t, server, instanceID)
	shared := cluster.NewCatalogStore(redis)
	fixture := newServiceFixture(t)
	bootstrap := loadSharedCatalogBootstrap(t.Context(), shared)
	fixture.service.catalogRuntime.Publish(bootstrap.Runtime.Load())
	coordinator := newCatalogSyncCoordinator(
		fixture.service, client, bootstrap.Metadata, bootstrap.HasLKG,
		shared, cluster.NewJobLease(redis),
	)
	applied := &atomic.Int32{}
	coordinator.applySnapshot = func(_ context.Context, snapshot *catalog.Snapshot) error {
		applied.Add(1)
		fixture.service.catalogRuntime.Publish(snapshot)
		return nil
	}
	return clusterCatalogInstance{fixture: fixture, coordinator: coordinator, applied: applied}
}

func storeSharedCatalog(t *testing.T, server *miniredis.Miniredis, result catalog.SyncResult) {
	t.Helper()
	document, err := catalog.EncodeCache(result)
	if err != nil {
		t.Fatalf("EncodeCache() error = %v", err)
	}
	shared := cluster.NewCatalogStore(clustertest.Connect(t, server, "seed"))
	if err := shared.Store(t.Context(), document, result.Metadata.SuccessfulFetchAtMillis); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
}

func TestClusterCatalogFetchesOncePerPeriodAndPeersAdopt(t *testing.T) {
	server := miniredis.RunT(t)
	storeSharedCatalog(t, server, catalogResultFixture(1000, "seed", nil))
	fresh := catalogResultFixture(2000, "fresh", map[string]catalog.Provider{
		"openai": catalogProviderFixture("openai", "OpenAI", "gpt-cluster", 1_000_000),
	})
	var fetches atomic.Int32
	client := catalogSyncClientFunc(func(_ context.Context, previous catalog.Metadata) (catalog.SyncResult, error) {
		if fetches.Add(1) == 1 && previous.ETag != "seed" {
			t.Errorf("conditional sync sent ETag %q, want the shared seed", previous.ETag)
		}
		return fresh, nil
	})
	instances := make([]clusterCatalogInstance, 3)
	for index := range instances {
		instances[index] = newClusterCatalogInstance(t, server, "node-"+string(rune('a'+index)), client)
		if !instances[index].coordinator.hasLastKnownGood() {
			t.Fatalf("instance %d did not bootstrap the shared catalog", index)
		}
	}

	var wait sync.WaitGroup
	for _, instance := range instances {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := instance.coordinator.syncAutomatically(t.Context(), CatalogSyncStartup); err != nil {
				t.Errorf("syncAutomatically() error = %v", err)
			}
		}()
	}
	wait.Wait()
	if got := fetches.Load(); got != 1 {
		t.Fatalf("Models.dev fetches = %d across three instances, want 1", got)
	}

	for index, instance := range instances {
		if err := instance.coordinator.adoptSharedCatalog(t.Context()); err != nil {
			t.Fatalf("instance %d adoptSharedCatalog() error = %v", index, err)
		}
		if got := instance.fixture.service.catalogRuntime.Load(); !reflect.DeepEqual(got, fresh.Snapshot) {
			t.Fatalf("instance %d catalog did not converge to the fetched snapshot", index)
		}
		if metadata := instance.coordinator.metadata; metadata != fresh.Metadata {
			t.Fatalf("instance %d metadata = %#v, want %#v", index, metadata, fresh.Metadata)
		}
	}
	var applied int32
	for _, instance := range instances {
		applied += instance.applied.Load()
	}
	if applied != 1 {
		t.Fatalf("model prices were reconciled %d times, want only by the fetching instance", applied)
	}

	if _, err := instances[2].coordinator.Sync(t.Context(), CatalogSyncManual); err != nil {
		t.Fatalf("manual Sync() error = %v", err)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("manual sync fetches = %d, want it to run despite the claim", got)
	}
}

func TestClusterCatalogWithoutSharedDocumentSyncsUnclaimed(t *testing.T) {
	server := miniredis.RunT(t)
	fresh := catalogResultFixture(3000, "first", nil)
	var fetches atomic.Int32
	client := catalogSyncClientFunc(func(context.Context, catalog.Metadata) (catalog.SyncResult, error) {
		fetches.Add(1)
		return fresh, nil
	})
	first := newClusterCatalogInstance(t, server, "node-a", client)
	second := newClusterCatalogInstance(t, server, "node-b", client)
	for _, instance := range []clusterCatalogInstance{first, second} {
		if err := instance.coordinator.syncAutomatically(t.Context(), CatalogSyncStartup); err != nil {
			t.Fatalf("syncAutomatically() error = %v", err)
		}
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches without a shared catalog = %d, want each instance to sync", got)
	}
	fetchedAt, err := cluster.NewCatalogStore(clustertest.Connect(t, server, "reader")).FetchedAt(t.Context())
	if err != nil || fetchedAt != 3000 {
		t.Fatalf("shared catalog fetched_at = %d, %v; want 3000", fetchedAt, err)
	}
}

func TestClusterCatalogAdoptionSkipsOlderOrPendingDocuments(t *testing.T) {
	server := miniredis.RunT(t)
	storeSharedCatalog(t, server, catalogResultFixture(1000, "old", nil))
	instance := newClusterCatalogInstance(t, server, "node-a", nil)
	instance.coordinator.metadata.SuccessfulFetchAtMillis = 5000
	if err := instance.coordinator.adoptSharedCatalog(t.Context()); err != nil {
		t.Fatalf("adoptSharedCatalog() error = %v", err)
	}
	if got := instance.coordinator.metadata.SuccessfulFetchAtMillis; got != 5000 {
		t.Fatalf("an older shared catalog replaced a newer local one: fetched_at = %d", got)
	}

	storeSharedCatalog(t, server, catalogResultFixture(6000, "newer", nil))
	pending := catalogResultFixture(7000, "pending", nil)
	instance.coordinator.pending = &pending
	if err := instance.coordinator.adoptSharedCatalog(t.Context()); err != nil {
		t.Fatalf("adoptSharedCatalog() error = %v", err)
	}
	if instance.coordinator.metadata.ETag == "newer" {
		t.Fatal("adoption replaced the catalog while a local result was pending")
	}
}
