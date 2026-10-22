package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"gpt-load/internal/channel"
	"gpt-load/internal/dialect"
	"gpt-load/internal/integration/ly"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
	"gpt-load/internal/telemetry"
	"gpt-load/internal/usage"
)

// fakeExternalBilling answers every admission with decision and keeps every
// call. An Allow decision without Admission stands for an unbilled key.
type fakeExternalBilling struct {
	mu       sync.Mutex
	decision ly.Decision
	admits   []ly.AdmitInput
	records  []fakeBillingRecord
}

type fakeBillingRecord struct {
	admission  ly.Admission
	event      telemetry.RequestEvent
	chargeNano int64
}

func (f *fakeExternalBilling) Admit(_ context.Context, in ly.AdmitInput) ly.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admits = append(f.admits, in)
	return f.decision
}

func (f *fakeExternalBilling) Record(admission *ly.Admission, event telemetry.RequestEvent, chargeNano int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, fakeBillingRecord{admission: *admission, event: event, chargeNano: chargeNano})
}

func (f *fakeExternalBilling) snapshot() ([]ly.AdmitInput, []fakeBillingRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ly.AdmitInput(nil), f.admits...), append([]fakeBillingRecord(nil), f.records...)
}

var billedAdmission = ly.Decision{Verdict: ly.Allow, Admission: &ly.Admission{UserID: "u1", KeyID: "k1", ProjectID: "p1"}}

// newBillingTestRuntime serves gpt-4o to the access key "ly:k1" (plaintext
// gl-client) in a CNY system, with billing deciding admission.
func newBillingTestRuntime(t *testing.T, billing *fakeExternalBilling, results ...UpstreamResult) (*gin.Engine, *scriptedForwarder) {
	t.Helper()
	forwarder := &scriptedForwarder{results: results}
	engine, handler, manager, _ := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-upstream")
	handler.billing = billing
	handler.dialects = dialect.NewSet(dialect.NewOpenAI(), dialect.NewAnthropic(), dialect.NewOpenAIResponses())
	input := gatewayAccessQuotaCompileInput(handler, nil)
	input.AccessKeys[0].Name = "ly:k1"
	input.SystemSettings = cnySettings("7.2")
	if _, err := manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	return engine, forwarder
}

