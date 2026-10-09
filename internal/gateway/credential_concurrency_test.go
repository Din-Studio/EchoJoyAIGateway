package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/cluster"
	"gpt-load/internal/state"
	"gpt-load/internal/testutil/clustertest"
)

type concurrencyCall struct {
	credentialID uint
	limit        int
}

// scriptedConcurrencyLimiter reports the first rejectCalls acquisitions as
// full, regardless of which credential the scheduler picks first, and tracks
// how many slots are held per credential.
type scriptedConcurrencyLimiter struct {
	mu          sync.Mutex
	rejectCalls int
	err         error
	calls       []concurrencyCall
	held        map[uint]int
}

func (limiter *scriptedConcurrencyLimiter) Acquire(
	_ context.Context, credentialID uint, limit int,
) (func(), bool, error) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.calls = append(limiter.calls, concurrencyCall{credentialID: credentialID, limit: limit})
	if limiter.err != nil {
		return nil, false, limiter.err
	}
	if len(limiter.calls) <= limiter.rejectCalls {
		return nil, false, nil
	}
	if limiter.held == nil {
		limiter.held = make(map[uint]int)
	}
	limiter.held[credentialID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			limiter.mu.Lock()
			limiter.held[credentialID]--
			limiter.mu.Unlock()
		})
	}, true, nil
}

func (limiter *scriptedConcurrencyLimiter) heldCount(credentialID uint) int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return limiter.held[credentialID]
}

func (limiter *scriptedConcurrencyLimiter) totalHeld() int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	total := 0
	for _, count := range limiter.held {
		total += count
	}
	return total
}

func (limiter *scriptedConcurrencyLimiter) snapshot() []concurrencyCall {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return append([]concurrencyCall(nil), limiter.calls...)
}

func setCredentialConcurrencyLimits(t *testing.T, registry *state.CredentialRegistry, limits map[uint]int) {
	t.Helper()
	ids := make([]uint, 0, len(limits))
	for id := range limits {
		ids = append(ids, id)
	}
	entries, err := registry.SnapshotGroupCredentialEntriesExact(1, ids)
	if err != nil {
		t.Fatal(err)
	}
	for index := range entries {
		entries[index].ConcurrencyLimit = limits[entries[index].ID]
	}
	if err := registry.RestoreGroupCredentialEntriesExact(1, entries); err != nil {
		t.Fatal(err)
	}
}

func serveConcurrencyRequest(engine http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gl-client")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func okUpstreamResult() UpstreamResult {
	return UpstreamResult{StatusCode: http.StatusOK, Header: make(http.Header), Body: []byte(`{"ok":true}`), RequestWritten: true}
}

func TestCredentialConcurrencySkipsFullCredentialWithoutSpendingRetry(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{okUpstreamResult()}}
	engine, handler, manager, registry := newRequestLogHandlerTestRuntime(
		t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-first", "sk-second",
	)
	manager.Current().Settings.RetryCount = 0
	setCredentialConcurrencyLimits(t, registry, map[uint]int{1: 3, 2: 3})
	limiter := &scriptedConcurrencyLimiter{rejectCalls: 1}
	handler.concurrency = limiter

	response := serveConcurrencyRequest(engine, `{"model":"gpt-4o"}`)

	calls := limiter.snapshot()
	if response.Code != http.StatusOK || len(forwarder.inputs) != 1 || len(calls) != 2 {
		t.Fatalf("status=%d forwards=%d calls=%#v body=%s", response.Code, len(forwarder.inputs), calls, response.Body.String())
	}
	if calls[0].credentialID == calls[1].credentialID || calls[0].limit != 3 || calls[1].limit != 3 {
		t.Fatalf("acquire calls = %#v, want two distinct credentials with limit 3", calls)
	}
	if got := forwarder.inputs[0].Credential.ID; got != calls[1].credentialID {
		t.Fatalf("forwarded credential = %d, want the one with a free slot (%d)", got, calls[1].credentialID)
	}
	for _, view := range registry.Snapshot() {
		if view.FailureCount != 0 || !view.CooldownUntil.IsZero() {
			t.Fatalf("full credential was marked unhealthy: %#v", view)
		}
	}
	if held := limiter.totalHeld(); held != 0 {
		t.Fatalf("slots held after the request = %d, want 0", held)
	}
}

