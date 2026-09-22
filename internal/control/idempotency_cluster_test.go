package control

import (
	"context"
	"testing"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/storage/models"
)

// 两个 Service 共享同一个 SQLite 连接模拟两个进程共用一个数据库；SQLite 单连接
// 不允许并行事务，因此用 PrepareMutation / beforeAdvanceOperationStage 钩子在
// 事务之间做确定性交错。

func newPeerFixtures(t *testing.T) (serviceFixture, serviceFixture) {
	t.Helper()
	first := newServiceFixture(t)
	second := newServiceFixtureWithDatabase(t, first.db)
	return first, second
}

func mustOperationRow(t *testing.T, fixture serviceFixture, key string) models.ControlOperation {
	t.Helper()
	var row models.ControlOperation
	if err := fixture.db.Where("idempotency_key = ?", key).Take(&row).Error; err != nil {
		t.Fatalf("read ControlOperation %s: %v", key, err)
	}
	return row
}

func mustCount(t *testing.T, fixture serviceFixture, model any, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := fixture.db.Model(model).Where(query, args...).Count(&count).Error; err != nil {
		t.Fatalf("count %T: %v", model, err)
	}
	return count
}

func TestExecuteIdempotentOperationReplaysAfterPeerCommittedSameKey(t *testing.T) {
	t.Parallel()
	first, second := newPeerFixtures(t)
	const key = "11a1f47a-9c35-4d6e-8b1a-1234567890ab"
	firstMutations, secondMutations := 0, 0
	firstInput := newDurableGroupOperationInput(t, first, key, &firstMutations)
	secondInput := newDurableGroupOperationInput(t, second, key, &secondMutations)

	var firstResult idempotentOperationResult
	secondInput.PrepareMutation = func() {
		// 第二个实例已经查过操作行（不存在），此时第一个实例完整提交同一请求。
		result, err := first.service.executeIdempotentOperation(t.Context(), firstInput)
		if err != nil {
			t.Fatalf("first instance executeIdempotentOperation() error = %v", err)
		}
		firstResult = result
	}

	secondResult, err := second.service.executeIdempotentOperation(t.Context(), secondInput)
	if err != nil {
		t.Fatalf("second instance executeIdempotentOperation() error = %v", err)
	}
	if !secondResult.Replayed || firstResult.Replayed {
		t.Fatalf("replayed = second:%t first:%t, want only the second instance to replay",
			secondResult.Replayed, firstResult.Replayed)
	}
	if secondResult.OperationID != firstResult.OperationID ||
		string(secondResult.CanonicalResult) != string(firstResult.CanonicalResult) {
		t.Fatalf("second result = %+v, want the first instance's committed result %+v",
			secondResult, firstResult)
	}
	if firstMutations != 1 || secondMutations != 1 {
		t.Fatalf("mutations = first:%d second:%d, want one attempt each", firstMutations, secondMutations)
	}
	if rows := mustCount(t, first, &models.ControlOperation{}, "idempotency_key = ?", key); rows != 1 {
		t.Fatalf("ControlOperation rows = %d, want 1", rows)
	}
	if groups := mustCount(t, first, &models.Group{}, "name = ?", "group-"+key[:4]); groups != 1 {
		t.Fatalf("Group rows = %d, want the peer's single commit", groups)
	}
}

func TestExecuteIdempotentOperationRejectsDifferentDigestAfterPeerCommittedSameKey(t *testing.T) {
	t.Parallel()
	first, second := newPeerFixtures(t)
	const key = "22a1f47a-9c35-4d6e-8b1a-1234567890ab"
	firstMutations, secondMutations := 0, 0
	firstInput := newDurableGroupOperationInput(t, first, key, &firstMutations)
	secondInput := newDurableGroupOperationInput(t, second, key, &secondMutations)
	secondInput.RequestDigest = [32]byte{0xff}
	secondInput.PrepareMutation = func() {
		if _, err := first.service.executeIdempotentOperation(t.Context(), firstInput); err != nil {
			t.Fatalf("first instance executeIdempotentOperation() error = %v", err)
		}
	}

	_, err := second.service.executeIdempotentOperation(t.Context(), secondInput)
	assertAPIErrorCode(t, err, app_errors.ErrIdempotencyKeyReused.Code)
	if firstMutations != 1 || secondMutations != 1 {
		t.Fatalf("mutations = first:%d second:%d, want one attempt each", firstMutations, secondMutations)
	}
	if groups := mustCount(t, first, &models.Group{}, "name = ?", "group-"+key[:4]); groups != 1 {
		t.Fatalf("Group rows = %d, want the second attempt rolled back", groups)
	}
}