func serveBillingRequest(engine *gin.Engine, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gl-client")
	request.Header.Set("X-Project-Id", "p1")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func okResult() UpstreamResult {
	return UpstreamResult{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, RequestWritten: true,
		Body:  []byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[]}`),
		Usage: usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 3, Output: 2}},
	}
}

// T1, T2, T7: a rejected account never reaches the upstream and is not reported.
func TestExternalBillingRejectionsStopBeforeTheUpstream(t *testing.T) {
	for _, test := range []struct {
		name     string
		decision ly.Decision
		status   int
		code     string
	}{
		{"insufficient balance", ly.Decision{Verdict: ly.Insufficient}, http.StatusPaymentRequired, "insufficient_balance"},
		{"unusable account", ly.Decision{Verdict: ly.Unusable, Reason: "disabled"}, http.StatusForbidden, "billing_account_unusable"},
		{"unknown key", ly.Decision{Verdict: ly.InvalidKey}, http.StatusUnauthorized, "invalid_access_key"},
		{"billing unavailable", ly.Decision{Verdict: ly.Unavailable}, http.StatusServiceUnavailable, "billing_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			billing := &fakeExternalBilling{decision: test.decision}
			engine, forwarder := newBillingTestRuntime(t, billing, okResult())

			response := serveBillingRequest(engine, "/v1/chat/completions", `{"model":"gpt-4o"}`)

			var body struct{ Code, Message string }
			_ = json.Unmarshal(response.Body.Bytes(), &body)
			admits, records := billing.snapshot()
			if response.Code != test.status || body.Code != test.code || len(forwarder.inputs) != 0 || len(admits) != 1 || len(records) != 0 {
				t.Fatalf("response %d %s, upstream calls %d, admits %d, records %d",
					response.Code, response.Body, len(forwarder.inputs), len(admits), len(records))
			}
			if test.decision.Reason != "" && !strings.Contains(body.Message, test.decision.Reason) {
				t.Fatalf("message %q does not carry the Lingying reason %q", body.Message, test.decision.Reason)
			}
		})
	}
}

// T10: admission gets the key, its plaintext, the project header and the
// frozen currency; the report carries the admission and the request outcome.
func TestExternalBillingAdmitsAndRecordsTheRequest(t *testing.T) {
	billing := &fakeExternalBilling{decision: billedAdmission}
	engine, forwarder := newBillingTestRuntime(t, billing, okResult())

	response := serveBillingRequest(engine, "/v1/chat/completions", `{"model":"gpt-4o"}`)

	admits, records := billing.snapshot()
	if response.Code != http.StatusOK || len(forwarder.inputs) != 1 || len(admits) != 1 || len(records) != 1 {
		t.Fatalf("response %d %s, upstream calls %d, admits %d, records %d",
			response.Code, response.Body, len(forwarder.inputs), len(admits), len(records))
	}
	if admits[0] != (ly.AdmitInput{KeyName: "ly:k1", Credential: ly.Credential{APIKey: "gl-client"}, ProjectID: "p1", Currency: pricing.CurrencyCNY}) {
		t.Fatalf("admit input = %+v", admits[0])
	}
	record := records[0]
	if record.admission != *billedAdmission.Admission || record.event.RequestID != fixedRequestID ||
		record.event.ClientModel != "gpt-4o" || record.event.Status != telemetry.RequestStatusSuccess ||
		record.event.Usage.Result.Tokens.Output != 2 {
		t.Fatalf("record = %+v", record)
	}
}

// T3, T5: requests that never reach a priced upstream call are not admitted.
func TestExternalBillingSkipsRequestsThatAreNotGenerations(t *testing.T) {
	for _, test := range []struct {
		name, path, body string
		forwarded        bool
	}{
		{"invalid body", "/v1/chat/completions", `{not json`, false},
		{"count tokens", "/v1/messages/count_tokens", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			billing := &fakeExternalBilling{decision: billedAdmission}
			engine, forwarder := newBillingTestRuntime(t, billing, okResult())

			response := serveBillingRequest(engine, test.path, test.body)

			if admits, records := billing.snapshot(); len(admits) != 0 || len(records) != 0 {
				t.Fatalf("admits %d, records %d, want none", len(admits), len(records))
			}
			// Both requests get past routing, so only the gate placement keeps them unbilled.
			if forwarded := len(forwarder.inputs) == 1; forwarded != test.forwarded {
				t.Fatalf("response %d %s, forwarded %t, want %t", response.Code, response.Body, forwarded, test.forwarded)
			}
		})
	}
}

// T6: a key billing lets through unbilled is served without a report.
func TestExternalBillingDoesNotReportUnbilledKeys(t *testing.T) {
	billing := &fakeExternalBilling{decision: ly.Decision{Verdict: ly.Allow}}
	engine, forwarder := newBillingTestRuntime(t, billing, okResult())

	response := serveBillingRequest(engine, "/v1/chat/completions", `{"model":"gpt-4o"}`)

	if _, records := billing.snapshot(); response.Code != http.StatusOK || len(forwarder.inputs) != 1 || len(records) != 0 {
		t.Fatalf("response %d, upstream calls %d, records %d", response.Code, len(forwarder.inputs), len(records))
	}
}

// T4, T11: admission runs before the auto-model decision, and the reported
// charge includes the decision cost, as the access key cost limit does.
func TestExternalBillingAndAutoModelDecision(t *testing.T) {
	for _, test := range []struct {
		name     string
		decision ly.Decision
	}{
		{"rejected before the decision", ly.Decision{Verdict: ly.Insufficient}},
		{"charge includes the decision", billedAdmission},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarder := &scriptedForwarder{results: []UpstreamResult{okResult()}}
			handler, manager, _ := newHandlerForTest(t, forwarder, "key-a")
			billing := &fakeExternalBilling{decision: test.decision}
			handler.billing = billing
			config := autoModelTestConfig()
			if _, err := manager.Publish(state.CompileInput{
				AutoModel: &config, Jev: testSharedJev(config), ChannelRegistry: channel.NewRegistry(), SystemSettings: cnySettings("7.2"),
				Groups: []state.GroupConfig{{ID: 1, Name: "openai", ChannelID: channel.OpenAI, ConnectionType: "api_key",
					Params: json.RawMessage(`{}`), Models: []state.ModelConfig{{ID: "gpt-4o"}, {ID: "gpt-4.1"}}, Enabled: true},
					{ID: 99, Name: "jev", ChannelID: channel.Jev, ConnectionType: "api_key", Params: json.RawMessage(`{}`), Models: []state.ModelConfig{{ID: "jev-latest", Alias: "jev-router"}}, Enabled: true}},
				Credentials: []state.CredentialConfig{{ID: 1, GroupID: 1, Status: state.CredentialStatusActive, Version: 1, IdentityGeneration: 1, Fingerprint: "credential-1"}},
				AccessKeys:  []state.AccessKeyConfig{{ID: 1, Name: "ly:k1", KeyHash: handler.encryption.Hash("gl-client"), Status: state.AccessKeyStatusActive}},
			}); err != nil {
				t.Fatal(err)
			}
			decisions := 0
			handler.decisionClient = autoDecisionClient(func(*http.Request) (*http.Response, error) {
				decisions++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
					`{"answers":{"preset":{"choice":"balanced","confidence":0.9}},"usage":{"input_tokens":2000,"output_tokens":0}}`))}, nil
			})
			engine := gin.New()
			bindGatewayRoutesForTest(t, engine, handler)

			response := serveBillingRequest(engine, "/v1/chat/completions", `{"model":"auto-probe","messages":[{"role":"user","content":"task"}]}`)

			_, records := billing.snapshot()
			if test.decision.Verdict != ly.Allow {
				if response.Code != http.StatusPaymentRequired || decisions != 0 {
					t.Fatalf("response %d, decision calls %d; want 402 before any decision", response.Code, decisions)
				}
				return
			}
			if response.Code != http.StatusOK || decisions != 1 || len(records) != 1 {
				t.Fatalf("response %d %s, decision calls %d, records %d", response.Code, response.Body, decisions, len(records))
			}
			event := records[0].event
			if event.AutoDecision == nil || event.AutoDecision.EstimatedCostNanoUSD == 0 {
				t.Fatalf("auto decision = %+v, want a priced decision", event.AutoDecision)
			}
			want := event.Currency.Settle(event.Usage.Pricing.EstimatedCostNanoUSD) +
				event.Currency.Settle(event.AutoDecision.EstimatedCostNanoUSD)
			if records[0].chargeNano != want {
				t.Fatalf("reported charge = %d, want answer plus decision %d", records[0].chargeNano, want)
			}
		})
	}
}

// T8: every websocket turn is admitted and reported on its own.
func TestExternalBillingAdmitsEveryWebsocketTurn(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var req struct {
				StreamID string `json:"stream_id"`
			}
			if conn.ReadJSON(&req) != nil {
				return
			}
			if conn.WriteMessage(websocket.TextMessage, websocketCompleted("resp_"+req.StreamID, req.StreamID)) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	handler, engine, input := websocketTestHandler(t, upstream.URL+"/v1", channel.OpenAI)
	billing := &fakeExternalBilling{decision: billedAdmission}
	handler.billing = billing
	handler.requestLogSink = &recordingRequestLogSink{}
	input.AccessKeys[0].Name = "ly:k1"
	input.SystemSettings = cnySettings("7.2")
	if _, err := handler.manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(engine)
	defer server.Close()
	conn := dialGatewayWebsocket(t, server.URL)
	defer conn.Close()

	for _, lane := range []string{"a", "b"} {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","stream_id":%q,"model":"public","input":"hello"}`, lane)))
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		admits, records := billing.snapshot()
		if len(admits) == 2 && len(records) == 2 {
			if records[0].event.RequestID == records[1].event.RequestID || admits[0].Credential.APIKey != "gl-client" {
				t.Fatalf("records = %+v, admits = %+v", records, admits)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admits %d, records %d, want 2 each", len(admits), len(records))
		}
		time.Sleep(time.Millisecond)
	}
}

