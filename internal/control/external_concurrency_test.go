package control

import (
	"crypto/rand"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/channel"
	"gpt-load/internal/storage/models"
)

// TestExternalDatabaseConcurrentIdempotentOperation proves the cross-instance
// idempotency contract on a shared MySQL or PostgreSQL: two Service instances
// submitting the same Idempotency-Key at the same time both receive the same
// committed result, create one resource, and leave one completed operation row.
func TestExternalDatabaseConcurrentIdempotentOperation(t *testing.T) {
	// 不标记 t.Parallel()：依赖 GPT_LOAD_DATABASE_TEST_DSN 的共享外部数据库，并发执行有唯一索引冲突等正确性风险。
	dsn := strings.TrimSpace(os.Getenv("GPT_LOAD_DATABASE_TEST_DSN"))
	if dsn == "" {
		t.Skip("GPT_LOAD_DATABASE_TEST_DSN is not set")
	}
	instances := []serviceFixture{newServiceFixtureWithDSN(t, dsn), newServiceFixtureWithDSN(t, dsn)}
	control := instances[0].db

	const rounds = 20
	var keys []string
	var groupNames []string
	t.Cleanup(func() {
		var groupIDs []uint
		if err := control.Model(&models.Group{}).Where("name IN ?", groupNames).Pluck("id", &groupIDs).Error; err != nil {
			t.Errorf("cleanup: list groups: %v", err)
		}
		for _, operation := range []struct {
			model any
			query string
			value any
		}{
			{&models.Credential{}, "group_id IN ?", groupIDs},
			{&models.Group{}, "id IN ?", groupIDs},
			{&models.ControlOperation{}, "idempotency_key IN ?", keys},
		} {
			if err := control.Where(operation.query, operation.value).Delete(operation.model).Error; err != nil {
				t.Errorf("cleanup concurrent idempotency fixtures: %v", err)
			}
		}
	})

	for round := 0; round < rounds; round++ {
		key, err := newOperationID(rand.Reader)
		if err != nil {
			t.Fatalf("generate idempotency key: %v", err)
		}
		keys = append(keys, key)
		groupName := fmt.Sprintf("external-concurrent-%s", key)
		groupNames = append(groupNames, groupName)

		results := make([]idempotentOperationResult, len(instances))
		errs := make([]error, len(instances))
		mutations := make([]int, len(instances))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for index, instance := range instances {
			input := idempotentOperationInput{
				IdempotencyKey: key,
				DigestVersion:  1,
				RequestDigest:  [32]byte{byte(round)},
				Kind:           operationKindGroupCreate,
				Mutate: func(tx *gorm.DB) (idempotentMutationResult, error) {
					mutations[index]++
					group := models.Group{
						Name: groupName, ChannelID: string(channel.OpenAI),
						Params: models.JSON(`{}`), Models: models.JSON(`[]`), Overrides: models.JSON(`{}`),
						Enabled: true,
					}
					if err := tx.Create(&group).Error; err != nil {
						return idempotentMutationResult{}, err
					}
					credential := models.Credential{
						GroupID: group.ID, Data: "cipher-one", Fingerprint: "fingerprint-" + key,
						Status: models.CredentialStatusActive, UpdatedAtMS: 1,
					}
					if err := tx.Create(&credential).Error; err != nil {
						return idempotentMutationResult{}, err
					}
					return idempotentMutationResult{
						ResourceIdentity: "group:" + strconv.FormatUint(uint64(group.ID), 10),
						CanonicalResult:  []byte(fmt.Sprintf(`{"group_id":%d}`, group.ID)),
					}, nil
				},
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[index], errs[index] = instance.service.executeIdempotentOperation(t.Context(), input)
			}()
		}
		close(start)
		wg.Wait()

		for index, err := range errs {
			if err != nil {
				t.Fatalf("round %d instance %d executeIdempotentOperation() error = %v", round, index, err)
			}
		}
		if results[0].OperationID != results[1].OperationID ||
			string(results[0].CanonicalResult) != string(results[1].CanonicalResult) {
			t.Fatalf("round %d results differ: %+v vs %+v", round, results[0], results[1])
		}
		if results[0].Replayed == results[1].Replayed {
			t.Fatalf("round %d replayed flags = %t/%t, want exactly one committer", round, results[0].Replayed, results[1].Replayed)
		}
		if mutations[0]+mutations[1] < 1 || mutations[0] > 1 || mutations[1] > 1 {
			t.Fatalf("round %d mutations = %v, want at most one attempt per instance", round, mutations)
		}
		var rows []models.ControlOperation
		if err := control.Where("idempotency_key = ?", key).Find(&rows).Error; err != nil {
			t.Fatalf("round %d read operations: %v", round, err)
		}
		if len(rows) != 1 || rows[0].LastCompletedStage != string(operationStageCompleted) ||
			rows[0].CompletedAtMS == nil || rows[0].FailedStage != "" {
			t.Fatalf("round %d operation rows = %+v, want one completed row", round, rows)
		}
		var groups int64
		if err := control.Model(&models.Group{}).Where("name = ?", groupName).Count(&groups).Error; err != nil {
			t.Fatalf("round %d count groups: %v", round, err)
		}
		if groups != 1 {
			t.Fatalf("round %d groups = %d, want exactly one resource", round, groups)
		}
	}
}
