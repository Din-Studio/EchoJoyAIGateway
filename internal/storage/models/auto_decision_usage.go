package models

// AutoDecisionUsageStat is a cost component, never an extra client request.
type AutoDecisionUsageStat struct {
	BucketStartMS        int64  `gorm:"primaryKey;not null"`
	AccessKeyID          uint   `gorm:"primaryKey;not null"`
	GroupID              uint   `gorm:"primaryKey;not null;default:0"`
	ChannelID            string `gorm:"type:varchar(64);primaryKey;not null;default:''"`
	CredentialID         uint   `gorm:"primaryKey;not null;default:0"`
	Model                string `gorm:"type:varchar(512);primaryKey;not null"`
	Currency             string `gorm:"type:varchar(3);primaryKey;not null;default:'USD'"`
	EstimatedCostNanoUSD int64  `gorm:"not null;default:0;check:chk_auto_decision_usage_cost,estimated_cost_nano_usd >= 0"`
	EstimatedCostNano    int64  `gorm:"column:estimated_cost_nano;not null;default:0"`
	UnpricedRequestCount int64  `gorm:"not null;default:0;check:chk_auto_decision_usage_unpriced,unpriced_request_count >= 0"`
	PricingPartialCount  int64  `gorm:"not null;default:0;check:chk_auto_decision_usage_partial,pricing_partial_count >= 0"`
}
