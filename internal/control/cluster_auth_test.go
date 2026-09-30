package control

import (
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"

	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/platform/i18n"
)

// newClusterAuthProbeServer assembles an instance's admin authentication the
// way the container does in cluster mode.
func newClusterAuthProbeServer(t *testing.T, server *miniredis.Miniredis, instanceID string) *gin.Engine {
	t.Helper()
	control := NewServerWithReleaseUpdateChecker(
		&config.Config{AuthKey: authTestKey},
		nil,
		nil,
		cluster.NewAuthFailures(newJobTestClient(t, server, instanceID)),
	)
	engine := gin.New()
	api := engine.Group("/api")
	api.Use(i18n.Middleware(), control.authenticate())
	api.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
	return engine
}

func TestClusterAdminLockoutAppliesToEveryInstance(t *testing.T) {
	initControlI18n(t)
	server := miniredis.RunT(t)
	first := newClusterAuthProbeServer(t, server, "node-a")
	second := newClusterAuthProbeServer(t, server, "node-b")
	const peer = "192.0.2.80:1234"

	for range 3 {
		assertAuthStatus(t, first, peer, "Bearer wrong-key", http.StatusUnauthorized)
	}
	assertAuthStatus(t, second, peer, "Bearer wrong-key", http.StatusUnauthorized)
	assertAuthStatus(t, second, peer, "Bearer wrong-key", http.StatusTooManyRequests)
	locked := serveAuthRequest(first, "/api/probe", peer, "Bearer wrong-key", nil)
	if locked.Code != http.StatusTooManyRequests || locked.Header().Get("Retry-After") == "" {
		t.Fatalf("other instance response = %d with Retry-After %q, want locked",
			locked.Code, locked.Header().Get("Retry-After"))
	}

	assertAuthStatus(t, second, peer, "Bearer "+authTestKey, http.StatusOK)
	assertAuthStatus(t, first, peer, "Bearer wrong-key", http.StatusUnauthorized)
}

func TestClusterAdminLockoutFallsBackToInstanceLimiterWhenRedisIsDown(t *testing.T) {
	initControlI18n(t)
	server := miniredis.RunT(t)
	engine := newClusterAuthProbeServer(t, server, "node-a")
	server.Close()
	const peer = "192.0.2.81:1234"

	lockPeer(t, engine, peer)
	assertAuthStatus(t, engine, peer, "Bearer "+authTestKey, http.StatusOK)
}
