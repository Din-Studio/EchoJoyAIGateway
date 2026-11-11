package pricing

import (
	"slices"

	"gpt-load/internal/usage"
)

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
	quote, receipt := table.quoteSchedules(
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
	if receipt.SchemaVersion != UpstreamAdvisorReceiptSchemaVersion && receipt.SchemaVersion != ClientAdvisorReceiptSchemaVersion {
		receipt.SchemaVersion = schemaVersion
	}
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
	return table.quoteSchedules(
		rule.Prices, rule.ContextTiers, rule.ModeSchedules, found,
		ReceiptRule{ChannelID: identity.ChannelID, ModelID: identity.ModelID}, result, mode,
	)
}

// quoteSchedules prices usage against one rule's schedules; found reports
// whether a rule exists for the receipt identity.
func (table *Table) quoteSchedules(
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

	receipt := &Receipt{
		SchemaVersion:          4,
		Method:                 ReceiptMethodUnitRateSum,
		MethodVersion:          1,
		Currency:               "USD",
		PricingMode:            selectedMode,
		Rule:                   receiptRule,
		ContextThresholdTokens: selectedThreshold,
		LineItems:              make([]ReceiptLine, 0, 6),
	}

	lines := quoteLines{receipt: receipt, partial: result.State == usage.StatePartial}
	for _, component := range quoteComponents(result.Tokens, prices) {
		if !lines.add("", "", component) {
			return unavailableQuote(), nil
		}
	}
	if result.Advisor != nil && len(result.Advisor.Calls) > 0 {
		if !table.addAdvisorLines(&lines, receiptRule, result.Advisor.Calls) {
			return unavailableQuote(), nil
		}
		receipt.SchemaVersion = UpstreamAdvisorReceiptSchemaVersion
		if receiptRule.ClientModel != "" {
			receipt.SchemaVersion = ClientAdvisorReceiptSchemaVersion
		}
		baseTotal := int64(lines.cost)
		receipt.BaseTotalNanoUSD = &baseTotal
		receipt.PriceMultipliers = &PriceMultipliers{Group: DefaultPriceMultiplier, AccessKey: DefaultPriceMultiplier}
	}
	receipt.TotalNanoUSD = int64(lines.cost)

	if lines.positiveBillable && !lines.pricedPositive {
		return unavailableQuote(), receipt
	}
	completeness := CompletenessComplete
	if lines.partial {
		completeness = CompletenessPartial
	}
	return Quote{
		State:                CostStatePriced,
		Completeness:         completeness,
		EstimatedCostNanoUSD: lines.cost,
	}, receipt
}

// advisorLinePrefix 标记 v8 上游成本或 v9 客户端收费中的 advisor 明细。
const advisorLinePrefix = "advisor_"

// quoteLines 逐项写入明细行并累计请求总价。
type quoteLines struct {
	receipt          *Receipt
	cost             NanoUSD
	positiveBillable bool
	pricedPositive   bool
	partial          bool
}

// quoteComponent 是一项计价分量；未知 TTL 的缓存写入没有单价，始终未计价。
type quoteComponent struct {
	code       string
	tokens     int64
	price      Price
	multiplier Multiplier
}

func quoteComponents(tokens usage.Tokens, prices Prices) [6]quoteComponent {
	return [...]quoteComponent{
		{code: "input", tokens: tokens.UncachedInput, price: prices.Input, multiplier: directPriceMultiplier},
		{code: "cache_read", tokens: tokens.CacheRead, price: prices.CacheRead, multiplier: directPriceMultiplier},
		{code: "cache_write_5m", tokens: tokens.CacheWrite5M, price: prices.CacheWrite, multiplier: directPriceMultiplier},
		{code: "cache_write_1h", tokens: tokens.CacheWrite1H, price: prices.CacheWrite, multiplier: cacheWriteOneHourMultiplier},
		{code: "output", tokens: tokens.Output, price: prices.Output, multiplier: directPriceMultiplier},
		{code: "cache_write", tokens: tokens.CacheWriteUnknown, multiplier: directPriceMultiplier},
	}
}

// add 为正数分量追加明细行；prefix 与 modelID 只用于 advisor 行。
func (lines *quoteLines) add(prefix, modelID string, component quoteComponent) bool {
	if component.tokens == 0 {
		return true
	}
	lines.positiveBillable = true
	line := ReceiptLine{
		Code:       prefix + component.code,
		ModelID:    modelID,
		Quantity:   component.tokens,
		Multiplier: component.multiplier,
		State:      ReceiptLineUnpriced,
	}
	if !component.price.Set {
		lines.partial = true
		lines.receipt.LineItems = append(lines.receipt.LineItems, line)
		return true
	}
	componentCost, ok := QuoteComponent(
		component.tokens,
		component.price.NanoUSDPerMillion,
		component.multiplier,
	)
	if !ok {
		return false
	}
	lines.cost, ok = CheckedAddNanoUSD(lines.cost, componentCost)
	if !ok {
		return false
	}
	rate := int64(component.price.NanoUSDPerMillion)
	amount := int64(componentCost)
	line.RateNanoUSDPerMillion = &rate
	line.AmountNanoUSD = &amount
	line.State = ReceiptLinePriced
	lines.receipt.LineItems = append(lines.receipt.LineItems, line)
	lines.pricedPositive = true
	return true
}

// addAdvisorLines 按收据身份选择统一价或渠道价，每次调用独立选标准上下文档。
// 客户端调用的 Model 已由网关按冻结分组映射为对外名称；两类价格不互相回退。
func (table *Table) addAdvisorLines(lines *quoteLines, identity ReceiptRule, calls []usage.ModelTokens) bool {
	type advisorComponent struct {
		model string
		quoteComponent
	}
	var merged []advisorComponent
	for _, call := range calls {
		inputTokens, inputOK := checkedInputTokens(call.Tokens)
		if _, totalOK := usage.CheckedTotal(call.Tokens); !totalOK || !inputOK || validateModelID(call.Model) != nil {
			return false
		}
		var prices Prices
		if identity.ClientModel != "" {
			if rule, found := table.clientRules[call.Model]; found {
				prices, _ = selectSchedulePrices(rule.Prices, rule.ContextTiers, inputTokens)
			}
		} else if rule, found := table.rules[Identity{ChannelID: identity.ChannelID, ModelID: call.Model}]; found {
			prices, _ = selectSchedulePrices(rule.Prices, rule.ContextTiers, inputTokens)
		}
		for _, component := range quoteComponents(call.Tokens, prices) {
			if component.tokens == 0 {
				continue
			}
			index := slices.IndexFunc(merged, func(existing advisorComponent) bool {
				return existing.model == call.Model && existing.code == component.code && existing.price == component.price
			})
			if index < 0 {
				merged = append(merged, advisorComponent{model: call.Model, quoteComponent: component})
				continue
			}
			var ok bool
			if merged[index].tokens, ok = usage.CheckedAdd(merged[index].tokens, component.tokens); !ok {
				return false
			}
		}
	}
	for _, component := range merged {
		if !lines.add(advisorLinePrefix, component.model, component.quoteComponent) {
			return false
		}
	}
	return true
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
