package control

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"gorm.io/gorm"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
)

// PriceRuntime owns the current immutable pricing table.
type PriceRuntime struct {
	current atomic.Pointer[pricing.Table]
}

func NewPriceRuntime() *PriceRuntime {
	return &PriceRuntime{}
}

func (runtime *PriceRuntime) Load() *pricing.Table {
	if runtime == nil {
		return nil
	}
	return runtime.current.Load()
}

func (runtime *PriceRuntime) Publish(table *pricing.Table) {
	if runtime == nil || table == nil {
		return
	}
	runtime.current.Store(table)
}

func loadPriceTable(
	ctx context.Context,
	tx *gorm.DB,
) (*pricing.Table, error) {
	var rows []models.ModelPrice
	if err := tx.WithContext(ctx).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load persisted model prices: %w", app_errors.ParseDBError(err))
	}
	rules := make([]pricing.Rule, 0, len(rows))
	for _, row := range rows {
		rule, err := persistedPriceRule(row)
		if err != nil {
			return nil, fmt.Errorf("decode persisted model price: %w", app_errors.ErrInternalServer)
		}
		rules = append(rules, rule)
	}
	var clientRows []models.ClientModelPrice
	if err := tx.WithContext(ctx).Order("id ASC").Find(&clientRows).Error; err != nil {
		return nil, fmt.Errorf("load persisted client model prices: %w", app_errors.ParseDBError(err))
	}
	clientRules := make([]pricing.ClientRule, 0, len(clientRows))
	for _, row := range clientRows {
		rule, err := persistedClientPriceRule(row)
		if err != nil {
			return nil, fmt.Errorf("decode persisted client model price: %w", app_errors.ErrInternalServer)
		}
		clientRules = append(clientRules, rule)
	}
	table, err := pricing.NewTableWithClientRules(rules, clientRules)
	if err != nil {
		return nil, fmt.Errorf("compile persisted model prices: %w", app_errors.ErrInternalServer)
	}
	return table, nil
}

func persistedPriceRule(
	row models.ModelPrice,
) (pricing.Rule, error) {
	identity, err := PriceIdentityForChannelModel(row.ChannelID, row.ModelID)
	if err != nil {
		return pricing.Rule{}, err
	}
	standard, modes, err := persistedPriceSchedules(
		[4]*int64{
			row.InputPriceNanoUSDPerMillionTokens, row.OutputPriceNanoUSDPerMillionTokens,
			row.CacheReadPriceNanoUSDPerMillionTokens, row.CacheWritePriceNanoUSDPerMillionTokens,
		},
		row.ContextPriceTiers, row.ModePriceSchedules,
	)
	if err != nil {
		return pricing.Rule{}, err
	}
	return pricing.Rule{
		Identity:      identity,
		Prices:        standard.Prices,
		ContextTiers:  standard.ContextTiers,
		ModeSchedules: modes,
		IsManual:      row.IsManual,
	}, nil
}

func persistedClientPriceRule(row models.ClientModelPrice) (pricing.ClientRule, error) {
	standard, modes, err := persistedPriceSchedules(
		[4]*int64{
			row.InputPriceNanoUSDPerMillionTokens, row.OutputPriceNanoUSDPerMillionTokens,
			row.CacheReadPriceNanoUSDPerMillionTokens, row.CacheWritePriceNanoUSDPerMillionTokens,
		},
		row.ContextPriceTiers, row.ModePriceSchedules,
	)
	if err != nil {
		return pricing.ClientRule{}, err
	}
	return pricing.ClientRule{
		ClientModel:   row.ClientModel,
		Prices:        standard.Prices,
		ContextTiers:  standard.ContextTiers,
		ModeSchedules: modes,
	}, nil
}

// persistedPriceSchedules decodes the price columns shared by upstream and
// client-model price rows into the standard schedule and the mode schedules;
// base holds input, output, cache read, and cache write.
func persistedPriceSchedules(
	base [4]*int64,
	contextTiers models.JSON,
	modeSchedules models.JSON,
) (pricing.Schedule, map[pricing.Mode]pricing.Schedule, error) {
	standard := pricing.Schedule{Prices: pricing.Prices{
		Input:      priceFromStoragePointer(base[0]),
		Output:     priceFromStoragePointer(base[1]),
		CacheRead:  priceFromStoragePointer(base[2]),
		CacheWrite: priceFromStoragePointer(base[3]),
	}}
	var err error
	standard.ContextTiers, err = decodePersistedContextTiers(contextTiers)
	if err != nil || len(modeSchedules) == 0 {
		return standard, nil, err
	}
	normalizedSchedules, err := models.NormalizeModePriceSchedules(modeSchedules)
	if err != nil {
		return pricing.Schedule{}, nil, err
	}
	var schedules map[string]models.ModePriceSchedule
	if err := json.Unmarshal(normalizedSchedules, &schedules); err != nil {
		return pricing.Schedule{}, nil, err
	}
	modes := make(map[pricing.Mode]pricing.Schedule, len(schedules))
	for mode, schedule := range schedules {
		tiers, err := pricingContextTiersFromStorage(schedule.ContextPriceTiers)
		if err != nil {
			return pricing.Schedule{}, nil, err
		}
		modes[pricing.Mode(mode)] = pricing.Schedule{
			Prices: pricing.Prices{
				Input:      priceFromStoragePointer(schedule.Prices.InputPriceNanoUSDPerMillionTokens),
				Output:     priceFromStoragePointer(schedule.Prices.OutputPriceNanoUSDPerMillionTokens),
				CacheRead:  priceFromStoragePointer(schedule.Prices.CacheReadPriceNanoUSDPerMillionTokens),
				CacheWrite: priceFromStoragePointer(schedule.Prices.CacheWritePriceNanoUSDPerMillionTokens),
			},
			ContextTiers: tiers,
		}
	}
	return standard, modes, nil
}

func decodePersistedContextTiers(raw models.JSON) ([]pricing.ContextTier, error) {
	normalized, err := models.NormalizeContextPriceTiers(raw)
	if err != nil || len(normalized) == 0 {
		return nil, err
	}
	var tiers []models.ContextPriceTier
	if err := json.Unmarshal(normalized, &tiers); err != nil {
		return nil, err
	}
	return pricingContextTiersFromStorage(tiers)
}

func pricingContextTiersFromStorage(tiers []models.ContextPriceTier) ([]pricing.ContextTier, error) {
	result := make([]pricing.ContextTier, 0, len(tiers))
	for _, tier := range tiers {
		result = append(result, pricing.ContextTier{
			InputThresholdTokens: tier.ThresholdTokens,
			Prices: pricing.Prices{
				Input:      priceFromStoragePointer(tier.InputPriceNanoUSDPerMillionTokens),
				Output:     priceFromStoragePointer(tier.OutputPriceNanoUSDPerMillionTokens),
				CacheRead:  priceFromStoragePointer(tier.CacheReadPriceNanoUSDPerMillionTokens),
				CacheWrite: priceFromStoragePointer(tier.CacheWritePriceNanoUSDPerMillionTokens),
			},
		})
	}
	return result, nil
}

func priceFromStoragePointer(value *int64) pricing.Price {
	if value == nil {
		return pricing.Price{}
	}
	return pricing.Price{NanoUSDPerMillion: pricing.NanoUSD(*value), Set: true}
}
