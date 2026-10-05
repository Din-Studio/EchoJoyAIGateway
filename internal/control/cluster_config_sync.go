package control

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/cluster"
)

const (
	defaultClusterPollInterval    = 30 * time.Second
	clusterSubscribeRetryInterval = time.Second
)

// ClusterConfigSync keeps this instance's runtime configuration aligned with
// control-plane commits made by any cluster instance, itself included. Redis
// events are the fast path; polling the PostgreSQL revision is the correctness
// path, so a lost event only costs latency.
type ClusterConfigSync struct {
	reload       func(context.Context) (uint64, error)
	readRevision func(context.Context) (uint64, error)
	// sweep marks refreshes interrupted by a crashed holder and realigns
	// shared auth state with the database on every poll.
	sweep        func(context.Context) error
	bus          *cluster.ConfigEventBus
	pollInterval time.Duration
	wake         chan struct{}
	lastApplied  atomic.Uint64
}

// NewClusterConfigSync wires the sync loop onto the control service.
func NewClusterConfigSync(service *Service, bus *cluster.ConfigEventBus) *ClusterConfigSync {
	return &ClusterConfigSync{
		reload: func(ctx context.Context) (uint64, error) {
			// A peer stores a new catalog before committing its prices, so
			// adopting first keeps the catalog no older than the prices.
			if err := service.catalogSync.adoptSharedCatalog(ctx); err != nil {
				logrus.WithError(err).WithField("event", "models_dev_catalog_adopt_failed").
					Warn("shared Models.dev catalog adoption failed; will retry on the next reload")
			}
			return service.reloadCommittedConfig(ctx)
		},
		readRevision: func(ctx context.Context) (uint64, error) {
			return readClusterConfigRevision(ctx, service.db)
		},
		sweep:        service.syncSubscriptionAuth,
		bus:          bus,
		pollInterval: defaultClusterPollInterval,
		wake:         make(chan struct{}, 1),
	}
}

// Run blocks until ctx is canceled. It reloads once at startup to cover
// commits made between this instance's initial load and the subscription,
// then reloads on every peer event or when polling observes a newer revision.
func (coordinator *ClusterConfigSync) Run(ctx context.Context) {
	coordinator.reloadNow(ctx)

	var subscriber sync.WaitGroup
	subscriber.Add(1)
	go func() {
		defer subscriber.Done()
		coordinator.runSubscriber(ctx)
	}()
	defer subscriber.Wait()

	interval := coordinator.pollInterval
	if interval <= 0 {
		interval = defaultClusterPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-coordinator.wake:
			coordinator.reloadNow(ctx)
		case <-ticker.C:
			coordinator.pollOnce(ctx)
		}
	}
}

func (coordinator *ClusterConfigSync) runSubscriber(ctx context.Context) {
	for {
		err := coordinator.bus.Subscribe(ctx, func() { coordinator.pollOnceAsync(ctx) }, coordinator.handle)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logrus.WithError(err).WithField("event", "control.cluster_subscribe_failed").
				Warn("cluster config subscription failed; retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(clusterSubscribeRetryInterval):
		}
	}
}

// handle runs on the subscriber goroutine. Every newer revision, including one
// this instance committed, requests a reload: the applied revision only
// advances after a reload has read it, so a peer commit that lost its own
// event is still picked up. The writer's own reload finds nothing to change.
func (coordinator *ClusterConfigSync) handle(change cluster.ConfigChange) {
	if change.Revision > coordinator.lastApplied.Load() {
		coordinator.requestReload()
	}
}

func (coordinator *ClusterConfigSync) requestReload() {
	select {
	case coordinator.wake <- struct{}{}:
	default:
	}
}

// pollOnceAsync compares the persisted revision from the subscriber goroutine
// and hands the reload to the main loop, so the subscription is never blocked
// by a reload.
func (coordinator *ClusterConfigSync) pollOnceAsync(ctx context.Context) {
	revision, err := coordinator.readRevision(ctx)
	if err != nil {
		logrus.WithError(err).WithField("event", "control.cluster_revision_read_failed").
			Warn("cluster config revision read failed")
		return
	}
	if revision > coordinator.lastApplied.Load() {
		coordinator.requestReload()
	}
}

func (coordinator *ClusterConfigSync) pollOnce(ctx context.Context) {
	defer coordinator.sweepNow(ctx)
	revision, err := coordinator.readRevision(ctx)
	if err != nil {
		logrus.WithError(err).WithField("event", "control.cluster_revision_read_failed").
			Warn("cluster config revision read failed")
		return
	}
	if revision > coordinator.lastApplied.Load() {
		coordinator.reloadNow(ctx)
	}
}

func (coordinator *ClusterConfigSync) sweepNow(ctx context.Context) {
	if coordinator.sweep == nil || ctx.Err() != nil {
		return
	}
	if err := coordinator.sweep(ctx); err != nil {
		logrus.WithError(err).WithField("event", "subscription.refresh_sweep_failed").
			Warn("interrupted refresh sweep failed; retrying on the next poll")
	}
}

func (coordinator *ClusterConfigSync) reloadNow(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	started := time.Now()
	revision, err := coordinator.reload(ctx)
	if err != nil {
		logrus.WithError(err).WithField("event", "control.cluster_config_reload_failed").
			Warn("cluster config reload failed; will retry on the next event or poll")
		return
	}
	coordinator.advanceApplied(revision)
	logrus.WithFields(logrus.Fields{
		"event":       "control.cluster_config_reloaded",
		"revision":    revision,
		"duration_ms": time.Since(started).Milliseconds(),
	}).Info("cluster config reloaded")
}

func (coordinator *ClusterConfigSync) advanceApplied(revision uint64) {
	for {
		current := coordinator.lastApplied.Load()
		if revision <= current || coordinator.lastApplied.CompareAndSwap(current, revision) {
			return
		}
	}
}
