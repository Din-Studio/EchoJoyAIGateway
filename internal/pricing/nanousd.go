package pricing

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// NanoUSD is a USD amount represented in billionths of one USD.
type NanoUSD int64

// Multiplier applies an exact positive ratio to a price component.
type Multiplier struct {
	Numerator   int64 `json:"numerator"`
	Denominator int64 `json:"denominator"`
}

// ParseUSD parses a non-negative decimal USD amount with at most nine decimal places.
func ParseUSD(value string) (NanoUSD, error) {
	amount, err := parseScaledDecimal(value, AmountDecimalPlaces)
	if err != nil {
		return 0, fmt.Errorf("USD price %w", err)
	}
	return NanoUSD(amount), nil
}

// parseScaledDecimal parses a non-negative decimal with at most places
// fractional digits into an integer scaled by 10^places.
func parseScaledDecimal(value string, places int) (int64, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return 0, errors.New("must be a non-empty decimal")
	}

	integer, fraction, hasFraction := strings.Cut(value, ".")
	if integer == "" || (hasFraction && fraction == "") || strings.Contains(fraction, ".") || len(fraction) > places {
		return 0, fmt.Errorf("must have at most %d decimal places", places)
	}
	if !decimalDigits(integer) || (hasFraction && !decimalDigits(fraction)) {
		return 0, errors.New("must be a non-negative decimal")
	}

	amount := new(big.Int)
	if _, ok := amount.SetString(integer+fraction+strings.Repeat("0", places-len(fraction)), 10); !ok || !amount.IsInt64() {
		return 0, errors.New("exceeds the int64 range")
	}
	return amount.Int64(), nil
}

// FormatUSD formats a NanoUSD amount as a plain decimal USD string.
func FormatUSD(value NanoUSD) string {
	return formatScaledDecimal(int64(value), AmountDecimalPlaces)
}

// formatScaledDecimal formats an integer scaled by 10^places as a plain
// decimal string without trailing fractional zeros.
func formatScaledDecimal(value int64, places int) string {
	amount := big.NewInt(value)
	negative := amount.Sign() < 0
	if negative {
		amount.Abs(amount)
	}

	whole, fraction := new(big.Int), new(big.Int)
	whole.QuoRem(amount, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil), fraction)
	prefix := ""
	if negative {
		prefix = "-"
	}
	if fraction.Sign() == 0 {
		return prefix + whole.String()
	}

	fractionText := fraction.String()
	fractionText = strings.Repeat("0", places-len(fractionText)) + fractionText
	return prefix + whole.String() + "." + strings.TrimRight(fractionText, "0")
}

// QuoteComponent returns the rounded nano USD cost of one usage component.
func QuoteComponent(tokens int64, price NanoUSD, multiplier Multiplier) (NanoUSD, bool) {
	if tokens < 0 || price < 0 || multiplier.Numerator <= 0 || multiplier.Denominator <= 0 {
		return 0, false
	}

	numerator := big.NewInt(tokens)
	numerator.Mul(numerator, big.NewInt(int64(price)))
	numerator.Mul(numerator, big.NewInt(multiplier.Numerator))
	denominator := big.NewInt(tokensPerMillion)
	denominator.Mul(denominator, big.NewInt(multiplier.Denominator))

	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	remainder.Lsh(remainder, 1)
	if remainder.Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if quotient.Sign() < 0 || !quotient.IsInt64() {
		return 0, false
	}
	return NanoUSD(quotient.Int64()), true
}

// quoteComponentWithPriceMultipliers 仅用于验证历史 v5 的分项倍率回执。
func quoteComponentWithPriceMultipliers(tokens int64, price NanoUSD, multiplier Multiplier, priceMultipliers PriceMultipliers) (NanoUSD, bool) {
	if tokens < 0 || price < 0 || multiplier.Numerator <= 0 || multiplier.Denominator <= 0 {
		return 0, false
	}
	if !priceMultipliers.Group.Valid() || !priceMultipliers.AccessKey.Valid() {
		return 0, false
	}

	numerator := big.NewInt(tokens)
	numerator.Mul(numerator, big.NewInt(int64(price)))
	numerator.Mul(numerator, big.NewInt(multiplier.Numerator))
	numerator.Mul(numerator, big.NewInt(int64(priceMultipliers.Group)))
	numerator.Mul(numerator, big.NewInt(int64(priceMultipliers.AccessKey)))
	denominator := big.NewInt(tokensPerMillion)
	denominator.Mul(denominator, big.NewInt(multiplier.Denominator))
	denominator.Mul(denominator, big.NewInt(int64(DefaultPriceMultiplier)))
	denominator.Mul(denominator, big.NewInt(int64(DefaultPriceMultiplier)))

	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	remainder.Lsh(remainder, 1)
	if remainder.Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if quotient.Sign() < 0 || !quotient.IsInt64() {
		return 0, false
	}
	return NanoUSD(quotient.Int64()), true
}

// CheckedAddNanoUSD adds non-negative NanoUSD values without overflow.
func CheckedAddNanoUSD(left, right NanoUSD) (NanoUSD, bool) {
	if left < 0 || right < 0 || left > NanoUSD(math.MaxInt64)-right {
		return 0, false
	}
	return left + right, true
}

func decimalDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
