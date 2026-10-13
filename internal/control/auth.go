package control

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/identity"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/platform/utils"
)

const controlPeerContextKey = "gpt-load.control.peer-ip"

const controlPrincipalContextKey = "gpt-load.control.principal"

// Identity resolves console sessions and reads teams from Casdoor.
type Identity interface {
	// Authenticate returns identity.ErrUnauthenticated when the cookie names no live session.
	Authenticate(ctx context.Context, cookie string) (identity.Principal, error)
	Teams(ctx context.Context) ([]identity.Team, error)
	TeamMembers(ctx context.Context, team string) ([]identity.Member, error)
}

// routeAccess is the least a Principal needs to reach a control route.
// Handlers narrow the data further.
type routeAccess string

const (
	// accessSession admits every signed-in Principal.
	accessSession routeAccess = "session"
	// accessPlatform admits platform administrators only.
	accessPlatform routeAccess = "platform"
	// accessTeamAdmin admits platform administrators and anyone who manages a
	// team; the handler checks the concrete team or key.
	accessTeamAdmin routeAccess = "team_admin"
)

func (access routeAccess) allows(principal identity.Principal) bool {
	switch access {
	case accessSession:
		return true
	case accessPlatform:
		return principal.Platform
	case accessTeamAdmin:
		return principal.Platform || len(principal.AdminTeams) > 0
	default:
		return false
	}
}

type authSessionUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

type authSessionResponse struct {
	User       authSessionUser `json:"user"`
	Teams      []string        `json:"teams"`
	Platform   bool            `json:"platform"`
	AdminTeams []string        `json:"admin_teams"`
}

func (s *Server) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		peer, err := utils.NormalizePeerIP(c.Request.RemoteAddr)
		if err != nil {
			logServiceError("authenticate_peer", err, app_errors.ErrInternalServer.Code)
			response.ErrorI18nFromAPIError(c, app_errors.ErrInternalServer, "internal_error")
			c.Abort()
			return
		}
		cookie, _ := c.Cookie(identity.SessionCookie)
		principal, err := s.identity.Authenticate(c.Request.Context(), cookie)
		if errors.Is(err, identity.ErrUnauthenticated) {
			response.ErrorI18nFromAPIError(c, app_errors.ErrUnauthorized, "auth.session_required")
			c.Abort()
			return
		}
		if err != nil {
			logServiceError("authenticate_session", err, app_errors.ErrInternalServer.Code)
			response.ErrorI18nFromAPIError(c, app_errors.ErrInternalServer, "internal_error")
			c.Abort()
			return
		}
		// SameSite=Lax still sends the cookie on top-level cross-site POSTs.
		if !isSafeMethod(c.Request.Method) && c.GetHeader("Origin") != s.publicOrigin {
			response.ErrorI18nFromAPIError(c, app_errors.ErrForbidden, "auth.forbidden")
			c.Abort()
			return
		}
		c.Set(controlPeerContextKey, peer)
		c.Set(controlPrincipalContextKey, principal)
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}

func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// requireAccess rejects a signed-in Principal below access with 403.
func requireAccess(access routeAccess) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := currentPrincipal(c)
		if !ok || !access.allows(principal) {
			response.ErrorI18nFromAPIError(c, app_errors.ErrForbidden, "auth.forbidden")
			c.Abort()
			return
		}
		c.Next()
	}
}

func currentPrincipal(c *gin.Context) (identity.Principal, bool) {
	value, exists := c.Get(controlPrincipalContextKey)
	if !exists {
		return identity.Principal{}, false
	}
	principal, ok := value.(identity.Principal)
	return principal, ok
}

func (s *Server) handleAuthSession(c *gin.Context) {
	principal, ok := currentPrincipal(c)
	if !ok {
		writeServiceError(c, "auth_session", app_errors.ErrInternalServer)
		return
	}
	teams := principal.Teams
	if teams == nil {
		teams = []string{}
	}
	adminTeams := principal.AdminTeams
	if adminTeams == nil {
		adminTeams = []string{}
	}
	response.SuccessI18n(c, "common.success", authSessionResponse{
		User: authSessionUser{
			ID:          principal.UserID,
			Name:        principal.Name,
			DisplayName: principal.DisplayName,
			Email:       principal.Email,
		},
		Teams:      teams,
		Platform:   principal.Platform,
		AdminTeams: adminTeams,
	})
}
