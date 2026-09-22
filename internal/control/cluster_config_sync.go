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
// control-plane commits made by cluster peers. Redis events are the fast path;
// polling the PostgreSQL revision is the correctness path, so a lost event
// only costs latency. It is nil in single-instance mode.
type ClusterConfigSync struct {
	reload       func(context.Context) (uint64, error)
	readRevision func(context.Context) (uint64, error)
	bus          *cluster.ConfigEventBus
	pollInterval time.Duration
	wake         chan struct{}
	lastApplied  atomic.Uint64
}

// NewClusterConfigSync wires the sync loop onto the control service. A nil bus
// means cluster mode is disabled and nothing is assembled.
func NewClusterConfigSync(service *Service, bus *cluster.ConfigEventBus) *ClusterConfigSync {
	if service == nil || bus == nil {
		return nil
	}
	return &ClusterConfigSync{
		reload: service.reloadCommittedConfig,
		readRevision: func(ctx context.Context) (uint64, error) {
			return readClusterConfigRevision(ctx, service.db)
		},
		bus:          bus,
		pollInterval: defaultClusterPollInterval,
		wake:         make(chan struct{}, 1),
	}
}

// Run blocks until ctx is canceled. It reloads once at startup to cover
// commits made between this instance's initial load and the subscription,
// then reloads on every peer event or when polling observes a newer revision.
func (coordinator *ClusterConfigSync) Run(ctx context.Context) {
	if coordinator == nil {
		return
	}
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
	if coordinator.bus == nil {
		return
	}
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

// handle runs on the subscriber goroutine. Events published by this instance
// only advance the applied revision because the local write path already
// published the change; peer events with a newer revision request a reload.
func (coordinator *ClusterConfigSync) handle(change cluster.ConfigChange) {
	if change.Origin == coordinator.bus.InstanceID() {
		coordinator.advanceApplied(change.Revision)
		return
	}
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
