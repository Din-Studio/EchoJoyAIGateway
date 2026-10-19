package pricing

import "gpt-load/internal/usage"

const tokensPerMillion = 1_000_000

var (
	directPriceMultiplier       = Multiplier{Numerator: 1, Denominator: 1}
	cacheWriteOneHourMultiplier = Multiplier{Numerator: 8, Denominator: 5}
)

// Quote prices a finalized usage result without modifying it.
func (table *Table) Quote(identity Identity, result usage.Result) Quote {
	quote, _ := table.QuoteForModeWithReceipt(identity, result, ModeStandard)
	return quote
}

// QuoteWithReceipt prices a finalized usage result and freezes the exact rule,
// tier, rates, multipliers, and component amounts used for the calculation.
func (table *Table) QuoteWithReceipt(
	identity Identity,
	result usage.Result,
) (Quote, *Receipt) {
	return table.QuoteForModeWithReceipt(identity, result, ModeStandard)
}

// QuoteForMode prices a finalized usage result. A matched standard context
// tier takes precedence over the requested mode schedule, which otherwise
// falls back to the standard base schedule when unavailable.
func (table *Table) QuoteForMode(identity Identity, result usage.Result, mode Mode) Quote {
	quote, _ := table.QuoteForModeWithReceipt(identity, result, mode)
	return quote
}

// QuoteForModeWithMultipliers 在上游价格计价完成后统一调整请求总费用。
func (table *Table) QuoteForModeWithMultipliers(identity Identity, result usage.Result, mode Mode, multipliers PriceMultipliers) (Quote, *Receipt) {
	quote, receipt := table.QuoteForModeWithReceipt(identity, result, mode)
	return applyQuoteMultipliers(quote, receipt, multipliers, UpstreamReceiptSchemaVersion)
}

// QuoteClientModel 按客户端模型的统一价计价，再乘访问密钥倍率；分组倍率只作用于
// 上游费用，因此这里固定为 1。未配置统一价的模型按未定价处理，不回退到上游价格。
func (table *Table) QuoteClientModel(clientModel string, result usage.Result, mode Mode, accessKey PriceMultiplier) (Quote, *Receipt) {
	var rule ClientRule
	found := false
	if table != nil {
		rule, found = table.clientRules[clientModel]
	}
	quote, receipt := quoteSchedules(
		rule.Prices, rule.ContextTiers, rule.ModeSchedules, found,
		ReceiptRule{ClientModel: clientModel}, result, mode,
	)
	multipliers := PriceMultipliers{Group: DefaultPriceMultiplier, AccessKey: accessKey}
	return applyQuoteMultipliers(quote, receipt, multipliers, ClientModelReceiptSchemaVersion)
}

func applyQuoteMultipliers(quote Quote, receipt *Receipt, multipliers PriceMultipliers, schemaVersion int) (Quote, *Receipt) {
	if !multipliers.Group.Valid() || !multipliers.AccessKey.Valid() {
		return unavailableQuote(), nil
	}
	if receipt == nil {
		return quote, nil
	}
	baseTotal := receipt.TotalNanoUSD
	if quote.State == CostStatePriced {
		adjusted, ok := applyPriceMultipliers(quote.EstimatedCostNanoUSD, multipliers)
		if !ok {
			return unavailableQuote(), nil
		}
		quote.EstimatedCostNanoUSD = adjusted
		receipt.TotalNanoUSD = int64(adjusted)
	}
	receipt.SchemaVersion = schemaVersion
	receipt.BaseTotalNanoUSD = &baseTotal
	receipt.PriceMultipliers = &multipliers
	return quote, receipt
}

// QuoteForModeWithReceipt freezes the exact tier or mode schedule used. A mode
// without a matching persisted schedule is indistinguishable from a standard
// quote.
func (table *Table) QuoteForModeWithReceipt(
	identity Identity,
	result usage.Result,
	mode Mode,
) (Quote, *Receipt) {
	rule, found := table.Lookup(identity)
	return quoteSchedules(
		rule.Prices, rule.ContextTiers, rule.ModeSchedules, found,
		ReceiptRule{ChannelID: identity.ChannelID, ModelID: identity.ModelID}, result, mode,
	)
}

