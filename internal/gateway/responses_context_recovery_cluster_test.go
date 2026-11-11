package gateway

import (
	"net/http"
	"strings"
	"testing"

	"gpt-load/internal/cluster"
	"gpt-load/internal/state"
	"gpt-load/internal/testutil/clustertest"
	"gpt-load/internal/usage"
)

func TestResponsesRecoverySharesRejectionsAcrossReplicas(t *testing.T) {
	for _, itemIDs := range []bool{false, true} {
		t.Run(map[bool]string{false: "digests", true: "item_ids"}[itemIDs], func(t *testing.T) {
			_, client := clustertest.NewClient(t)
			message := "The encrypted content could not be verified."
			if itemIDs {
				message = responseItemResourceError
			}
			failure := UpstreamResult{StatusCode: 400, Header: make(http.Header), ErrorSummary: message, RequestWritten: true}
			success := UpstreamResult{StatusCode: 200, Header: make(http.Header), Body: []byte(contextRecoverySuccess), RequestWritten: true}
			first := &scriptedForwarder{results: []UpstreamResult{failure, success}}
			second := &scriptedForwarder{results: []UpstreamResult{success, success}}
			a, engineA, _ := newContinuationFixture(t, first)
			b, engineB, _ := newContinuationFixture(t, second)
			for _, handler := range []*Handler{a, b} {
				handler.invalidLineage = cluster.NewInvalidLineage(client)
				if err := handler.registry.(*state.CredentialRegistry).SetCredentialStatus(2, state.CredentialStatusDisabled); err != nil {
					t.Fatal(err)
				}
			}
			body := strings.ReplaceAll(contextRecoveryRequest, "model-a", "gpt-4o")
			body = strings.TrimSuffix(body, "}") + `,"prompt_cache_key":"cross-replica"}`
			serveContinuation(t, engineA, "gl-client", body, http.StatusOK)
			serveContinuation(t, engineB, "gl-client", body, http.StatusOK)
			if len(first.inputs) != 2 || len(second.inputs) != 1 {
				t.Fatalf("attempts A=%d B=%d", len(first.inputs), len(second.inputs))
			}
			cleaned := string(second.inputs[0].Request.Body)
			if itemIDs {
				if strings.Contains(cleaned, "msg_old") || !strings.Contains(cleaned, "opaque") {
					t.Fatalf("ID-only cleanup: %s", cleaned)
				}
			} else if strings.Contains(cleaned, "opaque") || !strings.Contains(cleaned, "msg_old") {
				t.Fatalf("digest-only cleanup: %s", cleaned)
			}
			different := strings.Replace(body, "cross-replica", "another-session", 1)
			serveContinuation(t, engineB, "gl-client", different, http.StatusOK)
			if string(second.inputs[1].Request.Body) != different {
				t.Fatal("rejection leaked into another session")
			}
		})
	}
}

func TestResponsesRecoveryHonorsBodyLimit(t *testing.T) {
	for _, message := range []string{"The encrypted content could not be verified.", responseItemResourceError} {
		t.Run(message, func(t *testing.T) {
			wantUsage := usage.Result{State: usage.StatePartial, Tokens: usage.Tokens{UncachedInput: 123, Output: 4}}
			failure := UpstreamResult{StatusCode: 400, Header: make(http.Header), ErrorSummary: message, RequestWritten: true, Usage: wantUsage}
			forwarder := &scriptedForwarder{results: []UpstreamResult{failure}}
			handler, engine, sink := newContinuationFixture(t, forwarder)
			handler.requestBodyLimit = 1024
			if err := handler.registry.(*state.CredentialRegistry).SetCredentialStatus(2, state.CredentialStatusDisabled); err != nil {
				t.Fatal(err)
			}
			body := `{"model":"gpt-4o","prompt_cache_key":"size-session","input":[{"type":"reasoning","id":"rs_old","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"` + strings.Repeat("<", 500) + `"}]}]}`
			// First call rejects oversized recovery; the next uses cached evidence.
			for range 2 {
				response := serveContinuation(t, engine, "gl-client", body, http.StatusRequestEntityTooLarge)
				if !strings.Contains(response.Body.String(), "request_too_large") {
					t.Fatal(response.Body.String())
				}
			}
			if len(forwarder.inputs) != 1 {
				t.Fatalf("oversized body forwarded: %d attempts", len(forwarder.inputs))
			}
			events := sink.snapshot()
			if len(events) != 2 || events[0].Usage.Result != wantUsage || events[0].Usage.AttemptSequence != 1 || events[0].Usage.CredentialID != 1 {
				t.Fatalf("rejection usage lost on 413: %+v", events)
			}
		})
	}
}

func TestResponsesRecoveryContinuesWhenLineageRedisFails(t *testing.T) {
	server, client := clustertest.NewClient(t)
	server.Close()
	failure := UpstreamResult{StatusCode: 400, Header: make(http.Header), ErrorSummary: "The encrypted content could not be verified.", RequestWritten: true}
	success := UpstreamResult{StatusCode: 200, Header: make(http.Header), Body: []byte(contextRecoverySuccess), RequestWritten: true}
	forwarder := &scriptedForwarder{results: []UpstreamResult{failure, success}}
	handler, engine, _ := newContinuationFixture(t, forwarder)
	handler.invalidLineage = cluster.NewInvalidLineage(client)
	body := strings.ReplaceAll(contextRecoveryRequest, "model-a", "gpt-4o")
	serveContinuation(t, engine, "gl-client", body, http.StatusOK)
	if len(forwarder.inputs) != 2 {
		t.Fatalf("attempts=%d", len(forwarder.inputs))
	}
	assertRecoveredContext(t, string(forwarder.inputs[1].Request.Body))
}
