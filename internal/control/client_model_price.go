package control

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
)

// ClientModelPriceDTO is the unified outward price of one client model. Like
// ModelPriceDTO it is stored in USD and shown in the system currency.
type ClientModelPriceDTO struct {
	ClientModel string `json:"client_model"`
	PriceSchedulesDTO
	UpdatedAtMS int64 `json:"updated_at_ms"`
}

// ClientModelPriceRequest creates or replaces the unified price of a client
// model. The write transaction converts system-currency amounts to USD. Unlike an upstream price it cannot be
// marked unpriced: at least one base price is required.
type ClientModelPriceRequest struct {
	ClientModel string `json:"client_model"`
	ModelPriceUpdateRequest
}

func (request ClientModelPriceRequest) validate() error {
	if request.ConfirmUnpriced {
		return fmt.Errorf("client model prices cannot be marked unpriced: %w", app_errors.ErrValidation)
	}
	if err := request.ModelPriceUpdateRequest.validate(); err != nil {
		return err
	}
	if request.Input.nanoUSD == nil && request.Output.nanoUSD == nil &&
		request.CacheRead.nanoUSD == nil && request.CacheWrite.nanoUSD == nil {
		return fmt.Errorf("client model price requires a base price: %w", app_errors.ErrValidation)
	}
	return nil
}

func (dto ClientModelPriceDTO) inCurrency(currency pricing.Currency) (ClientModelPriceDTO, error) {
	var err error
	dto.PriceSchedulesDTO, err = dto.PriceSchedulesDTO.inCurrency(currency)
	return dto, err
}

func projectClientModelPrice(row models.ClientModelPrice) (ClientModelPriceDTO, error) {
	rule, err := persistedClientPriceRule(row)
	if err != nil {
		return ClientModelPriceDTO{}, fmt.Errorf("decode persisted client model price: %w", app_errors.ErrInternalServer)
	}
	return ClientModelPriceDTO{
		ClientModel: row.ClientModel,
		PriceSchedulesDTO: PriceSchedulesDTO{
			Currency:      pricing.CurrencyUSD,
			Prices:        projectPriceSlots(rule.Prices),
			ContextTiers:  projectContextPriceTiers(rule.ContextTiers),
			ModeSchedules: projectModeSchedules(rule.ModeSchedules),
		},
		UpdatedAtMS: row.UpdatedAtMS,
	}, nil
}

// PutClientModelPrice creates or replaces one client model's unified price and
// publishes the new pricing table. Peers reload it through the configuration
// revision that the control transaction advances.
func (s *Service) PutClientModelPrice(ctx context.Context, request ClientModelPriceRequest) (ClientModelPriceDTO, error) {
	if err := request.validate(); err != nil {
		return ClientModelPriceDTO{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.enforceOperationRecoveryBarrierLocked(ctx, 0); err != nil {
		return ClientModelPriceDTO{}, err
	}
	var result ClientModelPriceDTO
	err := s.withControlTransaction(ctx, func(tx *gorm.DB) error {
		currency, err := loadSettingsCurrency(tx)
		if err != nil {
			return err
		}
		if err := request.convertToUSD(currency); err != nil {
			return err
		}
		contextTiers, err := buildContextPriceTiersJSON(request.ContextTiers.tiers)
		if err != nil {
			return err
		}
		modeSchedules, err := buildModePriceSchedulesJSON(request.ModeSchedules.schedules)
		if err != nil {
			return err
		}
		desired := models.ClientModelPrice{
			ClientModel:                            request.ClientModel,
			InputPriceNanoUSDPerMillionTokens:      cloneModelPriceValue(request.Input.nanoUSD),
			OutputPriceNanoUSDPerMillionTokens:     cloneModelPriceValue(request.Output.nanoUSD),
			CacheReadPriceNanoUSDPerMillionTokens:  cloneModelPriceValue(request.CacheRead.nanoUSD),
			CacheWritePriceNanoUSDPerMillionTokens: cloneModelPriceValue(request.CacheWrite.nanoUSD),
			ContextPriceTiers:                      contextTiers,
			ModePriceSchedules:                     modeSchedules,
		}
		rule, err := persistedClientPriceRule(desired)
		if err != nil {
			return fmt.Errorf("invalid client model price: %w", app_errors.ErrValidation)
		}
		if _, err := pricing.NewTableWithClientRules(nil, []pricing.ClientRule{rule}); err != nil {
			return fmt.Errorf("invalid client model price: %w", app_errors.ErrValidation)
		}
		var existing []models.ClientModelPrice
		if err := tx.Where("client_model = ?", desired.ClientModel).Limit(1).Find(&existing).Error; err != nil {
			return fmt.Errorf("load client model price: %w", app_errors.ParseDBError(err))
		}
		if len(existing) == 1 {
			desired.ID, desired.CreatedAtMS = existing[0].ID, existing[0].CreatedAtMS
		}
		if err := tx.Save(&desired).Error; err != nil {
			return fmt.Errorf("save client model price: %w", app_errors.ParseDBError(err))
		}
		result, err = projectClientModelPrice(desired)
		if err != nil {
			return err
		}
		if currency != pricing.USDCurrency {
			result, err = result.inCurrency(currency)
			if err != nil {
				return app_errors.ErrValidation
			}
		}
		_, err = loadPriceTable(ctx, tx)
		return err
	})
	if err != nil {
		return ClientModelPriceDTO{}, err
	}
	if err := s.publishCommittedPriceTable(ctx); err != nil {
		return ClientModelPriceDTO{}, err
	}
	return result, nil
}

func (s *Server) handlePutClientModelPrice(c *gin.Context) {
	const operation = "put_client_model_price"
	if !modelPriceMutationQueryIsEmpty(c, operation) {
		return
	}
	var request ClientModelPriceRequest
	if err := bindStrictJSON(c, &request); err != nil {
		writeServiceError(c, operation, mapControlJSONError(err))
		return
	}
	setMutationResourceLocator(c, "client-model-price:"+request.ClientModel)
	result, err := s.service.PutClientModelPrice(c.Request.Context(), request)
	if err != nil {
		writeServiceError(c, operation, err)
		return
	}
	response.SuccessI18n(c, "common.success", result)
}
