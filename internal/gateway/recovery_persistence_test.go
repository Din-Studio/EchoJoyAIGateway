package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gpt-load/internal/platform/redact"
	"gpt-load/internal/requestlog"
	"gpt-load/internal/state"
	"gpt-load/internal/storage"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/testutil/pgtest"
	"gpt-load/internal/usage"
)

type recoveryRetentionPolicy struct{}

func (recoveryRetentionPolicy) RequestLogRetentionDays() int { return 7 }

func TestRecoveryTerminalPersistsBoundModelAndUsage(t *testing.T) {
	for _, name := range []string{"exhausted_missing_usage", "exhausted_partial_usage", "oversized_partial_usage", "no_candidate"} {
		t.Run(name, func(t *testing.T) {
			db, err := storage.Open(pgtest.NewDatabase(t))
			if err != nil {
				t.Fatal(err)
			}
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			want := usage.Result{State: usage.StateMissing}
			if strings.Contains(name, "partial") {
				want = usage.Result{State: usage.StatePartial, Tokens: usage.Tokens{UncachedInput: 123, Output: 4}}
			}
			failure := UpstreamResult{StatusCode: 400, Header: make(http.Header), ErrorSummary: "The encrypted content could not be verified.", RequestWritten: true, Usage: want}
			forwarder := &scriptedForwarder{results: []UpstreamResult{failure, failure}}
			handler, engine, _ := newContinuationFixture(t, forwarder)
			service := requestlog.NewService(db, redact.New(), recoveryRetentionPolicy{})
			if err := service.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Stop(context.Background()) })
			handler.requestLogSink = service
			body := strings.ReplaceAll(contextRecoveryRequest, "model-a", "gpt-4o")
			status, code, attempts := 400, "encrypted_content_recovery_exhausted", 2
			if name == "oversized_partial_usage" {
				handler.requestBodyLimit = 1024
				body = `{"model":"gpt-4o","input":[{"type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"` + strings.Repeat("<", 500) + `"}]}]}`
				status, code, attempts = http.StatusRequestEntityTooLarge, "request_too_large", 1
			}
			if name == "no_candidate" {
				for _, id := range []uint{1, 2} {
					if err := handler.registry.(*state.CredentialRegistry).SetCredentialStatus(id, state.CredentialStatusDisabled); err != nil {
						t.Fatal(err)
					}
				}
				status, code, attempts = 503, "no_available_candidate", 0
			}
			response := serveContinuation(t, engine, "gl-client", body, status)
			if !strings.Contains(response.Body.String(), code) {
				t.Fatal(response.Body.String())
			}
			if err := service.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
			var rows []models.RequestLog
			if err := db.Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("terminal log lost: rows=%d stats=%+v", len(rows), service.Stats())
			}
			row := rows[0]
			if row.ErrorCode != code || int(row.AttemptCount) != attempts {
				t.Fatalf("terminal=%+v", row)
			}
			if attempts > 0 {
				if row.UpstreamModel != "gpt-4o" || row.UsageState != string(want.State) || row.UncachedInputTokens != want.Tokens.UncachedInput || row.OutputTokens != want.Tokens.Output {
					t.Fatalf("bound identity/usage lost: %+v", row)
				}
			} else if row.UpstreamModel != "" {
				t.Fatalf("local rejection acquired a model: %+v", row)
			}
		})
	}
}
