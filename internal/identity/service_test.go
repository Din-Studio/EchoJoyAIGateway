package identity

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"

	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/platform/httproute"
	"gpt-load/internal/testutil/clustertest"
)

const testPublicURL = "http://gateway.test"

type identityFixture struct {
	fake    *fakeCasdoor
	service *Service
	engine  *gin.Engine
	clock   *time.Time
	redis   *miniredis.Miniredis
	client  *cluster.Client
}

func newIdentityFixture(t *testing.T) identityFixture {
	t.Helper()
	fake := newFakeCasdoor(t)
	redis, client := clustertest.NewClient(t)
	service := NewService(&config.Config{OIDC: config.OIDCConfig{
		PublicURL: testPublicURL, Issuer: fake.server.URL, ClientID: testClientID,
		ClientSecret: testClientSecret, Organization: testOrganization,
	}}, client)
	clock := time.Now()
	service.now = func() time.Time { return clock }
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registry, err := httproute.NewRegistry(service.HTTPModule())
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	if err := registry.Bind(engine); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	return identityFixture{fake: fake, service: service, engine: engine, clock: &clock, redis: redis, client: client}
}

func (f identityFixture) advance(d time.Duration) { *f.clock = f.clock.Add(d) }

func (f identityFixture) serve(request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	f.engine.ServeHTTP(recorder, request)
	return recorder
}

// pendingLogin is a login the browser started and Casdoor authorized, one
// request away from /auth/callback.
type pendingLogin struct {
	cookies  []*http.Cookie
	callback string
}

// startLogin drives /auth/login → Casdoor authorize as userID.
func (f identityFixture) startLogin(t *testing.T, userID string) pendingLogin {
	t.Helper()
	f.fake.mu.Lock()
	f.fake.loginAs = userID
	f.fake.mu.Unlock()
	start := f.serve(httptest.NewRequest(http.MethodGet, "/auth/login?redirect=/groups", nil))
	if start.Code != http.StatusFound || !strings.HasPrefix(start.Header().Get("Location"), f.fake.server.URL+"/login/oauth/authorize?") {
		t.Fatalf("login = %d %q", start.Code, start.Header().Get("Location"))
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	authorized, err := client.Get(start.Header().Get("Location"))
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = authorized.Body.Close()
	callback, err := url.Parse(authorized.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(callback.String(), testPublicURL+"/auth/callback?") {
		t.Fatalf("authorize redirect = %q", authorized.Header.Get("Location"))
	}
	return pendingLogin{cookies: start.Result().Cookies(), callback: callback.RequestURI()}
}

// finish sends the callback with the cookies the browser holds.
func (f identityFixture) finish(login pendingLogin, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, login.callback, nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	return f.serve(request)
}

// login signs userID in through the whole browser flow and returns the final
// callback response.
func (f identityFixture) login(t *testing.T, userID string) *httptest.ResponseRecorder {
	t.Helper()
	pending := f.startLogin(t, userID)
	return f.finish(pending, pending.cookies...)
}

func hasSessionCookie(recorder *httptest.ResponseRecorder) bool {
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == SessionCookie {
			return true
		}
	}
	return false
}

func sessionCookie(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == SessionCookie {
			return cookie
		}
	}
	t.Fatalf("no %s cookie in %v", SessionCookie, recorder.Header())
	return nil
}

func TestLoginCreatesSessionWithTeamRoles(t *testing.T) {
	fixture := newIdentityFixture(t)
	fixture.fake.addUser(User{Name: "alice", ID: "uid-alice", DisplayName: "Alice", Groups: []string{"echojoy/team-a", "echojoy/team-b"}})
	fixture.fake.allow("alice", "team-a", ActionTeam, true)

	callback := fixture.login(t, "uid-alice")
	if callback.Code != http.StatusFound || callback.Header().Get("Location") != "/groups" {
		t.Fatalf("callback = %d %q", callback.Code, callback.Header().Get("Location"))
	}
	cookie := sessionCookie(t, callback)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure || cookie.Path != "/" {
		t.Fatalf("cookie = %+v", cookie)
	}
	principal, err := fixture.service.Authenticate(t.Context(), cookie.Value)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	want := Principal{
		UserID: "uid-alice", Name: "alice", DisplayName: "Alice",
		Teams: []string{"team-a", "team-b"}, AdminTeams: []string{"team-a"},
	}
	if !reflect.DeepEqual(principal, want) {
		t.Fatalf("principal = %+v, want %+v", principal, want)
	}
}

