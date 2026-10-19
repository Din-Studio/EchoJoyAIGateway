package pricing

import (
	"testing"

	"gpt-load/internal/usage"
)

func mustClientTable(t *testing.T, rules []Rule, clientRules []ClientRule) *Table {
	t.Helper()
	table, err := NewTableWithClientRules(rules, clientRules)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func TestQuoteClientModelUsesOnlyTheClientModelPrice(t *testing.T) {
	identity := Identity{ChannelID: "openai", ModelID: "upstream-model"}
	table := mustClientTable(t,
		[]Rule{{Identity: identity, Prices: Prices{Input: fixedPrice(100)}}},
		[]ClientRule{{ClientModel: "gpt", Prices: Prices{Input: fixedPrice(300)}}},
	)
	result := usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1_000_000}}
	multipliers := PriceMultipliers{Group: DefaultPriceMultiplier, AccessKey: 2_000_000}

	quote, receipt := table.QuoteClientModel("gpt", result, ModeStandard, multipliers.AccessKey)
	if quote.State != CostStatePriced || quote.EstimatedCostNanoUSD != 600 {
		t.Fatalf("client quote = %+v, want priced 600", quote)
	}
	if receipt == nil || receipt.SchemaVersion != ClientModelReceiptSchemaVersion ||
		receipt.Rule != (ReceiptRule{ClientModel: "gpt"}) || *receipt.BaseTotalNanoUSD != 300 {
		t.Fatalf("client receipt = %+v", receipt)
	}
	if err := ValidateReceipt(*receipt); err != nil {
		t.Fatalf("client receipt rejected: %v", err)
	}

	upstream, upstreamReceipt := table.QuoteForModeWithMultipliers(identity, result, ModeStandard, multipliers)
	if upstream.EstimatedCostNanoUSD != 200 || upstreamReceipt.SchemaVersion != UpstreamReceiptSchemaVersion {
		t.Fatalf("upstream quote = %+v / %+v, want 200 at v6", upstream, upstreamReceipt)
	}
}

func TestQuoteClientModelWithoutPriceIsUnpricedEvenWhenUpstreamIsPriced(t *testing.T) {
	identity := Identity{ChannelID: "openai", ModelID: "gpt"}
	table := mustClientTable(t, []Rule{{Identity: identity, Prices: Prices{Input: fixedPrice(100)}}}, nil)
	quote, receipt := table.QuoteClientModel("gpt", usage.Result{
		State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1_000_000},
	}, ModeStandard, DefaultPriceMultiplier)
	if quote.State != CostStateUnpriced || quote.EstimatedCostNanoUSD != 0 || receipt != nil {
		t.Fatalf("missing client price = %+v / %+v, want unpriced without receipt", quote, receipt)
	}
}

func TestQuoteClientModelAppliesContextTiersAndModeSchedules(t *testing.T) {
	table := mustClientTable(t, nil, []ClientRule{{
		ClientModel:  "gpt",
		Prices:       Prices{Input: fixedPrice(100)},
		ContextTiers: []ContextTier{{InputThresholdTokens: 2_000_000, Prices: Prices{Input: fixedPrice(150)}}},
		ModeSchedules: map[Mode]Schedule{
			ModeFast: {Prices: Prices{Input: fixedPrice(600)}},
		},
	}})
	for _, test := range []struct {
		name   string
		tokens int64
		mode   Mode
		want   NanoUSD
	}{
		{"standard", 1_000_000, ModeStandard, 100},
		{"fast", 1_000_000, ModeFast, 600},
		{"context tier wins over mode", 2_000_000, ModeFast, 300},
	} {
		t.Run(test.name, func(t *testing.T) {
			quote, _ := table.QuoteClientModel("gpt", usage.Result{
				State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: test.tokens},
			}, test.mode, DefaultPriceMultiplier)
			if quote.EstimatedCostNanoUSD != test.want {
				t.Fatalf("cost = %d, want %d", quote.EstimatedCostNanoUSD, test.want)
			}
		})
	}
}

func TestNewTableRejectsInvalidClientRules(t *testing.T) {
	for name, rules := range map[string][]ClientRule{
		"empty model":    {{ClientModel: ""}},
		"padded model":   {{ClientModel: " gpt"}},
		"negative price": {{ClientModel: "gpt", Prices: Prices{Input: Price{NanoUSDPerMillion: -1, Set: true}}}},
		"duplicate":      {{ClientModel: "gpt"}, {ClientModel: "gpt"}},
	} {
		if _, err := NewTableWithClientRules(nil, rules); err == nil {
			t.Fatalf("%s: NewTableWithClientRules() accepted invalid rules", name)
		}
	}
}

