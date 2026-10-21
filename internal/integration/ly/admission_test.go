package ly

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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
		stop := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The balance call outlives the request that gave up; stop ends it
			// before the server closes.
			<-stop
		}))
		t.Cleanup(server.Close)
		t.Cleanup(func() { close(stop) })
		service := New(config.LingyingBillingConfig{BaseURL: server.URL, Token: "token"}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		started := time.Now()
		decision := service.Admit(ctx, lingyingInput())
		// The request gives up at its own deadline, well before balanceTimeout.
		if elapsed := time.Since(started); decision.Verdict != Unavailable || elapsed > time.Second {
			t.Fatalf("Admit() = %+v after %v, want Unavailable at the request deadline", decision, elapsed)
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

const allowingBalance = `{"user_id":"u1","key_id":"k1","usable":true,"available":"9"}`

// A balance that allows requests is reused for balanceCacheTTL.
func TestAdmitReusesAnAllowingBalance(t *testing.T) {
	service, calls := fakeBalance(t, http.StatusOK, allowingBalance)
	clock := &clock{now: time.UnixMilli(1_000_000)}
	service.now = clock.Now

	service.Admit(context.Background(), lingyingInput())
	clock.Add(balanceCacheTTL - time.Millisecond)
	input := lingyingInput()
	input.ProjectID = "p2"
	decision := service.Admit(context.Background(), input)

	if calls.Load() != 1 || decision.Verdict != Allow ||
		*decision.Admission != (Admission{UserID: "u1", KeyID: "k1", ProjectID: "p2"}) {
		t.Fatalf("Admit() = %+v %+v after %d calls, want a cached allow for project p2", decision, decision.Admission, calls.Load())
	}
	clock.Add(time.Millisecond)
	service.Admit(context.Background(), lingyingInput())
	if calls.Load() != 2 {
		t.Fatalf("balance calls = %d after the TTL, want 2", calls.Load())
	}
}

// Every rejection asks Lingying again.
func TestAdmitDoesNotReuseARejection(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		want       Verdict
	}{
		{name: "unknown key", status: 404, body: `{}`, want: InvalidKey},
		{name: "unusable", status: 200, body: `{"user_id":"u1","usable":false,"available":"9"}`, want: Unusable},
		{name: "zero balance", status: 200, body: `{"user_id":"u1","usable":true,"available":"0"}`, want: Insufficient},
		{name: "unavailable", status: 503, body: `{}`, want: Unavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, calls := fakeBalance(t, test.status, test.body)

			first := service.Admit(context.Background(), lingyingInput())
			second := service.Admit(context.Background(), lingyingInput())

			if first.Verdict != test.want || second.Verdict != test.want || calls.Load() != 2 {
				t.Fatalf("Admit() = %d then %d after %d calls, want %d twice from 2 calls",
					first.Verdict, second.Verdict, calls.Load(), test.want)
			}
		})
	}
}

// Concurrent lookups of one credential share one balance call.
func TestAdmitJoinsConcurrentLookups(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		_, _ = w.Write([]byte(allowingBalance))
	}))
	t.Cleanup(server.Close)
	service := New(config.LingyingBillingConfig{BaseURL: server.URL, Token: "token"}, nil)

	var admits sync.WaitGroup
	verdicts := make(chan Verdict, 10)
	for range 10 {
		admits.Go(func() { verdicts <- service.Admit(context.Background(), lingyingInput()).Verdict })
	}
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	admits.Wait()
	close(verdicts)

	for verdict := range verdicts {
		if verdict != Allow {
			t.Fatalf("Admit() verdict = %d, want Allow", verdict)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("balance calls = %d, want 1", calls.Load())
	}
}

// An access key and an access token user are cached apart.
func TestAdmitCachesEachCredentialApart(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var credential Credential
		_ = json.NewDecoder(r.Body).Decode(&credential)
		mu.Lock()
		calls[credential.APIKey+"|"+credential.UserID]++
		mu.Unlock()
		_, _ = w.Write([]byte(allowingBalance))
	}))
	t.Cleanup(server.Close)
	service := New(config.LingyingBillingConfig{BaseURL: server.URL, Token: "token"}, nil)
	byKey := lingyingInput()
	byUser := lingyingInput()
	byUser.Credential = Credential{UserID: "sk-plain"}

	for range 2 {
		service.Admit(context.Background(), byKey)
		service.Admit(context.Background(), byUser)
	}

	if len(calls) != 2 || calls["sk-plain|"] != 1 || calls["|sk-plain"] != 1 {
		t.Fatalf("balance calls = %v, want one per credential", calls)
	}
}

// A request that gives up does not fail the others waiting on its lookup.
func TestAdmitKeepsASharedLookupWhenItsCallerGivesUp(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		_, _ = w.Write([]byte(allowingBalance))
	}))
	t.Cleanup(server.Close)
	service := New(config.LingyingBillingConfig{BaseURL: server.URL, Token: "token"}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan Verdict, 1)
	go func() { first <- service.Admit(ctx, lingyingInput()).Verdict }()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	second := make(chan Verdict, 1)
	go func() { second <- service.Admit(context.Background(), lingyingInput()).Verdict }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-first
	close(release)

	if verdict := <-second; verdict != Allow || calls.Load() != 1 {
		t.Fatalf("waiting Admit() = %d after %d calls, want Allow from the shared call", verdict, calls.Load())
	}
}
