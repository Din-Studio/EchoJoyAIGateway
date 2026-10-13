package migrations

import (
	"fmt"

	"gorm.io/gorm"
)

const ID0020 = "0020_teams"

// 冻结本次迁移的 DDL，后续运行模型变化不能改写历史。
var teamStatements0020 = []string{
	`ALTER TABLE access_keys ADD COLUMN IF NOT EXISTS team_id VARCHAR(255) NULL`,
	`ALTER TABLE access_keys ADD COLUMN IF NOT EXISTS owner_user_id VARCHAR(64) NULL`,
	`ALTER TABLE access_keys ADD COLUMN IF NOT EXISTS owner_name VARCHAR(255) NULL`,
	`CREATE INDEX IF NOT EXISTS idx_access_keys_team_id ON access_keys (team_id)`,
	`CREATE INDEX IF NOT EXISTS idx_access_keys_owner_user_id ON access_keys (owner_user_id)`,
	`CREATE TABLE IF NOT EXISTS team_group_grants (
		team_id VARCHAR(255) NOT NULL,
		group_id BIGINT NOT NULL REFERENCES "groups" (id) ON UPDATE CASCADE ON DELETE CASCADE,
		created_at_ms BIGINT NOT NULL CONSTRAINT chk_team_group_grant_created_at CHECK (created_at_ms >= 0),
		PRIMARY KEY (team_id, group_id)
	)`,
}

var teamColumns0020 = []string{"team_id", "owner_user_id", "owner_name"}

var teamIndexes0020 = []string{"idx_access_keys_team_id", "idx_access_keys_owner_user_id"}

// Up0020 adds team ownership to AccessKeys and the per-team group grants.
// Existing keys keep NULL team_id and stay platform keys.
func Up0020(db *gorm.DB) error {
	if !db.Migrator().HasTable("access_keys") || !db.Migrator().HasTable("groups") {
		return fmt.Errorf("add teams: access_keys or groups table is missing")
	}
	for _, statement := range teamStatements0020 {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("add teams: %w", err)
		}
	}
	return Validate0020(db)
}

func Validate0020(db *gorm.DB) error {
	for _, column := range teamColumns0020 {
		if !db.Migrator().HasColumn("access_keys", column) {
			return fmt.Errorf("validate teams: column access_keys.%s is missing", column)
		}
	}
	for _, index := range teamIndexes0020 {
		if !db.Migrator().HasIndex("access_keys", index) {
			return fmt.Errorf("validate teams: index %s is missing", index)
		}
	}
	if !db.Migrator().HasTable("team_group_grants") {
		return fmt.Errorf("validate teams: table team_group_grants is missing")
	}
	return nil
}
