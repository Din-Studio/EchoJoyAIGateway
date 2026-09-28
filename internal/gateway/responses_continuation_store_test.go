package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gpt-load/internal/automodel"
	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/state"
)

var errTestSharedStore = errors.New("redis: connection refused")

// scriptedBindingStore wraps the in-process index and injects shared-store
// failures so gateway failure semantics can be observed without Redis.
type scriptedBindingStore struct {
	local     *state.ResponseBindings
	lookupErr error
	recordErr error
	lookups   atomic.Int32
}

func (store *scriptedBindingStore) Lookup(ctx context.Context, accessKeyID uint, responseID string) (state.ResponseBinding, bool, error) {
	store.lookups.Add(1)
	if store.lookupErr != nil {
		return state.ResponseBinding{}, false, store.lookupErr
	}
	return store.local.Lookup(ctx, accessKeyID, responseID)
}

func (store *scriptedBindingStore) Record(
	ctx context.Context,
	accessKeyID uint,
	responseID string,
	ref state.CredentialRef,
	autoSelections ...*automodel.Selection,
) (bool, error) {
	if store.recordErr != nil {
		return false, store.recordErr
	}
	return store.local.Record(ctx, accessKeyID, responseID, ref, autoSelections...)
}

func TestResponsesContinuationLookupFailureIsClusterStateUnavailable(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{storedResponse("first")}}
	handler, engine, _ := newContinuationFixture(t, forwarder)
	handler.responseBindings = &scriptedBindingStore{local: state.NewResponseBindings(), lookupErr: errTestSharedStore}

	response := serveContinuation(t, engine, "gl-client", `{"model":"gpt-4o","previous_response_id":"first","input":"continue"}`, http.StatusServiceUnavailable)
	if !bytes.Contains(response.Body.Bytes(), []byte("cluster_state_unavailable")) {
		t.Fatalf("body = %s, want cluster_state_unavailable", response.Body.String())
	}
	if len(forwarder.inputs) != 0 {
		t.Fatalf("upstream attempts = %d, want none", len(forwarder.inputs))
	}
}

func TestResponsesContinuationLooksUpOwnershipOnce(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{storedResponse("first"), storedResponse("second")}}
	handler, engine, _ := newContinuationFixture(t, forwarder)
	store := &scriptedBindingStore{local: state.NewResponseBindings()}
	handler.responseBindings = store

	serveContinuation(t, engine, "gl-client", `{"model":"gpt-4o","input":"initial"}`, http.StatusOK)
	serveContinuation(t, engine, "gl-client", `{"model":"gpt-4o","previous_response_id":"first","input":"continue"}`, http.StatusOK)
	if got := store.lookups.Load(); got != 1 {
		t.Fatalf("ownership lookups = %d, want 1", got)
	}
}

func TestResponsesOwnershipStoreFailureWithholdsJSONResponseWithoutPenalty(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{storedResponse("first"), storedResponse("retried")}}
	handler, engine, _ := newContinuationFixture(t, forwarder)
	handler.manager.Current().Settings.RetryCount = 3
	handler.responseBindings = &scriptedBindingStore{local: state.NewResponseBindings(), recordErr: errTestSharedStore}

	response := serveContinuation(t, engine, "gl-client", `{"model":"gpt-4o","input":"initial"}`, http.StatusServiceUnavailable)
	if bytes.Contains(response.Body.Bytes(), []byte("first")) ||
		!bytes.Contains(response.Body.Bytes(), []byte("cluster_state_unavailable")) {
		t.Fatalf("body = %s, want withheld response and cluster_state_unavailable", response.Body.String())
	}
	if len(forwarder.inputs) != 1 {
		t.Fatalf("upstream attempts = %d, want 1", len(forwarder.inputs))
	}
	assertCredentialsUnpenalized(t, handler, 1, 2)
}

