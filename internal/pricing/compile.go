package pricing

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

const (
	maxProviderIDBytes = 246
	maxChannelIDBytes  = 64
	maxModelIDBytes    = 255
	maxModeBytes       = 64
)

// Valid reports whether mode is a canonical Models.dev mode key.
func (mode Mode) Valid() bool {
	if len(mode) == 0 || len(mode) > maxModeBytes {
		return false
	}
	for index, character := range []byte(mode) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if index > 0 && index < len(mode)-1 && (character == '-' || character == '_') {
			continue
		}
		return false
	}
	return true
}

// ProviderScopeKey builds the canonical scope for a Models.dev provider ID.
func ProviderScopeKey(providerID string) (string, error) {
	if len(providerID) == 0 || len(providerID) > maxProviderIDBytes {
		return "", fmt.Errorf("provider ID must be 1 through %d ASCII bytes", maxProviderIDBytes)
	}
	separator := true
	for _, character := range []byte(providerID) {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			separator = false
		case character == '.' || character == '-':
			if separator {
				return "", fmt.Errorf("provider ID must contain non-empty lowercase slug segments")
			}
			separator = true
		default:
			return "", fmt.Errorf("provider ID must contain only lowercase ASCII slug segments")
		}
	}
	if separator {
		return "", fmt.Errorf("provider ID must contain non-empty lowercase slug segments")
	}
	return "provider:" + providerID, nil
}

// GroupScopeKey builds the canonical scope for a positive group ID.
func GroupScopeKey(groupID uint) (string, error) {
	if groupID == 0 {
		return "", fmt.Errorf("group ID must be positive")
	}
	return "group:" + strconv.FormatUint(uint64(groupID), 10), nil
}

// NewTable validates and deep-clones exact upstream pricing rules.
func NewTable(rules []Rule) (*Table, error) {
	return NewTableWithClientRules(rules, nil)
}

// NewTableWithClientRules validates and deep-clones upstream rules and
// client-model rules into one immutable snapshot.
func NewTableWithClientRules(rules []Rule, clientRules []ClientRule) (*Table, error) {
	table := &Table{
		rules:       make(map[Identity]Rule, len(rules)),
		clientRules: make(map[string]ClientRule, len(clientRules)),
	}
	for _, rule := range clientRules {
		if err := validateModelID(rule.ClientModel); err != nil {
			return nil, fmt.Errorf("client model: %w", err)
		}
		if err := validateSchedules(rule.Prices, rule.ContextTiers, rule.ModeSchedules); err != nil {
			return nil, err
		}
		if _, exists := table.clientRules[rule.ClientModel]; exists {
			return nil, fmt.Errorf("duplicate client model pricing %q", rule.ClientModel)
		}
		rule.ContextTiers, rule.ModeSchedules = cloneSchedules(rule.ContextTiers, rule.ModeSchedules)
		table.clientRules[rule.ClientModel] = rule
	}
	for _, rule := range rules {
		if err := validateRule(rule); err != nil {
			return nil, err
		}
		if _, exists := table.rules[rule.Identity]; exists {
			return nil, fmt.Errorf(
				"duplicate pricing identity channel_id=%q model_id=%q",
				rule.Identity.ChannelID,
				rule.Identity.ModelID,
			)
		}
		table.rules[rule.Identity] = cloneRule(rule)
	}
	return table, nil
}

// Lookup returns an immutable copy of the exact identity rule.
func (table *Table) Lookup(identity Identity) (Rule, bool) {
	if table == nil {
		return Rule{}, false
	}
	rule, ok := table.rules[identity]
	if !ok {
		return Rule{}, false
	}
	return cloneRule(rule), true
}

func validateRule(rule Rule) error {
	if err := validateIdentity(rule.Identity); err != nil {
		return err
	}
	return validateSchedules(rule.Prices, rule.ContextTiers, rule.ModeSchedules)
}

func validateSchedules(prices Prices, contextTiers []ContextTier, modeSchedules map[Mode]Schedule) error {
	if err := validatePrices(prices); err != nil {
		return err
	}
	if err := validateContextTiers(contextTiers); err != nil {
		return err
	}
	for mode, schedule := range modeSchedules {
		if !mode.Valid() || mode == ModeStandard {
			return fmt.Errorf("invalid non-standard pricing mode %q", mode)
		}
		if err := validatePrices(schedule.Prices); err != nil {
			return err
		}
		if !hasSetPrice(schedule.Prices) {
			return fmt.Errorf("pricing mode %q must set at least one price", mode)
		}
		if mode == ModeFast && len(schedule.ContextTiers) > 0 {
			return fmt.Errorf("Fast pricing mode does not support context tiers")
		}
		if err := validateContextTiers(schedule.ContextTiers); err != nil {
			return fmt.Errorf("pricing mode %q: %w", mode, err)
		}
	}
	return nil
}

func validateIdentity(identity Identity) error {
	if err := validateChannelID(identity.ChannelID); err != nil {
		return err
	}
	return validateModelID(identity.ModelID)
}