func TestValidateReceiptSeparatesClientAndUpstreamIdentities(t *testing.T) {
	table := mustClientTable(t, nil, []ClientRule{{ClientModel: "gpt", Prices: Prices{Input: fixedPrice(100)}}})
	_, receipt := table.QuoteClientModel("gpt", usage.Result{
		State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1_000_000},
	}, ModeStandard, DefaultPriceMultiplier)
	for name, mutate := range map[string]func(*Receipt){
		"client receipt with channel":     func(r *Receipt) { r.Rule.ChannelID = "openai" },
		"client receipt with model id":    func(r *Receipt) { r.Rule.ModelID = "gpt" },
		"client receipt without model":    func(r *Receipt) { r.Rule.ClientModel = "" },
		"upstream receipt with client":    func(r *Receipt) { r.SchemaVersion = UpstreamReceiptSchemaVersion },
		"historical receipt with client":  func(r *Receipt) { r.SchemaVersion = 4; r.PriceMultipliers = nil; r.BaseTotalNanoUSD = nil },
		"client receipt with scope key":   func(r *Receipt) { r.Rule.ScopeKey = "provider:openai" },
		"client receipt total tampered":   func(r *Receipt) { r.TotalNanoUSD = 1 },
		"client receipt base tampered":    func(r *Receipt) { *r.BaseTotalNanoUSD = 1 },
		"client receipt rate tampered":    func(r *Receipt) { *r.LineItems[0].RateNanoUSDPerMillion = 1 },
		"client receipt amount tampered":  func(r *Receipt) { *r.LineItems[0].AmountNanoUSD = 1 },
		"client receipt multiplier unset": func(r *Receipt) { r.PriceMultipliers = nil },
	} {
		cloned := *receipt
		cloned.LineItems = cloneReceiptLines(receipt.LineItems)
		base := *receipt.BaseTotalNanoUSD
		cloned.BaseTotalNanoUSD = &base
		multipliers := *receipt.PriceMultipliers
		cloned.PriceMultipliers = &multipliers
		mutate(&cloned)
		if err := ValidateReceipt(cloned); err == nil {
			t.Fatalf("%s: ValidateReceipt() accepted tampered receipt", name)
		}
	}
}

func cloneReceiptLines(lines []ReceiptLine) []ReceiptLine {
	cloned := make([]ReceiptLine, len(lines))
	for index, line := range lines {
		if line.RateNanoUSDPerMillion != nil {
			rate := *line.RateNanoUSDPerMillion
			line.RateNanoUSDPerMillion = &rate
		}
		if line.AmountNanoUSD != nil {
			amount := *line.AmountNanoUSD
			line.AmountNanoUSD = &amount
		}
		cloned[index] = line
	}
	return cloned
}

func TestInvalidMultipliersMakeBothQuotesUnavailable(t *testing.T) {
	identity := Identity{ChannelID: "openai", ModelID: "upstream-model"}
	table := mustClientTable(t,
		[]Rule{{Identity: identity, Prices: Prices{Input: fixedPrice(100)}}},
		[]ClientRule{{ClientModel: "gpt", Prices: Prices{Input: fixedPrice(300)}}},
	)
	invalid := PriceMultipliers{Group: -1, AccessKey: DefaultPriceMultiplier}
	// 只有输出 token 时所有计价项都未定价，报价本身会带一份明细；倍率非法时也不能返回它。
	for _, tokens := range []usage.Tokens{{UncachedInput: 1_000_000}, {Output: 1_000_000}} {
		result := usage.Result{State: usage.StateComplete, Tokens: tokens}
		if quote, receipt := table.QuoteClientModel("gpt", result, ModeStandard, -1); quote.State != CostStateUnpriced || receipt != nil {
			t.Fatalf("client quote of %+v with invalid multiplier = %+v / %+v", tokens, quote, receipt)
		}
		if quote, receipt := table.QuoteForModeWithMultipliers(identity, result, ModeStandard, invalid); quote.State != CostStateUnpriced || receipt != nil {
			t.Fatalf("upstream quote of %+v with invalid multiplier = %+v / %+v", tokens, quote, receipt)
		}
	}
}
