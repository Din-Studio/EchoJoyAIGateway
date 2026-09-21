package coordination

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
)

// ConfigVersion is the cross-instance signal that committed configuration
// moved. Redis stores no configuration content whatsoever: it holds one
// monotonic counter plus a doorbell channel, and the database remains the only
// source of configuration truth.
//
// A doorbell message is a hint to re-read the counter, never a value to trust.
// Pub/Sub guarantees neither delivery nor ordering, so a duplicated or
// reordered message must not be able to move a receiver's applied version.
//
// Both the counter and the channel are single-key operations. There is no hash
// tag convention here and later phases should not assume one.
type ConfigVersion struct {
	client  *Client
	key     string
	channel string
}

// bumpConfigVersion advances the counter and rings the doorbell in one atomic
// step so the published payload is always the version it announces. The
// payload exists for human troubleshooting; receivers never parse it.
var bumpConfigVersion = redis.NewScript(`
local version = redis.call('INCR', KEYS[1])
redis.call('PUBLISH', KEYS[2], version)
return version
`)

// NewConfigVersion binds the shared configuration version to a Redis client.
func NewConfigVersion(client *Client) *ConfigVersion {
	return &ConfigVersion{
		client:  client,
		key:     Key("config", "version"),
		channel: Key("config"),
	}
}

// Bump advances the shared configuration version and rings the doorbell,
// returning the new version.
func (version *ConfigVersion) Bump(ctx context.Context) (int64, error) {
	next, err := bumpConfigVersion.Run(
		ctx, version.client.Redis(), []string{version.key, version.channel},
	).Int64()
	if err != nil {
		return 0, fmt.Errorf("bump config version: %w", err)
	}
	return next, nil
}

// Current reads the shared configuration version. A missing key means no
// instance has published a change yet, which is version zero rather than a
// failure.
func (version *ConfigVersion) Current(ctx context.Context) (int64, error) {
	current, err := version.client.Redis().Get(ctx, version.key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read config version: %w", err)
	}
	return current, nil
}

// Subscribe turns the doorbell channel into payload-free wakeups. The returned
// release function closes the subscription; the wakeup channel is closed when
// the subscription ends, which callers must treat as "fall back to polling"
// rather than as a reason to stop.
//
// Reconnection is go-redis's job. Messages lost across a reconnect are covered
// by the caller's polling fallback, which is why the doorbell carries no state.
func (version *ConfigVersion) Subscribe(ctx context.Context) (<-chan struct{}, func()) {
	subscription := version.client.Redis().Subscribe(ctx, version.channel)
	messages := subscription.Channel()
	doorbell := make(chan struct{}, 1)
	done := make(chan struct{})

	go func() {
		defer close(doorbell)
		for {
			select {
			case <-done:
				return
			case _, ok := <-messages:
				if !ok {
					return
				}
				// One pending wakeup is enough: the receiver re-reads the
				// version, so coalescing loses nothing.
				select {
				case doorbell <- struct{}{}:
				default:
				}
			}
		}
	}()

	var once sync.Once
	return doorbell, func() {
		once.Do(func() {
			close(done)
			_ = subscription.Close()
		})
	}
}
