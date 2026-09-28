package cluster

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

const (
	catalogDocumentField  = "doc"
	catalogFetchedAtField = "fetched_at"
)

// ErrCatalogMissing reports that no instance has stored a catalog yet.
var ErrCatalogMissing = errors.New("shared catalog is missing")

// CatalogStore keeps the one Models.dev last-known-good document every
// instance serves, so a single instance fetches it and the rest adopt it. The
// document is an opaque encoded catalog; its fetch time lets instances detect
// a newer one without reading it. It is nil in single-instance mode.
type CatalogStore struct {
	client *Client
}

// NewCatalogStore returns nil when cluster mode is disabled.
func NewCatalogStore(client *Client) *CatalogStore {
	if client == nil {
		return nil
	}
	return &CatalogStore{client: client}
}

// Store replaces the shared document. Concurrent stores keep the last write.
func (store *CatalogStore) Store(ctx context.Context, document []byte, fetchedAtMS int64) error {
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if err := store.client.HSet(callCtx, store.key(),
		catalogDocumentField, document,
		catalogFetchedAtField, strconv.FormatInt(fetchedAtMS, 10),
	).Err(); err != nil {
		return fmt.Errorf("store shared catalog: %w", err)
	}
	return nil
}

// FetchedAt returns the successful fetch time of the shared document, or
// ErrCatalogMissing when none is stored.
func (store *CatalogStore) FetchedAt(ctx context.Context) (int64, error) {
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	fetchedAt, err := store.client.HGet(callCtx, store.key(), catalogFetchedAtField).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, ErrCatalogMissing
	}
	if err != nil {
		return 0, fmt.Errorf("read shared catalog fetch time: %w", err)
	}
	return fetchedAt, nil
}

// Load returns the shared document, or ErrCatalogMissing when none is stored.
func (store *CatalogStore) Load(ctx context.Context) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	document, err := store.client.HGet(callCtx, store.key(), catalogDocumentField).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrCatalogMissing
	}
	if err != nil {
		return nil, fmt.Errorf("read shared catalog: %w", err)
	}
	return document, nil
}

func (store *CatalogStore) key() string {
	return store.client.Key("catalog")
}
