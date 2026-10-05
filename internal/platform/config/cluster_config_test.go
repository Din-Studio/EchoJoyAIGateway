package config

import (
	"strings"
	"testing"
	"time"
)

const testPostgresDSN = "postgres://user:pass@127.0.0.1:5432/gpt_load?sslmode=disable"

// setRequiredEnv clears the environment and sets valid values for the four
// variables every gateway instance requires.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	clearEnvironment(t)
	t.Setenv("REDIS_ADDRS", "127.0.0.1:6379")
	t.Setenv("DATABASE_DSN", testPostgresDSN)
	t.Setenv("AUTH_KEY", "test-auth-key")
	t.Setenv("ENCRYPTION_KEY", "test-master-key-long")
}

func TestLoadRequiresEachStartupVariable(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T)
		wantErr string
	}{
		{name: "missing redis", prepare: func(t *testing.T) { t.Setenv("REDIS_ADDRS", "") }, wantErr: "REDIS_ADDRS is required"},
		{name: "blank redis list", prepare: func(t *testing.T) { t.Setenv("REDIS_ADDRS", " , ") }, wantErr: "REDIS_ADDRS is required"},
		{
			name:    "missing database",
			prepare: func(t *testing.T) { t.Setenv("DATABASE_DSN", "") },
			wantErr: "DATABASE_DSN is required and must be a PostgreSQL DSN",
		},
		{
			name:    "sqlite database",
			prepare: func(t *testing.T) { t.Setenv("DATABASE_DSN", ":memory:") },
			wantErr: "DATABASE_DSN is required and must be a PostgreSQL DSN",
		},
		{
			name:    "mysql database",
			prepare: func(t *testing.T) { t.Setenv("DATABASE_DSN", "mysql://root:root@127.0.0.1:3306/gpt_load") },
			wantErr: "DATABASE_DSN is required and must be a PostgreSQL DSN",
		},
		{name: "missing auth key", prepare: func(t *testing.T) { t.Setenv("AUTH_KEY", "") }, wantErr: "AUTH_KEY is required"},
		{
			name:    "auth key with whitespace",
			prepare: func(t *testing.T) { t.Setenv("AUTH_KEY", "admin key") },
			wantErr: "AUTH_KEY must not contain whitespace",
		},
		{
			name:    "missing encryption key",
			prepare: func(t *testing.T) { t.Setenv("ENCRYPTION_KEY", "") },
			wantErr: "ENCRYPTION_KEY is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			tc.prepare(t)

			_, err := Load()
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadParsesClusterConfiguration(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("REDIS_ADDRS", " redis-a:6379, ,redis-b:6379 ")
	t.Setenv("REDIS_PASSWORD", "secret")
	t.Setenv("REDIS_TLS", "true")
	t.Setenv("REDIS_KEY_PREFIX", "tenant")
	t.Setenv("INSTANCE_ID", "node-a")
	t.Setenv("RESPONSE_BINDING_TTL", "24h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Cluster.ResponseBindingTTL != 24*time.Hour {
		t.Fatalf("ResponseBindingTTL = %s, want 24h", cfg.Cluster.ResponseBindingTTL)
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
	setRequiredEnv(t)

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
	if first.Cluster.ResponseBindingTTL != 720*time.Hour {
		t.Fatalf("ResponseBindingTTL = %s, want 720h by default", first.Cluster.ResponseBindingTTL)
	}
}

func TestLoadRejectsInvalidClusterConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T)
		wantErr string
	}{
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
		{
			name: "unparsable response binding ttl",
			prepare: func(t *testing.T) {
				t.Setenv("RESPONSE_BINDING_TTL", "abc")
			},
			wantErr: "RESPONSE_BINDING_TTL must be a duration of at least 1m",
		},
		{
			name: "response binding ttl below one minute",
			prepare: func(t *testing.T) {
				t.Setenv("RESPONSE_BINDING_TTL", "30s")
			},
			wantErr: "RESPONSE_BINDING_TTL must be a duration of at least 1m",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			tc.prepare(t)

			_, err := Load()
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Load() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
