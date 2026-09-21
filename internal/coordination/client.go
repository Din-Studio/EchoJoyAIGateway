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
	"net"
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

// defaultSentinelPort is the port a sentinel address without one means. It is
// Sentinel's own convention, and it is not go-redis's URL default of 6379:
// that default belongs to a data node, and silently pointing a sentinel
// address at the master's port produces a connection that answers and then
// fails every sentinel command.
const defaultSentinelPort = "26379"

// parseDSN dispatches between a single node and a Sentinel topology. A
// master_name query parameter is what distinguishes them, matching go-redis's
// own failover URL convention.
func parseDSN(dsn string) (topology, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		// url.Error embeds the whole DSN, which may carry a password.
		return topology{}, fmt.Errorf("REDIS_DSN is invalid")
	}
	if parsed.Query().Get("master_name") == "" {
		options, err := redis.ParseURL(dsn)
		if err != nil {
			return topology{}, fmt.Errorf("parse REDIS_DSN: %w", err)
		}
		return topology{mode: modeStandalone, standalone: options}, nil
	}

	addrs, err := sentinelAddrs(parsed.Host)
	if err != nil {
		return topology{}, err
	}
	// go-redis takes exactly one sentinel from the URL host, so a DSN listing
	// several of them there would be dialled as one absurd hostname. Handing it
	// the first keeps it deriving everything else — scheme, TLS, credentials,
	// database, master name and any `addr` parameters — from the real DSN.
	single := *parsed
	single.Host = addrs[0]
	options, err := redis.ParseFailoverURL(single.String())
	if err != nil {
		return topology{}, fmt.Errorf("parse REDIS_DSN: %w", err)
	}
	// The host go-redis produced is the first entry; the rest came from `addr`
	// parameters and are kept. Both spellings of "more sentinels" therefore
	// work, and an operator using both does not lose half of them.
	options.SentinelAddrs = append(addrs, options.SentinelAddrs[1:]...)
	return topology{mode: modeSentinel, masterName: options.MasterName, failover: options}, nil
}

// sentinelAddrs splits a comma-separated sentinel list into dialable
// addresses. Listing every sentinel is the point of the topology: a client
// that knows only one of them loses the master whenever that one is the
// sentinel that happens to be down.
//
// Splitting on commas is safe for IPv6 literals, which separate their groups
// with colons and are already bracketed inside a URL host.
func sentinelAddrs(host string) ([]string, error) {
	addrs := make([]string, 0, strings.Count(host, ",")+1)
	for _, entry := range strings.Split(host, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(entry); err != nil {
			entry = net.JoinHostPort(entry, defaultSentinelPort)
		}
		addrs = append(addrs, entry)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("REDIS_DSN must list at least one sentinel address")
	}
	return addrs, nil
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
