package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"

	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/platform/canonicaljson"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/state"
	stateloader "gpt-load/internal/state/loader"
)

// SaveProxyIdempotent stores the redacted response in the existing operation
// ledger. Replays never read the current entry or reveal its encrypted URL.
func (s *Service) SaveProxyIdempotent(ctx context.Context, key string, id uint, request ProxySaveRequest) (ProxyItem, error) {
	request.Name = strings.TrimSpace(request.Name)
	if utf8.RuneCountInString(request.Name) > 255 {
		return ProxyItem{}, app_errors.ErrValidation
	}
	request.URL = strings.TrimSpace(request.URL)
	if id == 0 || request.URL != "" {
		config, err := outboundproxy.CanonicalConnection(outboundproxy.Config{Mode: outboundproxy.ModeCustom, URL: request.URL})
		if err != nil {
			return ProxyItem{}, app_errors.ErrValidation
		}
		request.URL = config.URL
	}
	kind, method, path, locator := operationKindProxyCreate, "POST", "/api/proxies", "new"
	if id != 0 {
		kind, method, path, locator = operationKindProxyUpdate, "PUT", "/api/proxies/:id", fmt.Sprintf("proxy:%d", id)
	}
	// Only the keyed hash reaches the digest framing. Neither the comparator
	// nor the durable result contains proxy authentication material.
	body, err := canonicalIdempotencyBody(struct {
		Name    string `json:"name"`
		URLHash string `json:"url_hash"`
	}{request.Name, s.encryption.Hash(strings.TrimSpace(request.URL))})
	if err != nil {
		return ProxyItem{}, app_errors.ErrInternalServer
	}
	digest, err := buildIdempotencyDigest(idempotencyDigestInput{
		Version: 1, Method: method, OperationKind: kind, PathTemplate: path,
		ResourceLocator: locator, AuthScopeID: idempotencyAuthScopeID, CanonicalBody: body,
	})
	if err != nil {
		return ProxyItem{}, app_errors.ErrInternalServer
	}
	operation, err := s.executeIdempotentOperation(ctx, idempotentOperationInput{
		IdempotencyKey: key, DigestVersion: 1, RequestDigest: digest.Digest, Kind: kind,
		Mutate: func(tx *gorm.DB) (idempotentMutationResult, error) {
			if err := lockProxyCatalog(tx); err != nil {
				return idempotentMutationResult{}, err
			}
			row, err := s.saveProxy(tx, id, request)
			if err != nil {
				return idempotentMutationResult{}, err
			}
			catalog, err := stateloader.LoadProxyCatalog(ctx, tx, s.encryption)
			if err != nil {
				return idempotentMutationResult{}, app_errors.ErrInternalServer
			}
			refs, err := s.proxyReferences(ctx, tx)
			if err != nil {
				return idempotentMutationResult{}, err
			}
			item, err := proxyItem(catalog[row.ID], refs[row.ID])
			if err != nil {
				return idempotentMutationResult{}, err
			}
			if err := s.validateProxyMutation(ctx, tx); err != nil {
				return idempotentMutationResult{}, err
			}
			result, err := canonicaljson.Marshal(item)
			if err != nil {
				return idempotentMutationResult{}, app_errors.ErrInternalServer
			}
			return idempotentMutationResult{ResourceIdentity: fmt.Sprintf("proxy:%d", row.ID), CanonicalResult: result}, nil
		},
	})
	if err != nil {
		return ProxyItem{}, err
	}
	var result ProxyItem
	if err := json.Unmarshal(operation.CanonicalResult, &result); err != nil {
		return ProxyItem{}, app_errors.ErrInternalServer
	}
	return result, nil
}

func (s *Service) ImportProxiesIdempotent(ctx context.Context, key, text string) (ProxyImportResult, error) {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 1000 {
		return ProxyImportResult{}, app_errors.ErrValidation
	}
	body, err := canonicalIdempotencyBody(struct {
		EntriesHash string `json:"entries_hash"`
	}{s.encryption.Hash(text)})
	if err != nil {
		return ProxyImportResult{}, app_errors.ErrInternalServer
	}
	digest, err := buildIdempotencyDigest(idempotencyDigestInput{
		Version: 1, Method: "POST", OperationKind: operationKindProxyImport,
		PathTemplate: "/api/proxies/import", ResourceLocator: "batch",
		AuthScopeID: idempotencyAuthScopeID, CanonicalBody: body,
	})
	if err != nil {
		return ProxyImportResult{}, app_errors.ErrInternalServer
	}
	operation, err := s.executeIdempotentOperation(ctx, idempotentOperationInput{
		IdempotencyKey: key, DigestVersion: 1, RequestDigest: digest.Digest, Kind: operationKindProxyImport,
		Mutate: func(tx *gorm.DB) (idempotentMutationResult, error) {
			if err := lockProxyCatalog(tx); err != nil {
				return idempotentMutationResult{}, err
			}
			result, err := s.importProxies(ctx, tx, lines)
			if err != nil {
				return idempotentMutationResult{}, err
			}
			if err := s.validateProxyMutation(ctx, tx); err != nil {
				return idempotentMutationResult{}, err
			}
			encoded, err := canonicaljson.Marshal(result)
			if err != nil {
				return idempotentMutationResult{}, app_errors.ErrInternalServer
			}
			return idempotentMutationResult{ResourceIdentity: "proxy:catalog", CanonicalResult: encoded}, nil
		},
	})
	if err != nil {
		return ProxyImportResult{}, err
	}
	var result ProxyImportResult
	if err := json.Unmarshal(operation.CanonicalResult, &result); err != nil {
		return ProxyImportResult{}, app_errors.ErrInternalServer
	}
	return result, nil
}

func (s *Service) validateProxyMutation(ctx context.Context, tx *gorm.DB) error {
	input, err := stateloader.BuildCompileInputWithProxy(ctx, tx, s.encryption, s.environmentProxy, s.channelRegistry)
	if err != nil {
		return err
	}
	_, err = state.Compile(input)
	return err
}
