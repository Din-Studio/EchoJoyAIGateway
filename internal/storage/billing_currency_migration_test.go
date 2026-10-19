package storage

import (
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/storage/models"
)

func TestExternalBillingCurrencyMigrationContract(t *testing.T) {
	open := externalMigrationContract(t)
	t.Run("upgrade", func(t *testing.T) { testBillingCurrencyUpgrade(t, open(t)) })
}

func testBillingCurrencyUpgrade(t *testing.T, db *gorm.DB) {
	if err := applyMigrationRegistry(db, migrations[:20]); err != nil {
		t.Fatal(err)
	}
	key := models.AccessKey{Name: "existing", KeyValue: "cipher", KeyHash: "billing-hash", KeySuffix: "cafe", Status: "active", Filters: models.JSON(`{}`)}
	if err := db.Omit("KeyPrefix").Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	var ruleID uint
	if err := db.Raw(`INSERT INTO access_key_cost_limit_rules
		(access_key_id, kind, limit_nano_usd, period_seconds, rule_revision, created_at_ms, updated_at_ms)
		VALUES (?, 'total', 5000000000, 0, 1, 1, 1) RETURNING id`, key.ID).Scan(&ruleID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO access_key_cost_limit_states
		(rule_id, rule_revision, used_nano_usd, window_generation, snapshot_version, updated_at_ms)
		VALUES (?, 1, 700, 0, 1, 1)`, ruleID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO usage_stats
		(bucket_start_ms, access_key_id, channel_id, group_id, credential_id, model, request_count, success_count, estimated_cost_nano_usd)
		VALUES (3600000, ?, 'openai', 1, 1, 'gpt', 1, 1, 42)`, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	legacyRequest := models.RequestLog{ID: "00000000-0000-4000-8000-000000000021", CompletedAtMS: 1000, AccessKeyID: key.ID,
		Protocol: "openai-completions", ClientModel: "gpt", UpstreamModel: "gpt", ModelConsistency: "match",
		Status: "success", StatusCode: 200, DurationMs: 1, UncachedInputTokens: 1, UsageState: "complete",
		CostState: "priced", PricingCompleteness: "complete", EstimatedCostNanoUSD: 42}
	if err := db.Omit("Currency", "ExchangeRateMicros", "EstimatedCostNano", "DecisionCostNano", "UpstreamCostNanoUSD", "UpstreamCostState", "UpstreamPricingCompleteness").Create(&legacyRequest).Error; err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := AutoMigrate(db); err != nil {
			t.Fatal(err)
		}
	}

	var rule models.AccessKeyCostLimitRule
	if err := db.Take(&rule, ruleID).Error; err != nil {
		t.Fatal(err)
	}
	var state models.AccessKeyCostLimitState
	if err := db.Take(&state, ruleID).Error; err != nil {
		t.Fatal(err)
	}
	if rule.Currency != "USD" || rule.LimitNano != 5_000_000_000 || state.UsedNano != 700 {
		t.Fatalf("limit after migration = %+v / %+v, want USD amounts kept", rule, state)
	}
	var request models.RequestLog
	if err := db.Take(&request, "id = ?", legacyRequest.ID).Error; err != nil {
		t.Fatal(err)
	}
	if request.Currency != "USD" || request.ExchangeRateMicros != 1_000_000 || request.EstimatedCostNano != 42 {
		t.Fatalf("request log after migration = %+v, want USD backfill at rate 1", request)
	}
	var stat models.UsageStat
	if err := db.Where("access_key_id = ?", key.ID).Take(&stat).Error; err != nil {
		t.Fatal(err)
	}
	if stat.Currency != "USD" || stat.EstimatedCostNano != 42 {
		t.Fatalf("usage stat after migration = %+v, want USD backfill", stat)
	}
	// 同一小时、同一身份的不同币种必须是两行汇总。
	cny := stat
	cny.ID, cny.Currency, cny.EstimatedCostNano = 0, "CNY", 300
	if err := db.Create(&cny).Error; err != nil {
		t.Fatalf("second currency in the same usage bucket rejected: %v", err)
	}
}
