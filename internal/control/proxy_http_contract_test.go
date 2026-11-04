package control

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/platform/config"
	"gpt-load/internal/storage/models"
)

func TestProxySecretMutationsRequireIdempotencyAndKeepResponseShape(t *testing.T) {
	initControlI18n(t)
	fixture := newServiceFixture(t)
	engine := gin.New()
	newTestServer(t, &config.Config{AuthKey: authTestKey}, fixture.service).RegisterRoutes(engine)
	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+authTestKey)
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		return recorder
	}
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/api/proxies", `{"url":"http://user:private-password@proxy.invalid:8080"}`},
		{"PUT", "/api/proxies/1", `{"name":"renamed"}`},
		{"POST", "/api/proxies/import", `{"entries":"http://proxy.invalid:8080"}`},
	} {
		for _, key := range []string{"", "not-a-uuid", "00000000-0000-1000-8000-000000000001"} {
			want := http.StatusBadRequest
			if key == "" {
				want = http.StatusPreconditionRequired
			}
			recorder := call(route.method, route.path, route.body, key)
			if recorder.Code != want {
				t.Fatalf("%s %s key=%q: status=%d body=%s", route.method, route.path, key, recorder.Code, recorder.Body.String())
			}
		}
	}
	if count := mustCount(t, fixture, &models.Proxy{}, "1=1"); count != 0 {
		t.Fatal("missing/invalid keys mutated catalog")
	}
	const key = "ee000000-0000-4000-8000-000000000001"
	const body = `{"url":"http://user:private-password@proxy.invalid:8080"}`
	created := call("POST", "/api/proxies", body, key)
	replayed := call("POST", "/api/proxies", body, key)
	if created.Code != http.StatusOK || replayed.Code != http.StatusOK || !bytes.Equal(created.Body.Bytes(), replayed.Body.Bytes()) {
		t.Fatalf("create/replay contract: first=%d %s replay=%d %s", created.Code, created.Body.String(), replayed.Code, replayed.Body.String())
	}
	for _, forbidden := range []string{"private-password", "operation_id", "canonical_result"} {
		if strings.Contains(created.Body.String(), forbidden) {
			t.Fatalf("response exposed %q", forbidden)
		}
	}
}

func TestProxyRoutesDenyAccessKeysAndAuditNonIdempotentTests(t *testing.T) {
	initControlI18n(t)
	fixture := newServiceFixture(t)
	item, err := fixture.service.SaveProxy(t.Context(), 0, ProxySaveRequest{URL: "http://proxy.invalid:8080"})
	if err != nil {
		t.Fatal(err)
	}
	access, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "proxy-readonly"})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server := newTestServer(t, &config.Config{AuthKey: authTestKey}, fixture.service)
	server.logger = newControlJSONLogger(&logs)
	engine := gin.New()
	server.RegisterRoutes(engine)
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		return recorder
	}
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/proxies", ""},
		{"POST", "/api/proxies", `{}`},
		{"PUT", fmt.Sprintf("/api/proxies/%d", item.ID), `{}`},
		{"POST", fmt.Sprintf("/api/proxies/%d/reveal", item.ID), `{}`},
		{"POST", "/api/proxies/impact", `{"ids":[1]}`},
		{"POST", "/api/proxies/batch", `{"ids":[1],"action":"delete"}`},
		{"POST", "/api/proxies/import", `{"entries":""}`},
		{"POST", fmt.Sprintf("/api/proxies/%d/test", item.ID), `{"url":"http://probe.invalid"}`},
		{"PUT", "/api/proxies/test-url", `{"url":"http://probe.invalid"}`},
	} {
		result := call(route.method, route.path, route.body, access.Key)
		if result.Code != http.StatusForbidden {
			t.Fatalf("access key reached %s %s: %d", route.method, route.path, result.Code)
		}
	}
	// Full capacity rejects immediately, without requiring Idempotency-Key.
	var releases []func()
	for range 8 {
		release, acquired, err := fixture.concurrency.AcquireProxyTest(t.Context())
		if err != nil || !acquired {
			t.Fatal("failed to occupy test slot")
		}
		releases = append(releases, release)
		defer release()
	}
	full := call("POST", fmt.Sprintf("/api/proxies/%d/test", item.ID), `{"url":"http://probe.invalid"}`, authTestKey)
	if full.Code != http.StatusTooManyRequests || full.Header().Get("Retry-After") != "1" {
		t.Fatalf("full probe pool response: %d %s", full.Code, full.Body.String())
	}
	preference := call("PUT", "/api/proxies/test-url", `{"url":"http://probe.invalid"}`, authTestKey)
	if preference.Code != http.StatusOK {
		t.Fatalf("test preference requires unexpected key: %d", preference.Code)
	}
	for _, release := range releases {
		release()
	}
	fixture.redis.Close()
	unavailable := call("POST", fmt.Sprintf("/api/proxies/%d/test", item.ID), `{"url":"http://probe.invalid"}`, authTestKey)
	if unavailable.Code != http.StatusServiceUnavailable || unavailable.Header().Get("Retry-After") != "1" {
		t.Fatalf("Redis failure response: %d %s", unavailable.Code, unavailable.Body.String())
	}
	events := controlEventsNamed(decodeControlJSONLogs(t, logs.Bytes()), "mutation")
	seen := make(map[string]bool)
	for _, event := range events {
		seen[event["operation"].(string)] = true
	}
	if !seen["proxy_test"] || !seen["proxy_test_url"] {
		t.Fatalf("non-idempotent mutations lack audit records: %#v", seen)
	}
}
