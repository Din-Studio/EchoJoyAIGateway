package requestlog

import (
	"context"
	"testing"
	"time"

	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
)

func TestQueryAccessKeyTotalCostUsesRetainedScopedAggregates(t *testing.T) {
	db := openRequestLogQueryDB(t)
	service := newRequestLogTestService(db)
	reader := service
	settled := func(row models.UsageStat, accessKeyID uint, currency string, costNano int64) models.UsageStat {
		row.AccessKeyID, row.Currency, row.EstimatedCostNano = accessKeyID, currency, costNano
		return row
	}
	old := settled(usageStat(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), 7, "model", 10), 1, "USD", 1_000_000_000)
	recent := settled(usageStat(time.Now().UTC().Truncate(time.Hour), 7, "model", 20), 1, "USD", 2_000_000_000)
	other := settled(old, 2, "USD", 50_000_000_000)
	other.EstimatedCostNanoUSD = 50_000_000_000
	// 切换币种前后的费用分属不同币种，只累计查询币种的部分。
	yuan := settled(usageStat(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), 7, "model", 10), 1, "CNY", 7_000_000_000)
	createUsageStats(t, db, old, recent, other, yuan)
	if err := db.Create(&models.AutoDecisionUsageStat{
		BucketStartMS: old.BucketStartMS, AccessKeyID: 1, GroupID: 7, Model: "decision", Currency: "USD",
		EstimatedCostNanoUSD: 500_000_000, EstimatedCostNano: 500_000_000,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id       uint
		currency pricing.CurrencyCode
		want     int64
	}{{1, "USD", 3_500_000_000}, {1, "CNY", 7_000_000_000}, {2, "USD", 50_000_000_000}, {3, "USD", 0}} {
		got, err := reader.QueryAccessKeyTotalCost(context.Background(), test.id, test.currency)
		if err != nil || got != test.want {
			t.Fatalf("key %d in %s: got %d, %v; want %d", test.id, test.currency, got, err, test.want)
		}
	}
	if _, err := reader.QueryAccessKeyTotalCost(context.Background(), 0, "USD"); err == nil {
		t.Fatal("zero key scope accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.QueryAccessKeyTotalCost(ctx, 1, "USD"); err == nil {
		t.Fatal("canceled query succeeded")
	}
}
