package requestlog

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/platform/config"
	"gpt-load/internal/storage"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/telemetry"
)

// TestExternalDatabaseConcurrentUsageAggregation proves that two request-log
// writers on a shared MySQL or PostgreSQL never lose each other's increments:
// each writer folds 1000 requests into the same hourly bucket concurrently and
// the bucket ends up with the exact sum.
func TestExternalDatabaseConcurrentUsageAggregation(t *testing.T) {
	// 不标记 t.Parallel()：依赖 GPT_LOAD_DATABASE_TEST_DSN 的共享外部数据库。
	dsn := strings.TrimSpace(os.Getenv("GPT_LOAD_DATABASE_TEST_DSN"))
	if dsn == "" {
		t.Skip("GPT_LOAD_DATABASE_TEST_DSN is not set")
	}
	const writers = 2
	const rowsPerWriter = 1000

	openWriter := func() *gorm.DB {
		db, err := storage.OpenWithSource(dsn, config.DatabaseSourceExternal)
		if err != nil {
			t.Fatalf("OpenWithSource() error = %v", err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatalf("DB() error = %v", err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
		return db
	}
	control := openWriter()
	if err := storage.AutoMigrate(control); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}

	unique := uint64(time.Now().UnixNano()) & 0xffffffff
	groupID := uint(unique%1_000_000) + 1
	credentialID := groupID + 1_000_000
	modelID := fmt.Sprintf("external-concurrent-%08x", unique)
	hour := time.Date(2026, time.September, 22, 9, 0, 0, 0, time.UTC)

	requestIDs := make([]string, 0, writers*rowsPerWriter)
	batches := make([][]models.RequestLog, writers)
	for writer := range batches {
		for index := 0; index < rowsPerWriter; index++ {
			requestID := fmt.Sprintf("%08x-%04x-4000-8000-%012x", unique, writer, index)
			row := aggregationRow(requestID, hour.Add(time.Duration(index)*time.Second), groupID, modelID)
			row.ChannelID = "openai"
			row.CredentialID = credentialID
			row.AttemptRows = []models.RequestLogAttempt{{
				RequestID: row.ID, Sequence: 1, CompletedAtMS: row.CompletedAtMS,
				GroupID: groupID, GroupName: "external-concurrent", ChannelID: row.ChannelID,
				CredentialID: credentialID, StatusCode: 200,
				FailureCategory: string(telemetry.FailureCategoryOK),
				Action:          string(telemetry.ActionTerminate),
			}}
			batches[writer] = append(batches[writer], row)
			requestIDs = append(requestIDs, requestID)
		}
	}
	t.Cleanup(func() {
		for _, operation := range []struct {
			model any
			query string
			value any
		}{
			{&models.UsageAggregationJournal{}, "request_id IN ?", requestIDs},
			{&models.RequestLogAttempt{}, "request_id IN ?", requestIDs},
			{&models.RequestLog{}, "id IN ?", requestIDs},
			{&models.UsageStat{}, "credential_id = ?", credentialID},
			{&models.CredentialAttemptStat{}, "credential_id = ?", credentialID},
		} {
			if err := control.Where(operation.query, operation.value).Delete(operation.model).Error; err != nil {
				t.Errorf("cleanup concurrent usage fixtures: %v", err)
			}
		}
	})

	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		db := openWriter()
		rows := batches[writer]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			batchWriter := &gormBatchWriter{db: db}
			for offset := 0; offset < len(rows); offset += batchSize {
				end := min(offset+batchSize, len(rows))
				if err := batchWriter.WriteBatch(context.Background(), rows[offset:end]); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent WriteBatch() error = %v", err)
	}

	template := batches[0][0]
	const total = writers * rowsPerWriter
	var stats []models.UsageStat
	if err := control.Where("credential_id = ?", credentialID).Find(&stats).Error; err != nil {
		t.Fatalf("query usage stats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("usage stat rows = %+v, want one hourly bucket", stats)
	}
	stat := stats[0]
	if stat.BucketStartMS != hour.UnixMilli() || stat.RequestCount != total ||
		stat.SuccessCount != total || stat.FailureCount != 0 ||
		stat.UncachedInputTokens != total*template.UncachedInputTokens ||
		stat.OutputTokens != total*template.OutputTokens ||
		stat.EstimatedCostNanoUSD != total*template.EstimatedCostNanoUSD {
		t.Fatalf("usage stat = %+v, want exact sums of %d requests", stat, total)
	}
	var attemptStats []models.CredentialAttemptStat
	if err := control.Where("credential_id = ?", credentialID).Find(&attemptStats).Error; err != nil {
		t.Fatalf("query credential attempt stats: %v", err)
	}
	if len(attemptStats) != 1 || attemptStats[0].SuccessCount != total || attemptStats[0].FailureCount != 0 {
		t.Fatalf("credential attempt stats = %+v, want %d successes in one bucket", attemptStats, total)
	}
	var applied int64
	if err := control.Model(&models.UsageAggregationJournal{}).
		Where("request_id IN ? AND applied = ?", requestIDs, true).
		Count(&applied).Error; err != nil {
		t.Fatalf("count applied journals: %v", err)
	}
	if applied != total {
		t.Fatalf("applied journals = %d, want %d", applied, total)
	}
}
