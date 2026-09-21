// Package coordination owns the process-wide Redis connection used by
// distributed instance mode. Redis-backed implementations of coordinated
// behavior belong in this package so the connection has a single owner.
package coordination

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

// KeyPrefix namespaces every key this gateway writes to a shared Redis.
const KeyPrefix = "gw:"

const (
	modeStandalone = "standalone"
	modeSentinel   = "sentinel"
)

// Key builds a namespaced Redis key from its colon-separated parts.
func Key(parts ...string) string {
	return KeyPrefix + strings.Join(parts, ":")
}

// hashTag wraps the part of a key that decides its slot. Only scripts that
// touch several keys at once need it, and they need every one of those keys to
// carry the same tag; a single-key operation uses it purely so the two key
// families stay readable side by side.
func hashTag(value string) string {
	return "{" + value + "}"
}

// NewInstanceIdentity returns a random identity for this process. It exists so
// coordinated state can tell instances apart — which one holds a period, which
// one wrote a window entry — and never to authorize anything.
//
// A failing random source is returned rather than swallowed: an instance that
// cannot identify itself would write entries indistinguishable from a peer's.
func NewInstanceIdentity() (string, error) {
	return newIdentity(rand.Reader)
}

func newIdentity(random io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", fmt.Errorf("generate instance identity: %w", err)
	}
	return hex.EncodeToString(value), nil
}

// Client is a connected Redis client whose reachability was verified when it
// was opened.
type Client struct {
	rdb        *redis.Client
	mode       string
	masterName string
}

// topology is the parsed REDIS_DSN: which client kind to build and how to
// describe it in logs.
type topology struct {
	mode       string
	masterName string
	standalone *redis.Options
	failover   *redis.FailoverOptions
}

func (t topology) open() *redis.Client {
	if t.failover != nil {
		return redis.NewFailoverClient(t.failover)
	}
	return redis.NewClient(t.standalone)
}

// parseDSN dispatches between a single node and a Sentinel topology. A
// master_name query parameter is what distinguishes them, matching go-redis's
// own failover URL convention.
func parseDSN(dsn string) (topology, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		// url.Error embeds the whole DSN, which may carry a password.
		return topology{}, fmt.Errorf("REDIS_DSN is invalid")
	}
	if parsed.Query().Get("master_name") != "" {
		options, err := redis.ParseFailoverURL(dsn)
		if err != nil {
			return topology{}, fmt.Errorf("parse REDIS_DSN: %w", err)
		}
		return topology{mode: modeSentinel, masterName: options.MasterName, failover: options}, nil
	}
	options, err := redis.ParseURL(dsn)
	if err != nil {
		return topology{}, fmt.Errorf("parse REDIS_DSN: %w", err)
	}
	return topology{mode: modeStandalone, standalone: options}, nil
}

// Open connects to Redis and verifies reachability with a PING. A failure here
// is fatal to startup: an instance that cannot coordinate has nothing to serve.
func Open(ctx context.Context, dsn string) (*Client, error) {
	parsed, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}

	rdb := parsed.open()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("connect redis: %w", err)
	}

	fields := logrus.Fields{"event": "startup.redis_open", "redis_mode": parsed.mode}
	if parsed.masterName != "" {
		fields["master_name"] = parsed.masterName
	}
	logrus.WithFields(fields).Info("redis opened")

	return &Client{rdb: rdb, mode: parsed.mode, masterName: parsed.masterName}, nil
}

// Ping reports whether Redis is currently reachable.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close releases the connection pool.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// Redis exposes the underlying client for Pub/Sub and scripted commands.
func (c *Client) Redis() *redis.Client {
	return c.rdb
}
