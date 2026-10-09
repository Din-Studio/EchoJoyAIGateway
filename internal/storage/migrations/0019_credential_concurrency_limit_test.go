package migrations_test

import (
	"testing"

	"gpt-load/internal/storage/migrations"
)

func TestCredentialConcurrencyLimitMigrationDefaultsExistingRowsToUnlimited(t *testing.T) {
	t.Parallel()
	db := openInitialTestDatabase(t)
	if err := migrations.Up0001(db); err != nil {
		t.Fatalf("prepare previous schema: %v", err)
	}
	if err := migrations.Validate0019(db); err == nil {
		t.Fatal("missing concurrency_limit column accepted")
	}
	if err := db.Exec(`INSERT INTO groups (
		id, name, channel_id, connection_type, params, models, enabled, created_at_ms, updated_at_ms
	) VALUES (1, 'concurrency migration', 'openai', 'api_key', '{}', '[]', true, 1, 1)`).Error; err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := db.Exec(`INSERT INTO credentials (
		id, group_id, data, fingerprint, identity_fingerprint, secret_version,
		auth_state, auth_error_code, status, created_at_ms, updated_at_ms
	) VALUES (1, 1, 'credential-cipher', 'fingerprint', 'identity', 1,
		'ready', '', 'active', 1, 1)`).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}

	for range 2 {
		if err := migrations.Up0019(db); err != nil {
			t.Fatalf("Up0019() error = %v", err)
		}
	}
	var limit int
	if err := db.Table("credentials").Select("concurrency_limit").Where("id = ?", 1).Scan(&limit).Error; err != nil {
		t.Fatalf("read concurrency_limit: %v", err)
	}
	if limit != 0 {
		t.Fatalf("existing concurrency_limit = %d, want 0", limit)
	}
	if err := db.Exec(`UPDATE credentials SET concurrency_limit = -1 WHERE id = 1`).Error; err == nil {
		t.Fatal("negative concurrency_limit accepted")
	}
}
