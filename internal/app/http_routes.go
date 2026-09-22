package app

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/platform/httproute"
	"gpt-load/internal/platform/version"
)

const readinessCheckTimeout = 2 * time.Second

// ReadinessProbe reports the health of every infrastructure dependency the
// process cannot serve without. Each key is a dependency name; a nil value
// means healthy. It is nil in single-instance mode, where /health stays a
// static liveness response.
type ReadinessProbe interface {
	Check(context.Context) map[string]error
}

// HTTPModule declares process-level HTTP endpoints.
func HTTPModule(probe ReadinessProbe) httproute.Module {
	return httproute.Module{
		Name:              "system",
		Owner:             httproute.OwnerSystem,
		Auth:              httproute.AuthNone,
		NamespacePrefixes: []string{"/health"},
		Routes: []httproute.Route{
			{
				Name:     "system.health",
				Methods:  []string{http.MethodGet},
				Path:     "/health",
				Handlers: gin.HandlersChain{healthHandler(probe)},
			},
		},
	}
}

func healthHandler(probe ReadinessProbe) gin.HandlerFunc {
	return func(c *gin.Context) {
		if probe == nil {
			c.JSON(http.StatusOK, gin.H{
				"status":  "ok",
				"version": version.Version,
			})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), readinessCheckTimeout)
		defer cancel()
		results := probe.Check(ctx)
		checks := make(map[string]string, len(results))
		status := http.StatusOK
		for name, err := range results {
			if err != nil {
				checks[name] = err.Error()
				status = http.StatusServiceUnavailable
				continue
			}
			checks[name] = "ok"
		}
		body := gin.H{
			"status":  "ok",
			"version": version.Version,
			"checks":  checks,
		}
		if status != http.StatusOK {
			body["status"] = "unavailable"
		}
		c.JSON(status, body)
	}
}
