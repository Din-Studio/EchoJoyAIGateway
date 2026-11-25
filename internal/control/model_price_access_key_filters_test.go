package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/catalog"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
)

func getProjectModelPage(t *testing.T, engine *gin.Engine, token, query string) (ProjectModelListResponse, string) {
	t.Helper()
	response := serveModelPriceHTTPRequest(engine, http.MethodGet, "/api/models"+query, "", token)
	var envelope struct {
		Data ProjectModelListResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusOK {
		t.Fatalf("GET /api/models%s = %d %s, %v", query, response.Code, response.Body.String(), err)
	}
	return envelope.Data, response.Body.String()
}

func TestAccessKeyModelCollectionFiltersUnifiedPriceBeforePagination(t *testing.T) {
	fixture, engine, _ := newModelPriceHTTPFixture(t, false)
	createPriceTestGroup(t, fixture.db, models.Group{
		Name: "mixed", Enabled: true, Overrides: models.JSON(`{}`), Models: models.JSON(`[
			{"id":"charged-source","alias":"a-pending"}, {"id":"empty-source","alias":"b-zero"},
			{"id":"charged-source","alias":"c-paid"}, {"id":"empty-source","alias":"c-paid"},
			{"id":"private-source","alias":"d-hidden"}]`),
	})
	charged := models.ModelPrice{ChannelID: "openai_compatible", ModelID: "charged-source", IsManual: true, InputPriceNanoUSDPerMillionTokens: int64Pointer(2_000_000_000)}
	if err := fixture.db.Create(&charged).Error; err != nil {
		t.Fatal(err)
	}
	mustEnsureInitialPrices(t, fixture)
	for model, input := range map[string]string{"b-zero": "0", "c-paid": "3", "d-hidden": "5"} {
		body := clientModelPriceBody(fmt.Sprintf("%q", model), fmt.Sprintf("%q", input))
		if res := serveModelPriceHTTPRequest(engine, http.MethodPut, "/api/client-model-prices", body, authTestKey); res.Code != http.StatusOK {
			t.Fatalf("set unified price: %d %s", res.Code, res.Body.String())
		}
	}
	key, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{
		Name: "caller", Filters: &AccessKeyFilters{Models: []string{"a-pending", "b-zero", "c-paid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSummary := ProjectModelSummaryDTO{ClientModelCount: 3, UpstreamModelCount: 4, PriceCount: 3, PendingPriceCount: 1}
	for status, models := range map[string][]string{
		"all": {"a-pending", "b-zero", "c-paid"}, "pending": {"a-pending"}, "configured": {"b-zero", "c-paid"},
	} {
		for i, model := range models {
			var baseline string
			for _, method := range []string{"all", "automatic", "manual", "linked"} {
				query := fmt.Sprintf("?pricing_status=%s&pricing_method=%s&page_size=1&page=%d", status, method, i+1)
				page, wire := getProjectModelPage(t, engine, key.Key, query)
				if page.Summary != wantSummary || page.Pagination.TotalItems != int64(len(models)) || page.Pagination.TotalPages != int64(len(models)) || len(page.Items) != 1 || page.Items[0].ClientModel != model {
					t.Fatalf("%s: unexpected page or scope: %s", query, wire)
				}
				item := page.Items[0]
				wantStatus, wantSources := PricingStatusConfigured, 1
				switch model {
				case "a-pending":
					wantStatus = PricingStatusPending
					if item.Price != nil {
						t.Fatal("pending caller price must be null")
					}
				case "b-zero":
					if item.Price == nil || item.Price.Prices.Input == nil || *item.Price.Prices.Input != "0" {
						t.Fatal("explicit zero caller price lost")
					}
				case "c-paid":
					wantSources = 2
				}
				if len(item.UpstreamModels) != wantSources {
					t.Fatalf("%s dropped an authorized source: %s", query, wire)
				}
				for _, source := range item.UpstreamModels {
					if !source.AliasApplied || source.Price.PricingStatus != wantStatus {
						t.Fatalf("source disagrees with outward card: %s", wire)
					}
				}
				if baseline != "" && wire != baseline {
					t.Fatalf("%s changed caller payload", query)
				}
				baseline = wire
			}
		}
	}
	for _, query := range []string{"?q=private-source", "?pricing_status=pending&q=c-paid"} {
		page, _ := getProjectModelPage(t, engine, key.Key, query)
		if len(page.Items) != 0 || page.Pagination.TotalItems != 0 || page.Summary != wantSummary {
			t.Fatalf("%s: %+v", query, page)
		}
	}
	for _, query := range []string{"?pricing_method=invalid", "?pricing_status=invalid", "?access_key_id=1"} {
		if res := serveModelPriceHTTPRequest(engine, http.MethodGet, "/api/models"+query, "", key.Key); res.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d", query, res.Code)
		}
	}
	// The same queries for an administrator filter upstream costs and sources, not client prices.
	admin, _ := getProjectModelPage(t, engine, authTestKey, "?pricing_method=manual&pricing_status=configured&page_size=1&page=2")
	if admin.Pagination.TotalItems != 2 || len(admin.Items) != 1 || admin.Items[0].ClientModel != "c-paid" || len(admin.Items[0].UpstreamModels) != 1 || admin.Items[0].UpstreamModels[0].Price.ID != charged.ID {
		t.Fatalf("admin configured manual page = %+v", admin)
	}
	admin, _ = getProjectModelPage(t, engine, authTestKey, "?pricing_method=automatic&pricing_status=pending&page_size=1&page=1")
	if admin.Pagination.TotalItems != 3 || len(admin.Items) != 1 || admin.Items[0].ClientModel != "b-zero" {
		t.Fatalf("admin pending automatic page = %+v", admin)
	}
}

func TestAccessKeyModelCollectionInvariantUnderCostChanges(t *testing.T) {
	fixture, engine, _ := newModelPriceHTTPFixture(t, false)
	rows, _ := priceReferenceFixture(t, fixture, 2)
	id := rows[0].ModelID
	// Higher priority unpriced metadata and lower priority priced metadata for the same model.
	catalogWithCosts := func(openai, anthropic *catalog.ModelCost) *catalog.Snapshot {
		return &catalog.Snapshot{Providers: map[string]catalog.Provider{
			"openai":    {ID: "openai", Models: map[string]catalog.Model{id: {ID: id, Name: "Public catalog metadata", Cost: openai}}},
			"anthropic": {ID: "anthropic", Models: map[string]catalog.Model{id: {ID: id, Name: "Other metadata", Cost: anthropic}}},
		}}
	}
	fixture.catalogRuntime.Publish(catalogWithCosts(nil, &catalog.ModelCost{Prices: pricing.Prices{Input: priceTestValue(6_000_000_000)}}))
	// Include both outward statuses in the invariance check.
	if _, err := fixture.service.PutClientModelPrice(t.Context(), ClientModelPriceRequest{
		ClientModel: rows[1].ModelID, ModelPriceUpdateRequest: mustModelPriceUpdateRequest(t, modelPriceHTTPUpdateBody(`"5"`, "false")),
	}); err != nil {
		t.Fatal(err)
	}
	key, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "invariant"})
	if err != nil {
		t.Fatal(err)
	}
	page, wire := getProjectModelPage(t, engine, key.Key, "")
	if len(page.Items) != 2 || len(page.Items[0].UpstreamModels) != 1 {
		t.Fatal(wire)
	}
	source := page.Items[0].UpstreamModels[0]
	if source.CatalogReference == nil || source.CatalogReference.ProviderID != "openai" || source.CatalogReference.Model.Name != "Public catalog metadata" {
		t.Fatalf("missing cost-independent metadata: %s", wire)
	}
	// Check the real JSON, not a round-trip through Go zero values. The modern reader needs these shapes.
	var raw struct {
		Data struct {
			Items []struct {
				UpstreamModels []struct {
					Price map[string]any `json:"price"`
				} `json:"upstream_models"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(wire), &raw); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"id": float64(rows[0].ID), "channel_id": rows[0].ChannelID, "model_id": id,
		"channel_name": "OpenAI Compatible", "channel_icon": "compatible", "channel_mark": "OC",
		"pricing_status": "pending", "method": nil, "matched_provider_id": nil, "match_source": nil,
		"referenced": false, "reference_count": float64(0), "reference_group_count": float64(0),
		"can_delete": false, "can_reset": false, "updated_at_ms": float64(0), "currency": "USD",
		"prices":        map[string]any{"input": nil, "output": nil, "cache_read": nil, "cache_write": nil},
		"context_tiers": []any{}, "mode_schedules": map[string]any{},
	}
	if got := raw.Data.Items[0].UpstreamModels[0].Price; !reflect.DeepEqual(got, want) {
		t.Fatalf("caller source shape = %#v, want %#v", got, want)
	}
	baseline := map[string]string{"": wire}
	for _, query := range []string{"?q=anthropic", "?q=public", "?pricing_status=pending&pricing_method=linked&page_size=1", "?pricing_status=configured"} {
		_, baseline[query] = getProjectModelPage(t, engine, key.Key, query)
	}
	assertInvariant := func(stage string) {
		t.Helper()
		for query, before := range baseline {
			_, after := getProjectModelPage(t, engine, key.Key, query)
			if before != after {
				t.Fatalf("%s changed caller response to %s\nbefore: %s\nafter: %s", stage, query, before, after)
			}
		}
	}
	automatic, err := fixture.service.ResetModelPrice(t.Context(), rows[0].ID)
	if err != nil || automatic.MatchedProviderID == nil || *automatic.MatchedProviderID != "anthropic" {
		t.Fatalf("reset = %+v, %v", automatic, err)
	}
	admin, _ := getProjectModelPage(t, engine, authTestKey, "?q=anthropic&pricing_method=automatic")
	if len(admin.Items) != 1 || admin.Items[0].UpstreamModels[0].CatalogReference.ProviderID != "anthropic" {
		t.Fatalf("admin catalog must follow cost match: %+v", admin)
	}
	assertInvariant("automatic price provider, metadata and amount")
	followPrice(t, fixture, rows[1].ID, rows[0].ID)
	admin, _ = getProjectModelPage(t, engine, authTestKey, "?pricing_method=linked&pricing_status=configured")
	if len(admin.Items) != 1 || admin.Items[0].UpstreamModels[0].Price.SourcePriceID == nil || *admin.Items[0].UpstreamModels[0].Price.SourcePriceID != rows[0].ID {
		t.Fatalf("admin cost link lost: %+v", admin)
	}
	assertInvariant("follow relationship")
	// Keep provider identities and metadata fixed; only change their catalog costs.
	if err := fixture.service.applyCatalogSnapshot(t.Context(), catalogWithCosts(&catalog.ModelCost{Prices: pricing.Prices{Input: priceTestValue(9_000_000_000)}}, nil)); err != nil {
		t.Fatal(err)
	}
	admin, _ = getProjectModelPage(t, engine, authTestKey, "?pricing_method=automatic&pricing_status=configured")
	if len(admin.Items) != 1 || len(admin.Items[0].UpstreamModels) != 1 {
		t.Fatalf("admin catalog refresh result = %+v", admin)
	}
	cost := admin.Items[0].UpstreamModels[0].Price
	if cost.MatchedProviderID == nil || *cost.MatchedProviderID != "openai" || cost.Prices.Input == nil || *cost.Prices.Input != "9" {
		t.Fatalf("expected changed catalog cost match, got %+v", cost)
	}
	assertInvariant("catalog refresh with only new costs")
	// Add an unreferenced cost, then change the linked target to automatic/pending.
	if err := fixture.db.Create(&models.ModelPrice{ChannelID: rows[0].ChannelID, ModelID: "orphan-cost", IsManual: true}).Error; err != nil {
		t.Fatal(err)
	}
	if pending, err := fixture.service.ResetModelPrice(t.Context(), rows[1].ID); err != nil || pending.PricingStatus != PricingStatusPending {
		t.Fatalf("reset to pending = %+v, %v", pending, err)
	}
	admin, _ = getProjectModelPage(t, engine, authTestKey, "?pricing_method=automatic&pricing_status=pending")
	if len(admin.Items) != 1 || admin.Summary.PendingPriceCount != 1 || admin.Summary.UnreferencedPriceCount != 1 {
		t.Fatalf("admin cost counts = %+v", admin)
	}
	assertInvariant("pending cost, counters and detach")
}

func TestAccessKeyModelCollectionReturnsOwnMultiplierAndRawUnifiedSchedule(t *testing.T) {
	fixture, engine, _ := newModelPriceHTTPFixture(t, false)
	rows, _ := priceReferenceFixture(t, fixture, 1)
	request := mustModelPriceUpdateRequest(t, `{"input":"0.000000001","output":null,"cache_read":null,"cache_write":null,"context_tiers":[{"threshold_tokens":1000,"input":"2","output":null,"cache_read":null,"cache_write":null}],"mode_schedules":{"fast":{"prices":{"input":"3","output":null,"cache_read":null,"cache_write":null},"context_tiers":[]}}}`)
	if _, err := fixture.service.PutClientModelPrice(t.Context(), ClientModelPriceRequest{ClientModel: rows[0].ModelID, ModelPriceUpdateRequest: request}); err != nil {
		t.Fatal(err)
	}
	var keys []AccessKeyCreateResult
	for _, multiplier := range []string{"1.25", "0"} {
		key, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "multiplier-" + multiplier, PriceMultiplier: optionalField[string]{Set: true, Value: multiplier}})
		if err != nil {
			t.Fatal(err)
		}
		if key.PriceMultiplier != multiplier {
			t.Fatalf("created multiplier = %q, want %q", key.PriceMultiplier, multiplier)
		}
		keys = append(keys, key)
	}
	// A peer committed new settings but the serving replica has not reloaded its published config yet.
	peer := newServiceFixtureWithDatabase(t, fixture.db)
	if _, err := updateCurrencySettings(t, peer, `{"currency":"CNY","exchange_rate":"7"}`); err != nil {
		t.Fatal(err)
	}
	if currency, err := fixture.service.currentCurrency(); err != nil || currency.Code != pricing.CurrencyUSD {
		t.Fatalf("test requires stale local USD snapshot, got %+v, %v", currency, err)
	}
	admin, wire := getProjectModelPage(t, engine, authTestKey, "")
	if strings.Contains(wire, `"access_key_price_multiplier"`) || len(admin.Items) != 1 || admin.Items[0].Price == nil {
		t.Fatalf("admin response = %s", wire)
	}
	price := admin.Items[0].Price
	// 0.00000001 CNY is the shortest display rate that converts back to 1 nano USD at rate 7.
	if price.Currency != pricing.CurrencyCNY || price.Prices.Input == nil || *price.Prices.Input != "0.00000001" || len(price.ContextTiers) != 1 || price.ContextTiers[0].Prices.Input == nil || *price.ContextTiers[0].Prices.Input != "14" || price.ModeSchedules[pricing.ModeFast].Prices.Input == nil || *price.ModeSchedules[pricing.ModeFast].Prices.Input != "21" {
		t.Fatalf("expected raw unified schedule in persisted CNY, got %s", wire)
	}
	for _, key := range keys {
		page, wire := getProjectModelPage(t, engine, key.Key, "?pricing_status=configured")
		if page.AccessKeyPriceMultiplier != key.PriceMultiplier || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0].Price, price) || page.Items[0].UpstreamModels[0].Price.Currency != pricing.CurrencyCNY {
			t.Fatalf("want own multiplier %q and unchanged schedule: %s", key.PriceMultiplier, wire)
		}
	}
	if _, err := fixture.service.UpdateAccessKey(t.Context(), keys[0].ID, AccessKeyUpdateRequest{PriceMultiplier: optionalField[string]{Set: true, Value: "0.75"}}); err != nil {
		t.Fatal(err)
	}
	if page, _ := getProjectModelPage(t, engine, keys[0].Key, "?q=absent"); page.AccessKeyPriceMultiplier != "0.75" || len(page.Items) != 0 {
		t.Fatalf("empty page omitted current key multiplier: %+v", page)
	}
}
