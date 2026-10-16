package control

import (
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage/models"
)

// CurrencyResponse is the system currency new requests settle in.
type CurrencyResponse struct {
	Currency     pricing.CurrencyCode `json:"currency"`
	ExchangeRate string               `json:"exchange_rate"`
}

func newCurrencyResponse(currency pricing.Currency) CurrencyResponse {
	return CurrencyResponse{
		Currency:     currency.Code,
		ExchangeRate: pricing.FormatExchangeRate(currency.RateMicros),
	}
}

func (s *Server) handleCurrency(c *gin.Context) {
	currency, err := s.service.currentCurrency()
	if err != nil {
		writeServiceError(c, "currency", err)
		return
	}
	response.SuccessI18n(c, "common.success", newCurrencyResponse(currency))
}

// loadSettingsCurrency resolves the system currency stored in the transaction.
func loadSettingsCurrency(tx *gorm.DB) (pricing.Currency, error) {
	var rows []models.SystemSetting
	if err := tx.Where("key IN ?", []string{state.SettingCurrency, state.SettingExchangeRate}).
		Find(&rows).Error; err != nil {
		return pricing.Currency{}, app_errors.ParseDBError(err)
	}
	settings, err := stateloader.MapSystemSettings(rows)
	if err != nil {
		return pricing.Currency{}, app_errors.ErrValidation
	}
	currency, err := state.ResolveCurrency(settings)
	if err != nil {
		return pricing.Currency{}, app_errors.ErrValidation
	}
	return currency, nil
}

// convertCostLimitRulesCurrency moves every cost limit into the new system
// currency through USD at the old and new rates. Used amounts were settled in
// the old currency, so each rule starts a new revision with nothing used.
func convertCostLimitRulesCurrency(tx *gorm.DB, from, to pricing.Currency) (int, error) {
	var rules []models.AccessKeyCostLimitRule
	if err := tx.Where("currency = ?", string(from.Code)).Order("id ASC").Find(&rules).Error; err != nil {
		return 0, app_errors.ParseDBError(err)
	}
	for _, rule := range rules {
		limit, err := convertLimitNano(rule.LimitNano, from, to)
		if err != nil {
			return 0, fmt.Errorf("convert access key cost limit rule %d: %w", rule.ID, err)
		}
		if rule.RuleRevision == ^uint64(0) {
			return 0, fmt.Errorf("advance access key cost limit rule %d revision: %w", rule.ID, app_errors.ErrInternalServer)
		}
		if err := tx.Model(&models.AccessKeyCostLimitRule{}).
			Where("id = ? AND rule_revision = ?", rule.ID, rule.RuleRevision).
			Updates(map[string]any{
				"limit_nano":    limit,
				"currency":      string(to.Code),
				"rule_revision": rule.RuleRevision + 1,
			}).Error; err != nil {
			return 0, app_errors.ParseDBError(err)
		}
		if err := resetAccessKeyCostLimitRuleState(tx, rule.ID, rule.RuleRevision, rule.RuleRevision+1); err != nil {
			return 0, err
		}
	}
	return len(rules), nil
}

// convertLimitNano converts a limit between currencies through USD; a limit
// stays positive so the rule keeps blocking rather than becoming invalid.
func convertLimitNano(value int64, from, to pricing.Currency) (int64, error) {
	usd, ok := from.ToNanoUSD(value)
	if !ok {
		return 0, app_errors.ErrValidation
	}
	converted, ok := to.FromNanoUSD(usd)
	if !ok {
		return 0, app_errors.ErrValidation
	}
	return max(converted, 1), nil
}

func logCurrencySwitch(from, to pricing.Currency, rules int) {
	logrus.WithFields(logrus.Fields{
		"event":       "system_currency.switched",
		"from":        from.Code,
		"from_rate":   pricing.FormatExchangeRate(from.RateMicros),
		"to":          to.Code,
		"to_rate":     pricing.FormatExchangeRate(to.RateMicros),
		"limit_rules": rules,
	}).Info("system currency switched; access key cost limits converted and reset")
}

// usdDecimalToCurrency converts a USD decimal string to the currency's decimal string.
func usdDecimalToCurrency(currency pricing.Currency, usd string) (string, error) {
	nanoUSD, err := pricing.ParseUSD(usd)
	if err != nil {
		return "", err
	}
	converted, ok := currency.FromNanoUSD(nanoUSD)
	if !ok {
		return "", fmt.Errorf("amount overflows in %s", currency.Code)
	}
	return pricing.FormatAmount(converted), nil
}

// currencyDecimalToUSD converts a decimal string in the currency to USD for storage.
func currencyDecimalToUSD(currency pricing.Currency, amount string) (string, error) {
	nano, err := pricing.ParseAmount(amount)
	if err != nil {
		return "", err
	}
	nanoUSD, ok := currency.ToNanoUSD(nano)
	if !ok {
		return "", fmt.Errorf("amount overflows in USD")
	}
	return pricing.FormatUSD(nanoUSD), nil
}

// currentCurrency is the system currency of the published snapshot.
func (s *Service) currentCurrency() (pricing.Currency, error) {
	snapshot := s.manager.Current()
	if snapshot == nil {
		return pricing.Currency{}, app_errors.ErrInternalServer
	}
	return snapshot.Settings.Currency, nil
}

// settledNanoString converts a stored nano USD amount at a frozen settlement
// rate for display; overflow saturates like settlement itself.
func settledNanoString(settlement pricing.Currency, nanoUSD int64) string {
	return strconv.FormatInt(settlement.Settle(nanoUSD), 10)
}
