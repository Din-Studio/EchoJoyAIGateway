package storage

import (
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/storage/models"
)

func TestExternalClientModelPriceMigrationContract(t *testing.T) {
	open := externalMigrationContract(t)
	t.Run("upgrade", func(t *testing.T) { testClientModelPriceUpgrade(t, open(t)) })
}

func testClientModelPriceUpgrade(t *testing.T, db *gorm.DB) {
	if err := applyMigrationRegistry(db, migrations[:21]); err != nil {
		t.Fatal(err)
	}
	key := models.AccessKey{Name: "existing", KeyValue: "cipher", KeyHash: "client-price-hash", KeySuffix: "cafe", Status: "active", Filters: models.JSON(`{}`)}
	if err := db.Omit("KeyPrefix", "ConcurrencyLimit").Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	legacyRequest := models.RequestLog{ID: "00000000-0000-4000-8000-000000000022", CompletedAtMS: 1000, AccessKeyID: key.ID,
		Protocol: "openai-completions", ClientModel: "gpt", UpstreamModel: "gpt", ModelConsistency: "match",
		Status: "success", StatusCode: 200, DurationMs: 1, UncachedInputTokens: 1, UsageState: "complete",
		CostState: "priced", PricingCompleteness: "complete", EstimatedCostNanoUSD: 42, EstimatedCostNano: 42,
		Currency: "USD", ExchangeRateMicros: 1_000_000}
	if err := db.Omit("RequestAudit", "AuditModel", "AuditGroupID", "AuditChannelID", "AuditCredentialID", "AuditCostNanoUSD", "AuditCostNano", "AuditPricingCompleteness", "UpstreamCostNanoUSD", "UpstreamCostState", "UpstreamPricingCompleteness", "DecisionGroupID", "DecisionChannelID", "DecisionCredentialID").Create(&legacyRequest).Error; err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := AutoMigrate(db); err != nil {
			t.Fatal(err)
		}
	}

	var request models.RequestLog
	if err := db.Take(&request, "id = ?", legacyRequest.ID).Error; err != nil {
		t.Fatal(err)
	}
	if request.UpstreamCostNanoUSD != 42 || request.UpstreamCostState != "priced" || request.UpstreamPricingCompleteness != "complete" {
		t.Fatalf("request log after migration = %+v, want upstream cost backfilled from the existing cost", request)
	}
	// 上游费用与用量状态的组合由约束保证。
	if err := db.Model(&models.RequestLog{}).Where("id = ?", legacyRequest.ID).
		Update("upstream_cost_state", "not_applicable").Error; err == nil {
		t.Fatal("upstream cost state accepted a combination the usage state forbids")
	}

	input := int64(100)
	price := models.ClientModelPrice{ClientModel: "gpt", InputPriceNanoUSDPerMillionTokens: &input}
	if err := db.Create(&price).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ClientModelPrice{ClientModel: "gpt"}).Error; err == nil {
		t.Fatal("client model prices accepted a duplicate client model")
	}
	negative := int64(-1)
	if err := db.Create(&models.ClientModelPrice{ClientModel: "other", OutputPriceNanoUSDPerMillionTokens: &negative}).Error; err == nil {
		t.Fatal("client model prices accepted a negative price")
	}
}
