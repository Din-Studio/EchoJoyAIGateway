package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gpt-load/internal/platform/utils"
	"gpt-load/internal/state"
)

type keyHasher interface {
	Hash(string) string
}

// AccessTokenVerifier resolves an EchoJoyAuth access token to its user id.
// *echojoyauth.Verifier implements it; nil turns access tokens off.
type AccessTokenVerifier interface {
	UserID(ctx context.Context, token string) (string, error)
}

type accessKeyAuthFailureReason string

const (
	accessKeyAuthFailureInvalid        accessKeyAuthFailureReason = "invalid_access_key"
	accessKeyAuthFailureExpired        accessKeyAuthFailureReason = "access_key_expired"
	accessKeyAuthFailurePeerNotAllowed accessKeyAuthFailureReason = "peer_not_allowed"
	accessKeyAuthFailureInvalidToken   accessKeyAuthFailureReason = "invalid_access_token"
	accessKeyAuthFailureNoDefaultKey   accessKeyAuthFailureReason = "no_default_access_key"
)

// isAccessToken reports whether a client credential is a JWT rather than an
// access key: three dot-separated segments, the first a base64url JSON object.
func isAccessToken(credential string) bool {
	return strings.HasPrefix(credential, "eyJ") && strings.Count(credential, ".") == 2
}

// authenticateAccessToken serves an access token as its user's default access
// key, which then limits and prices the request like any other key. It also
// returns the token's user id, which external billing uses in place of the
// key plaintext.
func (handler *Handler) authenticateAccessToken(
	request *http.Request,
	token string,
	snapshot *state.ConfigSnapshot,
	requestStarted time.Time,
) (state.AccessKeyView, string, bool, accessKeyAuthFailureReason) {
	if request == nil || snapshot == nil {
		return state.AccessKeyView{}, "", false, accessKeyAuthFailureInvalid
	}
	userID, err := handler.accessTokens.UserID(request.Context(), token)
	if err != nil {
		return state.AccessKeyView{}, "", false, accessKeyAuthFailureInvalidToken
	}
	accessKeyID, ok := snapshot.UserDefaultKeys[userID]
	if !ok {
		return state.AccessKeyView{}, "", false, accessKeyAuthFailureNoDefaultKey
	}
	accessKey, authenticated, reason := checkAccessKey(request, snapshot.AccessKeysByID[accessKeyID], requestStarted)
	return accessKey, userID, authenticated, reason
}

// authenticate checks plaintext, the key extractClientKey took from request.
func authenticate(
	request *http.Request,
	plaintext string,
	snapshot *state.ConfigSnapshot,
	hasher keyHasher,
	requestStarted time.Time,
) (state.AccessKeyView, bool, accessKeyAuthFailureReason) {
	if request == nil || snapshot == nil || hasher == nil {
		return state.AccessKeyView{}, false, accessKeyAuthFailureInvalid
	}
	if plaintext == "" {
		return state.AccessKeyView{}, false, accessKeyAuthFailureInvalid
	}
	accessKey, ok := snapshot.AccessKeysByHash[hasher.Hash(plaintext)]
	if !ok {
		return state.AccessKeyView{}, false, accessKeyAuthFailureInvalid
	}
	return checkAccessKey(request, accessKey, requestStarted)
}

// checkAccessKey applies an active key's expiry and peer restrictions.
func checkAccessKey(
	request *http.Request,
	accessKey state.AccessKeyView,
	requestStarted time.Time,
) (state.AccessKeyView, bool, accessKeyAuthFailureReason) {
	if accessKey.ExpiresAtMS != nil && requestStarted.UnixMilli() >= *accessKey.ExpiresAtMS {
		return accessKey, false, accessKeyAuthFailureExpired
	}
	if len(accessKey.AllowedPeerCIDRs) > 0 {
		peer, err := utils.NormalizePeerIP(request.RemoteAddr)
		if err != nil || !utils.AllowedCIDRsContain(accessKey.AllowedPeerCIDRs, peer) {
			return accessKey, false, accessKeyAuthFailurePeerNotAllowed
		}
	}
	return accessKey, true, ""
}

func extractClientKey(request *http.Request) string {
	if request == nil {
		return ""
	}

	queryKey := ""
	if request.URL != nil {
		queryKeySet := false
		segments := strings.Split(request.URL.RawQuery, "&")
		forwarded := make([]string, 0, len(segments))
		for _, segment := range segments {
			rawName, rawValue, _ := strings.Cut(segment, "=")
			name, err := url.QueryUnescape(rawName)
			if err != nil || name != "key" {
				forwarded = append(forwarded, segment)
				continue
			}
			if !queryKeySet {
				value, err := url.QueryUnescape(rawValue)
				if err == nil {
					queryKey = strings.TrimSpace(value)
					queryKeySet = true
				}
			}
		}
		request.URL.RawQuery = strings.Join(forwarded, "&")
	}

	fields := strings.Fields(request.Header.Get("Authorization"))
	if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && fields[1] != "" {
		return fields[1]
	}
	for _, name := range []string{"x-api-key", "x-goog-api-key"} {
		if value := strings.TrimSpace(request.Header.Get(name)); value != "" {
			return value
		}
	}
	return queryKey
}
