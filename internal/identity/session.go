package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"gpt-load/internal/cluster"
)

const (
	sessionTTL    = 12 * time.Hour
	loginStateTTL = 10 * time.Minute
)

// Session is what Redis holds for one signed-in browser.
type Session struct {
	Principal   Principal `json:"principal"`
	CheckedAtMS int64     `json:"checked_at_ms"`
}

// LoginState carries one authorization request from /auth/login to /auth/callback.
type LoginState struct {
	Verifier string `json:"verifier"`
	Nonce    string `json:"nonce"`
	Redirect string `json:"redirect"`
	// Browser is the digest of the login cookie set on the browser that
	// started the request, so a callback URL cannot sign in another browser.
	Browser string `json:"browser"`
}

// SessionStore keeps sessions and pending logins in the shared Redis, so
// every replica sees them.
type SessionStore struct {
	client *cluster.Client
}

// NewSessionStore builds the store on the cluster Redis.
func NewSessionStore(client *cluster.Client) *SessionStore {
	return &SessionStore{client: client}
}

// SessionKey is the Redis key of the session a cookie names. Only the cookie's
// digest reaches Redis.
func SessionKey(client *cluster.Client, cookie string) string {
	return client.Key("sess", tokenDigest(cookie))
}

func tokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

// Create stores a new session and returns its cookie value.
func (s *SessionStore) Create(ctx context.Context, session Session) (string, error) {
	cookie, err := randomToken()
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(session)
	if err != nil {
		return "", fmt.Errorf("encode session: %w", err)
	}
	if err := s.client.Set(ctx, SessionKey(s.client, cookie), encoded, sessionTTL).Err(); err != nil {
		return "", fmt.Errorf("store session: %w", err)
	}
	return cookie, nil
}

// Get returns the session a cookie names; found is false when it is missing or expired.
func (s *SessionStore) Get(ctx context.Context, cookie string) (Session, bool, error) {
	raw, err := s.client.Get(ctx, SessionKey(s.client, cookie)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("read session: %w", err)
	}
	var session Session
	if err := json.Unmarshal(raw, &session); err != nil {
		return Session{}, false, fmt.Errorf("decode session: %w", err)
	}
	return session, true, nil
}

// Save rewrites an existing session and keeps its expiry. XX keeps a refresh
// racing a logout from resurrecting the session without a TTL.
func (s *SessionStore) Save(ctx context.Context, cookie string, session Session) error {
	encoded, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	err = s.client.SetArgs(ctx, SessionKey(s.client, cookie), encoded, redis.SetArgs{Mode: "XX", KeepTTL: true}).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

// Delete removes the session a cookie names.
func (s *SessionStore) Delete(ctx context.Context, cookie string) error {
	if err := s.client.Del(ctx, SessionKey(s.client, cookie)).Err(); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// LoginStateKey is the Redis key of one pending login.
func LoginStateKey(client *cluster.Client, state string) string {
	return client.Key("oidc-login", state)
}

// PutLoginState stores a pending login under its state parameter.
func (s *SessionStore) PutLoginState(ctx context.Context, state string, login LoginState) error {
	encoded, err := json.Marshal(login)
	if err != nil {
		return fmt.Errorf("encode login state: %w", err)
	}
	if err := s.client.Set(ctx, LoginStateKey(s.client, state), encoded, loginStateTTL).Err(); err != nil {
		return fmt.Errorf("store login state: %w", err)
	}
	return nil
}

// TakeLoginState returns and removes a pending login, so each state is used once.
func (s *SessionStore) TakeLoginState(ctx context.Context, state string) (LoginState, bool, error) {
	raw, err := s.client.GetDel(ctx, LoginStateKey(s.client, state)).Bytes()
	if errors.Is(err, redis.Nil) {
		return LoginState{}, false, nil
	}
	if err != nil {
		return LoginState{}, false, fmt.Errorf("read login state: %w", err)
	}
	var login LoginState
	if err := json.Unmarshal(raw, &login); err != nil {
		return LoginState{}, false, fmt.Errorf("decode login state: %w", err)
	}
	return login, true, nil
}

func randomToken() (string, error) {
	var buffer [32]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer[:]), nil
}
