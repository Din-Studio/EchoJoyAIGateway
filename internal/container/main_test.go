package container

import (
	"math"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"gpt-load/internal/cluster"
	"gpt-load/internal/identity"
	"gpt-load/internal/storage"
	"gpt-load/internal/testutil/pgtest"
)

const testPublicURL = "http://gateway.test"

func TestMain(m *testing.M) {
	pgtest.Run(m, storage.AutoMigrate)
}

// setStartupEnv sets every variable startup requires: a fresh PostgreSQL
// database, the encryption key, Casdoor, and a miniredis server. Startup
// never contacts Casdoor, so its address need not resolve.
func setStartupEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_DSN", pgtest.NewEmptyDatabase(t))
	t.Setenv("ENCRYPTION_KEY", "test-master-key-long")
	t.Setenv("REDIS_ADDRS", miniredis.RunT(t).Addr())
	t.Setenv("PUBLIC_URL", testPublicURL)
	t.Setenv("OIDC_ISSUER", "http://casdoor.invalid")
	t.Setenv("OIDC_CLIENT_ID", "test-client")
	t.Setenv("OIDC_CLIENT_SECRET", "test-secret")
	t.Setenv("CASDOOR_ORGANIZATION", "echojoy")
}

// createPlatformSession stores a platform administrator's session in the
// container's Redis and returns its cookie. The far-future check time keeps
// the session from refreshing against Casdoor.
func createPlatformSession(t *testing.T, client *cluster.Client) string {
	t.Helper()
	cookie, err := identity.NewSessionStore(client).Create(t.Context(), identity.Session{
		Principal:   identity.Principal{UserID: "uid-platform", Name: "platform", Platform: true},
		CheckedAtMS: math.MaxInt64 / 2,
	})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	return cookie
}

// setSession sends cookie as the console session from the public origin.
func setSession(request *http.Request, cookie string) {
	request.AddCookie(&http.Cookie{Name: identity.SessionCookie, Value: cookie})
	request.Header.Set("Origin", testPublicURL)
}
