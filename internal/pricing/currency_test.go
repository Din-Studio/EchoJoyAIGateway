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

// 价格以美元存储：在系统币种下输入的价格读回来应与输入一致，原样再保存时存储值不变。
func TestPriceFromNanoUSDReadsBackEnteredPrices(t *testing.T) {
	for _, rateMicros := range []int64{7_300_000, 7_100_000, 16_250_000_000, 790_000} {
		currency, err := NewCurrency(CurrencyCNY, rateMicros)
		if err != nil {
			t.Fatal(err)
		}
		for _, entered := range []int64{
			30_000_000_000_000, 60_000_000_000_000, 7_300_000_000, 1_234_500_000, 15_000_000, 1,
		} {
			stored, ok := currency.ToNanoUSD(entered)
			if !ok {
				t.Fatalf("rate %d: ToNanoUSD(%d) overflowed", rateMicros, entered)
			}
			shown, ok := currency.PriceFromNanoUSD(stored)
			if !ok {
				t.Fatalf("rate %d: PriceFromNanoUSD(%d) overflowed", rateMicros, stored)
			}
			if again, _ := currency.ToNanoUSD(shown); again != stored {
				t.Fatalf("rate %d: shown %d stores %d, want unchanged %d", rateMicros, shown, again, stored)
			}
			// 汇率 ≥ 1 时输入值本身就能换算回同一个 nano USD，读回的小数位不会比输入多。
			if rateMicros >= ExchangeRateScale && decimalPlaces(shown) > decimalPlaces(entered) {
				t.Fatalf("rate %d: entered %s, shown %s", rateMicros, FormatAmount(entered), FormatAmount(shown))
			}
		}
	}
	cny, _ := NewCurrency(CurrencyCNY, 7_300_000)
	if shown, _ := cny.PriceFromNanoUSD(4_109_589_041_096); shown != 30_000_000_000_000 {
		t.Fatalf("¥30000 stored as nano USD reads back as %s", FormatAmount(shown))
	}
	if shown, _ := USDCurrency.PriceFromNanoUSD(4_109_589_041_096); shown != 4_109_589_041_096 {
		t.Fatalf("USD price changed to %d", shown)
	}
}

func decimalPlaces(nano int64) int {
	places := AmountDecimalPlaces
	for places > 0 && nano%10 == 0 {
		nano /= 10
		places--
	}
	return places
}
