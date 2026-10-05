// Package cluster owns the Redis-backed primitives every gateway instance
// shares. Redis is a hard dependency: a single instance is a one-replica
// cluster.
package cluster

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"gpt-load/internal/platform/config"
)

const (
	dialTimeout    = 5 * time.Second
	commandTimeout = 2 * time.Second
	startupPing    = 5 * time.Second
	maxRetries     = 3

	// stateTimeout bounds each data-plane round trip for shared request state,
	// so an unavailable Redis fails the request fast instead of stalling it.
	stateTimeout = 200 * time.Millisecond
)

// Client wraps the Redis connection shared by every cluster primitive.
type Client struct {
	redis.UniversalClient
	keyPrefix  string
	instanceID string
}

// NewClient connects to Redis. Redis is a hard dependency, so an unreachable
// server fails startup.
func NewClient(cfg *config.Config) (*Client, error) {
	options := &redis.UniversalOptions{
		Addrs:        cfg.Cluster.RedisAddrs,
		Password:     cfg.Cluster.RedisPassword,
		DialTimeout:  dialTimeout,
		ReadTimeout:  commandTimeout,
		WriteTimeout: commandTimeout,
		MaxRetries:   maxRetries,
		// Honor per-call context deadlines such as stateTimeout.
		ContextTimeoutEnabled: true,
	}
	if cfg.Cluster.RedisTLS {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := &Client{
		UniversalClient: redis.NewUniversalClient(options),
		keyPrefix:       cfg.Cluster.RedisKeyPrefix,
		instanceID:      cfg.Cluster.InstanceID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), startupPing)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect to Redis %s: %w", strings.Join(cfg.Cluster.RedisAddrs, ","), err)
	}
	logrus.WithFields(logrus.Fields{
		"event":           "startup.cluster_enabled",
		"cluster.enabled": true,
		"instance_id":     cfg.Cluster.InstanceID,
		"redis_nodes":     len(cfg.Cluster.RedisAddrs),
		"key_prefix":      cfg.Cluster.RedisKeyPrefix,
	}).Info("cluster mode enabled")
	return client, nil
}

// Ping verifies the Redis connection.
func (c *Client) Ping(ctx context.Context) error {
	return c.UniversalClient.Ping(ctx).Err()
}

// Close releases the Redis connection pool.
func (c *Client) Close() error {
	return c.UniversalClient.Close()
}

// Key joins parts under the configured key prefix.
func (c *Client) Key(parts ...string) string {
	return c.keyPrefix + ":" + strings.Join(parts, ":")
}

// InstanceID returns the identity this process uses in cluster events.
func (c *Client) InstanceID() string {
	return c.instanceID
}
