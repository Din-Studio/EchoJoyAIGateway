package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/platform/config"
	"gpt-load/internal/storage/models"
)

func TestAccessKeyGetHTTPReturnsMetadataByID(t *testing.T) {
	t.Parallel()
	initControlI18n(t)
	fixture := newServiceFixture(t)
	created, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{
		Name:    "alpha",
		Filters: &AccessKeyFilters{Models: []string{"gpt-4o"}},
	})
	if err != nil {
		t.Fatalf("create access key: %v", err)
	}
	var row models.AccessKey
	if err := fixture.db.First(&row, created.ID).Error; err != nil {
		t.Fatalf("load access key row: %v", err)
	}

	engine := gin.New()
	newTestServer(t, &config.Config{AuthKey: authTestKey}, fixture.service).RegisterRoutes(engine)
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/access-keys/%d", created.ID), nil)
	request.Header.Set("Authorization", "Bearer "+authTestKey)
	request.Header.Set("Accept-Language", "en-US")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/access-keys/%d = %d %s, want 200", created.ID, recorder.Code, recorder.Body.String())
	}

	data := decodeAccessKeyCollectionSuccessData(t, recorder)
	var result AccessKeyMetadata
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if result.ID != created.ID || result.Name != "alpha" ||
		result.MaskedKey != created.MaskedKey ||
		len(result.Filters.Models) != 1 || result.Filters.Models[0] != "gpt-4o" ||
		result.CostLimitRules == nil || result.CostLimitStatus != nil {
		t.Fatalf("metadata = %#v, want created access key metadata", result)
	}
	for _, forbidden := range []string{created.Key, row.KeyValue} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Fatalf("response exposed %q: %s", forbidden, recorder.Body.String())
		}
	}
}

func TestAccessKeyGetHTTPRejectsInvalidAndMissingID(t *testing.T) {
	t.Parallel()
	initControlI18n(t)
	fixture := newServiceFixture(t)
	engine := gin.New()
	newTestServer(t, &config.Config{AuthKey: authTestKey}, fixture.service).RegisterRoutes(engine)

	cases := []struct {
		path       string
		wantStatus int
		wantCode   string
	}{
		{path: "/api/access-keys/0", wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{path: "/api/access-keys/not-a-number", wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{path: "/api/access-keys/424242", wantStatus: http.StatusNotFound, wantCode: "NOT_FOUND"},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodGet, tc.path, nil)
		request.Header.Set("Authorization", "Bearer "+authTestKey)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		assertAccessKeyCollectionHTTPError(t, recorder, tc.wantStatus, tc.wantCode)
	}
}