// quoteSchedules prices usage against one rule's schedules; found reports
// whether a rule exists for the receipt identity.
func quoteSchedules(
	basePrices Prices,
	contextTiers []ContextTier,
	modeSchedules map[Mode]Schedule,
	found bool,
	receiptRule ReceiptRule,
	result usage.Result,
	mode Mode,
) (Quote, *Receipt) {
	switch result.State {
	case usage.StateNotApplicable:
		return Quote{State: CostStateNotApplicable, Completeness: CompletenessNotApplicable}, nil
	case usage.StateMissing:
		return unavailableQuote(), nil
	case usage.StateComplete, usage.StatePartial:
	default:
		return unavailableQuote(), nil
	}

	if !found {
		return unavailableQuote(), nil
	}
	if _, ok := usage.CheckedTotal(result.Tokens); !ok {
		return unavailableQuote(), nil
	}
	inputTokens, ok := checkedInputTokens(result.Tokens)
	if !ok {
		return unavailableQuote(), nil
	}

	selectedMode := ModeStandard
	prices, selectedThreshold := selectSchedulePrices(basePrices, contextTiers, inputTokens)
	if selectedThreshold == nil && mode != "" && mode != ModeStandard {
		if schedule, exists := modeSchedules[mode]; exists {
			selectedMode = mode
			prices, selectedThreshold = selectSchedulePrices(
				schedule.Prices,
				schedule.ContextTiers,
				inputTokens,
			)
		}
	}

	components := [...]struct {
		code       string
		tokens     int64
		price      Price
		multiplier Multiplier
	}{
		{code: "input", tokens: result.Tokens.UncachedInput, price: prices.Input, multiplier: directPriceMultiplier},
		{code: "cache_read", tokens: result.Tokens.CacheRead, price: prices.CacheRead, multiplier: directPriceMultiplier},
		{code: "cache_write_5m", tokens: result.Tokens.CacheWrite5M, price: prices.CacheWrite, multiplier: directPriceMultiplier},
		{code: "cache_write_1h", tokens: result.Tokens.CacheWrite1H, price: prices.CacheWrite, multiplier: cacheWriteOneHourMultiplier},
		{code: "output", tokens: result.Tokens.Output, price: prices.Output, multiplier: directPriceMultiplier},
	}
	receipt := &Receipt{
		SchemaVersion:          4,
		Method:                 ReceiptMethodUnitRateSum,
		MethodVersion:          1,
		Currency:               "USD",
		PricingMode:            selectedMode,
		Rule:                   receiptRule,
		ContextThresholdTokens: selectedThreshold,
		LineItems:              make([]ReceiptLine, 0, len(components)+1),
	}

	positiveBillable := result.Tokens.CacheWriteUnknown > 0
	pricedPositive := false
	partial := result.State == usage.StatePartial ||
		result.Tokens.CacheWriteUnknown > 0
	cost := NanoUSD(0)
	for _, component := range components {
		if component.tokens == 0 {
			continue
		}
		positiveBillable = true
		line := ReceiptLine{
			Code:       component.code,
			Quantity:   component.tokens,
			Multiplier: component.multiplier,
			State:      ReceiptLineUnpriced,
		}
		if !component.price.Set {
			partial = true
			receipt.LineItems = append(receipt.LineItems, line)
			continue
		}
		componentCost, ok := QuoteComponent(
			component.tokens,
			component.price.NanoUSDPerMillion,
			component.multiplier,
		)
		if !ok {
			return unavailableQuote(), nil
		}
		cost, ok = CheckedAddNanoUSD(cost, componentCost)
		if !ok {
			return unavailableQuote(), nil
		}
		rate := int64(component.price.NanoUSDPerMillion)
		amount := int64(componentCost)
		line.RateNanoUSDPerMillion = &rate
		line.AmountNanoUSD = &amount
		line.State = ReceiptLinePriced
		receipt.LineItems = append(receipt.LineItems, line)
		pricedPositive = true
	}
	if result.Tokens.CacheWriteUnknown > 0 {
		receipt.LineItems = append(receipt.LineItems, ReceiptLine{
			Code:       "cache_write",
			Quantity:   result.Tokens.CacheWriteUnknown,
			Multiplier: directPriceMultiplier,
			State:      ReceiptLineUnpriced,
		})
	}
	receipt.TotalNanoUSD = int64(cost)

	if positiveBillable && !pricedPositive {
		return unavailableQuote(), receipt
	}
	completeness := CompletenessComplete
	if partial {
		completeness = CompletenessPartial
	}
	return Quote{
		State:                CostStatePriced,
		Completeness:         completeness,
		EstimatedCostNanoUSD: cost,
	}, receipt
}

func selectSchedulePrices(
	base Prices,
	tiers []ContextTier,
	inputTokens int64,
) (Prices, *int64) {
	prices := base
	var selectedThreshold *int64
	for _, tier := range tiers {
		if inputTokens < tier.InputThresholdTokens {
			break
		}
		prices = tier.Prices
		threshold := tier.InputThresholdTokens
		selectedThreshold = &threshold
	}
	return prices, selectedThreshold
}

func checkedInputTokens(tokens usage.Tokens) (int64, bool) {
	total := int64(0)
	for _, value := range [...]int64{
		tokens.UncachedInput,
		tokens.CacheRead,
		tokens.CacheWrite5M,
		tokens.CacheWrite1H,
		tokens.CacheWriteUnknown,
	} {
		var ok bool
		total, ok = usage.CheckedAdd(total, value)
		if !ok {
			return 0, false
		}
	}
	return total, true
}

func unavailableQuote() Quote {
	return Quote{State: CostStateUnpriced, Completeness: CompletenessUnavailable}
}
