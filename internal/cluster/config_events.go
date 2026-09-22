package cluster

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

const configEventsChannel = "events"

// ConfigChange announces that a control-plane transaction committed the given
// configuration revision on the named instance.
type ConfigChange struct {
	Revision uint64 `json:"revision"`
	Origin   string `json:"origin"`
}

// ConfigEventBus publishes and receives ConfigChange events over Redis
// Pub/Sub. It is nil in single-instance mode.
type ConfigEventBus struct {
	client *Client
}

// NewConfigEventBus wires the event bus onto the shared client.
func NewConfigEventBus(client *Client) *ConfigEventBus {
	if client == nil {
		return nil
	}
	return &ConfigEventBus{client: client}
}

// InstanceID returns the origin used for events published by this process.
func (bus *ConfigEventBus) InstanceID() string {
	if bus == nil {
		return ""
	}
	return bus.client.InstanceID()
}

func (bus *ConfigEventBus) channel() string {
	return bus.client.Key(configEventsChannel)
}

// Publish broadcasts change to every subscriber. Delivery is best effort: the
// PostgreSQL revision remains the source of truth and pollers catch up.
func (bus *ConfigEventBus) Publish(ctx context.Context, change ConfigChange) error {
	if bus == nil {
		return nil
	}
	payload, err := json.Marshal(change)
	if err != nil {
		return fmt.Errorf("encode config change: %w", err)
	}
	if err := bus.client.Publish(ctx, bus.channel(), payload).Err(); err != nil {
		return fmt.Errorf("publish config change: %w", err)
	}
	return nil
}

// Subscribe blocks until ctx is canceled, invoking handle for every decoded
// ConfigChange. subscribed, when non-nil, runs once after the server confirms
// the subscription so callers can order their catch-up work after it.
// Malformed payloads are logged and skipped. Transport reconnects are handled
// by the Redis client; a terminal subscription failure is returned.
func (bus *ConfigEventBus) Subscribe(
	ctx context.Context,
	subscribed func(),
	handle func(ConfigChange),
) error {
	if bus == nil {
		<-ctx.Done()
		return nil
	}
	pubsub := bus.client.Subscribe(ctx, bus.channel())
	defer func() { _ = pubsub.Close() }()
	if _, err := pubsub.Receive(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("subscribe config changes: %w", err)
	}
	if subscribed != nil {
		subscribed()
	}
	messages := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case message, ok := <-messages:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("subscribe config changes: %w", redis.ErrClosed)
			}
			change, err := decodeConfigChange(message.Payload)
			if err != nil {
				logrus.WithError(err).WithField("event", "cluster.config_event_invalid").
					Warn("ignoring malformed cluster config event")
				continue
			}
			handle(change)
		}
	}
}

func decodeConfigChange(payload string) (ConfigChange, error) {
	var change ConfigChange
	if err := json.Unmarshal([]byte(payload), &change); err != nil {
		return ConfigChange{}, fmt.Errorf("decode config change: %w", err)
	}
	if change.Revision == 0 {
		return ConfigChange{}, fmt.Errorf("decode config change: revision is required")
	}
	return change, nil
}
