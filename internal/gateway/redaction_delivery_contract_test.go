package gateway

import (
	"context"
	"errors"
	"gpt-load/internal/execution"
	"gpt-load/internal/usage"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedactionDeliveryContract(t *testing.T) {
	c := websocketRedactionTestCipher(t)
	t.Run("usage-handler-log", func(t *testing.T) {
		executor := fakeExecutionExecutor{unary: func(context.Context, execution.AttemptSpec) execution.AttemptResult {
			return execution.AttemptResult{DispatchState: execution.DispatchMaybeSent, ResponseStarted: true, StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"choices":[{"message":{"content":"gld1_3_abc"}}]}`), Usage: &execution.UsageEvidence{Normalized: usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1000000, Output: 20}}}}
		}}
		sink := &recordingRequestLogSink{}
		engine, h, _, _ := newRequestLogHandlerTestRuntime(t, NewExecutionForwarder(executor), &recordingAccessKeyRPMLimiter{}, sink, "sk-synthetic-usage")
		quota := &scriptedAccessQuotaGate{}
		h.accessQuota = quota
		h.priceTables = &mutableGatewayPriceTableProvider{table: mustGatewayPriceTable(t, 2000000000, false)}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("Authorization", "Bearer gl-client")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		events := sink.snapshot()
		if len(events) != 1 {
			t.Fatalf("events=%d", len(events))
		}
		if recorder.Code != 502 || events[0].ErrorCode != "response_redaction_failed" {
			t.Fatal("restore error response changed")
		}
		if events[0].Usage.Result.State != usage.StateComplete || events[0].Usage.Pricing.EstimatedCostNanoUSD <= 0 {
			t.Fatal("paid usage missing from request log")
		}
		if quota.completes.Load() != 1 || quota.costNano.Load() != events[0].Usage.Pricing.EstimatedCostNanoUSD {
			t.Fatal("quota settlement differs from request log")
		}
	})
	t.Run("credential-conflict", func(t *testing.T) {
		credential := "synthetic-upstream-credential-12345"
		token, err := c.EncryptToken(credential)
		if err != nil {
			t.Fatal(err)
		}
		in := executionForwardInput()
		in.RedactionCipher = c
		in.APIKey = credential
		body := redactionBoundaryJSON(t, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": token}}}})
		_, err = (&responseProcessor{}).prepareSuccessRepresentation(in, 200, http.Header{}, body, []string{credential})
		if err == nil {
			t.Fatal("unary accepted a credential conflict")
		}
		executor := fakeExecutionExecutor{stream: func(_ context.Context, _ execution.AttemptSpec, sink execution.StreamSink) execution.StreamResult {
			if err := sink(execution.StreamEvent{Sequence: 1, Kind: execution.StreamEventReady, StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}}); err != nil {
				t.Fatal(err)
			}
			for i, part := range []string{token[:12], token[12:]} {
				if err := sink(execution.StreamEvent{Sequence: uint64(i + 2), Kind: execution.StreamEventData, Data: redactionBoundaryChat(t, map[string]any{"content": part}, nil)}); err != nil {
					return execution.StreamResult{DispatchState: execution.DispatchMaybeSent, ResponseStarted: true, StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}}
				}
			}
			if err := sink(execution.StreamEvent{Sequence: 4, Kind: execution.StreamEventData, Data: []byte("data: [DONE]\n\n")}); err != nil {
				t.Fatal(err)
			}
			return execution.StreamResult{DispatchState: execution.DispatchMaybeSent, ResponseStarted: true, StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}}
		}}
		recorder := httptest.NewRecorder()
		got := NewExecutionForwarder(executor).ForwardStream(context.Background(), in, recorder)
		if !errors.Is(got.Err, errRedactionStream) || strings.Contains(recorder.Body.String(), credential) {
			t.Fatal("SSE did not reject restored credential")
		}
	})
	t.Run("uncommitted-stream-handler", func(t *testing.T) {
		// 整段流在一次读取内到达时，伪造令牌在提交前就会失败；执行层此时只报告消费方中止。
		forged := "gld1_44_" + strings.Repeat("A", 44)
		data := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\" " + forged + "\"}}]}\n\ndata: [DONE]\n\n"
		executor := fakeExecutionExecutor{stream: func(_ context.Context, _ execution.AttemptSpec, sink execution.StreamSink) execution.StreamResult {
			header := http.Header{"Content-Type": {"text/event-stream"}}
			if err := sink(execution.StreamEvent{Sequence: 1, Kind: execution.StreamEventReady, StatusCode: 200, Header: header}); err != nil {
				t.Fatal(err)
			}
			if err := sink(execution.StreamEvent{Sequence: 2, Kind: execution.StreamEventData, Data: []byte(data)}); err == nil {
				t.Fatal("forged token was accepted")
			}
			return execution.StreamResult{
				DispatchState: execution.DispatchMaybeSent, ResponseStarted: true, StatusCode: 200, Header: header,
				Error: &execution.ErrorEvidence{Kind: execution.ErrorKindCanceled, Summary: "stream consumer stopped"},
			}
		}}
		sink := &recordingRequestLogSink{}
		engine, _, _, _ := newRequestLogHandlerTestRuntime(t, NewExecutionForwarder(executor), &recordingAccessKeyRPMLimiter{}, sink, "sk-synthetic-stream")
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("Authorization", "Bearer gl-client")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		events := sink.snapshot()
		if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "response_redaction_failed") {
			t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
		}
		if len(events) != 1 || events[0].ErrorCode != "response_redaction_failed" ||
			len(events[0].Attempts) != 1 || events[0].Attempts[0].ErrorSummary != reasonResponseRedactionFailed.Message {
			t.Fatalf("events = %+v", events)
		}
	})
	t.Run("first-error-CR", func(t *testing.T) {
		for _, eol := range []string{"\n", "\r"} {
			executor := fakeExecutionExecutor{stream: func(_ context.Context, _ execution.AttemptSpec, sink execution.StreamSink) execution.StreamResult {
				if err := sink(execution.StreamEvent{Sequence: 1, Kind: execution.StreamEventReady, StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}}); err != nil {
					t.Fatal(err)
				}
				if err := sink(execution.StreamEvent{Sequence: 2, Kind: execution.StreamEventData, Data: []byte("event: error" + eol + "data: {\"error\":{\"message\":\"synthetic\",\"code\":\"rate_limit_exceeded\"}}" + eol + eol)}); err != nil {
					t.Fatal(err)
				}
				return execution.StreamResult{DispatchState: execution.DispatchMaybeSent, ResponseStarted: true, StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}}
			}}
			in := executionForwardInput()
			in.RedactionCipher = c
			recorder := httptest.NewRecorder()
			got := NewExecutionForwarder(executor).ForwardStream(context.Background(), in, recorder)
			if got.Committed || !got.ProviderErrorBeforeCommit || recorder.Body.Len() != 0 {
				t.Fatalf("first upstream error was committed for eol=%q", eol)
			}
		}
	})
}
