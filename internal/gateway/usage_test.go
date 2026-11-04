package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
)

func usageQuotaRules() []accessquota.Rule {
	return []accessquota.Rule{
		{ID: 1, Revision: 1, Kind: accessquota.KindPeriodic, Currency: "USD", LimitNano: 5_000_000_000, PeriodSeconds: 300},
		{ID: 2, Revision: 1, Kind: accessquota.KindTotal, Currency: "USD", LimitNano: 100_000_000_000},
	}
}

func TestUsageReturnsOnlyTotalQuotaEvenWhenExhausted(t *testing.T) {
	for _, test := range []struct {
		name      string
		path      string
		used      int64
		remaining float64
	}{
		{"periodic exhausted", "/v1/user/balance", 8_000_000_000, 92},
		{"unversioned periodic exhausted", "/user/balance", 8_000_000_000, 92},
		{"total exceeded", "/v1/user/balance", 110_000_000_000, 0},
		{"unversioned total exceeded", "/user/balance", 110_000_000_000, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarder := &scriptedForwarder{}
			limiter := &recordingAccessKeyRPMLimiter{}
			sink := &recordingRequestLogSink{}
			engine, handler, manager, _ := newRequestLogHandlerTestRuntime(t, forwarder, limiter, sink)
			if _, err := manager.Publish(gatewayAccessQuotaCompileInput(handler, usageQuotaRules())); err != nil {
				t.Fatal(err)
			}
			quota := useSharedAccessQuota(t, handler)
			now := time.Unix(1000, 0)
			consumeGatewayQuota(t, quota, manager, now, test.used)
			handler.now = func() time.Time { return now.Add(time.Minute) }
			req := httptest.NewRequest(http.MethodGet, test.path+"?access_key_id=2", nil)
			req.Header.Set("Authorization", "Bearer gl-client")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			var data map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
				t.Fatal(err)
			}
			if len(data) != 4 || data["is_active"] != true || data["total"] != float64(100) || data["used"] != float64(test.used)/1e9 || data["balance"] != test.remaining {
				t.Fatalf("unexpected usage: %v", data)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
			if len(limiter.snapshot()) != 0 || len(sink.snapshot()) != 0 {
				t.Fatal("usage consumed RPM or emitted an inference log")
			}
			if len(forwarder.inputs) != 0 {
				t.Fatal("usage was forwarded")
			}
		})
	}
}

type testAccessKeyUsageReader struct {
	used       int64
	err        error
	ids        []uint
	currencies []pricing.CurrencyCode
}

func (reader *testAccessKeyUsageReader) QueryAccessKeyTotalCost(_ context.Context, id uint, currency pricing.CurrencyCode) (int64, error) {
	reader.ids = append(reader.ids, id)
	reader.currencies = append(reader.currencies, currency)
	return reader.used, reader.err
}