func TestOperationStageAdvanceAdoptsProgressMadeByPeerReplay(t *testing.T) {
	t.Parallel()
	first, second := newPeerFixtures(t)
	const key = "33a1f47a-9c35-4d6e-8b1a-1234567890ab"
	firstMutations, secondMutations := 0, 0
	firstInput := newDurableGroupOperationInput(t, first, key, &firstMutations)
	secondInput := newDurableGroupOperationInput(t, second, key, &secondMutations)

	var secondResult idempotentOperationResult
	peerRan := false
	first.service.beforeAdvanceOperationStage = func(
		_ context.Context, _ *models.ControlOperation, stage operationStage,
	) error {
		if stage != operationStagePricesPublished || peerRan {
			return nil
		}
		peerRan = true
		// 第一个实例刚提交、尚未推进任何阶段；第二个实例以同键重试，找到
		// db_committed 行后在自己的运行时执行全部剩余阶段并完成该操作。
		result, err := second.service.executeIdempotentOperation(t.Context(), secondInput)
		if err != nil {
			t.Fatalf("peer executeIdempotentOperation() error = %v", err)
		}
		secondResult = result
		return nil
	}

	firstResult, err := first.service.executeIdempotentOperation(t.Context(), firstInput)
	if err != nil {
		t.Fatalf("first instance executeIdempotentOperation() error = %v", err)
	}
	if !peerRan {
		t.Fatal("peer replay did not run")
	}
	if firstResult.Replayed || !secondResult.Replayed {
		t.Fatalf("replayed = first:%t second:%t", firstResult.Replayed, secondResult.Replayed)
	}
	if firstResult.OperationID != secondResult.OperationID ||
		string(firstResult.CanonicalResult) != string(secondResult.CanonicalResult) {
		t.Fatalf("results differ: first %+v second %+v", firstResult, secondResult)
	}
	if firstMutations != 1 || secondMutations != 0 {
		t.Fatalf("mutations = first:%d second:%d, want 1/0", firstMutations, secondMutations)
	}
	row := mustOperationRow(t, first, key)
	if row.LastCompletedStage != string(operationStageCompleted) ||
		row.CompletedAtMS == nil || row.FailedStage != "" {
		t.Fatalf("operation row = %+v, want completed without a failed stage", row)
	}
	groupID := mustResourceGroupID(t, firstResult.ResourceIdentity)
	for name, fixture := range map[string]serviceFixture{"first": first, "second": second} {
		if len(fixture.registry.CaptureActiveCredentialRefs([]uint{groupID})) != 1 {
			t.Fatalf("%s instance registry does not hold group %d", name, groupID)
		}
	}
}

func TestOperationBarrierAdoptsPeerProgressForUnrelatedOperation(t *testing.T) {
	t.Parallel()
	first, second := newPeerFixtures(t)
	const firstKey = "44a1f47a-9c35-4d6e-8b1a-1234567890ab"
	const secondKey = "55a1f47a-9c35-4d6e-8b1a-1234567890ab"
	firstMutations, secondMutations := 0, 0
	firstInput := newDurableGroupOperationInput(t, first, firstKey, &firstMutations)
	secondInput := newDurableGroupOperationInput(t, second, secondKey, &secondMutations)

	peerRan := false
	first.service.beforeAdvanceOperationStage = func(
		_ context.Context, _ *models.ControlOperation, stage operationStage,
	) error {
		if stage != operationStagePricesPublished || peerRan {
			return nil
		}
		peerRan = true
		// 第二个实例的写前屏障会看到第一个实例尚未完成的操作并替它推进到完成。
		if _, err := second.service.executeIdempotentOperation(t.Context(), secondInput); err != nil {
			t.Fatalf("peer executeIdempotentOperation() error = %v", err)
		}
		return nil
	}

	if _, err := first.service.executeIdempotentOperation(t.Context(), firstInput); err != nil {
		t.Fatalf("first instance executeIdempotentOperation() error = %v", err)
	}
	if !peerRan || firstMutations != 1 || secondMutations != 1 {
		t.Fatalf("peerRan=%t mutations = first:%d second:%d", peerRan, firstMutations, secondMutations)
	}
	for _, key := range []string{firstKey, secondKey} {
		row := mustOperationRow(t, first, key)
		if row.LastCompletedStage != string(operationStageCompleted) ||
			row.CompletedAtMS == nil || row.FailedStage != "" {
			t.Fatalf("operation %s row = %+v, want completed without a failed stage", key, row)
		}
	}
}

func TestOperationStageAdvanceStillFailsWhenDurableStageMovedBackwards(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	const key = "66a1f47a-9c35-4d6e-8b1a-1234567890ab"
	mutations := 0
	input := newDurableGroupOperationInput(t, fixture, key, &mutations)
	fixture.service.beforeAdvanceOperationStage = func(
		_ context.Context, _ *models.ControlOperation, stage operationStage,
	) error {
		if stage != operationStageRegistryApplied {
			return nil
		}
		return fixture.db.Model(&models.ControlOperation{}).
			Where("idempotency_key = ?", key).
			Update("last_completed_stage", string(operationStageDBCommitted)).Error
	}

	_, err := fixture.service.executeIdempotentOperation(t.Context(), input)
	assertAPIErrorCode(t, err, app_errors.ErrControlOperationIncomplete.Code)
	row := mustOperationRow(t, fixture, key)
	if row.LastCompletedStage != string(operationStageDBCommitted) ||
		row.FailedStage != string(operationStageRegistryApplied) {
		t.Fatalf("operation row = %+v, want backwards move rejected and recorded", row)
	}
}
