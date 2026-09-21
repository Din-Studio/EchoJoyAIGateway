package container

import (
	"testing"

	"go.uber.org/dig"
	"gorm.io/gorm"

	"gpt-load/internal/control"
	"gpt-load/internal/coordination"
)

func TestConfigVersionProvidersStayNilWithoutCoordination(t *testing.T) {
	if version := newConfigVersion(nil); version != nil {
		t.Fatalf("newConfigVersion(nil) = %#v, want nil", version)
	}
	// A typed nil stored in an interface is not a nil interface value, and the
	// configuration watch loop is assembled on exactly that test.
	if source := newConfigVersionSource(nil); source != nil {
		t.Fatalf("newConfigVersionSource(nil) = %#v, want a true nil interface", source)
	}
	if source := newConfigVersionSource(coordination.NewConfigVersion(&coordination.Client{})); source == nil {
		t.Fatal("newConfigVersionSource(version) = nil, want the version source")
	}
}

func TestBuildContainerOmitsConfigVersionWithoutRedisDSN(t *testing.T) {
	t.Setenv("AUTH_KEY", "test-auth-key")
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("DATABASE_DSN", ":memory:")
	t.Setenv("ENCRYPTION_KEY", "test-master-key-long")

	dependencyContainer, err := BuildContainer()
	if err != nil {
		t.Fatalf("BuildContainer() error = %v", err)
	}
	assertNoConfigVersion(t, dependencyContainer)
}

func assertNoConfigVersion(t *testing.T, dependencyContainer *dig.Container) {
	t.Helper()
	if err := dependencyContainer.Invoke(func(
		version *coordination.ConfigVersion,
		source control.ConfigVersionSource,
		db *gorm.DB,
	) {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			t.Cleanup(func() { _ = sqlDB.Close() })
		}
		if version != nil {
			t.Errorf("*coordination.ConfigVersion = %#v in single-instance mode, want nil", version)
		}
		if source != nil {
			t.Errorf("control.ConfigVersionSource = %#v in single-instance mode, want nil", source)
		}
	}); err != nil {
		t.Fatalf("resolve configuration version dependencies: %v", err)
	}
}
