package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/health"
	"gpt-load/internal/state"
	"gpt-load/internal/storage"
	"gpt-load/internal/testutil/pgtest"

	"github.com/sirupsen/logrus"
)

type runtimeCheckpointFake struct {
	restore func(context.Context) error
	save    func(context.Context) error
}

type checkpointLifecycleLogHook struct {
	entries []*logrus.Entry
}

func (hook *checkpointLifecycleLogHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (hook *checkpointLifecycleLogHook) Fire(entry *logrus.Entry) error {
	if entry.Data["event"] == "startup.checkpoint_restore" {
		hook.entries = append(hook.entries, entry)
	}
	return nil
}

func (fake runtimeCheckpointFake) Restore(ctx context.Context) error {
	if fake.restore == nil {
		return nil
	}
	return fake.restore(ctx)
}

func (fake runtimeCheckpointFake) Save(ctx context.Context) error {
	if fake.save == nil {
		return nil
	}
	return fake.save(ctx)
}

func TestAppRestoresCheckpointAfterRuntimeRecovery(t *testing.T) {
	db, err := storage.Open(pgtest.NewDatabase(t))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	var order []string
	checkpoint := runtimeCheckpointFake{
		restore: func(context.Context) error {
			order = append(order, "checkpoint")
			return nil
		},
	}
	application := NewApp(AppParams{
		ClusterClient: testClusterClient(t),
		Engine:        mustNewEngine(t),
		Config:        testConfig(t),
		DB:            db,
		StartupBootstrap: startupBootstrapFunc(func(context.Context) error {
			order = append(order, "bootstrap")
			return nil
		}),
		RuntimeState: runtimeStateLoaderFunc(func(context.Context) error {
			order = append(order, "runtime")
			return nil
		}),
		StartupRecovery: startupRecoveryFunc(func(context.Context) error {
			order = append(order, "recovery")
			return nil
		}),
		RuntimeCheckpoint: checkpoint,
		ControlRuntime:    newControlRuntimeFake(nil, false),
		RequestLogs:       newRequestLogRuntimeFake(nil, nil),
	})
	cleanupApp(t, application)

	if err := application.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if want := []string{"bootstrap", "runtime", "recovery", "checkpoint"}; !slices.Equal(order, want) {
		t.Fatalf("startup order = %#v, want %#v", order, want)
	}
}

func TestAppLogsCheckpointRestoreFailureOnce(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, runtimeStateCheckpointFileName)
	if err := os.WriteFile(path, []byte(`{"credentials":[`), 0o600); err != nil {
		t.Fatalf("write malformed checkpoint fixture: %v", err)
	}

	db, err := storage.Open(pgtest.NewDatabase(t))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	application := NewApp(AppParams{
		ClusterClient:     testClusterClient(t),
		Engine:            mustNewEngine(t),
		Config:            testConfig(t),
		DB:                db,
		StartupBootstrap:  startupBootstrapFunc(noopStartupBootstrap),
		RuntimeState:      runtimeStateLoaderFunc(func(context.Context) error { return nil }),
		RuntimeCheckpoint: NewFileRuntimeStateCheckpoint(dataDir, nil, nil, &recordingHydrator{}),
		ControlRuntime:    newControlRuntimeFake(nil, false),
		RequestLogs:       newRequestLogRuntimeFake(nil, nil),
	})
	cleanupApp(t, application)

	hook := &checkpointLifecycleLogHook{}
	logger := logrus.StandardLogger()
	previousHooks := make(logrus.LevelHooks, len(logger.Hooks))
	for level, hooks := range logger.Hooks {
		previousHooks[level] = append([]logrus.Hook(nil), hooks...)
	}
	logger.AddHook(hook)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks) })

	if err := application.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if len(hook.entries) != 1 {
		t.Fatalf("checkpoint restore lifecycle log count = %d, want 1", len(hook.entries))
	}
	if got := hook.entries[0].Level; got != logrus.WarnLevel {
		t.Fatalf("checkpoint restore lifecycle log level = %s, want warning", got)
	}
}

