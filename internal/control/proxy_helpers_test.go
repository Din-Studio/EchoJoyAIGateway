package control

import (
	"context"
	"crypto/rand"
)

// Fixtures use the production idempotent path with a fresh operation key.
func (s *Service) SaveProxy(ctx context.Context, id uint, request ProxySaveRequest) (ProxyItem, error) {
	key, err := newOperationID(rand.Reader)
	if err != nil {
		return ProxyItem{}, err
	}
	return s.SaveProxyIdempotent(ctx, key, id, request)
}

func (s *Service) ImportProxies(ctx context.Context, entries string) (ProxyImportResult, error) {
	key, err := newOperationID(rand.Reader)
	if err != nil {
		return ProxyImportResult{}, err
	}
	return s.ImportProxiesIdempotent(ctx, key, entries)
}
