package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/dialect"
	"gpt-load/internal/integration/ly"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
)

// fakeAccessTokens maps a token to its user id; any other token is invalid.
type fakeAccessTokens map[string]string

func (f fakeAccessTokens) UserID(_ context.Context, token string) (string, error) {
	if userID, ok := f[token]; ok {
		return userID, nil
	}
	return "", errors.New("invalid token")
}

const (
	userOneToken = "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ1c2VyLTEifQ.sig1"
	userTwoToken = "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ1c2VyLTIifQ.sig2"
)

// newAccessTokenTestRuntime serves gpt-4o in a CNY system. user-1 has the
// default key "ly:default:k9" (plaintext sk-default); user-2 has none.
func newAccessTokenTestRuntime(t *testing.T, billing *fakeExternalBilling) (*gin.Engine, *scriptedForwarder) {
	t.Helper()
	forwarder := &scriptedForwarder{results: []UpstreamResult{okResult()}}
	engine, handler, manager, _ := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-upstream")
	handler.billing = billing
	handler.dialects = dialect.NewSet(dialect.NewOpenAI(), dialect.NewAnthropic(), dialect.NewOpenAIResponses())
	handler.accessTokens = fakeAccessTokens{userOneToken: "user-1", userTwoToken: "user-2"}
	input := gatewayAccessQuotaCompileInput(handler, nil)
	input.AccessKeys = append(input.AccessKeys, state.AccessKeyConfig{
		ID: 2, Name: state.UserDefaultKeyNamePrefix + "k9", Note: "user-1",
		KeyHash: handler.encryption.Hash("sk-default"), Status: state.AccessKeyStatusActive,
	})
	input.SystemSettings = cnySettings("7.2")
	if _, err := manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	return engine, forwarder
}

func serveWithCredential(engine *gin.Engine, credential string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Project-Id", "p1")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func TestAccessTokenIsServedAsTheUsersDefaultKey(t *testing.T) {
	billing := &fakeExternalBilling{decision: billedAdmission}
	engine, forwarder := newAccessTokenTestRuntime(t, billing)

	response := serveWithCredential(engine, userOneToken)

	admits, records := billing.snapshot()
	if response.Code != http.StatusOK || len(forwarder.inputs) != 1 || len(admits) != 1 || len(records) != 1 {
		t.Fatalf("response %d %s, upstream calls %d, admits %d, records %d",
			response.Code, response.Body, len(forwarder.inputs), len(admits), len(records))
	}
	want := ly.AdmitInput{KeyName: "ly:default:k9", Credential: ly.Credential{UserID: "user-1"}, ProjectID: "p1", Currency: pricing.CurrencyCNY}
	if admits[0] != want {
		t.Fatalf("admit input = %+v, want %+v", admits[0], want)
	}
}

func TestAccessTokenRejections(t *testing.T) {
	for _, test := range []struct{ name, credential string }{
		{"invalid token", "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ4In0.forged"},
		{"user without a default key", userTwoToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			billing := &fakeExternalBilling{decision: billedAdmission}
			engine, forwarder := newAccessTokenTestRuntime(t, billing)

			response := serveWithCredential(engine, test.credential)

			var body struct{ Code string }
			_ = json.Unmarshal(response.Body.Bytes(), &body)
			admits, _ := billing.snapshot()
			if response.Code != http.StatusUnauthorized || body.Code != "invalid_access_key" || len(forwarder.inputs) != 0 || len(admits) != 0 {
				t.Fatalf("response %d %s, upstream calls %d, admits %d", response.Code, response.Body, len(forwarder.inputs), len(admits))
			}
		})
	}
}

func TestAccessKeysStillAuthenticateWithAccessTokensOn(t *testing.T) {
	billing := &fakeExternalBilling{decision: ly.Decision{Verdict: ly.Allow}}
	engine, forwarder := newAccessTokenTestRuntime(t, billing)

	if response := serveWithCredential(engine, "sk-default"); response.Code != http.StatusOK || len(forwarder.inputs) != 1 {
		t.Fatalf("response %d %s, upstream calls %d", response.Code, response.Body, len(forwarder.inputs))
	}
}