func validateChannelID(channelID string) error {
	if len(channelID) == 0 || len(channelID) > maxChannelIDBytes {
		return fmt.Errorf("channel ID must be 1 through %d bytes", maxChannelIDBytes)
	}
	if strings.TrimSpace(channelID) != channelID {
		return fmt.Errorf("channel ID must not have surrounding whitespace")
	}
	for _, character := range channelID {
		if unicode.IsControl(character) {
			return fmt.Errorf("channel ID must not contain control characters")
		}
	}
	return nil
}

func validateModelID(modelID string) error {
	if len(modelID) == 0 || len(modelID) > maxModelIDBytes {
		return fmt.Errorf("model ID must be 1 through %d bytes", maxModelIDBytes)
	}
	if strings.TrimSpace(modelID) != modelID {
		return fmt.Errorf("model ID must not have surrounding whitespace")
	}
	for _, character := range modelID {
		if unicode.IsControl(character) {
			return fmt.Errorf("model ID must not contain control characters")
		}
	}
	return nil
}

func validateReceiptRule(rule ReceiptRule, schemaVersion int) error {
	if schemaVersion == ClientModelReceiptSchemaVersion {
		if rule.ScopeKey != "" || rule.ChannelID != "" || rule.ModelID != "" {
			return fmt.Errorf("client model receipt must contain only a client model")
		}
		return validateModelID(rule.ClientModel)
	}
	if rule.ClientModel != "" {
		return fmt.Errorf("upstream receipt must not contain a client model")
	}
	switch schemaVersion {
	case 1:
		if err := validateModelID(rule.ModelID); err != nil {
			return err
		}
		if rule.ChannelID != "" {
			return fmt.Errorf("legacy v1 receipt must not contain a channel ID")
		}
		if rule.ScopeKey == "" {
			return fmt.Errorf("legacy receipt scope key is required")
		}
		return validateScopeKey(rule.ScopeKey)
	case 2:
		if err := validateModelID(rule.ModelID); err != nil {
			return err
		}
		if rule.ScopeKey != "" {
			return fmt.Errorf("global receipt must not contain a scope key")
		}
		if rule.ChannelID != "" {
			return fmt.Errorf("global receipt must not contain a channel ID")
		}
		return nil
	case 3, 4, 5, 6:
		if rule.ScopeKey != "" {
			return fmt.Errorf("channel receipt must not contain a scope key")
		}
		return validateIdentity(Identity{ChannelID: rule.ChannelID, ModelID: rule.ModelID})
	default:
		return fmt.Errorf("unsupported receipt rule schema version")
	}
}

func validateScopeKey(scopeKey string) error {
	if providerID, ok := strings.CutPrefix(scopeKey, "provider:"); ok {
		canonical, err := ProviderScopeKey(providerID)
		if err != nil || canonical != scopeKey {
			return fmt.Errorf("invalid provider scope key %q", scopeKey)
		}
		return nil
	}
	if groupText, ok := strings.CutPrefix(scopeKey, "group:"); ok {
		parsed, err := strconv.ParseUint(groupText, 10, strconv.IntSize)
		if err != nil {
			return fmt.Errorf("invalid group scope key %q", scopeKey)
		}
		canonical, err := GroupScopeKey(uint(parsed))
		if err != nil || canonical != scopeKey {
			return fmt.Errorf("invalid group scope key %q", scopeKey)
		}
		return nil
	}
	return fmt.Errorf("invalid pricing scope key %q", scopeKey)
}

func validatePrices(prices Prices) error {
	for _, price := range [...]Price{prices.Input, prices.Output, prices.CacheRead, prices.CacheWrite} {
		if price.NanoUSDPerMillion < 0 {
			return fmt.Errorf("pricing price must be non-negative")
		}
	}
	return nil
}

func hasSetPrice(prices Prices) bool {
	return prices.Input.Set || prices.Output.Set || prices.CacheRead.Set || prices.CacheWrite.Set
}

func cloneRule(rule Rule) Rule {
	rule.ContextTiers, rule.ModeSchedules = cloneSchedules(rule.ContextTiers, rule.ModeSchedules)
	return rule
}

func cloneSchedules(contextTiers []ContextTier, modeSchedules map[Mode]Schedule) ([]ContextTier, map[Mode]Schedule) {
	if contextTiers != nil {
		contextTiers = append([]ContextTier(nil), contextTiers...)
	}
	if modeSchedules != nil {
		cloned := make(map[Mode]Schedule, len(modeSchedules))
		for mode, schedule := range modeSchedules {
			schedule.ContextTiers = append([]ContextTier(nil), schedule.ContextTiers...)
			cloned[mode] = schedule
		}
		modeSchedules = cloned
	}
	return contextTiers, modeSchedules
}

func validateContextTiers(tiers []ContextTier) error {
	previousThreshold := int64(-1)
	for _, tier := range tiers {
		if tier.InputThresholdTokens < 0 {
			return fmt.Errorf("context tier threshold must be non-negative")
		}
		if tier.InputThresholdTokens <= previousThreshold {
			return fmt.Errorf("context tier thresholds must be strictly increasing")
		}
		if err := validatePrices(tier.Prices); err != nil {
			return err
		}
		if !hasSetPrice(tier.Prices) {
			return fmt.Errorf("context tier must set at least one price")
		}
		previousThreshold = tier.InputThresholdTokens
	}
	return nil
}
