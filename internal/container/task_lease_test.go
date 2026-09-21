package container

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/dig"
	"gorm.io/gorm"

	"gpt-load/internal/control"
	"gpt-load/internal/coordination"
	"gpt-load/internal/gateway"
	"gpt-load/internal/health"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/state"
)

func TestCoordinationProvidersStayNilWithoutCoordination(t *testing.T) {
	lease, err := newTaskLease(nil)
	if err != nil || lease != nil {
		t.Fatalf("newTaskLease(nil) = %#v, %v; want nil, nil", lease, err)
	}
	// A typed nil stored in an interface is not a nil interface value, and
	// every claim is decided on exactly that test.
	if claimer := newControlTaskLease(nil); claimer != nil {
		t.Fatalf("newControlTaskLease(nil) = %#v, want a true nil interface", claimer)
	}
	if claimer := newControlTaskLease(&coordination.Lease{}); claimer == nil {
		t.Fatal("newControlTaskLease(lease) = nil, want the lease")
	}
}

func TestResponseBindingStoreFollowsTheInstanceMode(t *testing.T) {
	local := state.NewResponseBindings()
	if store := newResponseBindingStore(nil, local); !isLocalBindingStore(store) {
		t.Fatalf("store without coordination = %T, want the in-process adapter", store)
	}
	if store := newResponseBindingStore(&coordination.Client{}, local); isLocalBindingStore(store) {
		t.Fatalf("store with coordination = %T, want the shared index", store)
	}
}

// Distributed mode keeps ownership in Redis, so the restart checkpoint must
// stop carrying it rather than persist an index nobody reads.
// Distributed mode keeps nothing on disk: credential health and response
// ownership are shared, and a file restoring either of them at startup would
// overwrite what the fleet already agreed on with what this process last saw.
func TestRuntimeStateCheckpointWritesNothingInDistributedMode(t *testing.T) {
	for name, test := range map[string]struct {
		mode           config.InstanceMode
		wantCheckpoint bool
	}{
		"single":      {mode: config.InstanceModeSingle, wantCheckpoint: true},
		"distributed": {mode: config.InstanceModeDistributed, wantCheckpoint: false},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			bindings := state.NewResponseBindings()
			if !bindings.Record(1, "resp_1", state.CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}) {
				t.Fatal("Record() = false, want the seeded ownership")
			}
			checkpoint := newRuntimeStateCheckpoint(
				&config.Config{DataDir: dataDir, InstanceMode: test.mode},
				state.NewCredentialRegistry(), health.NewStatsStore(), bindings,
			)
			if err := checkpoint.Save(context.Background()); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			raw, err := os.ReadFile(filepath.Join(dataDir, "runtime-state.checkpoint.json"))
			if !test.wantCheckpoint {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("read checkpoint = (%s, %v), want no file at all", raw, err)
				}
				// Restoring must be just as silent, not a missing-file error.
				if err := checkpoint.Restore(context.Background()); err != nil {
					t.Fatalf("Restore() error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read checkpoint: %v", err)
			}
			var document struct {
				Responses []state.ResponseBinding `json:"responses"`
			}
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatalf("decode checkpoint: %v", err)
			}
			if len(document.Responses) == 0 {
				t.Fatalf("checkpoint carried no ownership; file = %s", raw)
			}
		})
	}
}

func TestBuildContainerOmitsTaskLeaseWithoutRedisDSN(t *testing.T) {
	t.Setenv("AUTH_KEY", "test-auth-key")
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("DATABASE_DSN", ":memory:")
	t.Setenv("ENCRYPTION_KEY", "test-master-key-long")

	dependencyContainer, err := BuildContainer()
	if err != nil {
		t.Fatalf("BuildContainer() error = %v", err)
	}
	assertNoCoordinationCollaborators(t, dependencyContainer)
}

func assertNoCoordinationCollaborators(t *testing.T, dependencyContainer *dig.Container) {
	t.Helper()
	if err := dependencyContainer.Invoke(func(
		lease *coordination.Lease,
		claimer control.TaskLease,
		store gateway.ResponseBindingStore,
		db *gorm.DB,
	) {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			t.Cleanup(func() { _ = sqlDB.Close() })
		}
		if lease != nil {
			t.Errorf("*coordination.Lease = %#v in single-instance mode, want nil", lease)
		}
		if claimer != nil {
			t.Errorf("control.TaskLease = %#v in single-instance mode, want a true nil", claimer)
		}
		if !isLocalBindingStore(store) {
			t.Errorf("gateway.ResponseBindingStore = %T in single-instance mode, want the in-process adapter", store)
		}
	}); err != nil {
		t.Fatalf("resolve coordination dependencies: %v", err)
	}
}

func isLocalBindingStore(store gateway.ResponseBindingStore) bool {
	_, local := store.(*gateway.LocalResponseBindings)
	return local
}
