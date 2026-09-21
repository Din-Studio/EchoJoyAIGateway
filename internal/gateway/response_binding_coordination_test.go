package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/state"
)

var errTestCoordinationDown = errors.New("redis unreachable")

// The store can be remote, so one continuation must cost one resolution.
func TestResponsesContinuationResolvesOwnershipOnce(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{
		storedResponse("first"), storedResponse("second"),
	}}
	handler, engine, _ := newContinuationFixture(t, forwarder)
	store := newCountingResponseBindingStore()
	handler.responseBindings = store

	serveContinuation(t, engine, "gl-client", `{"model":"gpt-4o","input":"initial"}`, http.StatusOK)
	lookupsAfterRoot, _ := store.counts()
	serveContinuation(t, engine,
		"gl-client", `{"model":"gpt-4o","previous_response_id":"first","input":"continue"}`, http.StatusOK)

	lookups, _ := store.counts()
	if lookupsAfterRoot != 0 {
		t.Fatalf("lookups for a request without a previous id = %d, want 0", lookupsAfterRoot)
	}
	if lookups != 1 {
		t.Fatalf("lookups for one continuation = %d, want 1", lookups)
	}
	assertAffinityAttemptKeys(t, forwarder.inputs, []string{"sk-one", "sk-one"})
}

// An unreachable coordination backend is not the client sending an unknown id.
func TestResponsesContinuationFailsClosedWhenOwnershipLookupFails(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{storedResponse("first")}}
	handler, engine, sink := newContinuationFixture(t, forwarder)
	store := newCountingResponseBindingStore()
	handler.responseBindings = store

	serveContinuation(t, engine, "gl-client", `{"model":"gpt-4o","input":"initial"}`, http.StatusOK)
	store.failLookups(errTestCoordinationDown)
	response := serveContinuation(t, engine,
		"gl-client", `{"model":"gpt-4o","previous_response_id":"first","input":"continue"}`,
		http.StatusServiceUnavailable)

	if !strings.Contains(response.Body.String(), reasonCoordinationUnavailable.Code) {
		t.Fatalf("body = %s, want %q", response.Body.String(), reasonCoordinationUnavailable.Code)
	}
	if len(forwarder.inputs) != 1 {
		t.Fatalf("upstream attempts = %d; an unresolved owner must not reach an upstream", len(forwarder.inputs))
	}
	events := sink.snapshot()
	if got := events[len(events)-1].ErrorCode; got != reasonCoordinationUnavailable.Code {
		t.Fatalf("request log error code = %q, want %q", got, reasonCoordinationUnavailable.Code)
	}
}

// Recording ownership is the gateway's own step: failing it must not deliver
// the response, must not be filed as an upstream fault, and must not charge
// the credential that answered correctly.
func TestResponsesRecordFailureFailsClosedWithoutBlamingTheCredential(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{storedResponse("first")}}
	handler, engine, sink := newContinuationFixture(t, forwarder)
	store := newCountingResponseBindingStore()
	store.failRecords(errTestCoordinationDown)
	handler.responseBindings = store

	response := serveContinuation(t, engine,
		"gl-client", `{"model":"gpt-4o","input":"initial"}`, http.StatusServiceUnavailable)

	if bytes.Contains(response.Body.Bytes(), []byte("first")) {
		t.Fatalf("body = %s, want no upstream response for an unrecorded ownership", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), reasonCoordinationUnavailable.Code) {
		t.Fatalf("body = %s, want %q", response.Body.String(), reasonCoordinationUnavailable.Code)
	}
	events := sink.snapshot()
	last := events[len(events)-1]
	if last.ErrorCode != reasonCoordinationUnavailable.Code {
		t.Fatalf("request log error code = %q, want %q", last.ErrorCode, reasonCoordinationUnavailable.Code)
	}
	if len(last.Attempts) != 1 || last.Attempts[0].ErrorCode != reasonCoordinationUnavailable.Code {
		t.Fatalf("attempt telemetry = %#v, want the coordination code", last.Attempts)
	}
	for _, view := range handler.registry.(*state.CredentialRegistry).Snapshot() {
		if view.FailureCount != 0 || view.Blacklisted {
			t.Fatalf("credential %d = %#v, want an unpunished credential", view.ID, view)
		}
	}
}

// A conflicting ownership is still a conflict, not a coordination failure.
func TestResponsesRecordConflictStaysAnUpstreamProtocolError(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{
		storedResponse("shared-id"), storedResponse("shared-id"),
	}}
	handler, engine, _ := newContinuationFixture(t, forwarder)
	handler.responseBindings = newCountingResponseBindingStore()

	serveContinuation(t, engine, "gl-client", `{"model":"gpt-4o","input":"first"}`, http.StatusOK)
	response := serveContinuation(t, engine,
		"gl-client", `{"model":"gpt-4o","input":"independent"}`, http.StatusBadGateway)
	if !strings.Contains(response.Body.String(), reasonUpstreamProtocol.Code) {
		t.Fatalf("body = %s, want %q", response.Body.String(), reasonUpstreamProtocol.Code)
	}
}

