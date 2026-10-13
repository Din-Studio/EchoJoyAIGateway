package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const (
	testClientID     = "gateway-client"
	testClientSecret = "gateway-secret"
	testOrganization = "echojoy"
)

type fakeGrant struct {
	userID    string
	nonce     string
	challenge string
}

// fakeCasdoor serves the slice of Casdoor the gateway uses: authorize, token,
// JWKS, get-user, batch-enforce, get-groups and get-users.
type fakeCasdoor struct {
	t      *testing.T
	server *httptest.Server
	key    *rsa.PrivateKey

	mu      sync.Mutex
	loginAs string
	users   map[string]User
	allowed map[string]bool // "<owner/name>|<domain>|<action>"
	groups  []Team
	grants  map[string]fakeGrant
	failing bool
	// wrongNonce makes the ID token carry a nonce the gateway never sent.
	wrongNonce bool
	apiCalls   atomic.Int32
}

func newFakeCasdoor(t *testing.T) *fakeCasdoor {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	fake := &fakeCasdoor{
		t: t, key: key, users: map[string]User{}, allowed: map[string]bool{}, grants: map[string]fakeGrant{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", fake.authorize)
	mux.HandleFunc("POST /api/login/oauth/access_token", fake.token)
	mux.HandleFunc("GET /.well-known/jwks", fake.jwks)
	mux.HandleFunc("GET /api/get-user", fake.api(fake.getUser))
	mux.HandleFunc("POST /api/batch-enforce", fake.api(fake.batchEnforce))
	mux.HandleFunc("GET /api/get-groups", fake.api(fake.getGroups))
	mux.HandleFunc("GET /api/get-users", fake.api(fake.getUsers))
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeCasdoor) addUser(user User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user.Owner = testOrganization
	f.users[user.ID] = user
}

func (f *fakeCasdoor) allow(name, domain, action string, allowed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowed[testOrganization+"/"+name+"|"+domain+"|"+action] = allowed
}

func (f *fakeCasdoor) setFailing(failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = failing
}

func (f *fakeCasdoor) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("client_id") != testClientID || query.Get("code_challenge_method") != "S256" ||
		query.Get("response_type") != "code" || !strings.Contains(query.Get("scope"), "openid") {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	code := "code-" + query.Get("state")
	f.mu.Lock()
	f.grants[code] = fakeGrant{userID: f.loginAs, nonce: query.Get("nonce"), challenge: query.Get("code_challenge")}
	f.mu.Unlock()
	target, _ := url.Parse(query.Get("redirect_uri"))
	target.RawQuery = url.Values{"code": {code}, "state": {query.Get("state")}}.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (f *fakeCasdoor) token(w http.ResponseWriter, r *http.Request) {
	clientID, secret, ok := r.BasicAuth()
	if !ok || clientID != testClientID || secret != testClientSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	_ = r.ParseForm()
	f.mu.Lock()
	grant, found := f.grants[r.PostForm.Get("code")]
	delete(f.grants, r.PostForm.Get("code"))
	if f.wrongNonce {
		grant.nonce = "other-" + grant.nonce
	}
	f.mu.Unlock()
	digest := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || base64.RawURLEncoding.EncodeToString(digest[:]) != grant.challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	now := time.Now()
	idToken := f.sign(map[string]any{
		"iss": f.server.URL, "sub": grant.userID, "aud": []string{testClientID},
		"nonce": grant.nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "access", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
	})
}

func (f *fakeCasdoor) sign(claims map[string]any) string {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: f.key, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		f.t.Fatalf("signer: %v", err)
	}
	payload, _ := json.Marshal(claims)
	object, err := signer.Sign(payload)
	if err != nil {
		f.t.Fatalf("sign: %v", err)
	}
	raw, _ := object.CompactSerialize()
	return raw
}

func (f *fakeCasdoor) jwks(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &f.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
	}})
}

func (f *fakeCasdoor) api(handler func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls.Add(1)
		clientID, secret, ok := r.BasicAuth()
		f.mu.Lock()
		failing := f.failing
		f.mu.Unlock()
		if failing {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		envelope := map[string]any{"status": "ok", "msg": ""}
		if !ok || clientID != testClientID || secret != testClientSecret {
			envelope = map[string]any{"status": "error", "msg": "Unauthorized operation"}
		} else if data, err := handler(r); err != nil {
			envelope = map[string]any{"status": "error", "msg": err.Error()}
		} else {
			envelope["data"] = data
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}
}

func (f *fakeCasdoor) getUser(r *http.Request) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, found := f.users[r.URL.Query().Get("userId")]
	if !found || r.URL.Query().Get("owner") != testOrganization {
		return nil, nil
	}
	return user, nil
}

func (f *fakeCasdoor) batchEnforce(r *http.Request) (any, error) {
	if r.URL.Query().Get("modelId") != testOrganization+"/"+ModelName {
		return nil, errBadModel
	}
	var requests [][]string
	if err := json.NewDecoder(r.Body).Decode(&requests); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row := make([]bool, 0, len(requests))
	for _, request := range requests {
		row = append(row, f.allowed[request[0]+"|"+request[1]+"|"+request[3]])
	}
	return [][]bool{row}, nil
}

func (f *fakeCasdoor) getGroups(*http.Request) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	groups := make([]map[string]string, 0, len(f.groups))
	for _, group := range f.groups {
		groups = append(groups, map[string]string{"owner": testOrganization, "name": group.Name, "displayName": group.DisplayName})
	}
	return groups, nil
}

func (f *fakeCasdoor) getUsers(r *http.Request) (any, error) {
	group := testOrganization + "/" + r.URL.Query().Get("groupName")
	f.mu.Lock()
	defer f.mu.Unlock()
	users := make([]User, 0)
	for _, user := range f.users {
		if slices.Contains(user.Groups, group) {
			users = append(users, user)
		}
	}
	slices.SortFunc(users, func(a, b User) int { return strings.Compare(a.Name, b.Name) })
	return users, nil
}

type fakeError string

func (e fakeError) Error() string { return string(e) }

const errBadModel = fakeError("model does not exist")
