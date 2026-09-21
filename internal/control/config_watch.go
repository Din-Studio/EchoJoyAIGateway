package control

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/platform/utils"
)

// configPollInterval bounds how long a lost doorbell message can hide a
// committed change. Pub/Sub makes ordinary propagation sub-second, so this is
// pure fallback and stays a package constant rather than becoming another
// configuration surface nobody tunes.
const configPollInterval = 5 * time.Second

// ConfigVersionSource is the control plane's view of the shared configuration
// version. Redis owns the counter and the doorbell; the configuration itself
// always comes from the database.
type ConfigVersionSource interface {
	Current(context.Context) (int64, error)
	Subscribe(context.Context) (<-chan struct{}, func())
}

// configReloadRuntime is the receiving half of configuration propagation.
type configReloadRuntime interface {
	ReloadCommittedConfiguration(context.Context) error
	RetryPendingBroadcast(context.Context)
}

// runConfigWatch applies configuration committed by other instances. It wakes
// on the doorbell and, as a fallback for messages Pub/Sub never promised to
// deliver, on a timer.
//
// Failures leave this instance serving its current snapshot. Configuration
// propagation is not on the request path, so a degraded watcher affects
// neither the control plane nor the data plane.
func (runtime *Runtime) runConfigWatch(ctx context.Context, ticker runtimeTicker) {
	defer ticker.Stop()
	if ctx.Err() != nil {
		return
	}
	doorbell, release := runtime.configVersion.Subscribe(ctx)
	defer release()

	// Startup already loaded committed configuration, so whatever version is
	// current now is the one this process is running. Treating it as applied
	// avoids a pointless full reload on every boot. If the read fails the
	// version stays unknown and the first wakeup reloads.
	lastApplied, err := runtime.configVersion.Current(ctx)
	known := err == nil
	if !known {
		lastApplied = 0
		logConfigWatchEvent(logrus.WarnLevel, "config.watch_degraded", nil,
			"Initial configuration version read failed")
	}

	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-doorbell:
			if !open {
				// go-redis owns reconnection; a closed doorbell means the
				// subscription itself ended, so fall back to polling instead
				// of rebuilding it here.
				doorbell = nil
				logConfigWatchEvent(logrus.WarnLevel, "config.watch_degraded", nil,
					"Configuration doorbell closed; falling back to polling")
			}
		case <-ticker.C():
		}
		if ctx.Err() != nil {
			return
		}
		lastApplied, known = runtime.syncConfigVersion(ctx, lastApplied, known)
	}
}

// syncConfigVersion performs one convergence attempt and returns the version
// this instance has applied. The version is read before the reload and only
// recorded after it succeeds, so a change committed while the reload was
// running is still seen as outstanding on the next wakeup.
func (runtime *Runtime) syncConfigVersion(
	ctx context.Context,
	lastApplied int64,
	known bool,
) (int64, bool) {
	// A broadcast that failed while Redis was down never advanced the shared
	// version, so no other instance can poll its way to that change. Retrying
	// it here is that change's only route out of this process.
	runtime.configReload.RetryPendingBroadcast(ctx)

	version, err := runtime.configVersion.Current(ctx)
	if err != nil {
		logConfigWatchEvent(logrus.WarnLevel, "config.watch_degraded", err,
			"Configuration version read failed")
		return lastApplied, known
	}
	// Inequality, not "greater than": a flushed or rebuilt Redis restarts the
	// counter, and this instance still has to reconverge on the database.
	if known && version == lastApplied {
		return lastApplied, known
	}

	started := time.Now()
	if err := runtime.configReload.ReloadCommittedConfiguration(ctx); err != nil {
		logConfigWatchEvent(logrus.WarnLevel, "config.reload_failed", err,
			"Committed configuration reload failed")
		return lastApplied, known
	}
	logConfigEvent(
		logrus.InfoLevel,
		logrus.Fields{
			"event":          "config.reload",
			"config_version": version,
			"duration_ms":    time.Since(started).Milliseconds(),
		},
		"Committed configuration reloaded",
	)
	return version, true
}

// logConfigEvent emits a best-effort control-plane event for configuration
// propagation. Propagation is never allowed to fail because logging did.
func logConfigEvent(level logrus.Level, fields logrus.Fields, message string) {
	utils.LogPlaneBestEffort(
		logrus.StandardLogger(), level, utils.LogPlaneControl, fields, message,
	)
}

func logConfigWatchEvent(level logrus.Level, event string, err error, message string) {
	fields := logrus.Fields{"event": event}
	if err != nil {
		fields["error"] = err.Error()
	}
	logConfigEvent(level, fields, message)
}
