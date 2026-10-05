package requestlog

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/cluster"
	"gpt-load/internal/platform/redact"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/testutil/clustertest"
)

func TestAccessQuotaCheckpointWriterAppliesAndRetriesAbsoluteSnapshot(t *testing.T) {
	db := openRequestLogQueryDB(t)
	accessKey, rule := createCheckpointRule(t, db)
	writer := &gormAccessQuotaCheckpointWriter{db: db}
	startedAt := int64(1_787_184_000_000)
	endsAt := startedAt + 300_000
	snapshot := accessquota.RestoredState{
		AccessKeyID: accessKey.ID, RuleID: rule.ID, RuleRevision: 1,
		UsedNanoUSD: 75, WindowStartedAtMS: &startedAt, WindowEndsAtMS: &endsAt,
		WindowGeneration: 2, SnapshotVersion: 4,
	}
	if err := writer.WriteSnapshots(t.Context(), []accessquota.RestoredState{snapshot}); err != nil {
		t.Fatalf("first WriteSnapshots() error = %v", err)
	}
	if err := writer.WriteSnapshots(t.Context(), []accessquota.RestoredState{snapshot}); err != nil {
		t.Fatalf("same-version retry error = %v", err)
	}
	var persisted models.AccessKeyCostLimitState
	if err := db.First(&persisted, rule.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.UsedNanoUSD != 75 || persisted.SnapshotVersion != 4 ||
		persisted.WindowGeneration != 2 || persisted.WindowStartedAtMS == nil ||
		*persisted.WindowStartedAtMS != startedAt {
		t.Fatalf("persisted checkpoint = %#v", persisted)
	}
}

func TestAccessQuotaCheckpointWriterClassifiesStaleDeletedAndMissingState(t *testing.T) {
	t.Run("stale revision is discarded", func(t *testing.T) {
		db := openRequestLogQueryDB(t)
		accessKey, rule := createCheckpointRule(t, db)
		if err := db.Model(&models.AccessKeyCostLimitRule{}).Where("id = ?", rule.ID).
			Update("rule_revision", 2).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&models.AccessKeyCostLimitState{}).Where("rule_id = ?", rule.ID).
			Update("rule_revision", 2).Error; err != nil {
			t.Fatal(err)
		}
		writer := &gormAccessQuotaCheckpointWriter{db: db}
		err := writer.WriteSnapshots(t.Context(), []accessquota.RestoredState{{
			AccessKeyID: accessKey.ID, RuleID: rule.ID, RuleRevision: 1,
			UsedNanoUSD: 99, SnapshotVersion: 5,
		}})
		if err != nil {
			t.Fatalf("stale WriteSnapshots() error = %v", err)
		}
		var persisted models.AccessKeyCostLimitState
		if err := db.First(&persisted, rule.ID).Error; err != nil {
			t.Fatal(err)
		}
		if persisted.RuleRevision != 2 || persisted.UsedNanoUSD != 0 {
			t.Fatalf("stale snapshot changed state = %#v", persisted)
		}
	})

	t.Run("deleted rule is discarded", func(t *testing.T) {
		db := openRequestLogQueryDB(t)
		accessKey, rule := createCheckpointRule(t, db)
		if err := db.Delete(&rule).Error; err != nil {
			t.Fatal(err)
		}
		writer := &gormAccessQuotaCheckpointWriter{db: db}
		if err := writer.WriteSnapshots(t.Context(), []accessquota.RestoredState{{
			AccessKeyID: accessKey.ID, RuleID: rule.ID, RuleRevision: 1,
			UsedNanoUSD: 99, SnapshotVersion: 5,
		}}); err != nil {
			t.Fatalf("deleted WriteSnapshots() error = %v", err)
		}
	})

	t.Run("existing rule without state fails", func(t *testing.T) {
		db := openRequestLogQueryDB(t)
		accessKey, rule := createCheckpointRule(t, db)
		if err := db.Delete(&models.AccessKeyCostLimitState{}, rule.ID).Error; err != nil {
			t.Fatal(err)
		}
		writer := &gormAccessQuotaCheckpointWriter{db: db}
		if err := writer.WriteSnapshots(t.Context(), []accessquota.RestoredState{{
			AccessKeyID: accessKey.ID, RuleID: rule.ID, RuleRevision: 1,
			UsedNanoUSD: 99, SnapshotVersion: 5,
		}}); err == nil {
			t.Fatal("WriteSnapshots() error = nil, want missing state failure")
		}
	})
}

