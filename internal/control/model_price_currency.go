package control

import (
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/pricing"
)

// Model prices come from upstreams in USD and the service stores and returns
// them in USD. The HTTP handlers convert at the boundary: responses show the
// prices in the system currency at the current rate, and request prices given
// in the system currency are converted back to USD before the service runs.

// PriceSchedulesDTO is the price shape shared by an upstream model price and a
// client model's unified price.
type PriceSchedulesDTO struct {
	Currency      pricing.CurrencyCode                   `json:"currency"`
	Prices        PriceSlotsDTO                          `json:"prices"`
	ContextTiers  []ModelPriceContextTierDTO             `json:"context_tiers"`
	ModeSchedules map[pricing.Mode]ModelPriceScheduleDTO `json:"mode_schedules"`
}

// inCurrency returns the schedules with every USD price shown in the currency.
func (dto PriceSchedulesDTO) inCurrency(currency pricing.Currency) (PriceSchedulesDTO, error) {
	var err error
	dto.Currency = currency.Code
	if dto.Prices, err = dto.Prices.inCurrency(currency); err != nil {
		return PriceSchedulesDTO{}, err
	}
	if dto.ContextTiers, err = contextTiersInCurrency(dto.ContextTiers, currency); err != nil {
		return PriceSchedulesDTO{}, err
	}
	if dto.ModeSchedules, err = modeSchedulesInCurrency(dto.ModeSchedules, currency); err != nil {
		return PriceSchedulesDTO{}, err
	}
	return dto, nil
}

func (dto ModelPriceDTO) inCurrency(currency pricing.Currency) (ModelPriceDTO, error) {
	var err error
	dto.PriceSchedulesDTO, err = dto.PriceSchedulesDTO.inCurrency(currency)
	return dto, err
}

func modeSchedulesInCurrency(
	schedules map[pricing.Mode]ModelPriceScheduleDTO,
	currency pricing.Currency,
) (map[pricing.Mode]ModelPriceScheduleDTO, error) {
	if schedules == nil {
		return nil, nil
	}
	result := make(map[pricing.Mode]ModelPriceScheduleDTO, len(schedules))
	for mode, schedule := range schedules {
		var err error
		if schedule.Prices, err = schedule.Prices.inCurrency(currency); err != nil {
			return nil, err
		}
		if schedule.ContextTiers, err = contextTiersInCurrency(schedule.ContextTiers, currency); err != nil {
			return nil, err
		}
		result[mode] = schedule
	}
	return result, nil
}

func (slots PriceSlotsDTO) inCurrency(currency pricing.Currency) (PriceSlotsDTO, error) {
	for _, slot := range []**string{&slots.Input, &slots.Output, &slots.CacheRead, &slots.CacheWrite} {
		if *slot == nil {
			continue
		}
		converted, err := usdDecimalToCurrency(currency, **slot)
		if err != nil {
			return PriceSlotsDTO{}, err
		}
		*slot = &converted
	}
	return slots, nil
}

func contextTiersInCurrency(tiers []ModelPriceContextTierDTO, currency pricing.Currency) ([]ModelPriceContextTierDTO, error) {
	if tiers == nil {
		return nil, nil
	}
	result := make([]ModelPriceContextTierDTO, 0, len(tiers))
	for _, tier := range tiers {
		prices, err := tier.Prices.inCurrency(currency)
		if err != nil {
			return nil, err
		}
		tier.Prices = prices
		result = append(result, tier)
	}
	return result, nil
}

// convertToUSD turns every decoded price, given in the currency, into nano USD.
func (request *ModelPriceUpdateRequest) convertToUSD(currency pricing.Currency) error {
	if err := convertDecimalsToUSD(currency, &request.Input, &request.Output, &request.CacheRead, &request.CacheWrite); err != nil {
		return err
	}
	if err := convertTiersToUSD(currency, request.ContextTiers.tiers); err != nil {
		return err
	}
	for mode, schedule := range request.ModeSchedules.schedules {
		prices := &schedule.Prices
		if err := convertDecimalsToUSD(currency, &prices.Input, &prices.Output, &prices.CacheRead, &prices.CacheWrite); err != nil {
			return err
		}
		// 映射值是副本，分段切片也要复制后再改写，避免与请求共享底层数组。
		schedule.ContextTiers.tiers = append([]ModelPriceContextTierRequest(nil), schedule.ContextTiers.tiers...)
		if err := convertTiersToUSD(currency, schedule.ContextTiers.tiers); err != nil {
			return err
		}
		request.ModeSchedules.schedules[mode] = schedule
	}
	return nil
}

func convertTiersToUSD(currency pricing.Currency, tiers []ModelPriceContextTierRequest) error {
	for index := range tiers {
		tier := &tiers[index]
		if err := convertDecimalsToUSD(currency, &tier.Input, &tier.Output, &tier.CacheRead, &tier.CacheWrite); err != nil {
			return err
		}
	}
	return nil
}

func convertDecimalsToUSD(currency pricing.Currency, values ...*nullableDecimal) error {
	for _, value := range values {
		if err := value.convertToUSD(currency); err != nil {
			return err
		}
	}
	return nil
}

func (value *nullableDecimal) convertToUSD(currency pricing.Currency) error {
	if value.nanoUSD == nil {
		return nil
	}
	converted, ok := currency.ToNanoUSD(*value.nanoUSD)
	if !ok {
		return app_errors.ErrValidation
	}
	nanoUSD := int64(converted)
	value.nanoUSD = &nanoUSD
	return nil
}
