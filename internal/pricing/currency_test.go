package pricing

import (
	"math"
	"testing"
)

func TestCurrencyConversionsRoundHalfUp(t *testing.T) {
	cny, err := NewCurrency(CurrencyCNY, 7_100_000)
	if err != nil {
		t.Fatalf("NewCurrency() error = %v", err)
	}
	tests := []struct {
		name string
		got  func() (int64, bool)
		want int64
	}{
		{name: "one dollar", got: func() (int64, bool) { return cny.FromNanoUSD(1_000_000_000) }, want: 7_100_000_000},
		{name: "usd identity", got: func() (int64, bool) { return USDCurrency.FromNanoUSD(123) }, want: 123},
		{name: "half rounds up", got: func() (int64, bool) {
			half, _ := NewCurrency(CurrencyCNY, 1_500_000)
			return half.FromNanoUSD(1)
		}, want: 2},
		{name: "below half rounds down", got: func() (int64, bool) {
			low, _ := NewCurrency(CurrencyCNY, 1_400_000)
			return low.FromNanoUSD(1)
		}, want: 1},
		{name: "hundred yuan to usd", got: func() (int64, bool) {
			usd, ok := cny.ToNanoUSD(100_000_000_000)
			return int64(usd), ok
		}, want: 14_084_507_042},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.got()
			if !ok || got != tt.want {
				t.Fatalf("got %d, %v; want %d", got, ok, tt.want)
			}
		})
	}
}

func TestCurrencyConversionRejectsOverflowAndNegative(t *testing.T) {
	cny, _ := NewCurrency(CurrencyCNY, 7_100_000)
	if _, ok := cny.FromNanoUSD(NanoUSD(math.MaxInt64)); ok {
		t.Fatal("FromNanoUSD(MaxInt64) succeeded, want overflow")
	}
	if _, ok := cny.FromNanoUSD(-1); ok {
		t.Fatal("FromNanoUSD(-1) succeeded, want rejection")
	}
}

func TestNewCurrencyValidatesCodeAndRate(t *testing.T) {
	for _, tt := range []struct {
		code CurrencyCode
		rate int64
	}{
		{code: "EUR", rate: ExchangeRateScale},
		{code: CurrencyCNY, rate: 0},
		{code: CurrencyUSD, rate: 7_100_000},
	} {
		if _, err := NewCurrency(tt.code, tt.rate); err == nil {
			t.Fatalf("NewCurrency(%s, %d) succeeded, want error", tt.code, tt.rate)
		}
	}
}

func TestParseAndFormatExchangeRate(t *testing.T) {
	rate, err := ParseExchangeRate("7.123456")
	if err != nil || rate != 7_123_456 {
		t.Fatalf("ParseExchangeRate() = %d, %v", rate, err)
	}
	if got := FormatExchangeRate(rate); got != "7.123456" {
		t.Fatalf("FormatExchangeRate() = %q", got)
	}
	for _, value := range []string{"0", "0.000000", "-1", "7.1234567", "", " 7"} {
		if _, err := ParseExchangeRate(value); err == nil {
			t.Fatalf("ParseExchangeRate(%q) succeeded, want error", value)
		}
	}
}

func TestParseAndFormatAmount(t *testing.T) {
	amount, err := ParseAmount("100.5")
	if err != nil || amount != 100_500_000_000 {
		t.Fatalf("ParseAmount() = %d, %v", amount, err)
	}
	if got := FormatAmount(amount); got != "100.5" {
		t.Fatalf("FormatAmount() = %q", got)
	}
}
