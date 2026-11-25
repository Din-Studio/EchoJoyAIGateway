package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"gorm.io/gorm"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/storage/models"
)

func TestModelPriceSourcesAndBatchRequireManagementAuth(t *testing.T) {
	fixture, engine, _ := newModelPriceHTTPFixture(t, true)
	accessKey, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "price-read-only"})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/model-prices/sources", ""},
		{http.MethodPost, "/api/model-prices/batch", `{}`},
	} {
		response := serveModelPriceHTTPRequest(engine, request.method, request.path, request.body, "")
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s auth status = %d", request.path, response.Code)
		}
		response = serveModelPriceHTTPRequest(engine, request.method, request.path, request.body, accessKey.Key)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s access key status = %d", request.path, response.Code)
		}
	}
}

func TestModelPriceBatchDatabaseFailureRollsBackEveryTarget(t *testing.T) {
	fixture, _, first := newModelPriceHTTPFixture(t, true)
	second := models.ModelPrice{ChannelID: first.ChannelID, ModelID: "second-copy"}
	if err := fixture.db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	before := mustLoadPriceTable(t, fixture.db)
	fixture.priceRuntime.Publish(before)
	updates := 0
	const callback = "test:batch-price-failure"
	if err := fixture.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "model_prices" {
			updates++
			if updates == 2 {
				tx.AddError(errors.New("injected price write failure"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.db.Callback().Update().Remove(callback) })
	var price ModelPriceUpdateRequest
	if err := json.Unmarshal([]byte(modelPriceHTTPUpdateBody(`"7"`, "false")), &price); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.BatchModelPrices(t.Context(), ModelPriceBatchRequest{IDs: []uint{first.ID, second.ID}, Mode: "copy", Price: &price}); err == nil {
		t.Fatal("batch ignored a database write failure")
	}
	if updates != 2 || fixture.priceRuntime.Load() != before {
		t.Fatal("failed batch published partial prices")
	}
	var saved models.ModelPrice
	if err := fixture.db.First(&saved, first.ID).Error; err != nil || saved.IsManual || *saved.InputPriceNanoUSDPerMillionTokens != 2_500_000_000 {
		t.Fatalf("failed batch did not roll back the first write: %#v, %v", saved, err)
	}
}

func TestModelPriceBatchBoundsAndFullReplacement(t *testing.T) {
	fixture := newServiceFixture(t)
	rows, _ := priceReferenceFixture(t, fixture, maxModelPriceBatchSize)
	ids := make([]uint, len(rows))
	for index := range rows {
		ids[index] = rows[index].ID
	}
	price := mustModelPriceUpdateRequest(t, modelPriceHTTPUpdateBody(`"0"`, "false"))
	before := fixture.priceRuntime.Load()
	for _, invalid := range [][]uint{nil, {0}, {uint(maxSafeInteger) + 1}, append(append([]uint(nil), ids...), ids[0])} {
		_, err := fixture.service.BatchModelPrices(t.Context(), ModelPriceBatchRequest{IDs: invalid, Mode: "copy", Price: &price})
		if !errors.Is(err, app_errors.ErrValidation) {
			t.Fatalf("invalid batch returned %v", err)
		}
	}
	if fixture.priceRuntime.Load() != before {
		t.Fatal("invalid batch published prices")
	}
	result, err := fixture.service.BatchModelPrices(t.Context(), ModelPriceBatchRequest{IDs: ids, Mode: "copy", Price: &price})
	if err != nil || len(result.Items) != maxModelPriceBatchSize {
		t.Fatalf("100-target batch = %d, %v", len(result.Items), err)
	}
	for _, item := range result.Items {
		if item.Prices.Input == nil || *item.Prices.Input != "0" || item.Method == nil || *item.Method != "user_set" {
			t.Fatalf("zero price lost: %#v", item)
		}
	}
	clear := mustModelPriceUpdateRequest(t, modelPriceHTTPUpdateBody("null", "false"))
	_, err = fixture.service.BatchModelPrices(t.Context(), ModelPriceBatchRequest{IDs: ids, Mode: "copy", Price: &clear})
	var apiErr *app_errors.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != app_errors.ErrModelPriceUnpricedConfirmationRequired.Code {
		t.Fatalf("clear without confirmation = %v", err)
	}
	clear.ConfirmUnpriced = true
	result, err = fixture.service.BatchModelPrices(t.Context(), ModelPriceBatchRequest{IDs: ids, Mode: "copy", Price: &clear})
	if err != nil || len(result.Items) != maxModelPriceBatchSize {
		t.Fatalf("clear = %d, %v", len(result.Items), err)
	}
	for _, item := range result.Items {
		if item.Prices.Input != nil || item.Method == nil || *item.Method != "user_marked_unpriced" {
			t.Fatalf("clear did not replace zero: %#v", item)
		}
	}
}
