package pricing

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// CurrencyCode is the ISO 4217 code of a settlement currency.
type CurrencyCode string

const (
	CurrencyUSD CurrencyCode = "USD"
	CurrencyCNY CurrencyCode = "CNY"

	// ExchangeRateScale is the fixed-point scale of RateMicros.
	ExchangeRateScale int64 = 1_000_000
	// ExchangeRateDecimalPlaces is the most fractional digits a configured rate may have.
	ExchangeRateDecimalPlaces = 6
	// AmountDecimalPlaces is the fractional precision of every nano amount.
	AmountDecimalPlaces = 9
)

// SupportedCurrencyCodes lists the currencies the gateway can settle in.
var SupportedCurrencyCodes = []CurrencyCode{CurrencyUSD, CurrencyCNY}

// Currency is a settlement currency and its rate against USD:
// 1 USD = RateMicros / ExchangeRateScale units of Code.
type Currency struct {
	Code       CurrencyCode
	RateMicros int64
}

// USDCurrency is the identity currency every upstream price is quoted in.
var USDCurrency = Currency{Code: CurrencyUSD, RateMicros: ExchangeRateScale}

// ParseCurrencyCode accepts only supported currency codes.
func ParseCurrencyCode(value string) (CurrencyCode, error) {
	for _, code := range SupportedCurrencyCodes {
		if string(code) == value {
			return code, nil
		}
	}
	return "", fmt.Errorf("unsupported currency %q", value)
}

// NewCurrency validates a currency and its rate. USD always has rate 1.
func NewCurrency(code CurrencyCode, rateMicros int64) (Currency, error) {
	if _, err := ParseCurrencyCode(string(code)); err != nil {
		return Currency{}, err
	}
	if rateMicros <= 0 {
		return Currency{}, errors.New("exchange rate must be greater than 0")
	}
	if code == CurrencyUSD && rateMicros != ExchangeRateScale {
		return Currency{}, errors.New("USD exchange rate must be 1")
	}
	return Currency{Code: code, RateMicros: rateMicros}, nil
}

// Valid reports whether the currency could have been built by NewCurrency.
func (currency Currency) Valid() bool {
	_, err := NewCurrency(currency.Code, currency.RateMicros)
	return err == nil
}

// FromNanoUSD converts nano USD to nano units of the currency, rounding half up.
func (currency Currency) FromNanoUSD(value NanoUSD) (int64, bool) {
	return scaleRoundHalfUp(int64(value), currency.RateMicros, ExchangeRateScale)
}

// Settle converts a nano USD amount to nano units of the currency for
// settlement. Overflow saturates so cost limits fail closed.
func (currency Currency) Settle(nanoUSD int64) int64 {
	converted, ok := currency.FromNanoUSD(NanoUSD(nanoUSD))
	if !ok {
		return math.MaxInt64
	}
	return converted
}

// ToNanoUSD converts nano units of the currency to nano USD, rounding half up.
func (currency Currency) ToNanoUSD(value int64) (NanoUSD, bool) {
	converted, ok := scaleRoundHalfUp(value, ExchangeRateScale, currency.RateMicros)
	return NanoUSD(converted), ok
}

// ParseExchangeRate parses a positive decimal rate with at most six decimal places.
func ParseExchangeRate(value string) (int64, error) {
	rate, err := parseScaledDecimal(value, ExchangeRateDecimalPlaces)
	if err != nil {
		return 0, fmt.Errorf("exchange rate %w", err)
	}
	if rate <= 0 {
		return 0, errors.New("exchange rate must be greater than 0")
	}
	return rate, nil
}

// FormatExchangeRate formats a fixed-point rate as a plain decimal string.
func FormatExchangeRate(rateMicros int64) string {
	return formatScaledDecimal(rateMicros, ExchangeRateDecimalPlaces)
}

// ParseAmount parses a non-negative decimal amount with at most nine decimal
// places into nano units of whatever currency it is denominated in.
func ParseAmount(value string) (int64, error) {
	amount, err := parseScaledDecimal(value, AmountDecimalPlaces)
	if err != nil {
		return 0, fmt.Errorf("amount %w", err)
	}
	return amount, nil
}

// FormatAmount formats nano units of a currency as a plain decimal string.
func FormatAmount(value int64) string {
	return formatScaledDecimal(value, AmountDecimalPlaces)
}

// scaleRoundHalfUp returns value × numerator ÷ denominator for a non-negative
// value, rounded half up, or false when the inputs are invalid or overflow.
func scaleRoundHalfUp(value, numerator, denominator int64) (int64, bool) {
	if value < 0 || numerator <= 0 || denominator <= 0 {
		return 0, false
	}
	product := new(big.Int).Mul(big.NewInt(value), big.NewInt(numerator))
	quotient, remainder := new(big.Int).QuoRem(product, big.NewInt(denominator), new(big.Int))
	if remainder.Lsh(remainder, 1).Cmp(big.NewInt(denominator)) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, false
	}
	return quotient.Int64(), true
}
