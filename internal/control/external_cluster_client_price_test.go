package control

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"gpt-load/internal/pricing"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/usage"
)

func clientModelPriceRequest(clientModel string, input int64) ClientModelPriceRequest {
	return ClientModelPriceRequest{ClientModel: clientModel, ModelPriceUpdateRequest: ModelPriceUpdateRequest{
		Input:         nullableDecimal{present: true, nanoUSD: &input},
		Output:        nullableDecimal{present: true},
		CacheRead:     nullableDecimal{present: true},
		CacheWrite:    nullableDecimal{present: true},
		ContextTiers:  modelPriceTierListField{present: true},
		ModeSchedules: modelPriceModeScheduleMapField{present: true},
	}}
}

// T5：在实例 A 写入统一价后，实例 B 经集群配置同步重新加载，按新价格收费。
func TestExternalClusterClientModelPricePropagation(t *testing.T) {
	// 不标记 t.Parallel()：依赖 GPT_LOAD_DATABASE_TEST_DSN 的共享外部数据库。
	dsn, redisAddr := externalClusterTarget(t)
	keyPrefix := fmt.Sprintf("gltest-%d-%d", os.Getpid(), time.Now().UnixNano())
	instanceA := newClusterContractInstance(t, dsn, redisAddr, keyPrefix, "node-a")
	instanceB := newClusterContractInstance(t, dsn, redisAddr, keyPrefix, "node-b")
	clientModel := "cluster-client-" + keyPrefix
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := instanceA.fixture.db.WithContext(ctx).Where("client_model = ?", clientModel).
			Delete(&models.ClientModelPrice{}).Error; err != nil {
			t.Errorf("cleanup client model price: %v", err)
		}
		if err := instanceA.fixture.db.WithContext(ctx).Where("key = ?", clusterConfigRevisionKey).
			Delete(&models.SystemSetting{}).Error; err != nil {
			t.Errorf("cleanup cluster revision row: %v", err)
		}
	})

	syncB := NewClusterConfigSync(instanceB.fixture.service, instanceB.bus)
	syncB.pollInterval = time.Second
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncB.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	for _, input := range []int64{3_000_000_000, 5_000_000_000} {
		if _, err := instanceA.fixture.service.PutClientModelPrice(t.Context(), clientModelPriceRequest(clientModel, input)); err != nil {
			t.Fatalf("PutClientModelPrice(%d) error = %v", input, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			table := instanceB.fixture.priceRuntime.Load()
			quote, _ := table.QuoteClientModel(clientModel, usage.Result{
				State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1_000_000},
			}, pricing.ModeStandard, pricing.DefaultPriceMultiplier)
			if quote.EstimatedCostNanoUSD == pricing.NanoUSD(input) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("instance B quote = %+v, want client price %d", quote, input)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
