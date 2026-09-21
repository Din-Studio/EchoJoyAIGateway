package control

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
)

type taskClaim struct {
	task   string
	period time.Duration
}

type claimOutcome struct {
	claimed bool
	err     error
}

// fakeTaskLease answers per task so one sweep can lose a period while the
// others keep theirs.
type fakeTaskLease struct {
	mu       sync.Mutex
	claims   []taskClaim
	outcomes map[string]claimOutcome
	fallback claimOutcome
	calls    chan taskClaim
}

func newFakeTaskLease(fallback claimOutcome) *fakeTaskLease {
	return &fakeTaskLease{
		outcomes: make(map[string]claimOutcome),
		fallback: fallback,
		calls:    make(chan taskClaim, 16),
	}
}

func (lease *fakeTaskLease) answer(task string, outcome claimOutcome) *fakeTaskLease {
	lease.outcomes[task] = outcome
	return lease
}

func (lease *fakeTaskLease) Claim(_ context.Context, task string, period time.Duration) (bool, error) {
	lease.mu.Lock()
	lease.claims = append(lease.claims, taskClaim{task: task, period: period})
	lease.mu.Unlock()
	select {
	case lease.calls <- taskClaim{task: task, period: period}:
	default:
	}
	if outcome, ok := lease.outcomes[task]; ok {
		return outcome.claimed, outcome.err
	}
	return lease.fallback.claimed, lease.fallback.err
}

func (lease *fakeTaskLease) Holder() string { return "fake-holder" }

func (lease *fakeTaskLease) recorded() []taskClaim {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return append([]taskClaim(nil), lease.claims...)
}

func TestClaimTaskRunsEveryPeriodWithoutALease(t *testing.T) {
	t.Parallel()
	if !claimTask(t.Context(), nil, taskRequestLogRetention, time.Hour) {
		t.Fatal("claimTask() = false without a lease; single-instance mode must never skip a sweep")
	}
}

func TestClaimTaskSkipsThePeriodWhenClaimingFails(t *testing.T) {
	t.Parallel()
	lease := newFakeTaskLease(claimOutcome{err: errors.New("redis unreachable")})
	if claimTask(t.Context(), lease, taskOperationCompaction, time.Hour) {
		t.Fatal("claimTask() = true after a failed claim; an unknown owner must not duplicate the sweep")
	}
}

// The retention tick mixes one process-local step with two globally scheduled
// database sweeps. Losing a period must cost only the sweep whose period was
// lost.
func TestRetentionSweepClaimsEachDatabaseTaskAndKeepsCooldownExpiry(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		lease      *fakeTaskLease
		wantSweep  bool
		wantStages bool
	}{
		"both claimed": {
			lease:     newFakeTaskLease(claimOutcome{claimed: true}),
			wantSweep: true, wantStages: true,
		},
		"request log period lost": {
			lease: newFakeTaskLease(claimOutcome{claimed: true}).
				answer(taskRequestLogRetention, claimOutcome{claimed: false}),
			wantSweep: false, wantStages: true,
		},
		"stage cleanup period lost": {
			lease: newFakeTaskLease(claimOutcome{claimed: true}).
				answer(taskCredentialStageCleanup, claimOutcome{claimed: false}),
			wantSweep: true, wantStages: false,
		},
		"claiming failed": {
			lease:     newFakeTaskLease(claimOutcome{err: errors.New("redis unreachable")}),
			wantSweep: false, wantStages: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := time.Date(2026, time.September, 1, 9, 0, 0, 0, time.UTC)
			registry := newCooldownRegistry(t, base)
			cleaner := &countingRequestLogCleaner{}
			stages := &controlledStageCleaner{calls: make(chan time.Time, 2)}
			runtime := &Runtime{
				registry: registry, requestLogCleaner: cleaner, stageCleaner: stages,
				taskLease: test.lease,
			}

			sweepAt := base.Add(time.Hour)
			runtime.sweepRetention(t.Context(), sweepAt)

			if got := cleaner.count(); (got > 0) != test.wantSweep {
				t.Fatalf("request log sweeps = %d, want claimed = %t", got, test.wantSweep)
			}
			if got := len(stages.calls); (got > 0) != test.wantStages {
				t.Fatalf("stage cleanups = %d, want claimed = %t", got, test.wantStages)
			}
			// Model cooldown expiry is this process's own memory: it runs on
			// every instance whatever the claims did. Reading at base keeps
			// the read itself from pruning the entry.
			if cooldowns := registry.ModelCooldowns(1, base); len(cooldowns) != 0 {
				t.Fatalf("model cooldowns = %v after the sweep, want expiry on every instance", cooldowns)
			}
			assertClaimPeriods(t, test.lease.recorded(), map[string]time.Duration{
				taskRequestLogRetention:    retentionInterval,
				taskCredentialStageCleanup: retentionInterval,
			})
		})
	}
}

// A lost period is not a stopped task: the next tick claims from scratch.
func TestRetentionSweepRetriesTheClaimOnTheNextPeriod(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, time.September, 2, 9, 0, 0, 0, time.UTC)
	lease := newFakeTaskLease(claimOutcome{err: errors.New("redis unreachable")})
	cleaner := &countingRequestLogCleaner{}
	runtime := &Runtime{
		registry: newCooldownRegistry(t, base), requestLogCleaner: cleaner, taskLease: lease,
	}

	runtime.sweepRetention(t.Context(), base.Add(time.Hour))
	lease.outcomes[taskRequestLogRetention] = claimOutcome{claimed: true}
	runtime.sweepRetention(t.Context(), base.Add(2*time.Hour))

	if got := cleaner.count(); got != 1 {
		t.Fatalf("request log sweeps = %d, want exactly the recovered period", got)
	}
	if got := len(lease.recorded()); got != 2 {
		t.Fatalf("claims = %d, want one per period", got)
	}
}

