package requestlog

import (
	"context"
	"fmt"
	"math"

	"gorm.io/gorm"

	"gpt-load/internal/platform/epochms"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/dbtx"
)

// QueryAccessKeyTotalCost 从长期保留的小时汇总读取累计成本，不受明细留存或周期额度重置影响。
// 只累计以 currency 结算的费用，与用量报表口径一致；切换币种前的费用不计入。
func (service *Service) QueryAccessKeyTotalCost(
	ctx context.Context,
	accessKeyID uint,
	currency pricing.CurrencyCode,
) (int64, error) {
	if service == nil || service.db == nil || accessKeyID == 0 {
		return 0, fmt.Errorf("query access key total cost: invalid database or key scope")
	}
	var cost int64
	err := dbtx.Run(ctx, service.db, dbtx.Options{
		Mode: dbtx.ReadSnapshot, CleanupTimeout: usageRollbackTimeout, Operation: "access key total cost read",
	}, func(tx *gorm.DB) error {
		scope := usageStatScope(tx, UsageQuery{ToMS: math.MaxInt64, AccessKeyID: &accessKeyID})
		if err := validateUsageIntegrity(scope, epochms.MillisecondsPerHour); err != nil {
			return err
		}
		summary, err := queryUsageSummary(scope.Session(&gorm.Session{}), currency)
		if err != nil {
			return err
		}
		cost = summary.EstimatedCostNano
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("query access key total cost: %w", err)
	}
	return cost, nil
}
