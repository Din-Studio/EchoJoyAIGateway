package config

import (
	"strings"
	"testing"
)

const testPostgresDSN = "postgres://user:pass@127.0.0.1:5432/gpt_load?sslmode=disable"

func setClusterEnvironment(t *testing.T) {
	t.Helper()
	clearEnvironment(t)
	t.Setenv("AUTH_KEY", "test-auth-key")
	t.Setenv("ENCRYPTION_KEY", "test-master-key-long")
	t.Setenv("DATABASE_DSN", testPostgresDSN)
}

func TestLoadParsesClusterConfiguration(t *testing.T) {
	setClusterEnvironment(t)
	t.Setenv("REDIS_ADDRS", " redis-a:6379, ,redis-b:6379 ")
	t.Setenv("REDIS_PASSWORD", "secret")
	t.Setenv("REDIS_TLS", "true")
	t.Setenv("REDIS_KEY_PREFIX", "tenant")
	t.Setenv("INSTANCE_ID", "node-a")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Cluster.Enabled() {
		t.Fatal("Cluster.Enabled() = false, want true")
	}
	if got, want := strings.Join(cfg.Cluster.RedisAddrs, ","), "redis-a:6379,redis-b:6379"; got != want {
		t.Fatalf("RedisAddrs = %q, want %q", got, want)
	}
	if cfg.Cluster.RedisPassword != "secret" || !cfg.Cluster.RedisTLS {
		t.Fatalf("Cluster = %#v, want password secret and TLS", cfg.Cluster)
	}
	if cfg.Cluster.RedisKeyPrefix != "tenant" || cfg.Cluster.InstanceID != "node-a" {
		t.Fatalf("Cluster = %#v, want prefix tenant and instance node-a", cfg.Cluster)
	}
}

func TestLoadGeneratesDistinctInstanceIDs(t *testing.T) {
	setClusterEnvironment(t)
	t.Setenv("REDIS_ADDRS", "127.0.0.1:6379")

	first, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	second, err := Load()
	if err != nil {
		t.Fatalf("Load() second error = %v", err)
	}
	if first.Cluster.RedisKeyPrefix != "gl" {
		t.Fatalf("RedisKeyPrefix = %q, want gl", first.Cluster.RedisKeyPrefix)
	}
	if first.Cluster.InstanceID == "" || !strings.Contains(first.Cluster.InstanceID, "-") {
		t.Fatalf("InstanceID = %q, want <hostname>-<hex>", first.Cluster.InstanceID)
	}
	if first.Cluster.InstanceID == second.Cluster.InstanceID {
		t.Fatalf("InstanceID repeated across Load(): %q", first.Cluster.InstanceID)
	}
	if first.Cluster.RedisTLS {
		t.Fatal("RedisTLS = true, want false by default")
	}
}

func TestLoadRejectsInvalidClusterConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T)
		wantErr string
	}{
		{
			name: "sqlite",
			prepare: func(t *testing.T) {
				t.Setenv("DATABASE_DSN", ":memory:")
			},
			wantErr: "REDIS_ADDRS requires a PostgreSQL DATABASE_DSN",
		},
		{
			name: "managed database",
			prepare: func(t *testing.T) {
				t.Setenv("DATABASE_DSN", "")
			},
			wantErr: "REDIS_ADDRS requires a PostgreSQL DATABASE_DSN",
		},
		{
			name: "mysql",
			prepare: func(t *testing.T) {
				t.Setenv("DATABASE_DSN", "mysql://root:root@127.0.0.1:3306/gpt_load")
			},
			wantErr: "REDIS_ADDRS requires a PostgreSQL DATABASE_DSN",
		},
		{
			name: "missing auth key",
			prepare: func(t *testing.T) {
				t.Setenv("AUTH_KEY", "")
			},
			wantErr: "REDIS_ADDRS requires AUTH_KEY to be set explicitly",
		},
		{
			name: "missing encryption key",
			prepare: func(t *testing.T) {
				t.Setenv("ENCRYPTION_KEY", "")
			},
			wantErr: "REDIS_ADDRS requires ENCRYPTION_KEY to be set explicitly",
		},
		{
			name: "invalid tls",
			prepare: func(t *testing.T) {
				t.Setenv("REDIS_TLS", "sometimes")
			},
			wantErr: "REDIS_TLS must be a boolean",
		},
		{
			name: "prefix with trailing colon",
			prepare: func(t *testing.T) {
				t.Setenv("REDIS_KEY_PREFIX", "gl:")
			},
			wantErr: "REDIS_KEY_PREFIX must not contain whitespace or end with a colon",
		},
		{
			name: "prefix with whitespace",
			prepare: func(t *testing.T) {
				t.Setenv("REDIS_KEY_PREFIX", "gpt load")
			},
			wantErr: "REDIS_KEY_PREFIX must not contain whitespace or end with a colon",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setClusterEnvironment(t)
			t.Setenv("REDIS_ADDRS", "127.0.0.1:6379")
			tc.prepare(t)

			_, err := Load()
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Load() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadIgnoresClusterVariablesWithoutRedisAddrs(t *testing.T) {
	clearEnvironment(t)
	t.Setenv("AUTH_KEY", "test-auth-key")
	t.Setenv("REDIS_PASSWORD", "secret")
	t.Setenv("REDIS_KEY_PREFIX", "bad:")
	t.Setenv("INSTANCE_ID", "node-a")
	t.Setenv("REDIS_DSN", "://invalid-redis-dsn")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Cluster.Enabled() {
		t.Fatal("Cluster.Enabled() = true, want false")
	}
	if cfg.Cluster.RedisAddrs != nil || cfg.Cluster.RedisPassword != "" || cfg.Cluster.RedisTLS ||
		cfg.Cluster.RedisKeyPrefix != "" || cfg.Cluster.InstanceID != "" {
		t.Fatalf("Cluster = %#v, want zero value", cfg.Cluster)
	}
}
