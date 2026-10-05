package container

import (
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/cluster"
	"gpt-load/internal/control"
	"gpt-load/internal/gateway"
	"gpt-load/internal/state"
)

// TestBuildContainerAssemblesRedisSharedState pins the one runtime path:
// every piece of shared request and control state is the Redis implementation.
func TestBuildContainerAssemblesRedisSharedState(t *testing.T) {
	setStartupEnv(t)

	dependencyContainer, err := BuildContainer()
	if err != nil {
		t.Fatalf("BuildContainer() error = %v", err)
	}
	err = dependencyContainer.Invoke(func(
		shared gateway.SharedState,
		health state.SharedCredentialHealthStore,
		bootstrap *control.CatalogBootstrap,
		db *gorm.DB,
	) {
		t.Cleanup(func() {
			if sqlDB, dbErr := db.DB(); dbErr == nil {
				_ = sqlDB.Close()
			}
		})
		if _, ok := shared.AccessQuota.(*cluster.AccessQuota); !ok {
			t.Errorf("quota gate = %T, want *cluster.AccessQuota", shared.AccessQuota)
		}
		if _, ok := shared.Health.(*cluster.CredentialHealth); !ok || shared.Health != health {
			t.Errorf("gateway health = %T, want the shared *cluster.CredentialHealth", shared.Health)
		}
		if _, ok := shared.ResponseBindings.(*cluster.ResponseBindings); !ok {
			t.Errorf("response bindings = %T, want *cluster.ResponseBindings", shared.ResponseBindings)
		}
		if _, ok := shared.Affinity.(*cluster.Affinity); !ok {
			t.Errorf("affinity = %T, want *cluster.Affinity", shared.Affinity)
		}
		if bootstrap.HasLKG {
			t.Error("catalog bootstrap has an LKG; want the empty shared catalog")
		}
	})
	if err != nil {
		t.Fatalf("resolve shared state: %v", err)
	}
}
