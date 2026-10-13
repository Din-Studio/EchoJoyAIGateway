package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"

	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/config"
)

const (
	// SessionCookie names the httpOnly cookie that carries the session token.
	SessionCookie = "gl_session"
	// refreshInterval bounds how long a Casdoor permission change takes to apply.
	refreshInterval = 5 * time.Minute
)

// ErrUnauthenticated means the request carries no live session.
var ErrUnauthenticated = errors.New("identity: not signed in")

// errNoAccess means Casdoor knows the user but the gateway must not let them in.
var errNoAccess = errors.New("identity: no access")

// Service logs users in through Casdoor and resolves session cookies to Principals.
type Service struct {
	cfg      config.OIDCConfig
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	casdoor  *casdoorClient
	sessions *SessionStore
	http     *http.Client
	now      func() time.Time
	logger   *logrus.Logger
}

// NewService wires Casdoor's fixed endpoints. It does not touch the network:
// the JWKS is fetched on the first ID token verification.
func NewService(cfg *config.Config, client *cluster.Client) *Service {
	oidcConfig := cfg.OIDC
	httpClient := &http.Client{Timeout: casdoorTimeout}
	keySet := oidc.NewRemoteKeySet(
		oidc.ClientContext(context.Background(), httpClient),
		oidcConfig.Issuer+"/.well-known/jwks",
	)
	return &Service{
		cfg: oidcConfig,
		oauth: oauth2.Config{
			ClientID:     oidcConfig.ClientID,
			ClientSecret: oidcConfig.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:   oidcConfig.Issuer + "/login/oauth/authorize",
				TokenURL:  oidcConfig.Issuer + "/api/login/oauth/access_token",
				AuthStyle: oauth2.AuthStyleInHeader,
			},
			RedirectURL: oidcConfig.PublicURL + "/auth/callback",
			Scopes:      []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: oidc.NewVerifier(oidcConfig.Issuer, keySet, &oidc.Config{ClientID: oidcConfig.ClientID}),
		casdoor: newCasdoorClient(
			oidcConfig.Issuer, oidcConfig.ClientID, oidcConfig.ClientSecret, oidcConfig.Organization,
		),
		sessions: NewSessionStore(client),
		http:     httpClient,
		now:      time.Now,
		logger:   logrus.StandardLogger(),
	}
}

// Authenticate resolves a session cookie. Every five minutes it recompiles the
// Principal from Casdoor; when Casdoor is unreachable it keeps the cached one
// and waits another interval, so an outage costs one timeout per session per
// interval instead of one per request.
func (s *Service) Authenticate(ctx context.Context, cookie string) (Principal, error) {
	if cookie == "" {
		return Principal{}, ErrUnauthenticated
	}
	session, found, err := s.sessions.Get(ctx, cookie)
	if err != nil {
		return Principal{}, err
	}
	if !found {
		return Principal{}, ErrUnauthenticated
	}
	now := s.now().UnixMilli()
	if now-session.CheckedAtMS <= refreshInterval.Milliseconds() {
		return session.Principal, nil
	}
	principal, err := s.loadPrincipal(ctx, session.Principal.UserID)
	switch {
	case errors.Is(err, errNoAccess):
		if deleteErr := s.sessions.Delete(ctx, cookie); deleteErr != nil {
			return Principal{}, deleteErr
		}
		return Principal{}, ErrUnauthenticated
	case err != nil:
		s.logger.WithFields(logrus.Fields{
			"event":   "identity.refresh_failed",
			"user_id": session.Principal.UserID,
		}).WithError(err).Warn("Casdoor refresh failed; keeping the cached principal")
		principal = session.Principal
	}
	if err := s.sessions.Save(ctx, cookie, Session{Principal: principal, CheckedAtMS: now}); err != nil {
		return Principal{}, err
	}
	return principal, nil
}

// Logout deletes the session and returns where the browser goes to end its
// Casdoor session. client_id stands in for id_token_hint.
func (s *Service) Logout(ctx context.Context, cookie string) (string, error) {
	if cookie != "" {
		if err := s.sessions.Delete(ctx, cookie); err != nil {
			return "", err
		}
	}
	query := url.Values{
		"client_id":                {s.cfg.ClientID},
		"post_logout_redirect_uri": {s.cfg.PublicURL + "/login"},
	}
	return s.cfg.Issuer + "/api/logout?" + query.Encode(), nil
}

// Teams lists every group of the organization.
func (s *Service) Teams(ctx context.Context) ([]Team, error) {
	return s.casdoor.ListGroups(ctx)
}

// TeamMembers lists the direct members of one team.
func (s *Service) TeamMembers(ctx context.Context, team string) ([]Member, error) {
	users, err := s.casdoor.ListGroupUsers(ctx, team)
	if err != nil {
		return nil, err
	}
	members := make([]Member, 0, len(users))
	for _, user := range users {
		members = append(members, Member{ID: user.ID, Name: user.Name, DisplayName: user.DisplayName})
	}
	return members, nil
}

// SecureCookies reports whether the session cookie needs the Secure flag.
func (s *Service) SecureCookies() bool {
	return strings.HasPrefix(s.cfg.PublicURL, "https://")
}

// loadPrincipal asks Casdoor who userID is now and what they may do.
func (s *Service) loadPrincipal(ctx context.Context, userID string) (Principal, error) {
	user, err := s.casdoor.GetUser(ctx, userID)
	if err != nil {
		return Principal{}, err
	}
	if user == nil {
		return Principal{}, fmt.Errorf("%w: user %q is not in organization %q", errNoAccess, userID, s.cfg.Organization)
	}
	if user.IsForbidden {
		return Principal{}, fmt.Errorf("%w: user %q is forbidden", errNoAccess, userID)
	}
	requests := enforceRequests(*user, userTeams(*user, s.cfg.Organization))
	decisions, err := s.casdoor.BatchEnforce(ctx, requests)
	if err != nil {
		return Principal{}, fmt.Errorf("judge permissions: %w", err)
	}
	principal := compilePrincipal(*user, s.cfg.Organization, decisions)
	if !principal.HasAccess() {
		return Principal{}, fmt.Errorf("%w: user %q has no team and no platform permission", errNoAccess, userID)
	}
	return principal, nil
}
