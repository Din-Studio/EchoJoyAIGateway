package requestlog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"gpt-load/internal/platform/redact"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/testutil/pgtest"
)

func TestRequestBodyBudgetConcurrentEmit(t *testing.T) {
	for _, runningWorker := range []bool{false, true} {
		service := newService(batchWriterFunc(func(context.Context, []models.RequestLog) error { return nil }), nil, func(delay time.Duration) workerTimer { return &realWorkerTimer{timer: time.NewTimer(delay)} })
		service.logger.SetOutput(io.Discard)
		service.requestBodyBudget = 16 << 10
		if runningWorker {
			if err := service.Start(); err != nil {
				t.Fatal(err)
			}
		} else {
			service.state = lifecycleRunning
		}
		const producers = 128
		raw := bytes.Repeat([]byte(" "), 1024)
		copy(raw, `{"input":"read-only"}`)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for range producers {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				for range 16 {
					event := testEvent("00000000-0000-4000-8000-000000000999")
					event.RequestBody = raw
					service.Emit(event)
					if pending := service.pendingRequestBodyBytes.Load(); pending < 0 || pending > service.requestBodyBudget {
						t.Errorf("invalid pending: %d", pending)
					}
				}
			}()
		}
		close(start)
		workers.Wait()
		if runningWorker {
			if err := service.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		} else {
			var reserved, skipped int64
			for len(service.queue) > 0 {
				event := <-service.queue
				reserved += event.requestBodyReserved
				skipped += event.requestBodySkipped
				if event.requestBodyReserved > 0 && !bytes.Equal(event.Event.RequestBody, raw) {
					t.Fatal("body changed")
				}
				service.releaseRequestBodies([]queuedEvent{event})
			}
			if reserved != service.requestBodyBudget || reserved+skipped != producers*16*int64(len(raw)) {
				t.Fatalf("reserved=%d skipped=%d", reserved, skipped)
			}
		}
		if pending := service.pendingRequestBodyBytes.Load(); pending != 0 {
			t.Fatalf("pending=%d", pending)
		}
	}
}

func TestRequestBodyBudgetReleasedOnFailureAndDrop(t *testing.T) {
	for _, scenario := range []string{"projection", "write", "queue_full", "shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			writes := 0
			service := newService(batchWriterFunc(func(context.Context, []models.RequestLog) error {
				writes++
				return errors.New("forced write failure")
			}), redact.New(), nil)
			service.state = lifecycleRunning // No worker: exercise each release path deterministically.
			event := testEvent("00000000-0000-4000-8000-000000000801")
			event.RequestBody = []byte(`{"a":1}`)
			if scenario == "projection" {
				event.ClientIP = "invalid"
			}
			if scenario == "queue_full" {
				service.queue = make(chan queuedEvent)
			}
			service.Emit(event)
			switch scenario {
			case "projection", "write":
				service.writeBatch(t.Context(), []queuedEvent{<-service.queue})
			case "shutdown":
				service.dropUnattempted(nil)
			}
			if pending := service.pendingRequestBodyBytes.Load(); pending != 0 {
				t.Fatalf("pending after %s = %d", scenario, pending)
			}
			wantWrites := 0
			if scenario == "write" {
				wantWrites = 1
			}
			if writes != wantWrites {
				t.Fatalf("writer calls = %d, want %d", writes, wantWrites)
			}
		})
	}
}

func TestRequestBodyDuplicateBatchesAndNoAttempts(t *testing.T) {
	db := openRequestLogQueryDB(t)
	row := aggregationRow("00000000-0000-4000-8000-000000000802", time.Now(), 12, "body-model")
	body := `{"input":"original"}`
	row.RequestBodyRow = &models.RequestLogBody{RequestID: row.ID, State: "original", OriginalBytes: int64(len(body)), Body: &body}
	withoutAttempts := row
	withoutAttempts.ID = "00000000-0000-4000-8000-000000000803"
	withoutAttempts.AttemptCount = 0
	withoutAttempts.AttemptRows = nil
	withoutAttempts.RequestBodyRow = &models.RequestLogBody{RequestID: withoutAttempts.ID, State: "queue_full", OriginalBytes: 1024}
	writer := &gormBatchWriter{db: db}
	rows := []models.RequestLog{row, row, withoutAttempts, withoutAttempts}
	for range 2 {
		if err := writer.WriteBatch(t.Context(), rows); err != nil {
			t.Fatal(err)
		}
	}
	var bodies int64
	if err := db.Model(&models.RequestLogBody{}).Count(&bodies).Error; err != nil || bodies != 2 {
		t.Fatalf("body count = %d, error = %v", bodies, err)
	}
	var stat models.UsageStat
	if err := db.Take(&stat).Error; err != nil || stat.RequestCount != 1 || stat.EstimatedCostNanoUSD != row.EstimatedCostNanoUSD || stat.EstimatedCostNano != row.EstimatedCostNano {
		t.Fatalf("duplicate batch changed usage: %+v, error = %v", stat, err)
	}
}

func TestRequestBodyWriteIsAtomicWithLogAndUsage(t *testing.T) {
	for _, table := range []string{"request_log_bodies", "usage_stats"} {
		t.Run(table, func(t *testing.T) {
			db := openRequestLogQueryDB(t)
			pgtest.FailOn(t, db, table, "INSERT", "", "forced body transaction failure")
			row := aggregationRow("00000000-0000-4000-8000-000000000804", time.Now(), 12, "body-model")
			row.RequestBodyRow = &models.RequestLogBody{RequestID: row.ID, State: "non_json", OriginalBytes: 1}
			if err := (&gormBatchWriter{db: db}).WriteBatch(t.Context(), []models.RequestLog{row}); err == nil {
				t.Fatal("write unexpectedly succeeded")
			}
			for _, name := range []string{"request_logs", "request_log_bodies", "usage_stats", "request_log_attempts"} {
				var count int64
				if err := db.Table(name).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("%s count after rollback = %d, error = %v", name, count, err)
				}
			}
		})
	}
}

func TestRequestBodyRetentionKeepsCutoff(t *testing.T) {
	db := openRequestLogQueryDB(t)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-7 * 24 * time.Hour)
	for index, completed := range []time.Time{cutoff.Add(-time.Millisecond), cutoff, cutoff.Add(time.Millisecond)} {
		row := retentionRow(index+1, completed)
		row.RequestBodyRow = &models.RequestLogBody{RequestID: row.ID, State: "too_large", OriginalBytes: 1 << 20}
		if err := (&gormBatchWriter{db: db}).WriteBatch(t.Context(), []models.RequestLog{row}); err != nil {
			t.Fatal(err)
		}
	}
	newRequestLogTestService(db).Sweep(t.Context(), now)
	var ids []string
	if err := db.Model(&models.RequestLogBody{}).Order("request_id").Pluck("request_id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != retentionRow(2, cutoff).ID || ids[1] != retentionRow(3, cutoff).ID {
		t.Fatalf("retained body IDs = %v", ids)
	}
}
