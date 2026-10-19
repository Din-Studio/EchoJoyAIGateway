package echojoyauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"gpt-load/internal/platform/config"
)

const testIssuer = "echojoyauth"

func newTestVerifier(t *testing.T, audience string) (*Verifier, *ecdsa.PrivateKey, *atomic.Int32) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fetches := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "kid": "k1", "use": "sig", "alg": "ES256",
			"x": base64.RawURLEncoding.EncodeToString(private.X.FillBytes(make([]byte, 32))),
			"y": base64.RawURLEncoding.EncodeToString(private.Y.FillBytes(make([]byte, 32))),
		}}})
	}))
	t.Cleanup(server.Close)
	return New(config.AccessTokenConfig{JWKSURL: server.URL, Issuer: testIssuer, Audience: audience}), private, fetches
}

func sign(t *testing.T, key any, method jwt.SigningMethod, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub": "user-1", "iss": testIssuer, "aud": []string{"lingying"}, "sts": "active",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
}

func TestVerifierReturnsTheSubjectOfAValidToken(t *testing.T) {
	verifier, private, fetches := newTestVerifier(t, "lingying")

	for range 2 {
		userID, err := verifier.UserID(context.Background(), sign(t, private, jwt.SigningMethodES256, "k1", validClaims()))
		if err != nil || userID != "user-1" {
			t.Fatalf("UserID = (%q, %v), want user-1", userID, err)
		}
	}
	if fetches.Load() != 1 {
		t.Fatalf("JWKS fetched %d times, want 1 (cached)", fetches.Load())
	}
}

func TestVerifierRejectsInvalidTokens(t *testing.T) {
	verifier, private, _ := newTestVerifier(t, "lingying")
	with := func(key string, value any) jwt.MapClaims {
		claims := validClaims()
		if value == nil {
			delete(claims, key)
		} else {
			claims[key] = value
		}
		return claims
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for _, test := range []struct {
		name  string
		token string
	}{
		{"expired", sign(t, private, jwt.SigningMethodES256, "k1", with("exp", time.Now().Add(-time.Minute).Unix()))},
		{"no exp", sign(t, private, jwt.SigningMethodES256, "k1", with("exp", nil))},
		{"wrong issuer", sign(t, private, jwt.SigningMethodES256, "k1", with("iss", "someone-else"))},
		{"wrong audience", sign(t, private, jwt.SigningMethodES256, "k1", with("aud", []string{"other"}))},
		{"inactive user", sign(t, private, jwt.SigningMethodES256, "k1", with("sts", "banned"))},
		{"no subject", sign(t, private, jwt.SigningMethodES256, "k1", with("sub", nil))},
		{"foreign signature", sign(t, other, jwt.SigningMethodES256, "k1", validClaims())},
		{"unknown kid", sign(t, private, jwt.SigningMethodES256, "k2", validClaims())},
		{"HS256", sign(t, []byte("secret"), jwt.SigningMethodHS256, "k1", validClaims())},
		{"garbage", "eyJ.not.a-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if userID, err := verifier.UserID(context.Background(), test.token); err == nil {
				t.Fatalf("UserID = %q, want an error", userID)
			}
		})
	}
}

func TestVerifierWithoutAudienceAcceptsAnyAudience(t *testing.T) {
	verifier, private, _ := newTestVerifier(t, "")
	claims := validClaims()
	claims["aud"] = []string{"any-product"}

	if userID, err := verifier.UserID(context.Background(), sign(t, private, jwt.SigningMethodES256, "k1", claims)); err != nil || userID != "user-1" {
		t.Fatalf("UserID = (%q, %v), want user-1", userID, err)
	}
}

func TestVerifierFetchesTheJWKSEvenIfTheRequestIsCanceled(t *testing.T) {
	verifier, private, _ := newTestVerifier(t, "lingying")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if userID, err := verifier.UserID(ctx, sign(t, private, jwt.SigningMethodES256, "k1", validClaims())); err != nil || userID != "user-1" {
		t.Fatalf("UserID = (%q, %v), want user-1", userID, err)
	}
}

func TestVerifierRetriesAFailedJWKSFetchSooner(t *testing.T) {
	verifier, private, _ := newTestVerifier(t, "lingying")
	verifier.jwksURL = "http://127.0.0.1:0/unreachable"
	token := sign(t, private, jwt.SigningMethodES256, "k1", validClaims())

	if _, err := verifier.UserID(context.Background(), token); err == nil {
		t.Fatal("UserID succeeded without a JWKS")
	}
	if wait := time.Until(verifier.nextFetch); wait > failureRetry {
		t.Fatalf("next fetch in %v, want at most %v after a failure", wait, failureRetry)
	}
}
