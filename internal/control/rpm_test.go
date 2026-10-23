package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/state"
)

func TestRPMCollectionsSortBeforePagination(t *testing.T) {
	one, high := int64(1), int64(80)
	records := []accessKeyCollectionRecord{
		accessKeyCollectionQueryRecord(1, "first", "0001", state.AccessKeyStatusActive, 300),
		accessKeyCollectionQueryRecord(2, "second", "0002", state.AccessKeyStatusActive, 200),
		accessKeyCollectionQueryRecord(3, "third", "0003", state.AccessKeyStatusActive, 100),
	}
	records[0].RPMPeakHour = &one
	records[2].RPMPeakHour = &high
	result := queryAccessKeyCollectionRecords(records, AccessKeyCollectionQuery{Sort: "rpm_peak_desc", Page: 1, PageSize: 1})
	if len(result.Items) != 1 || result.Items[0].ID != 3 {
		t.Fatalf("RPM sort paged too early: %+v", result.Items)
	}
	if !modernCredentialLess(credentialCollectionRecord{item: CredentialItemResponse{CredentialID: 3, RPMPeakHour: &high}}, credentialCollectionRecord{item: CredentialItemResponse{CredentialID: 1, RPMPeakHour: &one}}, "rpm_peak_desc") {
		t.Fatal("upstream RPM sort was ignored")
	}
	if _, err := parseAccessKeyCollectionQuery("sort=rpm_peak_desc", false); err != nil {
		t.Fatal(err)
	}
	if _, err := parseModernCredentialQuery("sort=rpm_peak_desc"); err != nil {
		t.Fatal(err)
	}
}

func TestRPMPeakSortIsModernOnly(t *testing.T) {
	initControlI18n(t)
	fixture := newServiceFixture(t)
	created, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "rpm-peak"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.NewAccessKeyRPM(fixture.cluster).Allow(t.Context(), created.ID, 0); err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	newTestServer(t, &config.Config{AuthKey: authTestKey}, fixture.service).RegisterRoutes(engine)
	result := performGroupCollectionRequest(engine, "/api/access-keys?sort=rpm_peak_desc", "Bearer "+authTestKey)
	if result.Code != http.StatusBadRequest {
		t.Fatalf("classic RPM sort status = %d, want 400: %s", result.Code, result.Body.String())
	}
	result = performGroupCollectionRequest(engine, "/api/modern/access-keys?sort=rpm_peak_desc", "Bearer "+authTestKey)
	if result.Code != http.StatusOK {
		t.Fatalf("modern RPM sort status = %d, want 200: %s", result.Code, result.Body.String())
	}
	var page struct {
		Items []struct {
			ID          uint   `json:"id"`
			RPMPeakHour *int64 `json:"rpm_peak_hour"`
		} `json:"items"`
	}
	if err := json.Unmarshal(decodeGroupCollectionSuccessData(t, result), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].RPMPeakHour == nil || *page.Items[0].RPMPeakHour != 1 {
		t.Fatalf("modern items = %+v", page.Items)
	}
}

func TestRPMCredentialQueryUsesOwnershipAndAdminBoundary(t *testing.T) {
	initControlI18n(t)
	fixture, group, credentialID := newSubscriptionCredentialFixture(t)
	stats := cluster.NewRPMStats(fixture.cluster)
	for range 7 {
		stats.RecordCredential(t.Context(), credentialID)
	}
	engine := gin.New()
	newTestServer(t, &config.Config{AuthKey: authTestKey}, fixture.service).RegisterRoutes(engine)
	path := fmt.Sprintf("/api/modern/groups/%d/credentials/%d/rpm", group, credentialID)
	result := performGroupCollectionRequest(engine, path, "Bearer "+authTestKey)
	if result.Code != http.StatusOK {
		t.Fatalf("RPM query: %d %s", result.Code, result.Body.String())
	}
	var report cluster.RPMReport
	if err := json.Unmarshal(decodeGroupCollectionSuccessData(t, result), &report); err != nil {
		t.Fatal(err)
	}
	if report.Peak == nil || *report.Peak != 7 || report.Requests != 7 || report.Current == nil || report.Current.Requests != 7 {
		t.Fatalf("report: %+v", report)
	}
	if got := performGroupCollectionRequest(engine, path, ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", got.Code)
	}
	accessKey, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "rpm-reader"})
	if err != nil {
		t.Fatal(err)
	}
	if got := performGroupCollectionRequest(engine, path, "Bearer "+accessKey.Key); got.Code != http.StatusForbidden {
		t.Fatalf("access key could read upstream statistics: %d", got.Code)
	}
	accessPath := fmt.Sprintf("/api/access-keys/%d/rpm", accessKey.ID)
	if got := performGroupCollectionRequest(engine, accessPath, "Bearer "+accessKey.Key); got.Code != http.StatusForbidden {
		t.Fatalf("new endpoint bypassed access-key whitelist: %d", got.Code)
	}
	if got := performGroupCollectionRequest(engine, accessPath, "Bearer "+authTestKey); got.Code != http.StatusOK {
		t.Fatalf("admin could not read access-key statistics: %d %s", got.Code, got.Body.String())
	}
	wrong := fmt.Sprintf("/api/modern/groups/%d/credentials/%d/rpm", group+1, credentialID)
	if got := performGroupCollectionRequest(engine, wrong, "Bearer "+authTestKey); got.Code != http.StatusNotFound {
		t.Fatalf("cross-group request: %d %s", got.Code, got.Body.String())
	}
	if got := performGroupCollectionRequest(engine, path+"?range=7d", "Bearer "+authTestKey); got.Code != http.StatusBadRequest {
		t.Fatalf("unsupported range: %d", got.Code)
	}
}