// R2: a request that fails after admission is still reported.
func TestExternalBillingReportsRequestsThatFailAfterAdmission(t *testing.T) {
	billing := &fakeExternalBilling{decision: billedAdmission}
	engine, forwarder := newBillingTestRuntime(t, billing, UpstreamResult{
		StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, RequestWritten: true,
		Body: []byte(`{"error":{"message":"bad request"}}`),
	})

	response := serveBillingRequest(engine, "/v1/chat/completions", `{"model":"gpt-4o"}`)

	_, records := billing.snapshot()
	if response.Code != http.StatusBadRequest || len(forwarder.inputs) != 1 || len(records) != 1 ||
		records[0].event.Status == telemetry.RequestStatusSuccess {
		t.Fatalf("response %d, upstream calls %d, records %+v; want the failure reported", response.Code, len(forwarder.inputs), records)
	}
}

// A billed request is priced by its model, so one without a model is rejected
// instead of producing a report Lingying refuses.
func TestExternalBillingRejectsABilledRequestWithoutAModel(t *testing.T) {
	billing := &fakeExternalBilling{decision: billedAdmission}
	engine, forwarder := newBillingTestRuntime(t, billing, okResult())

	response := serveBillingRequest(engine, "/v1/responses", `{"input":"hi"}`)

	admits, records := billing.snapshot()
	if response.Code != http.StatusBadRequest || len(admits) != 1 || len(records) != 0 || len(forwarder.inputs) != 0 {
		t.Fatalf("response %d %s, admits %d, records %d, upstream calls %d",
			response.Code, response.Body, len(admits), len(records), len(forwarder.inputs))
	}
}
