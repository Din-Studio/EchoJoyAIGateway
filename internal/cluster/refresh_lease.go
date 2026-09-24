package cluster

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

const (
	// refreshLeaseTTL only proves the holder is alive; renewal keeps a lease
	// for as long as the refresh takes.
	refreshLeaseTTL           = 30 * time.Second
	refreshLeaseRenewInterval = 10 * time.Second
	refreshLeaseReleaseWait   = 2 * time.Second
)

// RefreshLease grants one instance at a time the right to refresh a
// subscription credential. Liveness is judged by the Redis server clock
// alone, so no two instance clocks are compared. It is nil in
// single-instance mode.
type RefreshLease struct {
	client        *Client
	renewInterval time.Duration
}

// NewRefreshLease returns nil when cluster mode is disabled.
func NewRefreshLease(client *Client) *RefreshLease {
	if client == nil {
		return nil
	}
	return &RefreshLease{client: client, renewInterval: refreshLeaseRenewInterval}
}

// Acquire takes the credential's lease without waiting. When acquired, the
// lease is renewed in the background until release is called; losing it is
// logged but never interrupts the refresh, whose commit stays safe because it
// is conditioned on the secret version alone.
func (lease *RefreshLease) Acquire(ctx context.Context, credentialID uint) (func(), bool, error) {
	key := lease.key(credentialID)
	token := lease.client.InstanceID() + ":" + randomToken()
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	acquired, err := lease.client.SetNX(callCtx, key, token, refreshLeaseTTL).Result()
	cancel()
	if err != nil {
		return nil, false, fmt.Errorf("acquire refresh lease for credential %d: %w", credentialID, err)
	}
	if !acquired {
		return nil, false, nil
	}
	stop := make(chan struct{})
	var renewer sync.WaitGroup
	renewer.Add(1)
	go func() {
		defer renewer.Done()
		lease.renew(stop, credentialID, key, token)
	}()
	var once sync.Once
	release := func() {
		once.Do(func() {
			close(stop)
			renewer.Wait()
			releaseCtx, cancel := context.WithTimeout(context.Background(), refreshLeaseReleaseWait)
			defer cancel()
			if err := releaseLeaseScript.Run(releaseCtx, lease.client, []string{key}, token).Err(); err != nil {
				logrus.WithError(err).WithFields(logrus.Fields{
					"event": "subscription.refresh_lease_release_failed", "credential_id": credentialID,
				}).Warn("refresh lease release failed; it expires on its own")
			}
		})
	}
	return release, true, nil
}

func (lease *RefreshLease) renew(stop <-chan struct{}, credentialID uint, key, token string) {
	ticker := time.NewTicker(lease.renewInterval)
	defer ticker.Stop()
	ttl := strconv.FormatInt(refreshLeaseTTL.Milliseconds(), 10)
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		renewed, err := renewLeaseScript.Run(ctx, lease.client, []string{key}, token, ttl).Int()
		cancel()
		if err == nil && renewed == 1 {
			continue
		}
		logrus.WithError(err).WithFields(logrus.Fields{
			"event": "subscription.refresh_lease_lost", "credential_id": credentialID,
		}).Warn("refresh lease could not be renewed; the refresh continues")
		if err == nil {
			// Another holder owns the lease now; renewing further is pointless.
			return
		}
	}
}

// Held reports which credentials currently have a live refresh lease.
func (lease *RefreshLease) Held(ctx context.Context, credentialIDs []uint) (map[uint]bool, error) {
	held := make(map[uint]bool, len(credentialIDs))
	if len(credentialIDs) == 0 {
		return held, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	pipeline := lease.client.Pipeline()
	commands := make([]*redis.IntCmd, 0, len(credentialIDs))
	for _, id := range credentialIDs {
		commands = append(commands, pipeline.Exists(callCtx, lease.key(id)))
	}
	if _, err := pipeline.Exec(callCtx); err != nil {
		return nil, fmt.Errorf("check refresh leases: %w", err)
	}
	for index, id := range credentialIDs {
		held[id] = commands[index].Val() == 1
	}
	return held, nil
}

func (lease *RefreshLease) key(credentialID uint) string {
	return lease.client.Key(credentialHashTag(credentialID), "refresh-lease")
}
