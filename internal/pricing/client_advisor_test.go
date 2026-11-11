package pricing

import (
	"gpt-load/internal/usage"
	"testing"
)

func TestClientAdvisorPricesAndVersions(t *testing.T) {
	price := func(n NanoUSD) Price { return Price{NanoUSDPerMillion: n, Set: true} }
	clients := []ClientRule{
		{ClientModel: "executor", Prices: Prices{Input: price(1_000_000), Output: price(1_000_000)}},
		{ClientModel: "advisor-public", Prices: Prices{Input: price(2_000_000), Output: price(3_000_000)},
			ContextTiers: []ContextTier{{InputThresholdTokens: 100_000, Prices: Prices{Input: price(20_000_000), Output: price(30_000_000)}}}},
	}
	table, err := NewTableWithClientRules([]Rule{{Identity: Identity{ChannelID: "anthropic", ModelID: "advisor-public"}, Prices: Prices{Input: price(999_000_000), Output: price(999_000_000)}}}, clients)
	if err != nil {
		t.Fatal(err)
	}
	calls := []usage.ModelTokens{{Model: "advisor-public", Tokens: usage.Tokens{UncachedInput: 60_000, Output: 100}}, {Model: "advisor-public", Tokens: usage.Tokens{UncachedInput: 60_000, Output: 100}}, {Model: "advisor-public", Tokens: usage.Tokens{UncachedInput: 120_000, Output: 100}}}
	result := usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 100, Output: 10}, Advisor: &usage.AdvisorUsage{Calls: calls}}
	multiplier, _ := ParsePriceMultiplier("3")
	quote, receipt := table.QuoteClientModel("executor", result, ModeFast, multiplier)
	if quote.EstimatedCostNanoUSD != 7_931_130 || quote.Completeness != CompletenessComplete || receipt == nil || receipt.SchemaVersion != ClientAdvisorReceiptSchemaVersion {
		t.Fatalf("quote=%+v receipt=%+v", quote, receipt)
	}
	if receipt.PriceMultipliers.Group != DefaultPriceMultiplier || receipt.Rule.ClientModel != "executor" || len(receipt.LineItems) != 6 {
		t.Fatalf("receipt=%+v", receipt)
	}
	if err := ValidateReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
	for _, line := range receipt.LineItems {
		if line.ModelID != "" && line.ModelID != "advisor-public" {
			t.Fatal(line)
		}
	}

	// Removing the unified advisor price never falls back to the matching channel price.
	delete(table.clientRules, "advisor-public")
	quote, receipt = table.QuoteClientModel("executor", result, ModeStandard, DefaultPriceMultiplier)
	if quote.EstimatedCostNanoUSD != 110 || quote.Completeness != CompletenessPartial || ValidateReceipt(*receipt) != nil {
		t.Fatalf("missing client price: %+v %+v", quote, receipt)
	}
	for _, line := range receipt.LineItems {
		if line.ModelID != "" && line.State != ReceiptLineUnpriced {
			t.Fatal(line)
		}
	}
	result.Advisor = nil
	_, receipt = table.QuoteClientModel("executor", result, ModeStandard, DefaultPriceMultiplier)
	if receipt.SchemaVersion != ClientModelReceiptSchemaVersion {
		t.Fatal(receipt)
	}
	// Unsupported detail remains a diagnostic; known token-price completeness is unchanged.
	result.Diagnostics.Add(usage.DiagnosticUnsupportedBillableDetail)
	quote, _ = table.QuoteClientModel("executor", result, ModeStandard, DefaultPriceMultiplier)
	if quote.Completeness != CompletenessComplete {
		t.Fatal(quote)
	}
}

func TestAdvisorReceiptIdentitySeparation(t *testing.T) {
	table := mustTable(t, Rule{Identity: advisorExecutor, Prices: Prices{Input: fixedPrice(1), Output: fixedPrice(1)}})
	_, receipt := table.QuoteForModeWithMultipliers(advisorExecutor, advisorResult(), ModeStandard, advisorDefaults)
	if receipt == nil || ValidateReceipt(*receipt) != nil {
		t.Fatal(receipt)
	}
	for _, version := range []int{6, 7, 9} {
		clone := *receipt
		clone.SchemaVersion = version
		if ValidateReceipt(clone) == nil {
			t.Fatalf("upstream receipt accepted as v%d", version)
		}
	}
}