func TestAppSavesCheckpointBeforeRequestLogsAndUsesIndependentContext(t *testing.T) {
	db, err := storage.Open(pgtest.NewDatabase(t))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	var order []string
	checkpoint := runtimeCheckpointFake{
		save: func(ctx context.Context) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			order = append(order, "checkpoint")
			return nil
		},
	}
	requestLogs := newRequestLogRuntimeFake(nil, func(context.Context) error {
		order = append(order, "request-logs")
		return nil
	})
	controlRuntime := newControlRuntimeFake(nil, true)
	application := NewApp(AppParams{
		ClusterClient:     testClusterClient(t),
		Engine:            mustNewEngine(t),
		Config:            testConfig(t),
		DB:                db,
		StartupBootstrap:  startupBootstrapFunc(noopStartupBootstrap),
		RuntimeState:      runtimeStateLoaderFunc(func(context.Context) error { return nil }),
		RuntimeCheckpoint: checkpoint,
		ControlRuntime:    controlRuntime,
		RequestLogs:       requestLogs,
	})
	cleanupApp(t, application)
	t.Cleanup(controlRuntime.Release)

	if err := application.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := application.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop() error = %v, want context cancellation from forced drain", err)
	}
	if want := []string{"checkpoint", "request-logs"}; !slices.Equal(order, want) {
		t.Fatalf("shutdown order = %#v, want %#v", order, want)
	}
}

func TestFileRuntimeStateCheckpointRestoresStatsAndConsumesFile(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, runtimeStateCheckpointFileName)

	registry := state.NewCredentialRegistry()
	if err := registry.ReplaceCredentials([]state.CredentialEntry{{
		ID: 1, GroupID: 10, Version: 1, IdentityGeneration: 1, Fingerprint: "test-1", Status: state.CredentialStatusActive,
		EncryptedValue: "cipher-one",
	}}); err != nil {
		t.Fatalf("replace registry: %v", err)
	}
	stats := health.NewStatsStore()
	stats.RecordFailure(1, health.FailureCategoryUpstreamHostError, 503, time.Date(2026, 8, 7, 11, 59, 0, 0, time.UTC))

	checkpoint := NewFileRuntimeStateCheckpoint(dataDir, registry, stats, &recordingHydrator{})
	if err := checkpoint.Save(context.Background()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("checkpoint file stat error = %v", err)
	}

	loadedRegistry := state.NewCredentialRegistry()
	if err := loadedRegistry.ReplaceCredentials([]state.CredentialEntry{{
		ID: 1, GroupID: 10, Version: 1, IdentityGeneration: 1, Fingerprint: "test-1", Status: state.CredentialStatusActive,
		EncryptedValue: "cipher-one",
	}, {
		ID: 2, GroupID: 20, Version: 1, IdentityGeneration: 2, Fingerprint: "test-2", Status: state.CredentialStatusActive,
		EncryptedValue: "cipher-two",
	}}); err != nil {
		t.Fatalf("replace loaded registry: %v", err)
	}
	loadedStats := health.NewStatsStore()
	loader := NewFileRuntimeStateCheckpoint(dataDir, loadedRegistry, loadedStats, &recordingHydrator{})
	if err := loader.Restore(context.Background()); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("checkpoint file still exists after Restore(), stat error = %v", err)
	}

	gotStats := loadedStats.Snapshot(1, time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC))
	if gotStats.Failure != 1 || gotStats.Problem != 1 || gotStats.LastStatusCode != 503 {
		t.Fatalf("restored key stats = %#v", gotStats)
	}
}

func TestFileRuntimeStateCheckpointReturnsErrorWhenDeleteFails(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, runtimeStateCheckpointFileName)
	raw, err := json.Marshal(runtimeStateCheckpointDocument{
		Scheduling: &state.SchedulingCheckpoint{},
	})
	if err != nil {
		t.Fatalf("marshal checkpoint fixture: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write checkpoint fixture: %v", err)
	}

	registry := state.NewCredentialRegistry()
	if err := registry.ReplaceCredentials([]state.CredentialEntry{{
		ID: 1, GroupID: 10, Version: 1, IdentityGeneration: 1, Fingerprint: "test-1", Status: state.CredentialStatusActive,
		EncryptedValue: "cipher-one",
	}}); err != nil {
		t.Fatalf("replace registry: %v", err)
	}
	checkpoint := NewFileRuntimeStateCheckpoint(dataDir, registry, health.NewStatsStore(), &recordingHydrator{})
	// The normal file implementation removes the file successfully. This test
	// documents that a failed removal must prevent applying stale data through
	// the injectable filesystem hook used by the implementation.
	checkpoint.removeFile = func(string) error { return os.ErrPermission }
	if err := checkpoint.Restore(context.Background()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Restore() error = %v, want permission error", err)
	}
	if got := registry.Snapshot()[0].WeightManual; got != nil {
		t.Fatalf("configured weight after failed checkpoint removal = %v, want unset", got)
	}
}