func TestCredentialConcurrencyRejectsWhenEveryCandidateIsFull(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{okUpstreamResult()}}
	engine, handler, _, registry := newRequestLogHandlerTestRuntime(
		t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-first", "sk-second",
	)
	setCredentialConcurrencyLimits(t, registry, map[uint]int{1: 1, 2: 1})
	limiter := &scriptedConcurrencyLimiter{rejectCalls: 100}
	handler.concurrency = limiter

	response := serveConcurrencyRequest(engine, `{"model":"gpt-4o"}`)

	if response.Code != http.StatusTooManyRequests ||
		!strings.Contains(response.Body.String(), `"code":"credential_concurrency_limited"`) ||
		response.Header().Get("Retry-After") != "1" {
		t.Fatalf("response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if len(forwarder.inputs) != 0 || len(limiter.snapshot()) != 2 {
		t.Fatalf("forwards=%d acquire calls=%#v, want 0 forwards after trying both credentials", len(forwarder.inputs), limiter.snapshot())
	}
}

func TestCredentialConcurrencyHoldsSlotUntilUpstreamExchangeReturns(t *testing.T) {
	streamResult := UpstreamResult{StatusCode: http.StatusOK, Committed: true, Stream: StreamObservation{EndReason: StreamEndCleanEOF}}
	for _, test := range []struct {
		name   string
		body   string
		stream bool
	}{
		{name: "unary", body: `{"model":"gpt-4o"}`},
		{name: "stream", body: `{"model":"gpt-4o","stream":true}`, stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarder := &scriptedForwarder{
				results:       []UpstreamResult{okUpstreamResult()},
				streamResults: []UpstreamResult{streamResult},
			}
			engine, handler, _, registry := newRequestLogHandlerTestRuntime(
				t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-only",
			)
			setCredentialConcurrencyLimits(t, registry, map[uint]int{1: 1})
			limiter := &scriptedConcurrencyLimiter{}
			handler.concurrency = limiter
			heldDuringExchange := -1
			forwarder.onCall = func(int) { heldDuringExchange = limiter.heldCount(1) }
			forwarder.onStreamCall = func(int, http.ResponseWriter) { heldDuringExchange = limiter.heldCount(1) }

			response := serveConcurrencyRequest(engine, test.body)

			forwards := len(forwarder.inputs)
			if test.stream {
				forwards = len(forwarder.streamInputs)
			}
			if response.Code != http.StatusOK || forwards != 1 {
				t.Fatalf("status=%d forwards=%d body=%s", response.Code, forwards, response.Body.String())
			}
			if heldDuringExchange != 1 {
				t.Fatalf("slots held during the upstream exchange = %d, want 1", heldDuringExchange)
			}
			if held := limiter.heldCount(1); held != 0 {
				t.Fatalf("slots held after the exchange = %d, want 0", held)
			}
		})
	}
}

