package migrations_test

import (
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/storage/migrations"
	"gpt-load/internal/storage/models"
)

func TestAutoDecisionAttributionMigrationPreservesHistoricalTotals(t *testing.T) {
	t.Parallel()
	db := openInitialTestDatabase(t)
	for _, up := range []func(*gorm.DB) error{
		migrations.Up0001, migrations.Up0002, migrations.Up0003, migrations.Up0004, migrations.Up0005,
		migrations.Up0006, migrations.Up0007, migrations.Up0008, migrations.Up0009, migrations.Up0010,
		migrations.Up0011, migrations.Up0012, migrations.Up0013, migrations.Up0014, migrations.Up0015,
		migrations.Up0016, migrations.Up0017, migrations.Up0018, migrations.Up0019, migrations.Up0020,
		migrations.Up0021, migrations.Up0022,
	} {
		if err := up(db); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrations.Validate0024(db); err == nil {
		t.Fatal("missing attribution columns accepted")
	}
	if err := db.Exec(`INSERT INTO auto_decision_usage_stats
		(bucket_start_ms, access_key_id, model, estimated_cost_nano_usd, unpriced_request_count, pricing_partial_count)
		VALUES (?, ?, ?, ?, ?, ?)`, int64(3_600_000), uint(7), "legacy-model", int64(84_000), int64(1), int64(0)).Error; err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := migrations.Up0024(db); err != nil {
			t.Fatal(err)
		}
	}
	var historical models.AutoDecisionUsageStat
	if err := db.Where("bucket_start_ms = ? AND access_key_id = ? AND model = ?", 3_600_000, 7, "legacy-model").Take(&historical).Error; err != nil {
		t.Fatal(err)
	}
	if historical.GroupID != 0 || historical.ChannelID != "" || historical.CredentialID != 0 ||
		historical.EstimatedCostNanoUSD != 84_000 || historical.UnpricedRequestCount != 1 {
		t.Fatalf("historical row = %#v", historical)
	}

	rows := []models.AutoDecisionUsageStat{
		{BucketStartMS: 7_200_000, AccessKeyID: 7, GroupID: 1, ChannelID: "jev", CredentialID: 11, Model: "jev-latest", EstimatedCostNanoUSD: 1},
		{BucketStartMS: 7_200_000, AccessKeyID: 7, GroupID: 2, ChannelID: "openrouter", CredentialID: 12, Model: "jev-latest", EstimatedCostNanoUSD: 2},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("route dimensions are not part of the identity: %v", err)
	}
}
