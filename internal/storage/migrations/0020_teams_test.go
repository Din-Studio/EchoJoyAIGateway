package migrations_test

import (
	"testing"

	"gpt-load/internal/storage/migrations"
)

func TestTeamsMigrationKeepsExistingKeysAsPlatformKeys(t *testing.T) {
	t.Parallel()
	db := openInitialTestDatabase(t)
	if err := migrations.Up0001(db); err != nil {
		t.Fatalf("prepare previous schema: %v", err)
	}
	if err := migrations.Validate0020(db); err == nil {
		t.Fatal("missing team columns accepted")
	}
	if err := db.Exec(`INSERT INTO groups (
		id, name, channel_id, connection_type, params, models, enabled, created_at_ms, updated_at_ms
	) VALUES (1, 'teams migration', 'openai', 'api_key', '{}', '[]', true, 1, 1)`).Error; err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := db.Exec(`INSERT INTO access_keys (
		id, name, key_value, key_hash, key_suffix, status, created_at_ms, updated_at_ms
	) VALUES (1, 'platform', 'cipher', 'hash', 'abcd', 'active', 1, 1)`).Error; err != nil {
		t.Fatalf("create access key: %v", err)
	}

	for range 2 {
		if err := migrations.Up0020(db); err != nil {
			t.Fatalf("Up0020() error = %v", err)
		}
	}
	var teamID *string
	if err := db.Table("access_keys").Select("team_id").Where("id = ?", 1).Scan(&teamID).Error; err != nil {
		t.Fatalf("read team_id: %v", err)
	}
	if teamID != nil {
		t.Fatalf("existing team_id = %q, want NULL", *teamID)
	}
	if err := db.Exec(`INSERT INTO team_group_grants (team_id, group_id, created_at_ms) VALUES ('team-a', 1, 1)`).Error; err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	if err := db.Exec(`INSERT INTO team_group_grants (team_id, group_id, created_at_ms) VALUES ('team-a', 1, 2)`).Error; err == nil {
		t.Fatal("duplicate grant accepted")
	}
	if err := db.Exec(`INSERT INTO team_group_grants (team_id, group_id, created_at_ms) VALUES ('team-a', 99, 1)`).Error; err == nil {
		t.Fatal("grant for missing group accepted")
	}
	if err := db.Exec(`DELETE FROM groups WHERE id = 1`).Error; err != nil {
		t.Fatalf("delete group: %v", err)
	}
	var grants int64
	if err := db.Table("team_group_grants").Count(&grants).Error; err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grants != 0 {
		t.Fatalf("grants after group delete = %d, want 0", grants)
	}
}
