package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/platform/config"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
)

func TestModelPriceBatchCNYBoundaryPreservesUnifiedCompilation(t *testing.T) {
	fixture, engine, _ := newModelPriceHTTPFixture(t, false)
	rows, _ := priceReferenceFixture(t, fixture, 2)
	clientRequest := mustModelPriceUpdateRequest(t, `{"input":"3","output":null,"cache_read":null,"cache_write":null,"context_tiers":[],"mode_schedules":{"fast":{"prices":{"input":"6","output":null,"cache_read":null,"cache_write":null},"context_tiers":[]},"ultrafast":{"prices":{"input":"9","output":null,"cache_read":null,"cache_write":null},"context_tiers":[]}}}`)
	if _, err := fixture.service.PutClientModelPrice(t.Context(), ClientModelPriceRequest{ClientModel: rows[1].ModelID, ModelPriceUpdateRequest: clientRequest}); err != nil {
		t.Fatal(err)
	}
	if _, err := updateCurrencySettings(t, fixture, `{"currency":"CNY","exchange_rate":"7"}`); err != nil {
		t.Fatal(err)
	}
	price := `{"input":"7","output":"0","cache_read":null,"cache_write":"7","context_tiers":[{"threshold_tokens":1000,"input":"14","output":null,"cache_read":null,"cache_write":null}],"mode_schedules":{"fast":{"prices":{"input":"21","output":null,"cache_read":null,"cache_write":null},"context_tiers":[]},"ultrafast":{"prices":{"input":"28","output":null,"cache_read":null,"cache_write":null},"context_tiers":[]}}}`
	body := fmt.Sprintf(`{"ids":[%d,%d],"mode":"copy","price":%s}`, rows[0].ID, rows[0].ID, price)
	response := serveModelPriceHTTPRequest(engine, http.MethodPost, "/api/model-prices/batch", body, authTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("copy = %d %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data ModelPriceBatchResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Items) != 1 {
		t.Fatalf("deduplicated results = %d", len(envelope.Data.Items))
	}
	assertCNY := func(item ModelPriceDTO) {
		t.Helper()
		if item.Currency != pricing.CurrencyCNY || item.Prices.Input == nil || *item.Prices.Input != "7" || item.Prices.Output == nil || *item.Prices.Output != "0" || item.Prices.CacheRead != nil || len(item.ContextTiers) != 1 || *item.ContextTiers[0].Prices.Input != "14" || *item.ModeSchedules[pricing.ModeFast].Prices.Input != "21" || *item.ModeSchedules[pricing.ModeUltrafast].Prices.Input != "28" {
			t.Fatalf("CNY schedule = %#v", item)
		}
	}
	assertCNY(envelope.Data.Items[0])
	body = fmt.Sprintf(`{"ids":[%d],"mode":"link","reference":{"price_id":%d}}`, rows[1].ID, rows[0].ID)
	response = serveModelPriceHTTPRequest(engine, http.MethodPost, "/api/model-prices/batch", body, authTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("link = %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	assertCNY(envelope.Data.Items[0])
	response = serveModelPriceHTTPRequest(engine, http.MethodGet, "/api/model-prices/sources", "", authTestKey)
	var sources struct {
		Data ModelPriceSourcesResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &sources); err != nil || response.Code != http.StatusOK || len(sources.Data.Items) != 2 {
		t.Fatalf("sources = %d %s, %v", response.Code, response.Body.String(), err)
	}
	for _, item := range sources.Data.Items {
		assertCNY(item.Price)
	}
	for _, row := range rows {
		var saved models.ModelPrice
		if err := fixture.db.First(&saved, row.ID).Error; err != nil {
			t.Fatal(err)
		}
		rule, err := persistedPriceRule(saved)
		if err != nil || int64(rule.Prices.Input.NanoUSDPerMillion) != 1_000_000_000 || int64(rule.ContextTiers[0].Prices.Input.NanoUSDPerMillion) != 2_000_000_000 || int64(rule.ModeSchedules[pricing.ModeFast].Prices.Input.NanoUSDPerMillion) != 3_000_000_000 || int64(rule.ModeSchedules[pricing.ModeUltrafast].Prices.Input.NanoUSDPerMillion) != 4_000_000_000 {
			t.Fatalf("stored schedule is not USD: %#v, %v", rule, err)
		}
	}
	assertUnified := func() {
		t.Helper()
		for mode, want := range map[pricing.Mode]int64{pricing.ModeStandard: 3_000_000_000, pricing.ModeFast: 6_000_000_000, pricing.ModeUltrafast: 9_000_000_000} {
			quote := quoteClientModel(fixture, rows[1].ModelID, mode)
			if int64(quote.EstimatedCostNanoUSD) != want {
				t.Fatalf("unified %s quote = %+v, want %d", mode, quote, want)
			}
		}
	}
	assertUnified()
	// Startup refresh, group writes, reload and plain load must all retain client rules.
	if err := fixture.service.EnsureInitialState(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertUnified()
	if _, err := fixture.service.UpdateGroupModels(t.Context(), sources.Data.Items[0].Groups[0].ID, GroupModelsUpdateRequest{Models: optionalGroupModels{Set: true, Values: []GroupModel{{ID: rows[0].ModelID}}}}); err != nil {
		t.Fatal(err)
	}
	assertUnified()
	if _, err := fixture.service.reloadCommittedConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertUnified()

	// Display round-trip at a non-integral rate follows the same single-price boundary.
	if _, err := updateCurrencySettings(t, fixture, `{"exchange_rate":"7.3"}`); err != nil {
		t.Fatal(err)
	}
	body = fmt.Sprintf(`{"ids":[%d],"mode":"copy","price":%s}`, rows[0].ID, modelPriceHTTPUpdateBody(`"30000"`, "false"))
	response = serveModelPriceHTTPRequest(engine, http.MethodPost, "/api/model-prices/batch", body, authTestKey)
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusOK || *envelope.Data.Items[0].Prices.Input != "30000" {
		t.Fatalf("round-trip = %d %s", response.Code, response.Body.String())
	}
	saved := assertFollowedPrice(t, fixture, rows[0], int64Pointer(4_109_589_041_096))
	if len(saved.ModePriceSchedules) != 0 || len(saved.ContextPriceTiers) != 0 || saved.CacheWritePriceNanoUSDPerMillionTokens != nil {
		t.Fatal("batch was not a full replacement")
	}
	assertUnified()
	if _, err := updateCurrencySettings(t, fixture, `{"exchange_rate":"0.000001"}`); err != nil {
		t.Fatal(err)
	}
	before := fixture.priceRuntime.Load()
	body = fmt.Sprintf(`{"ids":[%d],"mode":"copy","price":%s}`, rows[0].ID, modelPriceHTTPUpdateBody(`"100000000"`, "false"))
	response = serveModelPriceHTTPRequest(engine, http.MethodPost, "/api/model-prices/batch", body, authTestKey)
	if response.Code != http.StatusBadRequest || fixture.priceRuntime.Load() != before {
		t.Fatalf("overflow mutated prices: %d %s", response.Code, response.Body.String())
	}
}

func TestModelPriceSourcesAndBatchUseCommittedCurrencyOnStaleReplica(t *testing.T) {
	initControlI18n(t)
	first, dsn := newServiceFixtureWithSecondDSN(t)
	second := newServiceFixtureWithDSN(t, dsn)
	rows, _ := priceReferenceFixture(t, first, 2)
	if _, err := updateCurrencySettings(t, first, `{"currency":"CNY","exchange_rate":"7"}`); err != nil {
		t.Fatal(err)
	}
	if cached, err := second.service.currentCurrency(); err != nil || cached != pricing.USDCurrency {
		t.Fatalf("peer fixture did not retain its old USD snapshot: %+v, %v", cached, err)
	}
	assertInput := func(dto ModelPriceDTO, id uint, want string) {
		t.Helper()
		if dto.ID != id || dto.Currency != pricing.CurrencyCNY || dto.Prices.Input == nil || *dto.Prices.Input != want {
			t.Fatalf("price = %+v, want id %d input %s CNY", dto, id, want)
		}
	}

	// Both service methods use the system currency. Storage and the pricing table remain USD.
	const submitted = `{"input":"21","output":null,"cache_read":null,"cache_write":null,"context_tiers":[{"threshold_tokens":1000,"input":"42","output":null,"cache_read":null,"cache_write":null}],"mode_schedules":{"fast":{"prices":{"input":"63","output":null,"cache_read":null,"cache_write":null},"context_tiers":[]}}}`
	price := mustModelPriceUpdateRequest(t, submitted)
	copied, err := first.service.BatchModelPrices(t.Context(), ModelPriceBatchRequest{IDs: []uint{rows[1].ID}, Mode: "copy", Price: &price})
	if err != nil || len(copied.Items) != 1 {
		t.Fatalf("direct copy = %+v, %v", copied, err)
	}
	assertInput(copied.Items[0], rows[1].ID, "21")
	if !reflect.DeepEqual(price, mustModelPriceUpdateRequest(t, submitted)) {
		t.Fatal("currency conversion modified the caller's price")
	}
	assertFollowedPrice(t, first, rows[1], int64Pointer(3_000_000_000))
	sources, err := first.service.ModelPriceSources(t.Context())
	if err != nil || len(sources.Items) != 2 {
		t.Fatalf("direct sources = %+v, %v", sources, err)
	}
	assertInput(sources.Items[1].Price, rows[1].ID, "21")

	newEngine := func(service *Service) *gin.Engine {
		engine := gin.New()
		newTestServer(t, &config.Config{AuthKey: authTestKey}, service).RegisterRoutes(engine)
		return engine
	}
	readSource := func(engine *gin.Engine) string {
		t.Helper()
		response := serveModelPriceHTTPRequest(engine, http.MethodGet, "/api/model-prices/sources", "", authTestKey)
		var get struct{ Data ModelPriceSourcesResponse }
		if err := json.Unmarshal(response.Body.Bytes(), &get); err != nil || response.Code != http.StatusOK || len(get.Data.Items) != 2 {
			t.Fatalf("sources = %d %s, %v", response.Code, response.Body.String(), err)
		}
		source := get.Data.Items[0].Price
		assertInput(source, rows[0].ID, "14")
		return *source.Prices.Input
	}
	shown := readSource(newEngine(first.service))
	peer := newEngine(second.service)
	readSource(peer)
	// Submit the first replica's displayed amount unchanged to the stale replica.
	body := fmt.Sprintf(`{"ids":[%d],"mode":"copy","price":%s}`, rows[1].ID, modelPriceHTTPUpdateBody(fmt.Sprintf("%q", shown), "false"))
	response := serveModelPriceHTTPRequest(peer, http.MethodPost, "/api/model-prices/batch", body, authTestKey)
	var batch struct{ Data ModelPriceBatchResponse }
	if err := json.Unmarshal(response.Body.Bytes(), &batch); err != nil || response.Code != http.StatusOK || len(batch.Data.Items) != 1 {
		t.Fatalf("peer copy = %d %s, %v", response.Code, response.Body.String(), err)
	}
	assertInput(batch.Data.Items[0], rows[1].ID, shown)
	assertFollowedPrice(t, second, rows[1], int64Pointer(2_000_000_000))
}

func TestModelPriceBatchLinkDisplayOverflowRollsBack(t *testing.T) {
	fixture, engine, _ := newModelPriceHTTPFixture(t, false)
	rows, _ := priceReferenceFixture(t, fixture, 3)
	// The base price fits; only the nested tier overflows when projected at 7 CNY/USD.
	request := mustModelPriceUpdateRequest(t, `{"input":"2","output":null,"cache_read":null,"cache_write":null,"context_tiers":[{"threshold_tokens":1000,"input":"2000000000","output":null,"cache_read":null,"cache_write":null}],"mode_schedules":{}}`)
	if _, err := fixture.service.UpdateModelPrice(t.Context(), rows[0].ID, request); err != nil {
		t.Fatal(err)
	}
	if _, err := updateCurrencySettings(t, fixture, `{"currency":"CNY","exchange_rate":"7"}`); err != nil {
		t.Fatal(err)
	}
	readRows := func() []models.ModelPrice {
		t.Helper()
		var saved []models.ModelPrice
		if err := fixture.db.Order("id").Find(&saved).Error; err != nil {
			t.Fatal(err)
		}
		return saved
	}
	beforeRows := readRows()
	beforeRevision, ok := readStoredClusterRevision(t, fixture.db)
	if !ok {
		t.Fatal("fixture has no committed revision")
	}
	beforeTable := fixture.priceRuntime.Load()
	publisher := &recordingConfigEventPublisher{}
	fixture.service.clusterEvents = publisher
	body := fmt.Sprintf(`{"ids":[%d,%d],"mode":"link","reference":{"price_id":%d}}`, rows[1].ID, rows[2].ID, rows[0].ID)
	response := serveModelPriceHTTPRequest(engine, http.MethodPost, "/api/model-prices/batch", body, authTestKey)
	assertModelPriceHTTPError(t, response, http.StatusBadRequest, app_errors.ErrValidation.Code, nil)
	if after := readRows(); !reflect.DeepEqual(beforeRows, after) {
		t.Fatalf("rejected link changed durable prices: before %+v; after %+v", beforeRows, after)
	}
	if after, ok := readStoredClusterRevision(t, fixture.db); !ok || after != beforeRevision {
		t.Fatalf("rejected link changed revision from %s to %s", beforeRevision, after)
	}
	if changes := publisher.published(); len(changes) != 0 || fixture.priceRuntime.Load() != beforeTable {
		t.Fatalf("rejected link published a price table or change event: %+v", changes)
	}
}

func TestModelPriceLinkedCostsRemainHiddenFromAccessKey(t *testing.T) {
	fixture, engine, _ := newModelPriceHTTPFixture(t, false)
	rows, _ := priceReferenceFixture(t, fixture, 2)
	followPrice(t, fixture, rows[1].ID, rows[0].ID)
	request := mustModelPriceUpdateRequest(t, modelPriceHTTPUpdateBody(`"5"`, "false"))
	if _, err := fixture.service.PutClientModelPrice(t.Context(), ClientModelPriceRequest{ClientModel: rows[1].ModelID, ModelPriceUpdateRequest: request}); err != nil {
		t.Fatal(err)
	}
	key, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "linked-cost-viewer"})
	if err != nil {
		t.Fatal(err)
	}
	response := serveModelPriceHTTPRequest(engine, http.MethodGet, "/api/models?group_status=all&page_size=100", "", key.Key)
	var envelope struct {
		Data ProjectModelListResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusOK || len(envelope.Data.Items) != 2 {
		t.Fatalf("models = %d %s, %v", response.Code, response.Body.String(), err)
	}
	for _, item := range envelope.Data.Items {
		if item.ClientModel == rows[1].ModelID && (item.Price == nil || *item.Price.Prices.Input != "5") {
			t.Fatal("AccessKey lost its unified price")
		}
		for _, upstream := range item.UpstreamModels {
			price := upstream.Price
			if price.Prices.Input != nil || price.Prices.Output != nil || price.Prices.CacheRead != nil || price.Prices.CacheWrite != nil || len(price.ContextTiers) != 0 || len(price.ModeSchedules) != 0 || price.SourcePriceID != nil || price.Method != nil || price.MatchSource != nil || price.MatchedProviderID != nil || len(upstream.RouteGroups) != 0 || len(upstream.AffectedGroups) != 0 {
				t.Fatalf("AccessKey received upstream cost details: %#v", upstream)
			}
		}
	}
}
