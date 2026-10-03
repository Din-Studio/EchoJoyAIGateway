package requestlog

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/redact"
	"gpt-load/internal/pricing"
	"gpt-load/internal/protocol"
	"gpt-load/internal/storage"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/telemetry"
	"gpt-load/internal/testutil/pgtest"
	"gpt-load/internal/usage"
)

type staticRetentionPolicy struct{ days int }

func (policy staticRetentionPolicy) RequestLogRetentionDays() int {
	return policy.days
}

func newRequestLogTestService(db *gorm.DB) *Service {
	return NewService(
		db,
		redact.New(),
		staticRetentionPolicy{days: 7},
	)
}

type batchWriterFunc func(context.Context, []models.RequestLog) error

func (fn batchWriterFunc) WriteBatch(ctx context.Context, rows []models.RequestLog) error {
	return fn(ctx, rows)
}

type manualTimer struct {
	ch   chan time.Time
	once sync.Once
}

func newManualTimer() *manualTimer {
	return &manualTimer{ch: make(chan time.Time, 1)}
}

func (timer *manualTimer) C() <-chan time.Time {
	return timer.ch
}

func (timer *manualTimer) Stop() bool {
	stopped := false
	timer.once.Do(func() {
		stopped = true
	})
	return stopped
}

func (timer *manualTimer) Fire() {
	timer.ch <- time.Unix(1, 0)
}

type manualTimerFactory struct {
	created chan *manualTimer
}

func newManualTimerFactory() *manualTimerFactory {
	return &manualTimerFactory{created: make(chan *manualTimer, 32)}
}

func (factory *manualTimerFactory) New(time.Duration) workerTimer {
	timer := newManualTimer()
	factory.created <- timer
	return timer
}

func receiveValue[T any](t *testing.T, ch <-chan T) T {
	t.Helper()

	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test coordination")
		var zero T
		return zero
	}
}

func waitGroupDone(t *testing.T, group *sync.WaitGroup) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	receiveValue(t, done)
}

// openRequestLogQueryDB returns an isolated, migrated PostgreSQL database.
func openRequestLogQueryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, _ := openRequestLogDBWithDSN(t)
	return db
}

// openRequestLogDBWithDSN returns an isolated, migrated PostgreSQL database and
// its DSN, so a test can open a second connection to the same data.
func openRequestLogDBWithDSN(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	dsn := pgtest.NewDatabase(t)
	return openRequestLogDSN(t, dsn), dsn
}

func openRequestLogDSN(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close request log database: %v", err)
		}
	})
	return db
}

func aggregationRow(
	id string,
	completedAt time.Time,
	groupID uint,
	model string,
) models.RequestLog {
	return models.RequestLog{
		ID:                   id,
		CompletedAtMS:        completedAt.UTC().UnixMilli(),
		AccessKeyID:          1,
		GroupID:              groupID,
		Protocol:             string(protocol.OpenAICompletions),
		ClientModel:          model,
		UpstreamModel:        model,
		Status:               string(telemetry.RequestStatusSuccess),
		StatusCode:           200,
		DurationMs:           10,
		AttemptCount:         1,
		UncachedInputTokens:  1,
		OutputTokens:         2,
		EstimatedCostNanoUSD: 250_000_000,
		UsageState:           string(usage.StateComplete),
		CostState:            string(pricing.CostStatePriced),
		PricingCompleteness:  string(pricing.CompletenessComplete),
	}
}

func aggregationRequestID(index int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", 900000+index)
}

func testEvent(id string) telemetry.RequestEvent {
	return telemetry.RequestEvent{
		RequestID:             id,
		CompletedAt:           time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC),
		AccessKeyID:           42,
		ClientModel:           "client-model",
		UpstreamModel:         "upstream-model",
		UpstreamReportedModel: "upstream-model",
		ModelConsistency:      telemetry.ModelConsistencyMatch,
		Status:                telemetry.RequestStatusSuccess,
		StatusCode:            200,
		DurationMs:            25,
		Attempts: []telemetry.Attempt{{
			Sequence:        1,
			GroupID:         7,
			GroupName:       "primary",
			ChannelID:       channel.OpenAI,
			CredentialID:    8,
			Operation:       execution.OperationChatCompletion,
			RouteMode:       channel.RouteNative,
			UpstreamModel:   "upstream-model",
			DispatchState:   execution.DispatchMaybeSent,
			StatusCode:      200,
			DurationMs:      20,
			FailureCategory: telemetry.FailureCategoryOK,
			Action:          telemetry.ActionTerminate,
		}},
		Usage: telemetry.UsageObservation{
			GroupID:         7,
			ChannelID:       channel.OpenAI,
			CredentialID:    8,
			AttemptSequence: 1,
			Result: usage.Result{
				State: usage.StateNotApplicable,
			},
			Pricing: telemetry.PricingObservation{
				UpstreamModel:       "upstream-model",
				CostState:           string(pricing.CostStateNotApplicable),
				PricingCompleteness: string(pricing.CompletenessNotApplicable),
			},
		},
	}
}

func mustMapEvent(
	t testing.TB,
	redactor *redact.Redactor,
	event telemetry.RequestEvent,
	_ ...*pricing.Table,
) models.RequestLog {
	t.Helper()
	row, err := mapEvent(redactor, event)
	if err != nil {
		t.Fatalf("mapEvent() error = %v", err)
	}
	return row
}