func TestRequestLogServiceFlushesAndAcknowledgesQuotaDirtyStateWithoutLogEvent(t *testing.T) {
	db := openRequestLogQueryDB(t)
	accessKey, rule := createCheckpointRule(t, db)
	quota := newTestSharedQuota(t, AccessQuotaStateReader{DB: db})
	snapshot := periodicCheckpointSnapshot(accessKey.ID, rule.ID)
	service := NewService(db, redact.New(), staticRetentionPolicy{days: 7})
	service.SetAccessQuotaCheckpointSource(quota)
	spendSharedQuota(t, quota, snapshot, accessKey.ID, 75)
	if err := service.writeBatch(t.Context(), nil); err != nil {
		t.Fatalf("writeBatch() error = %v", err)
	}

	var persisted models.AccessKeyCostLimitState
	if err := db.First(&persisted, rule.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.UsedNanoUSD != 75 || persisted.SnapshotVersion <= 1 {
		t.Fatalf("persisted checkpoint = %#v", persisted)
	}
	if quota.HasDirty() {
		t.Fatal("shared quota still dirty after checkpoint")
	}
}

func TestRequestLogWorkerWakesForQuotaCheckpointAndDrainsOnStop(t *testing.T) {
	db := openRequestLogQueryDB(t)
	accessKey, rule := createCheckpointRule(t, db)
	quota := newTestSharedQuota(t, AccessQuotaStateReader{DB: db})
	snapshot := periodicCheckpointSnapshot(accessKey.ID, rule.ID)
	timers := newManualTimerFactory()
	service := NewService(db, redact.New(), staticRetentionPolicy{days: 7})
	service.SetAccessQuotaCheckpointSource(quota)
	service.timerFactory = timers.New
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	spendSharedQuota(t, quota, snapshot, accessKey.ID, 75)
	receiveValue(t, timers.created).Fire()
	waitForCheckpointCost(t, db, rule.ID, 75)

	spendSharedQuota(t, quota, snapshot, accessKey.ID, 5)
	if err := service.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	waitForCheckpointCost(t, db, rule.ID, 80)
}

func TestRequestLogWorkerDrainsEveryQuotaCheckpointBatchOnStop(t *testing.T) {
	owners := make(initialQuotaStates, batchSize+1)
	for index := 1; index <= batchSize+1; index++ {
		owners[uint(index)] = uint(index)
	}
	quota := newTestSharedQuota(t, owners)
	snapshot := owners.snapshot()
	for index := 1; index <= batchSize+1; index++ {
		spendSharedQuota(t, quota, snapshot, uint(index), 1)
	}

	timers := newManualTimerFactory()
	batchSizes := make([]int, 0, 2)
	service := newService(
		batchWriterFunc(func(context.Context, []models.RequestLog) error { return nil }),
		redact.New(),
		timers.New,
	)
	service.SetAccessQuotaCheckpointSource(quota)
	service.quotaWriter = accessQuotaCheckpointWriterFunc(func(
		_ context.Context,
		snapshots []accessquota.RestoredState,
	) error {
		batchSizes = append(batchSizes, len(snapshots))
		return nil
	})
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	if err := service.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if len(batchSizes) != 2 || batchSizes[0] != batchSize || batchSizes[1] != 1 {
		t.Fatalf("checkpoint batch sizes = %v, want [%d 1]", batchSizes, batchSize)
	}
	if quota.HasDirty() {
		t.Fatal("shared quota still dirty after stop")
	}
}

func TestRequestLogWorkerReportsFinalQuotaCheckpointFailureOnStop(t *testing.T) {
	owners := initialQuotaStates{302: 1}
	quota := newTestSharedQuota(t, owners)
	spendSharedQuota(t, quota, owners.snapshot(), 1, 75)

	service := newService(
		batchWriterFunc(func(context.Context, []models.RequestLog) error { return nil }),
		redact.New(),
		newManualTimerFactory().New,
	)
	service.SetAccessQuotaCheckpointSource(quota)
	service.quotaWriter = accessQuotaCheckpointWriterFunc(func(
		context.Context,
		[]accessquota.RestoredState,
	) error {
		return errors.New("checkpoint unavailable")
	})
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	err := service.Stop(context.Background())
	if err == nil || !strings.Contains(err.Error(), "final access key cost limit checkpoint") {
		t.Fatalf("Stop() error = %v, want final checkpoint failure", err)
	}
	if !quota.HasDirty() {
		t.Fatal("shared quota lost its dirty state after a failed final checkpoint")
	}
}

func TestRequestLogWorkerRetriesQuotaCheckpointWithoutClearingDirtyVersion(t *testing.T) {
	owners := initialQuotaStates{301: 1}
	quota := newTestSharedQuota(t, owners)
	timers := newManualTimerFactory()
	writes := make(chan []accessquota.RestoredState, 2)
	writeCount := 0
	service := newService(
		batchWriterFunc(func(context.Context, []models.RequestLog) error { return nil }),
		redact.New(),
		timers.New,
	)
	service.SetAccessQuotaCheckpointSource(quota)
	service.quotaWriter = accessQuotaCheckpointWriterFunc(func(
		_ context.Context,
		snapshots []accessquota.RestoredState,
	) error {
		writes <- append([]accessquota.RestoredState(nil), snapshots...)
		writeCount++
		if writeCount == 1 {
			return errors.New("checkpoint unavailable")
		}
		return nil
	})
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	spendSharedQuota(t, quota, owners.snapshot(), 1, 75)
	receiveValue(t, timers.created).Fire()
	first := receiveValue(t, writes)
	failureDeadline := time.Now().Add(time.Second)
	for !service.Stats().AccessQuotaCheckpointDegraded && time.Now().Before(failureDeadline) {
		time.Sleep(time.Millisecond)
	}
	if stats := service.Stats(); !stats.AccessQuotaCheckpointDegraded ||
		stats.AccessQuotaCheckpointWriteFailureTotal != 1 {
		t.Fatalf("checkpoint state after failed write = %#v", stats)
	}
	receiveValue(t, timers.created).Fire()
	second := receiveValue(t, writes)
	if len(first) != 1 || len(second) != 1 ||
		first[0].SnapshotVersion != second[0].SnapshotVersion ||
		first[0].UsedNanoUSD != second[0].UsedNanoUSD {
		t.Fatalf("checkpoint retries = %#v / %#v", first, second)
	}
	deadline := time.Now().Add(time.Second)
	for quota.HasDirty() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if quota.HasDirty() {
		t.Fatal("shared quota still dirty after successful retry")
	}
	if err := service.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := service.Stats()
	if stats.WriteFailureTotal != 0 || stats.AccessQuotaCheckpointWriteFailureTotal != 1 ||
		stats.AccessQuotaCheckpointDegraded ||
		stats.LastAccessQuotaCheckpointWriteFailureAt.IsZero() ||
		stats.DroppedPersistFailedTotal != 0 {
		t.Fatalf("checkpoint failure stats = %#v", stats)
	}
}

// newTestSharedQuota builds the Redis cost-limit state on miniredis.
func newTestSharedQuota(t *testing.T, states cluster.AccessQuotaStateReader) *cluster.AccessQuota {
	t.Helper()
	_, client := clustertest.NewClient(t)
	return cluster.NewAccessQuota(client, states)
}

// spendSharedQuota admits one request and settles cost against every rule
// the snapshot defines for accessKeyID.
func spendSharedQuota(t *testing.T, quota *cluster.AccessQuota, snapshot *state.ConfigSnapshot, accessKeyID uint, cost int64) {
	t.Helper()
	ticket, decision, err := quota.Admit(t.Context(), snapshot, accessKeyID, time.Now())
	if err != nil || !decision.Allowed {
		t.Fatalf("Admit(%d) = %#v, %v", accessKeyID, decision, err)
	}
	if _, err := quota.Complete(t.Context(), ticket, cost); err != nil {
		t.Fatalf("Complete(%d) error = %v", accessKeyID, err)
	}
}

// periodicCheckpointSnapshot defines the rule createCheckpointRule persists.
func periodicCheckpointSnapshot(accessKeyID, ruleID uint) *state.ConfigSnapshot {
	return &state.ConfigSnapshot{AccessKeysByID: map[uint]state.AccessKeyView{accessKeyID: {
		ID: accessKeyID, CostLimitRules: []accessquota.Rule{{
			ID: ruleID, Revision: 1, Kind: accessquota.KindPeriodic, LimitNanoUSD: 100, PeriodSeconds: 300,
		}},
	}}}
}

// initialQuotaStates maps rule IDs to their AccessKey and serves an empty
// revision-1 checkpoint for each, standing in for the database.
type initialQuotaStates map[uint]uint

func (owners initialQuotaStates) ReadAccessQuotaStates(
	_ context.Context,
	ruleIDs []uint,
) ([]accessquota.RestoredState, error) {
	states := make([]accessquota.RestoredState, 0, len(ruleIDs))
	for _, ruleID := range ruleIDs {
		if accessKeyID, exists := owners[ruleID]; exists {
			states = append(states, accessquota.RestoredState{
				AccessKeyID: accessKeyID, RuleID: ruleID, RuleRevision: 1, SnapshotVersion: 1,
			})
		}
	}
	return states, nil
}

// snapshot defines one total rule with a large limit per entry.
func (owners initialQuotaStates) snapshot() *state.ConfigSnapshot {
	snapshot := &state.ConfigSnapshot{AccessKeysByID: make(map[uint]state.AccessKeyView, len(owners))}
	for ruleID, accessKeyID := range owners {
		snapshot.AccessKeysByID[accessKeyID] = state.AccessKeyView{
			ID: accessKeyID, CostLimitRules: []accessquota.Rule{{
				ID: ruleID, Revision: 1, Kind: accessquota.KindTotal, LimitNanoUSD: 1_000,
			}},
		}
	}
	return snapshot
}

type accessQuotaCheckpointWriterFunc func(context.Context, []accessquota.RestoredState) error

func (fn accessQuotaCheckpointWriterFunc) WriteSnapshots(
	ctx context.Context,
	snapshots []accessquota.RestoredState,
) error {
	return fn(ctx, snapshots)
}

func createCheckpointRule(t *testing.T, db *gorm.DB) (models.AccessKey, models.AccessKeyCostLimitRule) {
	t.Helper()
	accessKey := models.AccessKey{
		Name: "checkpoint", KeyValue: "cipher", KeyHash: "checkpoint-hash",
		KeySuffix: "cafe", Status: "active", Filters: models.JSON(`{}`),
	}
	if err := db.Create(&accessKey).Error; err != nil {
		t.Fatal(err)
	}
	rule := models.AccessKeyCostLimitRule{
		AccessKeyID: accessKey.ID, Kind: models.AccessKeyCostLimitKindPeriodic,
		LimitNanoUSD: 100, PeriodSeconds: 300, RuleRevision: 1,
	}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.AccessKeyCostLimitState{
		RuleID: rule.ID, RuleRevision: 1, SnapshotVersion: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return accessKey, rule
}

func waitForCheckpointCost(t *testing.T, db *gorm.DB, ruleID uint, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var state models.AccessKeyCostLimitState
		if err := db.First(&state, ruleID).Error; err != nil {
			t.Fatal(err)
		}
		if state.UsedNanoUSD == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("checkpoint cost = %d, want %d", state.UsedNanoUSD, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAccessQuotaStateReaderReturnsCheckpointsWithAccessKey(t *testing.T) {
	db := openRequestLogQueryDB(t)
	accessKey, rule := createCheckpointRule(t, db)
	startedAt := int64(1_787_184_000_000)
	endsAt := startedAt + 300_000
	written := accessquota.RestoredState{
		AccessKeyID: accessKey.ID, RuleID: rule.ID, RuleRevision: 1,
		UsedNanoUSD: 42, WindowStartedAtMS: &startedAt, WindowEndsAtMS: &endsAt,
		WindowGeneration: 3, SnapshotVersion: 7,
	}
	if err := (&gormAccessQuotaCheckpointWriter{db: db}).WriteSnapshots(t.Context(), []accessquota.RestoredState{written}); err != nil {
		t.Fatal(err)
	}

	states, err := AccessQuotaStateReader{DB: db}.ReadAccessQuotaStates(t.Context(), []uint{rule.ID, rule.ID + 100})
	if err != nil {
		t.Fatalf("ReadAccessQuotaStates() error = %v", err)
	}
	if !reflect.DeepEqual(states, []accessquota.RestoredState{written}) {
		t.Fatalf("ReadAccessQuotaStates() = %#v, want %#v", states, written)
	}
}

// scriptedCheckpointSource stands in for the cluster-mode shared quota state.
type scriptedCheckpointSource struct {
	mu       sync.Mutex
	pending  []accessquota.RestoredState
	readErr  error
	acked    []accessquota.RestoredState
	notifier func()
}

func (source *scriptedCheckpointSource) HasDirty() bool {
	source.mu.Lock()
	defer source.mu.Unlock()
	return len(source.pending) > 0
}

func (source *scriptedCheckpointSource) DirtySnapshots(
	context.Context,
	int,
) ([]accessquota.RestoredState, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.readErr != nil {
		return nil, source.readErr
	}
	return append([]accessquota.RestoredState(nil), source.pending...), nil
}

func (source *scriptedCheckpointSource) Ack(accessKeyID, ruleID uint, revision, snapshotVersion uint64) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.acked = append(source.acked, accessquota.RestoredState{
		AccessKeyID: accessKeyID, RuleID: ruleID, RuleRevision: revision, SnapshotVersion: snapshotVersion,
	})
	source.pending = nil
}

func (source *scriptedCheckpointSource) SetDirtyNotifier(notifier func()) {
	source.mu.Lock()
	source.notifier = notifier
	source.mu.Unlock()
}

func TestRequestLogServiceCheckpointsSharedQuotaSource(t *testing.T) {
	db := openRequestLogQueryDB(t)
	accessKey, rule := createCheckpointRule(t, db)
	service := NewService(db, redact.New(), staticRetentionPolicy{days: 7})
	source := &scriptedCheckpointSource{
		pending: []accessquota.RestoredState{{
			AccessKeyID: accessKey.ID, RuleID: rule.ID, RuleRevision: 1, UsedNanoUSD: 33, SnapshotVersion: 5,
		}},
		readErr: errors.New("redis: connection refused"),
	}
	service.SetAccessQuotaCheckpointSource(source)
	if source.notifier == nil {
		t.Fatal("SetAccessQuotaCheckpointSource() did not install the dirty notifier")
	}

	if err := service.writeBatch(t.Context(), nil); err == nil {
		t.Fatal("writeBatch() with unreadable source error = nil")
	}
	if stats := service.Stats(); !stats.AccessQuotaCheckpointDegraded || stats.AccessQuotaCheckpointWriteFailureTotal != 1 {
		t.Fatalf("stats after read failure = %#v", stats)
	}
	select {
	case <-service.quotaWake:
	default:
		t.Fatal("read failure did not schedule a retry")
	}

	source.mu.Lock()
	source.readErr = nil
	source.mu.Unlock()
	if err := service.writeBatch(t.Context(), nil); err != nil {
		t.Fatalf("writeBatch() retry error = %v", err)
	}
	waitForCheckpointCost(t, db, rule.ID, 33)
	if len(source.acked) != 1 || source.acked[0].SnapshotVersion != 5 || source.HasDirty() {
		t.Fatalf("acked = %#v dirty=%v", source.acked, source.HasDirty())
	}
	if stats := service.Stats(); stats.AccessQuotaCheckpointDegraded {
		t.Fatalf("stats after recovery = %#v", stats)
	}
}