func TestUsageWithoutTotalQuotaReturnsCumulativeCost(t *testing.T) {
	for _, periodic := range []bool{false, true} {
		engine, handler, manager, _ := newRequestLogHandlerTestRuntime(t, &scriptedForwarder{}, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{})
		reader := &testAccessKeyUsageReader{used: 12_500_000_000}
		handler.usageReader = reader
		if periodic {
			if _, err := manager.Publish(gatewayAccessQuotaCompileInput(handler, usageQuotaRules()[:1])); err != nil {
				t.Fatal(err)
			}
			quota := useSharedAccessQuota(t, handler)
			consumeGatewayQuota(t, quota, manager, time.Now(), 2_000_000_000)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/user/balance?access_key_id=99", nil)
		req.Header.Set("Authorization", "Bearer gl-client")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		if response.Code != http.StatusOK || response.Body.String() != `{"is_active":true,"balance":0,"used":12.5,"total":0}` {
			t.Fatalf("periodic=%v: %d %s", periodic, response.Code, response.Body.String())
		}
		if len(reader.ids) != 1 || reader.ids[0] != 1 || reader.currencies[0] != manager.Current().Settings.Currency.Code {
			t.Fatalf("queried keys = %v in %v", reader.ids, reader.currencies)
		}
	}
}

func TestUsageFailsClosedWhenSharedQuotaIsUnavailable(t *testing.T) {
	engine, handler, manager, _ := newRequestLogHandlerTestRuntime(t, &scriptedForwarder{}, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{})
	if _, err := manager.Publish(gatewayAccessQuotaCompileInput(handler, usageQuotaRules())); err != nil {
		t.Fatal(err)
	}
	reader := &testAccessKeyUsageReader{}
	handler.usageReader = reader
	handler.accessQuota = &scriptedAccessQuotaGate{checkErr: errors.New("redis: connection refused")}
	req := httptest.NewRequest(http.MethodGet, "/v1/user/balance", nil)
	req.Header.Set("Authorization", "Bearer gl-client")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" ||
		!strings.Contains(response.Body.String(), "cluster_state_unavailable") || len(reader.ids) != 0 {
		t.Fatalf("response = %d %v %s, queried %v", response.Code, response.Header(), response.Body.String(), reader.ids)
	}
}

func TestUsagePreservesAuthenticationAndErrors(t *testing.T) {
	for _, test := range []struct {
		name, key, method, path string
		status                  int
		failure                 bool
	}{
		{name: "unversioned missing key", path: "/user/balance", method: http.MethodGet, status: http.StatusUnauthorized},
		{name: "unversioned invalid key", path: "/user/balance", key: "invalid", method: http.MethodGet, status: http.StatusUnauthorized},
		{name: "missing key", method: http.MethodGet, status: http.StatusUnauthorized},
		{name: "invalid key", key: "invalid", method: http.MethodGet, status: http.StatusUnauthorized},
		{name: "wrong method", key: "gl-client", method: http.MethodPost, status: http.StatusMethodNotAllowed},
		{name: "storage failure", key: "gl-client", method: http.MethodGet, status: http.StatusServiceUnavailable, failure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine, handler, _, _ := newRequestLogHandlerTestRuntime(t, &scriptedForwarder{}, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{})
			reader := &testAccessKeyUsageReader{}
			if test.failure {
				reader.err = errors.New("private storage detail")
			}
			handler.usageReader = reader
			path := test.path
			if path == "" {
				path = "/v1/user/balance"
			}
			req := httptest.NewRequest(test.method, path, nil)
			if test.key != "" {
				req.Header.Set("Authorization", "Bearer "+test.key)
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, req)
			if response.Code != test.status || strings.Contains(response.Body.String(), "private storage detail") {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if !test.failure && len(reader.ids) != 0 {
				t.Fatalf("queried before auth/method check: %v", reader.ids)
			}
		})
	}
}

func TestUsageRejectsDisabledAndExpiredKeys(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		engine, handler, manager, _ := newRequestLogHandlerTestRuntime(t, &scriptedForwarder{}, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{})
		input := gatewayAccessQuotaCompileInput(handler, nil)
		if disabled {
			input.AccessKeys[0].Status = state.AccessKeyStatusDisabled
		} else {
			expired := int64(1)
			input.AccessKeys[0].ExpiresAtMS = &expired
		}
		if _, err := manager.Publish(input); err != nil {
			t.Fatal(err)
		}
		reader := &testAccessKeyUsageReader{}
		handler.usageReader = reader
		req := httptest.NewRequest(http.MethodGet, "/v1/user/balance", nil)
		req.Header.Set("Authorization", "Bearer gl-client")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized || len(reader.ids) != 0 {
			t.Fatalf("response = %d %s, queried %v", response.Code, response.Body.String(), reader.ids)
		}
	}
}

func TestResponseBodyShapeReportsKeysWithoutContent(t *testing.T) {
	for _, test := range []struct {
		body, keys, usageType string
	}{
		{`{"id":"x","choices":[{"message":{"content":"secret"}}]}`, "choices,id", "absent"},
		{`{"usage":null,"id":"x"}`, "id,usage", "null"},
		{`{"usage":{"prompt_tokens":1}}`, "usage", "object"},
		{`data: not json`, "", "non_json"},
	} {
		keys, usageType := responseBodyShape([]byte(test.body))
		if keys != test.keys || usageType != test.usageType {
			t.Errorf("responseBodyShape(%s) = %q/%q, want %q/%q", test.body, keys, usageType, test.keys, test.usageType)
		}
	}
}