// Compaction is the only globally scheduled work in the operation recovery
// loop, so a lost period must leave the durable result untouched.
func TestOperationCompactionClaimsItsPeriod(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		lease         *fakeTaskLease
		wantCompacted bool
	}{
		"claimed":     {lease: newFakeTaskLease(claimOutcome{claimed: true}), wantCompacted: true},
		"period lost": {lease: newFakeTaskLease(claimOutcome{claimed: false}), wantCompacted: false},
		"claim failed": {
			lease: newFakeTaskLease(claimOutcome{err: errors.New("redis unreachable")}), wantCompacted: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newServiceFixture(t)
			fixture.service.taskLease = test.lease
			completeOneOperation(t, fixture, 0x41)

			fixture.service.compactOnSchedule(
				t.Context(), time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC),
			)

			var row models.ControlOperation
			if err := fixture.db.First(&row).Error; err != nil {
				t.Fatalf("read operation: %v", err)
			}
			if (row.CompactedAtMS != nil) != test.wantCompacted {
				t.Fatalf("compacted = %t, want %t", row.CompactedAtMS != nil, test.wantCompacted)
			}
			assertClaimPeriods(t, test.lease.recorded(), map[string]time.Duration{
				taskOperationCompaction: operationCompactionInterval,
			})
		})
	}
}

// The drain path repairs this instance's own post-commit state, so it must run
// even while every claim is being refused.
func TestOperationRecoveryDrainNeverClaimsAPeriod(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	lease := newFakeTaskLease(claimOutcome{claimed: false})
	fixture.service.taskLease = lease
	fixture.service.operationRandom = bytes.NewReader(bytes.Repeat([]byte{0x42}, 16))
	fixture.service.reconcileRegistryGroup = func(uint, []state.CredentialEntry) (bool, error) {
		return false, errors.New("injected registry failure")
	}
	mutations := 0
	input := newDurableGroupOperationInput(
		t, fixture, "128f47a2-9c35-4d6e-8b1a-1234567890ab", &mutations,
	)
	if _, err := fixture.service.executeIdempotentOperation(t.Context(), input); err == nil {
		t.Fatal("executeIdempotentOperation() error = nil, want the injected post-commit failure")
	}
	fixture.service.reconcileRegistryGroup = fixture.registry.ReconcileGroup

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.service.RunOperationRecovery(ctx)
	}()
	fixture.service.wakeOperationRecovery()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var pending int64
		if err := fixture.db.Model(&models.ControlOperation{}).
			Where("completed_at_ms IS NULL").Count(&pending).Error; err != nil {
			t.Fatalf("count pending operations: %v", err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the woken drain never completed the pending operation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if claims := lease.recorded(); len(claims) != 0 {
		t.Fatalf("drain claimed %v; post-commit repair belongs to this instance alone", claims)
	}
}

func assertClaimPeriods(t *testing.T, claims []taskClaim, want map[string]time.Duration) {
	t.Helper()
	for _, claim := range claims {
		period, known := want[claim.task]
		if !known {
			t.Fatalf("claimed unexpected task %q", claim.task)
		}
		if claim.period != period {
			t.Fatalf("claim period for %q = %v, want %v", claim.task, claim.period, period)
		}
	}
}

type countingRequestLogCleaner struct {
	mu     sync.Mutex
	sweeps int
}

func (cleaner *countingRequestLogCleaner) Sweep(context.Context, time.Time) {
	cleaner.mu.Lock()
	cleaner.sweeps++
	cleaner.mu.Unlock()
}

func (cleaner *countingRequestLogCleaner) count() int {
	cleaner.mu.Lock()
	defer cleaner.mu.Unlock()
	return cleaner.sweeps
}

// newCooldownRegistry returns a registry holding one model cooldown that has
// already lapsed by the time the sweep runs.
func newCooldownRegistry(t *testing.T, base time.Time) *state.CredentialRegistry {
	t.Helper()
	registry := state.NewCredentialRegistry()
	if err := registry.ApplyCredentialImport(1, []state.CredentialEntry{{
		ID: 1, GroupID: 1, Status: state.CredentialStatusActive,
		Version: 1, IdentityGeneration: 1, Fingerprint: "fingerprint-one",
		EncryptedValue: "cipher-one",
	}}); err != nil {
		t.Fatalf("ApplyCredentialImport() error = %v", err)
	}
	ref, ok := registry.CredentialRef(1)
	if !ok {
		t.Fatal("CredentialRef() missing the imported credential")
	}
	if accepted, _ := registry.SetModelCooldown(ref, "gpt-4o", base.Add(30*time.Minute), base); !accepted {
		t.Fatal("SetModelCooldown() was not accepted")
	}
	return registry
}

func completeOneOperation(t *testing.T, fixture serviceFixture, seed byte) {
	t.Helper()
	fixture.service.operationRandom = bytes.NewReader(bytes.Repeat([]byte{seed}, 16))
	fixture.service.now = func() time.Time {
		return time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	}
	if _, err := fixture.service.executeIdempotentOperation(t.Context(), idempotentOperationInput{
		IdempotencyKey: "0a8f47a2-9c35-4d6e-8b1a-1234567890ab",
		DigestVersion:  1,
		RequestDigest:  [32]byte{seed},
		Kind:           operationKindAccessKeyCreate,
		Mutate: func(operationTransaction) (idempotentMutationResult, error) {
			return idempotentMutationResult{
				ResourceIdentity: "access-key:7",
				CanonicalResult:  []byte(`{"id":7}`),
			}, nil
		},
	}); err != nil {
		t.Fatalf("execute operation: %v", err)
	}
}