func TestLoginRejectsUsersWithoutAccess(t *testing.T) {
	fixture := newIdentityFixture(t)
	fixture.fake.addUser(User{Name: "nobody", ID: "uid-nobody"})
	fixture.fake.addUser(User{Name: "banned", ID: "uid-banned", Groups: []string{"echojoy/team-a"}, IsForbidden: true})
	for _, userID := range []string{"uid-nobody", "uid-banned", "uid-missing"} {
		callback := fixture.login(t, userID)
		if callback.Header().Get("Location") != "/login?error=no_access" || hasSessionCookie(callback) {
			t.Fatalf("%s callback = %d %q", userID, callback.Code, callback.Header().Get("Location"))
		}
	}
}

func TestCallbackRejectsReplayedState(t *testing.T) {
	fixture := newIdentityFixture(t)
	fixture.fake.addUser(User{Name: "demo", ID: "uid-demo"})
	fixture.fake.allow("demo", PlatformDomain, ActionPlatform, true)
	pending := fixture.startLogin(t, "uid-demo")
	state, _ := url.Parse(pending.callback)
	stateKey := LoginStateKey(fixture.client, state.Query().Get("state"))
	if !fixture.redis.Exists(stateKey) {
		t.Fatal("login state missing before the callback")
	}
	if first := fixture.finish(pending, pending.cookies...); first.Header().Get("Location") != "/groups" {
		t.Fatalf("first callback = %q", first.Header().Get("Location"))
	}
	if fixture.redis.Exists(stateKey) {
		t.Fatal("login state survived the callback")
	}
	if replay := fixture.finish(pending, pending.cookies...); replay.Header().Get("Location") != "/login?error=login_failed" {
		t.Fatalf("replayed callback = %q", replay.Header().Get("Location"))
	}
}

func TestCallbackRejectsAnotherBrowser(t *testing.T) {
	fixture := newIdentityFixture(t)
	fixture.fake.addUser(User{Name: "demo", ID: "uid-demo"})
	fixture.fake.allow("demo", PlatformDomain, ActionPlatform, true)
	// The attacker starts a login and hands the callback URL to a victim
	// whose browser never saw the login cookie.
	pending := fixture.startLogin(t, "uid-demo")
	if len(pending.cookies) != 1 || pending.cookies[0].Name != loginCookie || !pending.cookies[0].HttpOnly || pending.cookies[0].Path != "/auth" {
		t.Fatalf("login cookies = %+v", pending.cookies)
	}
	victim := fixture.finish(pending)
	if victim.Header().Get("Location") != "/login?error=login_failed" {
		t.Fatalf("callback without login cookie = %q", victim.Header().Get("Location"))
	}
	if hasSessionCookie(victim) {
		t.Fatal("victim received a session")
	}
	other := fixture.finish(fixture.startLogin(t, "uid-demo"), &http.Cookie{Name: loginCookie, Value: "forged"})
	if other.Header().Get("Location") != "/login?error=login_failed" {
		t.Fatalf("callback with a forged login cookie = %q", other.Header().Get("Location"))
	}
}

func TestCallbackRejectsWrongNonce(t *testing.T) {
	fixture := newIdentityFixture(t)
	fixture.fake.addUser(User{Name: "demo", ID: "uid-demo"})
	fixture.fake.allow("demo", PlatformDomain, ActionPlatform, true)
	fixture.fake.mu.Lock()
	fixture.fake.wrongNonce = true
	fixture.fake.mu.Unlock()
	callback := fixture.login(t, "uid-demo")
	if callback.Header().Get("Location") != "/login?error=login_failed" || hasSessionCookie(callback) {
		t.Fatalf("callback = %q cookies %v", callback.Header().Get("Location"), callback.Result().Cookies())
	}
}