func TestResponsesOwnershipStoreFailureWithholdsSSEResponseWithoutPenalty(t *testing.T) {
	created := "event: response.created\r\n" + `data: {"type":"response.created","response":{"id":"first","object":"response","store":true}}` + "\r\n\r\n"
	var attempts atomic.Int32
	executor := fakeExecutionExecutor{
		stream: func(_ context.Context, _ execution.AttemptSpec, sink execution.StreamSink) execution.StreamResult {
			attempts.Add(1)
			for _, event := range []execution.StreamEvent{
				{Sequence: 1, Kind: execution.StreamEventReady, StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}},
				{Sequence: 2, Kind: execution.StreamEventData, Data: []byte(created)},
			} {
				if err := sink(event); err != nil {
					return execution.StreamResult{StatusCode: http.StatusOK, DispatchState: execution.DispatchMaybeSent, ResponseStarted: true}
				}
			}
			return execution.StreamResult{StatusCode: http.StatusOK, DispatchState: execution.DispatchMaybeSent, ResponseStarted: true}
		},
	}
	handler, engine, _ := newContinuationFixture(t, NewExecutionForwarder(executor))
	setContinuationChannel(t, handler, channel.NewAPI, "https://upstream.example")
	handler.manager.Current().Settings.RetryCount = 3
	handler.responseBindings = &scriptedBindingStore{local: state.NewResponseBindings(), recordErr: errTestSharedStore}

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-4o","input":"initial","stream":true}`))
	request.Header.Set("Authorization", "Bearer gl-client")
	writer := httptest.NewRecorder()
	engine.ServeHTTP(writer, request)
	if bytes.Contains(writer.Body.Bytes(), []byte(`"first"`)) {
		t.Fatalf("SSE body = %q, want response ID withheld", writer.Body.String())
	}
	if writer.Code != http.StatusServiceUnavailable || !bytes.Contains(writer.Body.Bytes(), []byte("cluster_state_unavailable")) {
		t.Fatalf("status = %d body = %s, want 503 cluster_state_unavailable", writer.Code, writer.Body.String())
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("upstream attempts = %d, want 1", got)
	}
	assertCredentialsUnpenalized(t, handler, 1, 2)
}

func assertCredentialsUnpenalized(t *testing.T, handler *Handler, credentialIDs ...uint) {
	t.Helper()
	registry := handler.registry.(*state.CredentialRegistry)
	if candidates := registry.CollectCredentialCandidates([]uint{1}, nil, time.Now()); len(candidates) != len(credentialIDs) {
		t.Fatalf("available credentials = %d, want %d", len(candidates), len(credentialIDs))
	}
	entries, err := registry.SnapshotGroupCredentialEntriesExact(1, credentialIDs)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.FailureCount != 0 || entry.Blacklisted || !entry.CooldownUntil.IsZero() || len(entry.ModelCooldowns) != 0 {
			t.Fatal(fmt.Sprintf("credential %d penalized by a shared-store failure: %+v", entry.ID, entry))
		}
	}
}

func TestWebsocketOwnershipStoreFailureWithholdsResponseWithoutPenalty(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var request map[string]any
			if conn.ReadJSON(&request) != nil {
				return
			}
			if conn.WriteMessage(websocket.TextMessage, websocketCompleted("resp_1", "")) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	h, engine, _ := websocketTestHandler(t, upstream.URL+"/v1", channel.OpenAI)
	sink := &recordingRequestLogSink{}
	h.requestLogSink = sink
	h.responseBindings = &scriptedBindingStore{local: state.NewResponseBindings(), recordErr: errTestSharedStore}
	server := httptest.NewServer(engine)
	defer server.Close()

	conn := dialGatewayWebsocket(t, server.URL)
	defer conn.Close()
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"public","input":"hello"}`))
	_, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(message, []byte("resp_1")) || !bytes.Contains(message, []byte("cluster_state_unavailable")) {
		t.Fatalf("message = %s, want response ID withheld and cluster_state_unavailable", message)
	}
	waitWebsocketLogs(t, sink, 1)
	assertCredentialsUnpenalized(t, h, 1)
}

func TestWebsocketContinuationLookupFailureIsClusterStateUnavailable(t *testing.T) {
	var turns atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	h, engine, _ := websocketTestHandler(t, upstream.URL+"/v1", channel.OpenAI)
	h.responseBindings = &scriptedBindingStore{local: state.NewResponseBindings(), lookupErr: errTestSharedStore}
	server := httptest.NewServer(engine)
	defer server.Close()

	conn := dialGatewayWebsocket(t, server.URL)
	defer conn.Close()
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"public","previous_response_id":"resp_1","input":"continue"}`))
	_, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(message, []byte("cluster_state_unavailable")) {
		t.Fatalf("message = %s, want cluster_state_unavailable", message)
	}
	if turns.Load() != 0 {
		t.Fatal("continuation reached the upstream despite unavailable ownership")
	}
}