func TestCredentialConcurrencyReleasesSlotOnRetryAndEarlyReturn(t *testing.T) {
	t.Run("retry to next credential", func(t *testing.T) {
		failure := UpstreamResult{
			StatusCode: http.StatusForbidden, RequestWritten: true, Header: make(http.Header),
			Body: []byte(`{"error":{"message":"unclassified upstream rejection"}}`),
		}
		forwarder := &scriptedForwarder{results: []UpstreamResult{failure, okUpstreamResult()}}
		engine, handler, manager, registry := newRequestLogHandlerTestRuntime(
			t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-first", "sk-second",
		)
		manager.Current().Settings.RetryCount = 1
		setCredentialConcurrencyLimits(t, registry, map[uint]int{1: 1, 2: 1})
		limiter := &scriptedConcurrencyLimiter{}
		handler.concurrency = limiter
		heldPerForward := []int{}
		forwarder.onCall = func(int) { heldPerForward = append(heldPerForward, limiter.totalHeld()) }

		response := serveConcurrencyRequest(engine, `{"model":"gpt-4o"}`)

		if response.Code != http.StatusOK || len(forwarder.inputs) != 2 {
			t.Fatalf("status=%d forwards=%d body=%s", response.Code, len(forwarder.inputs), response.Body.String())
		}
		// The failed attempt's slot is freed before the retry takes its own.
		if len(heldPerForward) != 2 || heldPerForward[0] != 1 || heldPerForward[1] != 1 {
			t.Fatalf("slots held per forward = %v, want [1 1]", heldPerForward)
		}
		if held := limiter.totalHeld(); held != 0 {
			t.Fatalf("slots held after the request = %d, want 0", held)
		}
	})
	t.Run("quota admission fails after acquisition", func(t *testing.T) {
		forwarder := &scriptedForwarder{results: []UpstreamResult{okUpstreamResult()}}
		engine, handler, _, registry := newRequestLogHandlerTestRuntime(
			t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-only",
		)
		setCredentialConcurrencyLimits(t, registry, map[uint]int{1: 1})
		limiter := &scriptedConcurrencyLimiter{}
		handler.concurrency = limiter
		handler.accessQuota = &scriptedAccessQuotaGate{admitErr: errors.New("redis: connection refused")}

		response := serveConcurrencyRequest(engine, `{"model":"gpt-4o"}`)

		if response.Code != http.StatusServiceUnavailable || len(forwarder.inputs) != 0 || len(limiter.snapshot()) != 1 {
			t.Fatalf("status=%d forwards=%d calls=%#v", response.Code, len(forwarder.inputs), limiter.snapshot())
		}
		if held := limiter.heldCount(1); held != 0 {
			t.Fatalf("slots held after the early return = %d, want 0", held)
		}
	})
}

func TestCredentialConcurrencyFailsClosedWhenSharedStateIsUnavailable(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{okUpstreamResult()}}
	engine, handler, _, registry := newRequestLogHandlerTestRuntime(
		t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-only",
	)
	setCredentialConcurrencyLimits(t, registry, map[uint]int{1: 2})
	handler.concurrency = &scriptedConcurrencyLimiter{err: errors.New("redis: connection refused")}

	response := serveConcurrencyRequest(engine, `{"model":"gpt-4o"}`)

	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"code":"cluster_state_unavailable"`) ||
		response.Header().Get("Retry-After") != "1" || len(forwarder.inputs) != 0 {
		t.Fatalf("response = %d headers=%v forwards=%d body=%s", response.Code, response.Header(), len(forwarder.inputs), response.Body.String())
	}
}

// With the real shared limiter on a Redis that is down, an unlimited
// credential still serves while a limited one fails closed.
func TestCredentialConcurrencyUnlimitedCredentialNeverTouchesRedis(t *testing.T) {
	for _, test := range []struct {
		name     string
		limit    int
		wantCode int
	}{
		{name: "unlimited", limit: 0, wantCode: http.StatusOK},
		{name: "limited", limit: 1, wantCode: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarder := &scriptedForwarder{results: []UpstreamResult{okUpstreamResult()}}
			engine, handler, _, registry := newRequestLogHandlerTestRuntime(
				t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-only",
			)
			setCredentialConcurrencyLimits(t, registry, map[uint]int{1: test.limit})
			server := miniredis.RunT(t)
			handler.concurrency = cluster.NewCredentialConcurrency(clustertest.Connect(t, server, "node-down"))
			server.Close()

			response := serveConcurrencyRequest(engine, `{"model":"gpt-4o"}`)

			if response.Code != test.wantCode {
				t.Fatalf("status=%d, want %d; body=%s", response.Code, test.wantCode, response.Body.String())
			}
		})
	}
}
