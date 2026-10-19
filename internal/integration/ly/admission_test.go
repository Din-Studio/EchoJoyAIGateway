package ly

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gpt-load/internal/platform/config"
	"gpt-load/internal/pricing"
)

// fakeBalance serves POST /internal/aigw/balance with a fixed status and body
// and counts the calls.
func fakeBalance(t *testing.T, status int, body string) (*Service, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			APIKey string `json:"api_key"`
		}
		if r.URL.Path != balancePath || r.Header.Get("Authorization") != "Bearer token" ||
			json.NewDecoder(r.Body).Decode(&request) != nil || request.APIKey != "sk-plain" {
			t.Errorf("balance request = %s %s, auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return New(config.LingyingBillingConfig{BaseURL: server.URL + "/", Token: "token"}, nil), &calls
}

func lingyingInput() AdmitInput {
	return AdmitInput{KeyName: "ly:k1", Credential: Credential{APIKey: "sk-plain"}, ProjectID: "p1", Currency: pricing.CurrencyCNY}
}

func TestAdmitAllowsAPositiveBalanceAndKeepsTheAccount(t *testing.T) {
	service, _ := fakeBalance(t, http.StatusOK,
		`{"user_id":"u1","key_id":"k1","usable":true,"available":"0.01","balance":"0","bonus_balance":"0.01"}`)

	decision := service.Admit(context.Background(), lingyingInput())

	if decision.Verdict != Allow || decision.Admission == nil ||
		*decision.Admission != (Admission{UserID: "u1", KeyID: "k1", ProjectID: "p1"}) {
		t.Fatalf("Admit() = %+v %+v", decision, decision.Admission)
	}
}

func TestAdmitRejectsByTheLingyingAccount(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		want       Verdict
		reason     string
	}{
		{name: "zero balance", status: 200, body: `{"user_id":"u1","usable":true,"available":"0"}`, want: Insufficient},
		{name: "negative balance", status: 200, body: `{"user_id":"u1","usable":true,"available":"-3.5"}`, want: Insufficient},
		{name: "unusable", status: 200, body: `{"user_id":"u1","usable":false,"reason":"disabled","available":"9"}`, want: Unusable, reason: "disabled"},
		{name: "unknown key", status: 404, body: `{"error_code":"NOT_FOUND"}`, want: InvalidKey},
		{name: "storage unavailable", status: 503, body: `{"error_code":"AUTH_UNAVAILABLE"}`, want: Unavailable},
		{name: "bad token", status: 401, body: `{}`, want: Unavailable},
		{name: "no user", status: 200, body: `{"usable":true,"available":"9"}`, want: Unavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _ := fakeBalance(t, test.status, test.body)

			decision := service.Admit(context.Background(), lingyingInput())

			if decision.Verdict != test.want || decision.Reason != test.reason || decision.Admission != nil {
				t.Fatalf("Admit() = %+v, want verdict %d reason %q", decision, test.want, test.reason)
			}
		})
	}
}

func TestAdmitLetsOtherKeysThroughWithoutCallingLingying(t *testing.T) {
	service, calls := fakeBalance(t, http.StatusOK, `{}`)
	input := lingyingInput()
	input.KeyName = "admin-key"

	decision := service.Admit(context.Background(), input)

	if decision.Verdict != Allow || decision.Admission != nil || calls.Load() != 0 {
		t.Fatalf("Admit() = %+v after %d calls, want unbilled allow without a call", decision, calls.Load())
	}
}

func TestAdmitFailsClosed(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		decision := New(config.LingyingBillingConfig{}, nil).Admit(context.Background(), lingyingInput())
		if decision.Verdict != Unavailable {
			t.Fatalf("Admit() = %+v, want Unavailable", decision)
		}
	})
	t.Run("system currency is not CNY", func(t *testing.T) {
		service, calls := fakeBalance(t, http.StatusOK, `{"user_id":"u1","usable":true,"available":"9"}`)
		input := lingyingInput()
		input.Currency = pricing.CurrencyUSD
		if decision := service.Admit(context.Background(), input); decision.Verdict != Unavailable || calls.Load() != 0 {
			t.Fatalf("Admit() = %+v after %d calls, want Unavailable without a call", decision, calls.Load())
		}
	})
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Draining the body lets the server notice the client hanging up.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		}))
		t.Cleanup(server.Close)
		service := New(config.LingyingBillingConfig{BaseURL: server.URL, Token: "token"}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if decision := service.Admit(ctx, lingyingInput()); decision.Verdict != Unavailable {
			t.Fatalf("Admit() = %+v, want Unavailable", decision)
		}
	})
}

func TestAdmitAsksForAnAccessTokenUsersDefaultKey(t *testing.T) {
	var got map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"user_id":"u1","key_id":"d1","usable":true,"available":"9"}`))
	}))
	t.Cleanup(server.Close)
	service := New(config.LingyingBillingConfig{BaseURL: server.URL, Token: "token"}, nil)

	decision := service.Admit(context.Background(), AdmitInput{
		KeyName: "ly:default:d1", Credential: Credential{UserID: "u1"}, ProjectID: "p1", Currency: pricing.CurrencyCNY,
	})

	if len(got) != 1 || got["user_id"] != "u1" {
		t.Fatalf("balance request = %v, want only user_id u1", got)
	}
	if decision.Verdict != Allow || decision.Admission == nil || *decision.Admission != (Admission{UserID: "u1", KeyID: "d1", ProjectID: "p1"}) {
		t.Fatalf("Admit() = %+v %+v", decision, decision.Admission)
	}
}
