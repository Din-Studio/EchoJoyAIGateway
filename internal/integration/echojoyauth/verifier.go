// Package echojoyauth verifies EchoJoyAuth access tokens offline: ES256 JWTs
// whose signing keys are published at the auth service's JWKS endpoint. It
// mirrors github.com/Din-Studio/EchoJoyAuth/pkg/jwtverify, which is private and
// would make every build need a GitHub credential.
package echojoyauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"gpt-load/internal/platform/config"
)

const (
	maxJWKSBytes = 1 << 20
	// minRefresh throttles JWKS fetches triggered by an unknown kid, so a flood
	// of forged kids cannot hammer the auth service.
	minRefresh = 5 * time.Minute
	// failureRetry is the shorter wait after a failed fetch, so a brief auth
	// service outage does not reject every access token for minRefresh.
	failureRetry = 10 * time.Second
	fetchTimeout = 10 * time.Second
)

var (
	errUnknownKey = errors.New("echojoyauth: unknown signing key")
	errInactive   = errors.New("echojoyauth: user is not active")
)

type claims struct {
	jwt.RegisteredClaims
	// Status is the user's status at issuance; only "active" may call.
	Status string `json:"sts"`
}

// Verifier checks the signature, issuer, audience (when configured), expiry
// and user status of an access token.
type Verifier struct {
	jwksURL  string
	issuer   string
	audience string
	http     *http.Client

	mu        sync.RWMutex
	keys      map[string]*ecdsa.PublicKey
	nextFetch time.Time
}

func New(cfg config.AccessTokenConfig) *Verifier {
	return &Verifier{
		jwksURL:  cfg.JWKSURL,
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		http:     &http.Client{Timeout: fetchTimeout},
		keys:     make(map[string]*ecdsa.PublicKey),
	}
}

// UserID returns the token's subject, the EchoJoyAuth user id.
func (v *Verifier) UserID(ctx context.Context, token string) (string, error) {
	options := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"ES256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
	}
	if v.audience != "" {
		options = append(options, jwt.WithAudience(v.audience))
	}
	var parsed claims
	_, err := jwt.ParseWithClaims(token, &parsed, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errUnknownKey
		}
		return v.key(ctx, kid)
	}, options...)
	if err != nil {
		return "", err
	}
	if parsed.Status != "active" {
		return "", errInactive
	}
	if parsed.Subject == "" {
		return "", errors.New("echojoyauth: token has no subject")
	}
	return parsed.Subject, nil
}

// key returns the public key for kid, refetching the JWKS on a miss at most
// once per minRefresh, or failureRetry after a failed fetch. The fetch outlives
// the request that triggered it, so a client disconnect cannot fail it.
func (v *Verifier) key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	v.mu.RLock()
	key, ok := v.keys[kid]
	v.mu.RUnlock()
	if ok {
		return key, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}
	if time.Now().Before(v.nextFetch) {
		return nil, errUnknownKey
	}
	keys, err := v.fetch(context.WithoutCancel(ctx))
	if err != nil {
		v.nextFetch = time.Now().Add(failureRetry)
		return nil, err
	}
	v.nextFetch = time.Now().Add(minRefresh)
	v.keys = keys
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	return nil, errUnknownKey
}

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

func (v *Verifier) fetch(ctx context.Context) (map[string]*ecdsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := v.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("echojoyauth: fetch jwks: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("echojoyauth: fetch jwks: status %d", response.StatusCode)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxJWKSBytes)).Decode(&set); err != nil {
		return nil, fmt.Errorf("echojoyauth: decode jwks: %w", err)
	}
	keys := make(map[string]*ecdsa.PublicKey, len(set.Keys))
	for _, entry := range set.Keys {
		if entry.Use != "sig" || entry.Alg != "ES256" {
			continue
		}
		if key, err := publicKey(entry); err == nil {
			keys[entry.Kid] = key
		}
	}
	return keys, nil
}

// publicKey decodes an EC P-256 JWK.
func publicKey(entry jwk) (*ecdsa.PublicKey, error) {
	if entry.Kty != "EC" || entry.Crv != "P-256" {
		return nil, errors.New("not an EC P-256 key")
	}
	x, err := base64.RawURLEncoding.DecodeString(entry.X)
	if err != nil {
		return nil, err
	}
	y, err := base64.RawURLEncoding.DecodeString(entry.Y)
	if err != nil {
		return nil, err
	}
	if len(x) > 32 || len(y) > 32 {
		return nil, errors.New("coordinate exceeds the P-256 field size")
	}
	key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !key.Curve.IsOnCurve(key.X, key.Y) {
		return nil, errors.New("point is not on the P-256 curve")
	}
	return key, nil
}
