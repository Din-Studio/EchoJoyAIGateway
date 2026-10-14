package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/identity"
	"gpt-load/internal/platform/config"
)

// serveSessionRequest sends one console request carrying cookie as the session.
func serveSessionRequest(engine *gin.Engine, method, target, cookie string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Accept-Language", "en-US")
	setTestSession(request, cookie)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func newSessionProbeEngine(t *testing.T) (*Server, *gin.Engine) {
	t.Helper()
	initControlI18n(t)
	server := newTestServer(t, &config.Config{}, nil)
	engine := gin.New()
	server.RegisterRoutes(engine)
	return server, engine
}

func TestAuthSessionReturnsTheSignedInPrincipal(t *testing.T) {
	t.Parallel()
	_, engine := newSessionProbeEngine(t)
	recorder := serveSessionRequest(engine, http.MethodGet, "/api/auth/session", testTeamAdminSession)
	if recorder.Code != http.StatusOK {
		t.Fatalf("session = %d %s", recorder.Code, recorder.Body)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", recorder.Header().Get("Cache-Control"))
	}
	var envelope struct {
		Data authSessionResponse `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	want := authSessionResponse{
		User:       authSessionUser{ID: "uid-alice", Name: "alice", DisplayName: "Alice"},
		Teams:      []string{"team-a", "team-b"},
		AdminTeams: []string{"team-a"},
		// Members and team admins may only scope the console to their own teams.
		SelectableTeams: []identity.Team{{Name: "team-a", DisplayName: "team-a"}, {Name: "team-b", DisplayName: "team-b"}},
	}
	if !reflect.DeepEqual(envelope.Data, want) {
		t.Fatalf("session = %+v, want %+v", envelope.Data, want)
	}

	// Platform admins may scope the console to any Casdoor team.
	recorder = serveSessionRequest(engine, http.MethodGet, "/api/auth/session", testPlatformSession)
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	wantPlatform := []identity.Team{{Name: "team-a", DisplayName: "Team A"}, {Name: "team-b", DisplayName: "Team B"}}
	if recorder.Code != http.StatusOK || !reflect.DeepEqual(envelope.Data.SelectableTeams, wantPlatform) {
		t.Fatalf("platform session = %d %+v, want selectable %+v", recorder.Code, envelope.Data, wantPlatform)
	}
}

func TestControlRoutesRequireASession(t *testing.T) {
	t.Parallel()
	_, engine := newSessionProbeEngine(t)
	for _, cookie := range []string{"", "expired-session"} {
		recorder := serveSessionRequest(engine, http.MethodGet, "/api/auth/session", cookie)
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), `"code":"UNAUTHORIZED"`) ||
			!strings.Contains(recorder.Body.String(), "Sign in to continue") {
			t.Fatalf("cookie %q = %d %s", cookie, recorder.Code, recorder.Body)
		}
	}
}

func TestUnsafeControlRequestsRequireThePublicOrigin(t *testing.T) {
	t.Parallel()
	_, engine := newSessionProbeEngine(t)
	for _, origin := range []string{"", "http://evil.test", testPublicURL + ".evil"} {
		request := httptest.NewRequest(http.MethodPost, "/api/model-prices/sync", nil)
		setTestPlatformSession(request)
		if origin == "" {
			request.Header.Del("Origin")
		} else {
			request.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("origin %q = %d %s", origin, recorder.Code, recorder.Body)
		}
	}
	get := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	setTestPlatformSession(get)
	get.Header.Del("Origin")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, get)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET without Origin = %d", recorder.Code)
	}
}

func TestMembersCannotReachPlatformRoutes(t *testing.T) {
	t.Parallel()
	_, engine := newSessionProbeEngine(t)
	for _, target := range []string{"/api/groups", "/api/settings", "/api/system/info"} {
		recorder := serveSessionRequest(engine, http.MethodGet, target, testMemberSession)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"code":"FORBIDDEN"`) {
			t.Fatalf("member %s = %d %s", target, recorder.Code, recorder.Body)
		}
	}
}

func TestIdentityFailureIsAnInternalError(t *testing.T) {
	t.Parallel()
	server, engine := newSessionProbeEngine(t)
	server.identity.(*stubIdentity).err = errors.New("redis down")
	recorder := serveSessionRequest(engine, http.MethodGet, "/api/auth/session", testPlatformSession)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("session = %d %s", recorder.Code, recorder.Body)
	}
}
