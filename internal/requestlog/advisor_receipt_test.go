package requestlog

import (
	"gpt-load/internal/platform/redact"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/usage"
	"testing"
)

func TestAdvisorReceiptPostgresRoundTrip(t *testing.T) {
	db := openRequestLogQueryDB(t)
	for _, advisor := range []bool{false, true} {
		id := "00000000-0000-4000-8000-000000009040"
		if advisor {
			id = "00000000-0000-4000-8000-000000009041"
		}
		event := channelScopedEvent(t, id)
		event.Currency = pricing.Currency{Code: pricing.CurrencyCNY, RateMicros: 7_000_000}
		prices := pricing.Prices{Input: pricing.Price{NanoUSDPerMillion: 1_000_000, Set: true}}
		table, err := pricing.NewTableWithClientRules([]pricing.Rule{
			{Identity: pricing.Identity{ChannelID: "openai", ModelID: event.UpstreamModel}, Prices: prices},
			{Identity: pricing.Identity{ChannelID: "openai", ModelID: "advisor"}, Prices: prices},
		}, []pricing.ClientRule{{ClientModel: event.ClientModel, Prices: prices}, {ClientModel: "advisor", Prices: prices}})
		if err != nil {
			t.Fatal(err)
		}
		result := usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 10}}
		if advisor {
			result.Advisor = &usage.AdvisorUsage{Calls: []usage.ModelTokens{{Model: "advisor", Tokens: usage.Tokens{UncachedInput: 20}}}}
		}
		charge, cr := table.QuoteClientModel(event.ClientModel, result, pricing.ModeStandard, pricing.DefaultPriceMultiplier)
		cost, ur := table.QuoteForModeWithMultipliers(pricing.Identity{ChannelID: "openai", ModelID: event.UpstreamModel}, result, pricing.ModeStandard, pricing.PriceMultipliers{Group: pricing.DefaultPriceMultiplier, AccessKey: pricing.DefaultPriceMultiplier})
		event.Usage.Result = result
		event.Usage.Pricing.EstimatedCostNanoUSD = int64(charge.EstimatedCostNanoUSD)
		event.Usage.Pricing.UpstreamCostNanoUSD = int64(cost.EstimatedCostNanoUSD)
		event.Usage.Pricing.ReceiptJSON = encodeReceiptJSON(t, *cr)
		event.Usage.Pricing.UpstreamReceiptJSON = encodeReceiptJSON(t, *ur)
		row, err := mapEvent(redact.New(), event)
		if err != nil {
			t.Fatal(err)
		}
		if row.EstimatedCostNano != int64(charge.EstimatedCostNanoUSD)*7 {
			t.Fatalf("settlement=%+v", row)
		}
		if err = (&gormBatchWriter{db: db}).WriteBatch(t.Context(), []models.RequestLog{row}); err != nil {
			t.Fatal(err)
		}
		record, err := newRequestLogTestService(db).Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if len(record.Attempts) != 1 || record.Attempts[0].PricingReceipt.SchemaVersion != cr.SchemaVersion || record.Attempts[0].UpstreamPricingReceipt.SchemaVersion != ur.SchemaVersion {
			t.Fatalf("record=%+v", record)
		}
		bad := row.AttemptRows[0]
		bad.ChannelID = "anthropic"
		if _, err = decodeAttemptUpstreamPricingReceipt(bad); err == nil {
			t.Fatal("cross-channel cost receipt accepted")
		}
		bad = row.AttemptRows[0]
		bad.UpstreamPricingReceipt = bad.PricingReceipt
		if _, err = decodeAttemptUpstreamPricingReceipt(bad); err == nil {
			t.Fatal("charge accepted as cost receipt")
		}
	}
}
