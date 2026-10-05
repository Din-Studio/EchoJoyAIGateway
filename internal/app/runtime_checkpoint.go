package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gpt-load/internal/health"
	"gpt-load/internal/state"
)

const runtimeStateCheckpointFileName = "runtime-state.checkpoint.json"

// RuntimeStateCheckpoint persists best-effort mutable runtime state between
// process runs. Implementations must not become a startup or shutdown blocker.
type RuntimeStateCheckpoint interface {
	Restore(context.Context) error
	Save(context.Context) error
}

// CredentialHealthHydrator restores credential health from the cluster store.
type CredentialHealthHydrator interface {
	Hydrate(context.Context) error
}

// runtimeStateCheckpointDocument keeps only this instance's scheduling and
// stats; credential health and Responses ownership live in Redis.
type runtimeStateCheckpointDocument struct {
	Stats      []health.StatsRuntimeCheckpoint `json:"stats,omitempty"`
	Scheduling *state.SchedulingCheckpoint     `json:"scheduling,omitempty"`
}

// FileRuntimeStateCheckpoint stores the small, disposable runtime checkpoint
// in DATA_DIR. The startup path consumes the file before parsing it so a
// malformed or partially written file cannot be retried on every restart.
type FileRuntimeStateCheckpoint struct {
	path     string
	registry *state.CredentialRegistry
	stats    *health.StatsStore
	// credentialHealth owns credential health; Restore always hydrates the
	// registry from it after the file restores scheduling and stats.
	credentialHealth CredentialHealthHydrator
	removeFile       func(string) error
}

func NewFileRuntimeStateCheckpoint(
	dataDir string,
	registry *state.CredentialRegistry,
	stats *health.StatsStore,
	credentialHealth CredentialHealthHydrator,
) *FileRuntimeStateCheckpoint {
	return &FileRuntimeStateCheckpoint{
		path:             filepath.Join(dataDir, runtimeStateCheckpointFileName),
		registry:         registry,
		stats:            stats,
		credentialHealth: credentialHealth,
		removeFile:       os.Remove,
	}
}

// Restore consumes the checkpoint file if present. Failures are returned to
// the lifecycle owner, which continues startup with database-backed state.
func (checkpoint *FileRuntimeStateCheckpoint) Restore(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	err := checkpoint.restoreFile()
	if hydrateErr := checkpoint.credentialHealth.Hydrate(ctx); hydrateErr != nil {
		err = errors.Join(err, fmt.Errorf("hydrate shared credential health: %w", hydrateErr))
	}
	return err
}

func (checkpoint *FileRuntimeStateCheckpoint) restoreFile() error {
	raw, err := os.ReadFile(checkpoint.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read runtime state checkpoint: %w", err)
	}
	if err := checkpoint.removeFile(checkpoint.path); err != nil {
		return fmt.Errorf("consume runtime state checkpoint: %w", err)
	}

	var document runtimeStateCheckpointDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("decode runtime state checkpoint: %w", err)
	}
	if checkpoint.registry != nil && document.Scheduling != nil {
		checkpoint.registry.SchedulingState().RestoreCheckpoint(*document.Scheduling)
	}
	if checkpoint.stats != nil {
		checkpoint.stats.RestoreRuntimeCheckpoint(document.Stats)
	}
	return nil
}

// Save serializes the current runtime state and writes it directly to the
// fixed checkpoint file. A failed write is removed best-effort and returned so
// the lifecycle owner can log it without blocking shutdown.
func (checkpoint *FileRuntimeStateCheckpoint) Save(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	document := runtimeStateCheckpointDocument{}
	if checkpoint.registry != nil {
		scheduling := checkpoint.registry.SchedulingState().CaptureCheckpoint()
		document.Scheduling = &scheduling
	}
	if checkpoint.stats != nil {
		document.Stats = checkpoint.stats.CaptureRuntimeCheckpoint()
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if err := os.WriteFile(checkpoint.path, payload, 0o600); err != nil {
		_ = os.Remove(checkpoint.path)
		return err
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
