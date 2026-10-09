package storage_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/storage"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/testutil/pgtest"
)

func TestCredentialStatusAcceptsOnlyDurableOperatorStates(t *testing.T) {
	t.Parallel()

	db := openMigratedDatabase(t)
	group := models.Group{
		Name: "credential-status-parent", ChannelID: "openai_compatible",
		Params: models.JSON(`{"base_url":"https://credential-status.example.com"}`), Models: models.JSON(`[]`),
	}
	if err := db.Create(&group).Error; err != nil {
		t.Fatalf("create parent group: %v", err)
	}
	for index, status := range []models.CredentialStatus{
		models.CredentialStatusActive,
		models.CredentialStatusDisabled,
	} {
		credential := models.Credential{
			GroupID: group.ID, Data: "encrypted-data",
			Fingerprint: "allowed-credential-status-" + string(rune('a'+index)),
			Status:      status,
		}
		if err := db.Create(&credential).Error; err != nil {
			t.Fatalf("create credential with status %q: %v", status, err)
		}
	}
	invalid := models.Credential{
		GroupID: group.ID, Data: "encrypted-data", Fingerprint: "invalid-credential-status",
		Status: models.CredentialStatus("blacklisted"),
	}
	if err := db.Create(&invalid).Error; err == nil {
		t.Fatal("create credential with runtime-only blacklisted status error = nil, want constraint error")
	}
}

func TestGroupNormalizesMissingChannelParamsToEmptyObject(t *testing.T) {
	t.Parallel()

	db := openMigratedDatabase(t)
	group := models.Group{
		Name: "normalized-channel-params", ChannelID: "staged-channel",
		Models: models.JSON(`[]`),
	}
	if err := db.Create(&group).Error; err != nil {
		t.Fatalf("create group without explicit channel params: %v", err)
	}
	var params string
	if err := db.Table("groups").Select("params").Where("id = ?", group.ID).Scan(&params).Error; err != nil {
		t.Fatalf("load normalized channel params: %v", err)
	}
	if params != `{}` {
		t.Fatalf("normalized channel params = %q, want {}", params)
	}
}

func TestAccessKeyStatusAcceptsOnlyDurableOperatorStates(t *testing.T) {
	t.Parallel()

	db := openMigratedDatabase(t)
	for index, status := range []string{"active", "disabled"} {
		key := models.AccessKey{
			Name:      "allowed-" + string(rune('a'+index)),
			KeyValue:  "ciphertext",
			KeyHash:   "allowed-status-" + string(rune('a'+index)),
			KeySuffix: "7f2a",
			Status:    status,
			Filters:   models.JSON(`{}`),
		}
		if err := db.Create(&key).Error; err != nil {
			t.Fatalf("create access key with status %q: %v", status, err)
		}
	}

	invalid := models.AccessKey{
		Name:      "invalid",
		KeyValue:  "ciphertext",
		KeyHash:   "invalid-status",
		KeySuffix: "7f2a",
		Status:    "blacklisted",
		Filters:   models.JSON(`{}`),
	}
	if err := db.Create(&invalid).Error; err == nil {
		t.Fatal("blacklisted status error = nil, want CHECK constraint error")
	}
}

func TestAutoMigrateCreatesReviewedIndexesAndPrimaryKeys(t *testing.T) {
	t.Parallel()

	db := openMigratedDatabase(t)

	for _, table := range []string{"request_logs", "jobs", "system_settings"} {
		columns, err := db.Migrator().ColumnTypes(table)
		if err != nil {
			t.Fatalf("inspect %s columns: %v", table, err)
		}

		var found bool
		for _, column := range columns {
			keyName := "id"
			if table == "system_settings" {
				keyName = "key"
			}
			if column.Name() == keyName {
				found = true
				if nullable, ok := column.Nullable(); !ok || nullable {
					t.Errorf("%s.%s nullable = %t/%t, want NOT NULL", table, keyName, nullable, ok)
				}
			}
		}
		if !found {
			t.Errorf("%s does not contain primary key column", table)
		}
	}

	if db.Migrator().HasTable("upstream_keys") {
		t.Fatal("retired upstream_keys table exists")
	}
	for _, table := range []string{"credentials", "access_keys"} {
		if db.Migrator().HasIndex(table, "idx_"+table+"_status") {
			t.Errorf("%s has ordinary status index %q", table, "idx_"+table+"_status")
		}
	}
}

func TestOpenRejectsInvalidDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dsn  string
	}{
		{name: "empty", dsn: ""},
		{name: "unsupported scheme", dsn: "redis://localhost/gpt-load"},
		{name: "sqlite memory", dsn: ":memory:"},
		{name: "sqlite path", dsn: "data/gpt-load.db"},
		{name: "sqlite URL", dsn: "sqlite:///var/lib/gpt-load/gpt-load.db"},
		{name: "mysql URL", dsn: "mysql://user:password@127.0.0.1:3306/gpt_load"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := storage.Open(tt.dsn); err == nil {
				t.Fatalf("Open(%q) error = nil, want a validation error", tt.dsn)
			}
		})
	}
}

func TestOpenAllowsUnversionedDatabaseWithExternalTables(t *testing.T) {
	t.Parallel()
	dsn := pgtest.NewEmptyDatabase(t)
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	if err := db.Exec("CREATE TABLE legacy_data (id integer PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := storage.Open(dsn)
	if err != nil {
		t.Fatalf("Open(database with external table) error = %v, want success", err)
	}
	reopenedSQL, err := reopened.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedSQL.Close() })
	if err := storage.AutoMigrate(reopened); err != nil {
		t.Fatalf("AutoMigrate(database with external table) error = %v", err)
	}
	if !reopened.Migrator().HasTable("legacy_data") {
		t.Fatal("AutoMigrate() removed the external table")
	}
}

func TestOpenConfiguresParameterizedSQLLogging(t *testing.T) {
	t.Parallel()

	db := openEmptyDatabase(t)

	filter, ok := db.Logger.(gorm.ParamsFilter)
	if !ok {
		t.Fatalf("database logger %T does not implement gorm.ParamsFilter", db.Logger)
	}
	const query = "INSERT INTO secrets(value) VALUES (?)"
	const secret = "known-plaintext-secret"
	filteredQuery, params := filter.ParamsFilter(context.Background(), query, secret)
	if filteredQuery != query {
		t.Fatalf("filtered query = %q, want %q", filteredQuery, query)
	}
	if len(params) != 0 {
		t.Fatalf("parameterized SQL logger retained %d parameter(s), want 0", len(params))
	}
}

func TestAutoMigrateCreatesSchemaAndMigrationLedger(t *testing.T) {
	t.Parallel()

	db := openMigratedDatabase(t)

	wantTables := []string{
		"groups",
		"credentials",
		"access_keys",
		"access_key_cost_limit_rules",
		"access_key_cost_limit_states",
		"request_logs",
		"usage_stats",
		"model_prices",
		"system_settings",
		"jobs",
		"control_operations",
		"credential_stages",
		"credential_observations",
		"credential_reset_operations",
		"credential_attempt_stats",
		"schema_migrations",
	}
	for _, table := range wantTables {
		if !db.Migrator().HasTable(table) {
			t.Errorf("AutoMigrate() did not create table %q", table)
		}
	}
	if db.Migrator().HasTable("auto_response_bindings") {
		t.Fatal("AutoMigrate() created an unnecessary shared binding table")
	}
	if db.Migrator().HasTable("usage_aggregation_journal") {
		t.Fatal("AutoMigrate() created the retired usage aggregation journal table")
	}

	var migrationIDs []string
	if err := db.Table("schema_migrations").Order("id ASC").Pluck("id", &migrationIDs).Error; err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	wantMigrationIDs := []string{
		"0001_initial",
		"0002_access_key_cost_limits",
		"0003_remove_observation_fresh_until",
		"0004_usage_stats_group_activity_index",
		"0005_proxy_config",
		"0006_error_decision",
		"0007_access_key_lifecycle",
		"0008_remove_inject_usage_options",
		"0009_price_multipliers",
		"0010_model_cooldown",
		"0011_custom_access_keys",
		"0012_access_key_mask_prefix",
		"0013_validation_protocol",
		"0014_affinity_kind",
		"0015_group_usage_index",
		"0016_credential_quota_history",
		"0017_request_log_operation_index",
		"0018_auto_model",
		"0019_credential_concurrency_limit",
	}
	if !reflect.DeepEqual(migrationIDs, wantMigrationIDs) {
		t.Fatalf("schema_migrations IDs = %v, want %v", migrationIDs, wantMigrationIDs)
	}

	if err := storage.AutoMigrate(db); err != nil {
		t.Fatalf("second AutoMigrate() error = %v", err)
	}
	var count int64
	if err := db.Table("schema_migrations").Count(&count).Error; err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != int64(len(wantMigrationIDs)) {
		t.Fatalf("schema_migrations row count after a second migration = %d, want %d", count, len(wantMigrationIDs))
	}
}