func TestAuthenticateRefreshesFromCasdoorEveryInterval(t *testing.T) {
	fixture := newIdentityFixture(t)
	fixture.fake.addUser(User{Name: "alice", ID: "uid-alice", Groups: []string{"echojoy/team-a"}})
	fixture.fake.allow("alice", "team-a", ActionTeam, true)
	cookie := sessionCookie(t, fixture.login(t, "uid-alice")).Value
	ctx := t.Context()

	fixture.fake.allow("alice", "team-a", ActionTeam, false)
	if principal, _ := fixture.service.Authenticate(ctx, cookie); len(principal.AdminTeams) != 1 {
		t.Fatalf("principal changed before the interval: %+v", principal)
	}
	fixture.advance(refreshInterval + time.Second)
	if principal, err := fixture.service.Authenticate(ctx, cookie); err != nil || len(principal.AdminTeams) != 0 {
		t.Fatalf("refreshed principal = %+v, %v", principal, err)
	}

	// Casdoor outage: keep the cached principal and wait a full interval before retrying.
	fixture.fake.allow("alice", "team-a", ActionTeam, true)
	fixture.fake.setFailing(true)
	fixture.advance(refreshInterval + time.Second)
	before := fixture.fake.apiCalls.Load()
	for range 3 {
		if principal, err := fixture.service.Authenticate(ctx, cookie); err != nil || principal.UserID != "uid-alice" {
			t.Fatalf("outage principal = %+v, %v", principal, err)
		}
	}
	if calls := fixture.fake.apiCalls.Load() - before; calls != 1 {
		t.Fatalf("Casdoor calls during outage = %d, want 1", calls)
	}

	// Losing every team ends the session.
	fixture.fake.setFailing(false)
	fixture.fake.addUser(User{Name: "alice", ID: "uid-alice"})
	fixture.advance(refreshInterval + time.Second)
	if _, err := fixture.service.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
	}
	if _, found, _ := fixture.service.sessions.Get(ctx, cookie); found {
		t.Fatal("session survived losing access")
	}
}

func TestSessionSaveDoesNotResurrectDeletedSession(t *testing.T) {
	server, client := clustertest.NewClient(t)
	store := NewSessionStore(client)
	ctx := t.Context()
	cookie, err := store.Create(ctx, Session{Principal: Principal{UserID: "u"}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	server.FastForward(time.Hour)
	if err := store.Save(ctx, cookie, Session{Principal: Principal{UserID: "u"}, CheckedAtMS: 1}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if ttl := server.TTL(SessionKey(client, cookie)); ttl != sessionTTL-time.Hour {
		t.Fatalf("TTL after Save = %v, want %v", ttl, sessionTTL-time.Hour)
	}
	if err := store.Delete(ctx, cookie); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Save(ctx, cookie, Session{Principal: Principal{UserID: "u"}}); err != nil {
		t.Fatalf("Save() after Delete error = %v", err)
	}
	if server.Exists(SessionKey(client, cookie)) {
		t.Fatal("Save recreated a deleted session")
	}
	service := &Service{sessions: store, now: time.Now}
	if _, err := service.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Authenticate() error = %v", err)
	}
}

func TestLogoutChecksOriginAndEndsSession(t *testing.T) {
	fixture := newIdentityFixture(t)
	fixture.fake.addUser(User{Name: "demo", ID: "uid-demo"})
	fixture.fake.allow("demo", PlatformDomain, ActionPlatform, true)
	cookie := sessionCookie(t, fixture.login(t, "uid-demo"))

	forged := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	forged.AddCookie(cookie)
	forged.Header.Set("Origin", "http://evil.test")
	if recorder := fixture.serve(forged); recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin logout = %d", recorder.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", testPublicURL)
	recorder := fixture.serve(request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("logout = %d %s", recorder.Code, recorder.Body)
	}
	wantRedirect := fixture.fake.server.URL + "/api/logout?client_id=" + testClientID +
		"&post_logout_redirect_uri=" + url.QueryEscape(testPublicURL+"/login")
	var body struct {
		Data logoutResponse `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Data.RedirectURL != wantRedirect {
		t.Fatalf("logout body = %s, want %s", recorder.Body, wantRedirect)
	}
	if cleared := sessionCookie(t, recorder); cleared.MaxAge >= 0 {
		t.Fatalf("cookie not cleared: %+v", cleared)
	}
	if _, err := fixture.service.Authenticate(t.Context(), cookie.Value); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Authenticate() after logout error = %v", err)
	}
}

func TestTeamsAndMembersComeFromCasdoor(t *testing.T) {
	fixture := newIdentityFixture(t)
	fixture.fake.groups = []Team{{Name: "team-a", DisplayName: "Team A"}}
	fixture.fake.addUser(User{Name: "m1", ID: "uid-m1", DisplayName: "M1", Groups: []string{"echojoy/team-a"}})
	fixture.fake.addUser(User{Name: "other", ID: "uid-other", Groups: []string{"echojoy/team-b"}})
	teams, err := fixture.service.Teams(t.Context())
	if err != nil || !reflect.DeepEqual(teams, []Team{{Name: "team-a", DisplayName: "Team A"}}) {
		t.Fatalf("Teams() = %v, %v", teams, err)
	}
	members, err := fixture.service.TeamMembers(t.Context(), "team-a")
	if err != nil || !reflect.DeepEqual(members, []Member{{ID: "uid-m1", Name: "m1", DisplayName: "M1"}}) {
		t.Fatalf("TeamMembers() = %v, %v", members, err)
	}
}
