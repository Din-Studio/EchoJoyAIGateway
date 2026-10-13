package identity

import (
	"errors"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/httproute"
	"gpt-load/internal/platform/i18n"
	"gpt-load/internal/platform/response"
)

const (
	loginErrorNoAccess = "no_access"
	loginErrorFailed   = "login_failed"
	// loginCookie ties a pending login to the browser that started it.
	loginCookie = "gl_login"
)

type logoutResponse struct {
	RedirectURL string `json:"redirect_url"`
}

// HTTPModule declares the browser login flow under /auth.
func (s *Service) HTTPModule() httproute.Module {
	return httproute.Module{
		Name:              "auth",
		Owner:             httproute.OwnerSystem,
		Auth:              httproute.AuthNone,
		Prefix:            "/auth",
		NamespacePrefixes: []string{"/auth"},
		BeforeAuth:        gin.HandlersChain{i18n.Middleware()},
		Routes: []httproute.Route{
			{Name: "auth.login", Methods: []string{http.MethodGet}, Path: "/login", Handlers: gin.HandlersChain{s.handleLogin}},
			{Name: "auth.callback", Methods: []string{http.MethodGet}, Path: "/callback", Handlers: gin.HandlersChain{s.handleCallback}},
			{Name: "auth.logout", Methods: []string{http.MethodPost}, Path: "/logout", Handlers: gin.HandlersChain{s.handleLogout}},
		},
	}
}

func (s *Service) handleLogin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	state, stateErr := randomToken()
	nonce, nonceErr := randomToken()
	browser, browserErr := randomToken()
	if err := errors.Join(stateErr, nonceErr, browserErr); err != nil {
		s.failLogin(c, "login", err)
		return
	}
	verifier := oauth2.GenerateVerifier()
	login := LoginState{
		Verifier: verifier, Nonce: nonce, Redirect: safeRedirect(c.Query("redirect")),
		Browser: tokenDigest(browser),
	}
	if err := s.sessions.PutLoginState(c.Request.Context(), state, login); err != nil {
		s.failLogin(c, "login", err)
		return
	}
	s.setLoginCookie(c, browser, int(loginStateTTL.Seconds()))
	c.Redirect(http.StatusFound, s.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)))
}

func (s *Service) handleCallback(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	s.setLoginCookie(c, "", -1)
	ctx := c.Request.Context()
	if c.Query("error") != "" {
		s.failLogin(c, "callback", errors.New("authorization denied: "+c.Query("error")))
		return
	}
	login, found, err := s.sessions.TakeLoginState(ctx, c.Query("state"))
	if err == nil && !found {
		err = errors.New("unknown or expired login state")
	}
	if err != nil {
		s.failLogin(c, "callback", err)
		return
	}
	// The state alone is not enough: a callback URL sent to another browser
	// would sign that browser in as the attacker.
	if browser, _ := c.Cookie(loginCookie); tokenDigest(browser) != login.Browser {
		s.failLogin(c, "callback", errors.New("login was started by another browser"))
		return
	}
	token, err := s.oauth.Exchange(
		oidc.ClientContext(ctx, s.http), c.Query("code"), oauth2.VerifierOption(login.Verifier),
	)
	if err != nil {
		s.failLogin(c, "token_exchange", err)
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	idToken, err := s.verifier.Verify(oidc.ClientContext(ctx, s.http), rawIDToken)
	if err == nil && idToken.Nonce != login.Nonce {
		err = errors.New("ID token nonce mismatch")
	}
	if err != nil {
		s.failLogin(c, "id_token", err)
		return
	}
	principal, err := s.loadPrincipal(ctx, idToken.Subject)
	if errors.Is(err, errNoAccess) {
		s.logger.WithField("event", "identity.login_denied").WithError(err).Info("Login denied")
		c.Redirect(http.StatusFound, "/login?error="+loginErrorNoAccess)
		return
	}
	if err != nil {
		s.failLogin(c, "principal", err)
		return
	}
	cookie, err := s.sessions.Create(ctx, Session{Principal: principal, CheckedAtMS: s.now().UnixMilli()})
	if err != nil {
		s.failLogin(c, "session", err)
		return
	}
	s.setSessionCookie(c, cookie, int(sessionTTL.Seconds()))
	s.logger.WithFields(logrus.Fields{
		"event":    "identity.login",
		"user_id":  principal.UserID,
		"name":     principal.Name,
		"platform": principal.Platform,
	}).Info("User signed in")
	c.Redirect(http.StatusFound, login.Redirect)
}

func (s *Service) handleLogout(c *gin.Context) {
	if c.GetHeader("Origin") != s.cfg.PublicOrigin() {
		response.ErrorI18nFromAPIError(c, app_errors.ErrForbidden, "auth.forbidden")
		return
	}
	cookie, _ := c.Cookie(SessionCookie)
	redirectURL, err := s.Logout(c.Request.Context(), cookie)
	if err != nil {
		s.logger.WithField("event", "identity.logout_failed").WithError(err).Warn("Logout failed")
		response.ErrorI18nFromAPIError(c, app_errors.ErrInternalServer, "internal_error")
		return
	}
	s.setSessionCookie(c, "", -1)
	response.SuccessI18n(c, "common.success", logoutResponse{RedirectURL: redirectURL})
}

func (s *Service) setSessionCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

// setLoginCookie scopes the login cookie to /auth so it never rides on API calls.
func (s *Service) setLoginCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     loginCookie,
		Value:    value,
		Path:     "/auth",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) failLogin(c *gin.Context, stage string, err error) {
	s.logger.WithFields(logrus.Fields{
		"event": "identity.login_failed",
		"stage": stage,
	}).WithError(err).Warn("Login failed")
	c.Redirect(http.StatusFound, "/login?error="+loginErrorFailed)
}

// safeRedirect keeps the post-login redirect on this site.
func safeRedirect(raw string) string {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
		return "/"
	}
	return raw
}