func TestFileRuntimeStateCheckpointConsumesMalformedFileAndReturnsError(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, runtimeStateCheckpointFileName)
	if err := os.WriteFile(path, []byte(`{"credentials":[`), 0o600); err != nil {
		t.Fatalf("write malformed checkpoint fixture: %v", err)
	}
	registry := state.NewCredentialRegistry()
	if err := registry.ReplaceCredentials([]state.CredentialEntry{{
		ID: 1, GroupID: 10, Version: 1, IdentityGeneration: 1, Fingerprint: "test-1", Status: state.CredentialStatusActive,
		EncryptedValue: "cipher-one",
	}}); err != nil {
		t.Fatalf("replace registry: %v", err)
	}
	checkpoint := NewFileRuntimeStateCheckpoint(dataDir, registry, health.NewStatsStore(), &recordingHydrator{})
	if err := checkpoint.Restore(context.Background()); err == nil {
		t.Fatal("Restore() error = nil, want malformed checkpoint error")
	}
	if got := registry.Snapshot()[0].WeightManual; got != nil {
		t.Fatalf("configured weight after malformed checkpoint = %v, want unset", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("malformed checkpoint file was not consumed, stat error = %v", err)
	}
}

type recordingHydrator struct {
	calls int
	err   error
}

func (hydrator *recordingHydrator) Hydrate(context.Context) error {
	hydrator.calls++
	return hydrator.err
}

func TestFileRuntimeStateCheckpointLeavesHealthToSharedStore(t *testing.T) {
	dataDir := t.TempDir()
	entries := []state.CredentialEntry{{
		ID: 1, GroupID: 10, Version: 1, IdentityGeneration: 1, Fingerprint: "test-1", Status: state.CredentialStatusActive,
		EncryptedValue: "cipher-one",
	}}
	registry := state.NewCredentialRegistry()
	if err := registry.ReplaceCredentials(entries); err != nil {
		t.Fatal(err)
	}
	registry.SetBlacklisted(1)
	registry.SchedulingState().WithLock(func(ledger *state.SchedulingLedger) { ledger.Sequence = 42 })

	// A checkpoint written by an older release may still carry credential
	// health; it must not leak past the shared store.
	path := filepath.Join(dataDir, runtimeStateCheckpointFileName)
	if err := NewFileRuntimeStateCheckpoint(dataDir, registry, nil, &recordingHydrator{}).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &legacy)
	}
	if err != nil {
		t.Fatal(err)
	}
	legacy["credentials"] = json.RawMessage(`[{"id":1,"group_id":10,"blacklisted":true}]`)
	if raw, err = json.Marshal(legacy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := state.NewCredentialRegistry()
	if err := loaded.ReplaceCredentials(entries); err != nil {
		t.Fatal(err)
	}
	hydrator := &recordingHydrator{}
	if err := NewFileRuntimeStateCheckpoint(dataDir, loaded, nil, hydrator).Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hydrator.calls != 1 || loaded.Snapshot()[0].Blacklisted {
		t.Fatalf("hydrate calls = %d, blacklisted = %t; want Redis to own health", hydrator.calls, loaded.Snapshot()[0].Blacklisted)
	}
	var sequence uint64
	loaded.SchedulingState().WithLock(func(ledger *state.SchedulingLedger) { sequence = ledger.Sequence })
	if sequence != 42 {
		t.Fatalf("scheduling sequence = %d, want the local ledger restored", sequence)
	}

	// Without a file the shared health is still hydrated, and errors surface.
	hydrator.err = errors.New("redis down")
	if err := NewFileRuntimeStateCheckpoint(dataDir, loaded, nil, hydrator).Restore(context.Background()); err == nil || hydrator.calls != 2 {
		t.Fatalf("Restore() without file = %v, calls = %d", err, hydrator.calls)
	}

	if err := NewFileRuntimeStateCheckpoint(dataDir, registry, nil, hydrator).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"credentials"`) {
		t.Fatalf("checkpoint wrote credential health: %s", raw)
	}
}