func TestAutoMigrateRejectsRetiredV2MigrationLedgers(t *testing.T) {
	t.Parallel()
	for _, retiredID := range []string{"0001_initial_v2", "0001_final_v2"} {
		t.Run(retiredID, func(t *testing.T) {
			db := openEmptyDatabase(t)

			if err := db.Exec(`CREATE TABLE schema_migrations (
				id varchar(255) PRIMARY KEY NOT NULL
			)`).Error; err != nil {
				t.Fatalf("create legacy migration ledger: %v", err)
			}
			if err := db.Exec("INSERT INTO schema_migrations(id) VALUES (?)", retiredID).Error; err != nil {
				t.Fatalf("seed legacy migration ledger: %v", err)
			}

			err := storage.AutoMigrate(db)
			if err == nil || !strings.Contains(err.Error(), "unknown or non-contiguous migration") {
				t.Fatalf("AutoMigrate() error = %v, want retired v2 ledger rejection", err)
			}
			if db.Migrator().HasTable("groups") {
				t.Fatal("AutoMigrate() modified a retired v2 database")
			}
		})
	}
}

func TestAutoMigrateRejectsAppliedMigrationWithIncompleteSchema(t *testing.T) {
	t.Parallel()
	db := openEmptyDatabase(t)

	if err := db.Exec(`CREATE TABLE schema_migrations (
		id varchar(255) PRIMARY KEY NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create migration ledger: %v", err)
	}
	if err := db.Exec("INSERT INTO schema_migrations(id) VALUES ('0001_initial')").Error; err != nil {
		t.Fatalf("seed migration ledger: %v", err)
	}
	if err := db.Exec(`CREATE TABLE groups (
		id integer PRIMARY KEY,
		name varchar(255) NOT NULL,
		channel_id varchar(64) NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create incomplete pre-Beta schema: %v", err)
	}

	err := storage.AutoMigrate(db)
	if err == nil || !strings.Contains(err.Error(), "validate applied migration 0001_initial") {
		t.Fatalf("AutoMigrate() error = %v, want incomplete applied migration rejection", err)
	}
}

func TestAutoMigrateAllowsEmptyLedgerBesideExistingExternalTables(t *testing.T) {
	t.Parallel()

	db := openEmptyDatabase(t)

	if err := db.Exec(`CREATE TABLE schema_migrations (
		id varchar(255) PRIMARY KEY NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create empty migration ledger: %v", err)
	}
	if err := db.Exec("CREATE TABLE legacy_data (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatalf("create legacy table: %v", err)
	}

	err := storage.AutoMigrate(db)
	if err != nil {
		t.Fatalf("AutoMigrate() error = %v, want success with external table", err)
	}
	if !db.Migrator().HasTable("groups") {
		t.Fatal("AutoMigrate() did not create the application schema")
	}
	if !db.Migrator().HasTable("legacy_data") {
		t.Fatal("AutoMigrate() removed the external table")
	}
}

func TestAutoMigrateCreatesRequestLogFieldsAndCompositeIndexes(t *testing.T) {
	t.Parallel()

	db := openMigratedDatabase(t)

	columns, err := db.Migrator().ColumnTypes("request_logs")
	if err != nil {
		t.Fatalf("inspect request_logs columns: %v", err)
	}
	columnNames := make(map[string]struct{}, len(columns))
	for _, column := range columns {
		columnNames[column.Name()] = struct{}{}
	}
	for _, name := range []string{
		"error_code",
		"error_summary",
		"reasoning_mode",
		"reasoning_effort",
		"reasoning_budget_tokens",
		"upstream_reported_model",
		"model_consistency",
	} {
		if _, ok := columnNames[name]; !ok {
			t.Errorf("request_logs column %q is missing", name)
		}
	}

	wantIndexes := map[string][]struct {
		name string
		desc bool
	}{
		"idx_request_logs_completed_id": {
			{name: "completed_at_ms", desc: true},
			{name: "id", desc: true},
		},
		"idx_request_logs_access_completed_id": {
			{name: "access_key_id"},
			{name: "completed_at_ms", desc: true},
			{name: "id", desc: true},
		},
		"idx_request_logs_status_completed_id": {
			{name: "status"},
			{name: "completed_at_ms", desc: true},
			{name: "id", desc: true},
		},
		"idx_request_logs_model_completed_id": {
			{name: "client_model"},
			{name: "completed_at_ms", desc: true},
			{name: "id", desc: true},
		},
		"idx_request_logs_upstream_model_completed_id": {
			{name: "upstream_model"},
			{name: "completed_at_ms", desc: true},
			{name: "id", desc: true},
		},
	}
	for indexName, wantColumns := range wantIndexes {
		gotColumns := indexKeyColumns(t, db, "request_logs", indexName)
		if len(gotColumns) == 0 {
			t.Errorf("request_logs index %q is missing", indexName)
			continue
		}
		if len(gotColumns) != len(wantColumns) {
			t.Errorf("%s columns = %+v, want %d key columns", indexName, gotColumns, len(wantColumns))
			continue
		}
		for position, want := range wantColumns {
			got := gotColumns[position]
			if got.Name != want.name || got.Desc != want.desc {
				t.Errorf("%s column %d = name:%q desc:%t, want name:%q desc:%t",
					indexName, position, got.Name, got.Desc, want.name, want.desc)
			}
		}
	}
}

func TestAutoMigrateOmitsGroupSignature(t *testing.T) {
	t.Parallel()

	db := openMigratedDatabase(t)
	if db.Migrator().HasColumn("groups", "signature") {
		t.Fatal("fresh groups schema still contains signature")
	}
}

func TestAutoMigrateAllowsDuplicateChannelTargets(t *testing.T) {
	t.Parallel()

	db := openMigratedDatabase(t)
	first := models.Group{
		Name:      "group-one",
		ChannelID: "openai_compatible",
		Params:    models.JSON(`{"base_url":"https://same.example.com/v1"}`),
		Models:    models.JSON(`[]`),
		Overrides: models.JSON(`{}`),
		Enabled:   true,
	}
	second := first
	second.Name = "group-two"
	if err := db.Create(&first).Error; err != nil {
		t.Fatalf("create first group: %v", err)
	}
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("create group with duplicate channel target: %v", err)
	}
}

func TestAutoMigrateAllowsUnversionedDatabaseWithExternalTables(t *testing.T) {
	t.Parallel()

	db := openEmptyDatabase(t)

	if err := db.Exec("CREATE TABLE legacy_data (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatalf("create legacy table: %v", err)
	}

	err := storage.AutoMigrate(db)
	if err != nil {
		t.Fatalf("AutoMigrate() error = %v, want success for an unversioned database with external tables", err)
	}
	if !db.Migrator().HasTable("groups") {
		t.Fatal("AutoMigrate() did not create the application schema")
	}
	if !db.Migrator().HasTable("legacy_data") {
		t.Fatal("AutoMigrate() removed the external table")
	}
}

func TestAutoMigrateRejectsFirstInitializationWithExistingGroups(t *testing.T) {
	t.Parallel()

	db := openEmptyDatabase(t)

	if err := db.Exec("CREATE TABLE groups (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatalf("create existing groups table: %v", err)
	}

	err := storage.AutoMigrate(db)
	if err == nil || !strings.Contains(err.Error(), "groups table already exists") {
		t.Fatalf("AutoMigrate() error = %v, want existing groups rejection", err)
	}
	if db.Migrator().HasTable("schema_migrations") {
		t.Fatal("AutoMigrate() created the migration ledger after rejecting initial groups table")
	}
}

func TestAutoMigrateCreatesCriticalUniqueConstraints(t *testing.T) {
	t.Parallel()

	db := openMigratedDatabase(t)

	t.Run("group name", func(t *testing.T) {
		first := models.Group{
			Name:      "group-one",
			ChannelID: "openai_compatible",
			Params:    models.JSON(`{"base_url":"https://one.example.com"}`),
			Models:    models.JSON(`[]`),
			Overrides: models.JSON(`{}`),
		}
		second := first
		second.ID = 0
		second.Params = models.JSON(`{"base_url":"https://two.example.com"}`)

		assertDuplicateRejected(t, db.Create(&first).Error, db.Create(&second).Error)
	})

	t.Run("credential group and fingerprint", func(t *testing.T) {
		group := models.Group{
			Name:      "credential-parent",
			ChannelID: "openai_compatible",
			Params:    models.JSON(`{"base_url":"https://credentials.example.com"}`),
			Models:    models.JSON(`[]`),
		}
		if err := db.Create(&group).Error; err != nil {
			t.Fatalf("create credential parent group: %v", err)
		}
		first := models.Credential{
			GroupID: group.ID, Data: "encrypted-one", Fingerprint: "same-fingerprint",
		}
		second := first
		second.ID = 0
		second.Data = "encrypted-two"
		assertDuplicateRejected(t, db.Create(&first).Error, db.Create(&second).Error)
	})

	t.Run("model price channel and model", func(t *testing.T) {
		first := models.ModelPrice{ChannelID: "openai", ModelID: "same-model"}
		duplicate := first
		otherChannel := first
		otherChannel.ChannelID = "anthropic"
		if err := db.Create(&first).Error; err != nil {
			t.Fatalf("create model price: %v", err)
		}
		if err := db.Create(&otherChannel).Error; err != nil {
			t.Fatalf("create same model for another channel: %v", err)
		}
		if err := db.Create(&duplicate).Error; err == nil {
			t.Fatal("create duplicate channel/model price error = nil, want unique constraint error")
		}
	})

	t.Run("access key hash", func(t *testing.T) {
		first := models.AccessKey{
			Name:      "access-one",
			KeyValue:  "ciphertext-one",
			KeyHash:   "same-access-key-hash",
			KeySuffix: "7f2a",
			Filters:   models.JSON(`{}`),
		}
		second := first
		second.ID = 0
		second.Name = "access-two"
		second.KeyValue = "ciphertext-two"

		assertDuplicateRejected(t, db.Create(&first).Error, db.Create(&second).Error)
	})

	t.Run("usage bucket access group and model", func(t *testing.T) {
		bucket := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
		first := models.UsageStat{
			BucketStartMS: bucket.UnixMilli(),
			AccessKeyID:   101,
			GroupID:       202,
			Model:         "model-a",
		}
		second := first
		second.ID = 0

		assertDuplicateRejected(t, db.Create(&first).Error, db.Create(&second).Error)
	})
}

func TestAutoMigrateCreatesCredentialForeignKeyWithCascade(t *testing.T) {
	t.Parallel()

	db := openMigratedDatabase(t)
	credentialForeignKeys := foreignKeys(t, db, "credentials")
	var groupForeignKey *foreignKey
	for index := range credentialForeignKeys {
		candidate := &credentialForeignKeys[index]
		if candidate.Table == "groups" && candidate.From == "group_id" && candidate.To == "id" {
			groupForeignKey = candidate
			break
		}
	}
	if groupForeignKey == nil || groupForeignKey.OnDelete != "CASCADE" {
		t.Fatalf("credentials foreign keys = %+v, want cascading group_id -> groups.id", credentialForeignKeys)
	}

	group := models.Group{
		Name:      "credential-cascade-parent",
		ChannelID: "openai_compatible",
		Params:    models.JSON(`{"base_url":"https://credential-cascade.example.com"}`),
		Models:    models.JSON(`[]`),
	}
	if err := db.Create(&group).Error; err != nil {
		t.Fatalf("create credential parent group: %v", err)
	}
	credential := models.Credential{
		GroupID: group.ID, Data: "encrypted-data", Fingerprint: "cascade-fingerprint",
	}
	if err := db.Create(&credential).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if err := db.Delete(&group).Error; err != nil {
		t.Fatalf("delete credential parent group: %v", err)
	}
	var count int64
	if err := db.Model(&models.Credential{}).Where("id = ?", credential.ID).Count(&count).Error; err != nil {
		t.Fatalf("count credential after deleting group: %v", err)
	}
	if count != 0 {
		t.Fatalf("credential count after deleting group = %d, want 0", count)
	}
}

// openMigratedDatabase opens an isolated clone of the fully migrated
// PostgreSQL template.
func openMigratedDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	return openTestDatabase(t, pgtest.NewDatabase(t))
}

// openEmptyDatabase opens an isolated empty PostgreSQL database.
func openEmptyDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	return openTestDatabase(t, pgtest.NewEmptyDatabase(t))
}

func openTestDatabase(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	return db
}

type indexKeyColumn struct {
	Name string
	Desc bool
}

// indexKeyColumns lists the key columns of one index in index order.
func indexKeyColumns(t *testing.T, db *gorm.DB, table, indexName string) []indexKeyColumn {
	t.Helper()
	var columns []indexKeyColumn
	if err := db.Raw(`
		SELECT a.attname AS name, (i.indoption[k.position - 1] & 1) = 1 AS "desc"
		FROM pg_index i
		JOIN pg_class ci ON ci.oid = i.indexrelid
		CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, position)
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = ?::regclass AND ci.relname = ? AND k.position <= i.indnkeyatts
		ORDER BY k.position
	`, table, indexName).Scan(&columns).Error; err != nil {
		t.Fatalf("inspect %s columns: %v", indexName, err)
	}
	return columns
}

type foreignKey struct {
	Table    string
	From     string
	To       string
	OnDelete string `gorm:"column:on_delete"`
}

// foreignKeys lists the foreign keys declared on table.
func foreignKeys(t *testing.T, db *gorm.DB, table string) []foreignKey {
	t.Helper()
	var result []foreignKey
	if err := db.Raw(`
		SELECT ccu.table_name AS "table", kcu.column_name AS "from",
			ccu.column_name AS "to", rc.delete_rule AS on_delete
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON kcu.constraint_schema = tc.constraint_schema AND kcu.constraint_name = tc.constraint_name
		JOIN information_schema.constraint_column_usage ccu
			ON ccu.constraint_schema = tc.constraint_schema AND ccu.constraint_name = tc.constraint_name
		JOIN information_schema.referential_constraints rc
			ON rc.constraint_schema = tc.constraint_schema AND rc.constraint_name = tc.constraint_name
		WHERE tc.constraint_type = 'FOREIGN KEY'
			AND tc.table_schema = current_schema() AND tc.table_name = ?
		ORDER BY tc.constraint_name, kcu.ordinal_position
	`, table).Scan(&result).Error; err != nil {
		t.Fatalf("inspect %s foreign keys: %v", table, err)
	}
	return result
}

func assertDuplicateRejected(t *testing.T, firstErr, duplicateErr error) {
	t.Helper()

	if firstErr != nil {
		t.Fatalf("create initial record: %v", firstErr)
	}
	if duplicateErr == nil {
		t.Fatal("create duplicate record error = nil, want unique constraint error")
	}
}
