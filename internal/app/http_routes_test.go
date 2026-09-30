package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/platform/httproute"
	"gpt-load/internal/platform/version"
)

type readinessProbeFunc func(context.Context) map[string]error

func (probe readinessProbeFunc) Check(ctx context.Context) map[string]error {
	return probe(ctx)
}

func serveHealth(t *testing.T, probe ReadinessProbe) *httptest.ResponseRecorder {
	t.Helper()
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	registry, err := httproute.NewRegistry(HTTPModule(probe))
	if err != nil {
		t.Fatalf("NewRegistry(system) error = %v", err)
	}
	if err := registry.Bind(engine); err != nil {
		t.Fatalf("Bind(system) error = %v", err)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	return recorder
}

func TestHealthWithoutProbeKeepsStaticBody(t *testing.T) {
	recorder := serveHealth(t, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	gin.SetMode(gin.ReleaseMode)
	expected, err := json.Marshal(gin.H{"status": "ok", "version": version.Version})
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.Body.String(); got != string(expected) {
		t.Fatalf("body = %s, want %s", got, expected)
	}
}

func TestHealthWithProbeReportsChecks(t *testing.T) {
	healthy := readinessProbeFunc(func(context.Context) map[string]error {
		return map[string]error{"database": nil, "redis": nil}
	})
	recorder := serveHealth(t, healthy)
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthy status = %d, want 200", recorder.Code)
	}
	var body struct {
		Status  string            `json:"status"`
		Version string            `json:"version"`
		Checks  map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode healthy body: %v", err)
	}
	if body.Status != "ok" || body.Version != version.Version ||
		body.Checks["database"] != "ok" || body.Checks["redis"] != "ok" {
		t.Fatalf("healthy body = %#v", body)
	}

	var sawDeadline bool
	failing := readinessProbeFunc(func(ctx context.Context) map[string]error {
		_, sawDeadline = ctx.Deadline()
		return map[string]error{"database": nil, "redis": errors.New("dial tcp: connection refused")}
	})
	recorder = serveHealth(t, failing)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing status = %d, want 503", recorder.Code)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode failing body: %v", err)
	}
	if body.Status != "unavailable" || body.Checks["database"] != "ok" ||
		body.Checks["redis"] != "dial tcp: connection refused" {
		t.Fatalf("failing body = %#v", body)
	}
	if !sawDeadline {
		t.Fatal("probe context has no deadline")
	}
}