// A streaming attempt carries the same response through several events. Only
// the first one may reach the store, which is also why the fail-closed path
// runs before any byte is committed downstream.
func TestResponsesStreamRecordsOwnershipOncePerResponse(t *testing.T) {
	for name, test := range map[string]struct {
		recordErr  error
		wantStatus int
		wantBody   bool
	}{
		"available":   {recordErr: nil, wantStatus: http.StatusOK, wantBody: true},
		"unavailable": {recordErr: errTestCoordinationDown, wantStatus: http.StatusServiceUnavailable, wantBody: false},
	} {
		t.Run(name, func(t *testing.T) {
			created := "event: response.created\r\n" +
				`data: {"type":"response.created","response":{"id":"first","object":"response","store":true}}` + "\r\n\r\n"
			delta := "event: response.output_text.delta\r\n" +
				`data: {"type":"response.output_text.delta","response":{"id":"first","object":"response","store":true}}` + "\r\n\r\n"
			completed := "event: response.completed\r\n" +
				`data: {"type":"response.completed","response":{"id":"first","object":"response","store":true}}` + "\r\n\r\n"
			executor := fakeExecutionExecutor{
				stream: func(_ context.Context, _ execution.AttemptSpec, sink execution.StreamSink) execution.StreamResult {
					events := []execution.StreamEvent{
						{Sequence: 1, Kind: execution.StreamEventReady, StatusCode: http.StatusOK,
							Header: http.Header{"Content-Type": {"text/event-stream"}}},
						{Sequence: 2, Kind: execution.StreamEventData, Data: []byte(created)},
						{Sequence: 3, Kind: execution.StreamEventData, Data: []byte(delta)},
						{Sequence: 4, Kind: execution.StreamEventData, Data: []byte(completed)},
					}
					for _, event := range events {
						if err := sink(event); err != nil {
							break
						}
					}
					return execution.StreamResult{StatusCode: http.StatusOK,
						DispatchState: execution.DispatchMaybeSent, ResponseStarted: true}
				},
			}
			handler, engine, _ := newContinuationFixture(t, NewExecutionForwarder(executor))
			setContinuationChannel(t, handler, channel.NewAPI, "https://upstream.example")
			store := newCountingResponseBindingStore()
			store.failRecords(test.recordErr)
			handler.responseBindings = store

			request := httptest.NewRequest(http.MethodPost, "/v1/responses",
				strings.NewReader(`{"model":"gpt-4o","input":"initial","stream":true}`))
			request.Header.Set("Authorization", "Bearer gl-client")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
			if _, records := store.counts(); records != 1 {
				t.Fatalf("recordings for one streamed response = %d, want 1", records)
			}
			if got := strings.Contains(response.Body.String(), "response.created"); got != test.wantBody {
				t.Fatalf("delivered stream = %t, want %t; body = %s", got, test.wantBody, response.Body.String())
			}
			if !test.wantBody && !strings.Contains(response.Body.String(), reasonCoordinationUnavailable.Code) {
				t.Fatalf("body = %s, want %q", response.Body.String(), reasonCoordinationUnavailable.Code)
			}
		})
	}
}

// A websocket turn resolves ownership at most once, from the session's own
// parent map when it can, and an unreachable store rejects the turn as a
// coordination failure rather than a missing parent.
func TestWebsocketContinuationResolvesOwnershipOnceAndFailsClosed(t *testing.T) {
	var turns atomic.Int32
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
			id := fmt.Sprintf("resp_%d", turns.Add(1))
			if conn.WriteMessage(websocket.TextMessage, websocketCompleted(id, "")) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	handler, engine, _ := websocketTestHandler(t, upstream.URL+"/v1", channel.OpenAI)
	sink := &recordingRequestLogSink{}
	handler.requestLogSink = sink
	store := newCountingResponseBindingStore()
	handler.responseBindings = store
	server := httptest.NewServer(engine)
	defer server.Close()

	session := dialGatewayWebsocket(t, server.URL)
	for _, body := range []string{
		`{"type":"response.create","model":"public","input":"initial"}`,
		`{"type":"response.create","model":"public","previous_response_id":"resp_1","input":"continue"}`,
	} {
		_ = session.WriteMessage(websocket.TextMessage, []byte(body))
		if _, _, err := session.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}
	waitWebsocketLogs(t, sink, 2)
	if lookups, _ := store.counts(); lookups != 0 {
		t.Fatalf("lookups = %d; a parent created in this session must not be re-resolved remotely", lookups)
	}
	_ = session.Close()

	store.failLookups(errTestCoordinationDown)
	other := dialGatewayWebsocket(t, server.URL)
	defer other.Close()
	_ = other.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"response.create","model":"public","previous_response_id":"resp_1","input":"continue"}`))
	var rejection struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := other.ReadJSON(&rejection); err != nil ||
		rejection.Error.Code != reasonCoordinationUnavailable.Code {
		t.Fatalf("rejection = %+v, err = %v; want %q", rejection, err, reasonCoordinationUnavailable.Code)
	}
}
