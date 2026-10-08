package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadUsesDefaultConfiguration(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("Host = %q, want 127.0.0.1", cfg.Server.Host)
	}
	if cfg.Server.Port != 3001 {
		t.Fatalf("Port = %d, want 3001", cfg.Server.Port)
	}
	if cfg.Server.GracefulShutdownTimeout != 10 {
		t.Fatalf("GracefulShutdownTimeout = %d, want 10", cfg.Server.GracefulShutdownTimeout)
	}
	if cfg.Server.ReadTimeout != 60 {
		t.Fatalf("ReadTimeout = %d, want 60", cfg.Server.ReadTimeout)
	}
	if cfg.Server.IdleTimeout != 120 {
		t.Fatalf("IdleTimeout = %d, want 120", cfg.Server.IdleTimeout)
	}
	if cfg.DatabaseDSN != testPostgresDSN {
		t.Fatalf("DatabaseDSN = %q", cfg.DatabaseDSN)
	}
	if len(cfg.Cluster.RedisAddrs) == 0 {
		t.Fatal("Cluster.RedisAddrs is empty")
	}
	if cfg.DatabasePool.MaxOpenConnections != 10 || cfg.DatabasePool.MaxIdleConnections != 5 {
		t.Fatalf("DatabasePool = %#v, want max open/idle 10/5", cfg.DatabasePool)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" {
		t.Fatalf("Log = %#v, want info/text", cfg.Log)
	}
	// setRequiredEnv runs Load from an empty working directory; it must stay empty.
	if entries, err := os.ReadDir("."); err != nil || len(entries) != 0 {
		t.Fatalf("Load() wrote local files: %v (error %v)", entries, err)
	}
}

func TestLoadPreservesExplicitAllInterfacesHost(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("HOST", "0.0.0.0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("Host = %q, want explicit 0.0.0.0", cfg.Server.Host)
	}
}

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("HOST", "127.0.0.1")
	t.Setenv("PORT", "4010")
	t.Setenv("ENCRYPTION_KEY", "explicit-encryption-key")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "json")
	t.Setenv("GRACEFUL_SHUTDOWN_TIMEOUT", "25")
	t.Setenv("READ_TIMEOUT", "45")
	t.Setenv("IDLE_TIMEOUT", "90")
	t.Setenv("DATABASE_MAX_OPEN_CONNECTIONS", "24")
	t.Setenv("DATABASE_MAX_IDLE_CONNECTIONS", "12")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 4010 {
		t.Fatalf("Server = %#v", cfg.Server)
	}
	if cfg.Server.GracefulShutdownTimeout != 25 {
		t.Fatalf("GracefulShutdownTimeout = %d", cfg.Server.GracefulShutdownTimeout)
	}
	if cfg.Server.ReadTimeout != 45 || cfg.Server.IdleTimeout != 90 {
		t.Fatalf("read/idle timeouts not loaded: %#v", cfg.Server)
	}
	if cfg.EncryptionKey != "explicit-encryption-key" {
		t.Fatalf("encryption override not loaded: %#v", cfg)
	}
	if cfg.DatabasePool.MaxOpenConnections != 24 || cfg.DatabasePool.MaxIdleConnections != 12 {
		t.Fatalf("DatabasePool = %#v, want max open/idle 24/12", cfg.DatabasePool)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Fatalf("Log = %#v", cfg.Log)
	}
}

