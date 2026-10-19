package models

import "gorm.io/gorm"

// ClientModelPrice stores the unified outward price of one client model. It
// applies across every Group and channel that exposes the model; a missing
// row means the model is not yet priced.
type ClientModelPrice struct {
	ID                                     uint   `gorm:"primaryKey;autoIncrement"`
	ClientModel                            string `gorm:"type:varchar(255);not null;uniqueIndex:idx_client_model_prices_client_model"`
	InputPriceNanoUSDPerMillionTokens      *int64 `gorm:"column:input_price_nano_usd_per_million_tokens;check:chk_client_model_price_input_nano,input_price_nano_usd_per_million_tokens IS NULL OR input_price_nano_usd_per_million_tokens >= 0"`
	OutputPriceNanoUSDPerMillionTokens     *int64 `gorm:"column:output_price_nano_usd_per_million_tokens;check:chk_client_model_price_output_nano,output_price_nano_usd_per_million_tokens IS NULL OR output_price_nano_usd_per_million_tokens >= 0"`
	CacheReadPriceNanoUSDPerMillionTokens  *int64 `gorm:"column:cache_read_price_nano_usd_per_million_tokens;check:chk_client_model_price_cache_read_nano,cache_read_price_nano_usd_per_million_tokens IS NULL OR cache_read_price_nano_usd_per_million_tokens >= 0"`
	CacheWritePriceNanoUSDPerMillionTokens *int64 `gorm:"column:cache_write_price_nano_usd_per_million_tokens;check:chk_client_model_price_cache_write_nano,cache_write_price_nano_usd_per_million_tokens IS NULL OR cache_write_price_nano_usd_per_million_tokens >= 0"`
	ContextPriceTiers                      JSON   `gorm:"type:json"`
	ModePriceSchedules                     JSON   `gorm:"column:mode_price_schedules;type:json"`
	CreatedAtMS                            int64  `gorm:"column:created_at_ms;not null;autoCreateTime:milli;check:chk_client_model_price_created_at,created_at_ms >= 0"`
	UpdatedAtMS                            int64  `gorm:"column:updated_at_ms;not null;autoUpdateTime:milli;check:chk_client_model_price_updated_at,updated_at_ms >= 0"`
}

func (ClientModelPrice) TableName() string { return "client_model_prices" }

// BeforeSave normalizes the schedule JSON with the same rules as ModelPrice.
func (price *ClientModelPrice) BeforeSave(_ *gorm.DB) error {
	normalized, err := NormalizeContextPriceTiers(price.ContextPriceTiers)
	if err != nil {
		return err
	}
	price.ContextPriceTiers = normalized
	modeSchedules, err := NormalizeModePriceSchedules(price.ModePriceSchedules)
	if err != nil {
		return err
	}
	price.ModePriceSchedules = modeSchedules
	return nil
}
