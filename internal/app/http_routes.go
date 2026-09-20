package app

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/platform/httproute"
	"gpt-load/internal/platform/version"
)

// healthCoordinationTimeout bounds the liveness probe's Redis round trip so a
// stalled coordination backend cannot hold the health endpoint open.
const healthCoordinationTimeout = 2 * time.Second

// HTTPModule declares process-level HTTP endpoints. A nil coordination backend
// is single-instance mode and keeps the response body unchanged.
func HTTPModule(coordination Coordination) httproute.Module {
	return httproute.Module{
		Name:              "system",
		Owner:             httproute.OwnerSystem,
		Auth:              httproute.AuthNone,
		NamespacePrefixes: []string{"/health"},
		Routes: []httproute.Route{
			{
				Name:    "system.health",
				Methods: []string{http.MethodGet},
				Path:    "/health",
				Handlers: gin.HandlersChain{
					func(c *gin.Context) {
						body := gin.H{
							"status":  "ok",
							"version": version.Version,
						}
						if coordination != nil {
							// Redis reachability is reported in the body, never
							// in the status code: a Sentinel failover must not
							// take every instance out of rotation at once.
							ctx, cancel := context.WithTimeout(c.Request.Context(), healthCoordinationTimeout)
							defer cancel()
							if err := coordination.Ping(ctx); err != nil {
								body["redis"] = "unavailable"
							} else {
								body["redis"] = "ok"
							}
						}
						c.JSON(http.StatusOK, body)
					},
				},
			},
		},
	}
}