func TestLoadModelsDevAutoSyncOverrideIsOptionalAndStrict(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		want    *bool
		wantErr bool
	}{
		{name: "unset"},
		{name: "true", value: "true", want: boolPointer(true)},
		{name: "false", value: "false", want: boolPointer(false)},
		{name: "strconv true syntax", value: "1", want: boolPointer(true)},
		{name: "invalid", value: "enabled", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("MODELS_DEV_AUTO_SYNC_ENABLED", test.value)

			cfg, err := Load()
			if test.wantErr {
				if err == nil {
					t.Fatal("Load() error = nil, want strict boolean error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if (cfg.ModelsDevAutoSyncOverride == nil) != (test.want == nil) {
				t.Fatalf("ModelsDevAutoSyncOverride = %#v, want %#v", cfg.ModelsDevAutoSyncOverride, test.want)
			}
			if test.want != nil && *cfg.ModelsDevAutoSyncOverride != *test.want {
				t.Fatalf("ModelsDevAutoSyncOverride = %t, want %t", *cfg.ModelsDevAutoSyncOverride, *test.want)
			}
		})
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func TestLoadAcceptsPostgreSQLURLs(t *testing.T) {
	for _, dsn := range []string{
		"postgres://user:password@db.example:5432/gpt_load",
		"postgresql://user:password@db.example:5432/gpt_load",
	} {
		t.Run(dsn, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("DATABASE_DSN", dsn)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.DatabaseDSN != dsn {
				t.Fatalf("DatabaseDSN = %q, want %q", cfg.DatabaseDSN, dsn)
			}
		})
	}
}

func TestParseDatabaseDSNAcceptsPostgreSQLURLs(t *testing.T) {
	for _, test := range []struct {
		dsn  string
		want string
	}{
		{dsn: "postgres://user:password@db.example:5432/gpt_load?sslmode=require", want: "postgres://user:password@db.example:5432/gpt_load?sslmode=require"},
		{dsn: " postgresql://user@db.example/gpt_load ", want: "postgresql://user@db.example/gpt_load"},
		{dsn: "POSTGRES://user@db.example/gpt_load", want: "POSTGRES://user@db.example/gpt_load"},
	} {
		got, err := ParseDatabaseDSN(test.dsn)
		if err != nil {
			t.Fatalf("ParseDatabaseDSN(%q) error = %v", test.dsn, err)
		}
		if got != test.want {
			t.Fatalf("ParseDatabaseDSN(%q) = %q, want %q", test.dsn, got, test.want)
		}
	}
}

func TestParseDatabaseDSNRejectsEverythingElse(t *testing.T) {
	for _, dsn := range []string{
		"",
		":memory:",
		"data/gpt-load.db",
		"file:gpt-load.db",
		"sqlite:///var/lib/gpt-load/gpt-load.db",
		"mysql://user:password@db.example:3306/gpt_load",
		"redis://localhost/0",
		"postgres://localhost",
		"postgres:///gpt_load",
		"postgres://localhost/gpt_load/extra",
		"postgres://localhost/gpt_load#fragment",
	} {
		if _, err := ParseDatabaseDSN(dsn); err == nil {
			t.Fatalf("ParseDatabaseDSN(%q) error = nil, want validation error", dsn)
		}
	}
}

func TestLoadRejectsInvalidRequiredAndNumericValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "whitespace-only auth key", env: map[string]string{"AUTH_KEY": "   "}},
		{name: "auth key with internal space", env: map[string]string{"AUTH_KEY": "admin key"}},
		{name: "auth key with tab", env: map[string]string{"AUTH_KEY": "admin\tkey"}},
		{name: "auth key with unicode whitespace", env: map[string]string{"AUTH_KEY": "admin\u00a0key"}},
		{name: "invalid port", env: map[string]string{"AUTH_KEY": "x", "PORT": "nope"}},
		{name: "port out of range", env: map[string]string{"AUTH_KEY": "x", "PORT": "70000"}},
		{name: "invalid shutdown timeout", env: map[string]string{"AUTH_KEY": "x", "GRACEFUL_SHUTDOWN_TIMEOUT": "0"}},
		{name: "invalid read timeout", env: map[string]string{"AUTH_KEY": "x", "READ_TIMEOUT": "0"}},
		{name: "invalid idle timeout", env: map[string]string{"AUTH_KEY": "x", "IDLE_TIMEOUT": "nope"}},
		{name: "invalid database max open connections", env: map[string]string{"AUTH_KEY": "x", "DATABASE_MAX_OPEN_CONNECTIONS": "0"}},
		{name: "invalid database max idle connections", env: map[string]string{"AUTH_KEY": "x", "DATABASE_MAX_IDLE_CONNECTIONS": "nope"}},
		{name: "database max idle connections exceed max open", env: map[string]string{
			"AUTH_KEY": "x", "DATABASE_MAX_OPEN_CONNECTIONS": "4", "DATABASE_MAX_IDLE_CONNECTIONS": "5",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil, want error")
			}
		})
	}
}

func clearEnvironment(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, key := range []string{
		"HOST", "PORT", "DATABASE_DSN", "ENCRYPTION_KEY", "AUTH_KEY",
		"LOG_LEVEL", "LOG_FORMAT", "GRACEFUL_SHUTDOWN_TIMEOUT",
		"READ_TIMEOUT", "IDLE_TIMEOUT", "MODELS_DEV_AUTO_SYNC_ENABLED",
		"DATABASE_MAX_OPEN_CONNECTIONS", "DATABASE_MAX_IDLE_CONNECTIONS",
		"REDIS_ADDRS", "REDIS_PASSWORD", "REDIS_TLS", "REDIS_KEY_PREFIX", "INSTANCE_ID",
		"RESPONSE_BINDING_TTL",
	} {
		t.Setenv(key, "")
	}
}

func TestRequirePostgreSQLDSNReportsUnparsableURLAsInvalid(t *testing.T) {
	_, err := requirePostgreSQLDSN("postgres://user:p%zz@db:5432/gpt_load")
	if err == nil || !strings.Contains(err.Error(), "DATABASE_DSN is invalid") {
		t.Fatalf("error = %v, want DATABASE_DSN is invalid diagnostic", err)
	}
	if strings.Contains(err.Error(), "p%zz") {
		t.Fatalf("error leaks DSN password: %v", err)
	}
}
