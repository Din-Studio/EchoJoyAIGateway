package ly

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpt-load/internal/platform/config"
	"gpt-load/internal/storage"
	"gpt-load/internal/telemetry"
	"gpt-load/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	pgtest.Run(m, storage.AutoMigrate)
}

func openOutboxDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := storage.Open(pgtest.NewDatabase(t))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// fakeLingying is an idempotent POST /internal/aigw/usage/batch: a
// request_id is booked once, and later deliveries answer duplicate. script,
// when set, decides the status of every report before booking; omitResult
// leaves the report out of the response. batchStatus, when set, answers the
// whole request with that status instead.
type fakeLingying struct {
	mu          sync.Mutex
	deliveries  map[string][]string // request_id -> raw payloads received
	booked      map[string]int      // request_id -> times booked
	batches     []int               // reports per request received
	script      func(requestID string, delivery int) int
	batchStatus int
}

const omitResult = -1

func newFakeLingying(t *testing.T) (*fakeLingying, string) {
	t.Helper()
	fake := &fakeLingying{deliveries: map[string][]string{}, booked: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	return fake, server.URL
}

func (f *fakeLingying) serve(w http.ResponseWriter, r *http.Request) {
	var batch struct {
		Reports []json.RawMessage `json:"reports"`
	}
	if r.URL.Path != usageBatchPath || json.NewDecoder(r.Body).Decode(&batch) != nil || len(batch.Reports) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, len(batch.Reports))
	if f.batchStatus != 0 {
		w.WriteHeader(f.batchStatus)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
		return
	}
	results := []batchResult{}
	for _, raw := range batch.Reports {
		var report usageReport
		if json.Unmarshal(raw, &report) != nil || report.RequestID == "" {
			results = append(results, batchResult{Status: http.StatusBadRequest, Error: "invalid report"})
			continue
		}
		f.deliveries[report.RequestID] = append(f.deliveries[report.RequestID], string(raw))
		status := http.StatusOK
		if f.script != nil {
			status = f.script(report.RequestID, len(f.deliveries[report.RequestID]))
		}
		if status == omitResult {
			continue
		}
		if status == http.StatusOK && f.booked[report.RequestID] == 0 {
			f.booked[report.RequestID]++
		}
		result := batchResult{RequestID: report.RequestID, Status: status}
		if status != http.StatusOK {
			result.Error = "scripted failure"
		}
		results = append(results, result)
	}
	_ = json.NewEncoder(w).Encode(map[string][]batchResult{"results": results})
}

// clock is a settable time source shared by services standing in for replicas.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

func newReplica(db *sql.DB, baseURL string, clock *clock) *Service {
	service := New(config.LingyingBillingConfig{BaseURL: baseURL, Token: "token", DeliveryBatchSize: 100}, db)
	service.now = clock.Now
	return service
}

func recordRequest(service *Service, requestID string) {
	service.Record(&Admission{UserID: "u1", KeyID: "k1"}, telemetry.RequestEvent{
		RequestID: requestID, ClientModel: "gpt-5", Status: telemetry.RequestStatusSuccess,
		CompletedAt: time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC), DurationMs: 10,
	}, 12_345_000)
}

type outboxRow struct {
	attempts    int
	nextAttempt sql.NullInt64
	lastError   string
}

func readRow(t *testing.T, db *sql.DB, requestID string) (outboxRow, bool) {
	t.Helper()
	var row outboxRow
	err := db.QueryRow(`SELECT attempts, next_attempt_at_ms, last_error FROM ly_usage_reports WHERE request_id = $1`, requestID).
		Scan(&row.attempts, &row.nextAttempt, &row.lastError)
	if err == sql.ErrNoRows {
		return row, false
	}
	if err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	return row, true
}

func TestRecordStoresOneReportPerRequest(t *testing.T) {
	db := openOutboxDB(t)
	service := newReplica(db, "http://unused", &clock{now: time.UnixMilli(1_000_000)})

	recordRequest(service, "req-1")
	recordRequest(service, "req-1")
	service.Record(nil, telemetry.RequestEvent{RequestID: "req-unbilled"}, 1)

	var payload string
	var count int
	if err := db.QueryRow(`SELECT count(*), max(payload::text) FROM ly_usage_reports`).Scan(&count, &payload); err != nil {
		t.Fatal(err)
	}
	var report usageReport
	if err := json.Unmarshal([]byte(payload), &report); err != nil {
		t.Fatal(err)
	}
	if count != 1 || report.RequestID != "req-1" || report.UserID != "u1" || report.Cost != "0.012345" {
		t.Fatalf("outbox = %d rows, payload %s", count, payload)
	}
	if row, _ := readRow(t, db, "req-1"); row.attempts != 0 || row.nextAttempt.Int64 != 1_000_000 {
		t.Fatalf("new row = %+v, want due now with no attempts", row)
	}
}

// T12: a report survives failed deliveries and a replica restart, and is
// delivered with the same request_id and payload until Lingying accepts it.
func TestDeliveryRetriesTheSameReportUntilAccepted(t *testing.T) {
	db := openOutboxDB(t)
	fake, baseURL := newFakeLingying(t)
	fake.script = func(_ string, delivery int) int {
		if delivery <= 2 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}
	clock := &clock{now: time.UnixMilli(1_000_000)}
	first := newReplica(db, baseURL, clock)
	recordRequest(first, "req-1")

	if first.deliverBatch(context.Background()) != 1 {
		t.Fatal("first delivery found nothing due")
	}
	row, _ := readRow(t, db, "req-1")
	if row.attempts != 1 || row.nextAttempt.Int64 != 1_000_000+30_000+1_000 || row.lastError == "" {
		t.Fatalf("after a 503 the row = %+v, want retry at now+timeout+1s", row)
	}
	if first.deliverBatch(context.Background()) != 0 {
		t.Fatal("a report was delivered again before its retry time")
	}
	clock.Add(31 * time.Second)
	first.deliverBatch(context.Background())
	if row, _ := readRow(t, db, "req-1"); row.attempts != 2 || row.nextAttempt.Int64 != clock.Now().UnixMilli()+30_000+2_000 {
		t.Fatalf("after a second 503 the row = %+v, want a 2s backoff", row)
	}

	restarted := newReplica(db, baseURL, clock)
	clock.Add(32 * time.Second)
	if restarted.deliverBatch(context.Background()) != 1 {
		t.Fatal("restarted replica found nothing due")
	}
	if _, exists := readRow(t, db, "req-1"); exists {
		t.Fatal("accepted report is still in the outbox")
	}
	deliveries := fake.deliveries["req-1"]
	if len(deliveries) != 3 || deliveries[0] != deliveries[2] || fake.booked["req-1"] != 1 {
		t.Fatalf("Lingying received %d deliveries, booked %d", len(deliveries), fake.booked["req-1"])
	}
}

// T13: a report answered 400 or 422 is parked for good; other failures retry it.
func TestDeliveryClassifiesLingyingResponses(t *testing.T) {
	for _, test := range []struct {
		status int
		parked bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusInternalServerError, false},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			db := openOutboxDB(t)
			fake, baseURL := newFakeLingying(t)
			fake.script = func(string, int) int { return test.status }
			clock := &clock{now: time.UnixMilli(1_000_000)}
			service := newReplica(db, baseURL, clock)
			recordRequest(service, "req-1")

			service.deliverBatch(context.Background())
			clock.Add(time.Hour)
			retried := service.deliverBatch(context.Background()) > 0

			row, exists := readRow(t, db, "req-1")
			if !exists || row.nextAttempt.Valid == test.parked || retried == test.parked || row.lastError == "" {
				t.Fatalf("status %d: row %+v exists %t retried %t, want parked %t", test.status, row, exists, retried, test.parked)
			}
		})
	}
}

// T14: a report claimed by a replica that then dies is claimed again once its
// retry time passes, and not before.
func TestClaimedReportIsRetriedAfterAReplicaDies(t *testing.T) {
	db := openOutboxDB(t)
	clock := &clock{now: time.UnixMilli(1_000_000)}
	dead := newReplica(db, "http://unused", clock)
	recordRequest(dead, "req-1")
	if reports, err := dead.outbox.claim(context.Background(), clock.Now(), 100); err != nil || len(reports) != 1 {
		t.Fatalf("claim() = %+v, %v", reports, err)
	}

	survivor := newReplica(db, "http://unused", clock)
	clock.Add(30 * time.Second)
	if reports, _ := survivor.outbox.claim(context.Background(), clock.Now(), 100); len(reports) != 0 {
		t.Fatal("a report in flight was claimed again within the request timeout")
	}
	clock.Add(time.Second)
	if reports, _ := survivor.outbox.claim(context.Background(), clock.Now(), 100); len(reports) != 1 || reports[0].Attempts != 2 {
		t.Fatalf("claim() after the retry time = %+v", reports)
	}
}

// T15: replicas delivering concurrently book every report exactly once.
func TestConcurrentReplicasBookEveryReportOnce(t *testing.T) {
	db := openOutboxDB(t)
	fake, baseURL := newFakeLingying(t)
	clock := &clock{now: time.UnixMilli(1_000_000)}
	const reports = 100
	for index := range reports {
		recordRequest(newReplica(db, baseURL, clock), fmt.Sprintf("req-%03d", index))
	}

	var replicas sync.WaitGroup
	for range 4 {
		replica := newReplica(db, baseURL, clock)
		replica.deliveryBatchSize = 8
		replicas.Go(func() {
			for replica.deliverBatch(context.Background()) > 0 {
			}
		})
	}
	replicas.Wait()

	var left int
	if err := db.QueryRow(`SELECT count(*) FROM ly_usage_reports`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	for _, payloads := range fake.deliveries {
		deliveries += len(payloads)
	}
	t.Logf("%d reports took %d deliveries", reports, deliveries)
	if left != 0 || len(fake.booked) != reports {
		t.Fatalf("outbox left %d rows, Lingying booked %d of %d reports", left, len(fake.booked), reports)
	}
	for requestID, times := range fake.booked {
		if times != 1 {
			t.Fatalf("%s booked %d times", requestID, times)
		}
	}
}

func countOutbox(t *testing.T, db *sql.DB) int {
	t.Helper()
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM ly_usage_reports`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	return left
}

// A batch of due reports goes to Lingying in one request.
func TestDeliveryPostsDueReportsInOneRequest(t *testing.T) {
	db := openOutboxDB(t)
	fake, baseURL := newFakeLingying(t)
	service := newReplica(db, baseURL, &clock{now: time.UnixMilli(1_000_000)})
	for index := range 32 {
		recordRequest(service, fmt.Sprintf("req-%03d", index))
	}

	delivered := service.deliverBatch(context.Background())

	if delivered != 32 || len(fake.batches) != 1 || fake.batches[0] != 32 || len(fake.booked) != 32 {
		t.Fatalf("deliverBatch() = %d, Lingying received batches %v and booked %d", delivered, fake.batches, len(fake.booked))
	}
	if left := countOutbox(t, db); left != 0 {
		t.Fatalf("outbox left %d rows", left)
	}
}

// Each report of a batch is handled by its own result.
func TestDeliveryHandlesEachResultOfABatch(t *testing.T) {
	db := openOutboxDB(t)
	fake, baseURL := newFakeLingying(t)
	statuses := map[string]int{
		"req-ok": http.StatusOK, "req-invalid": http.StatusBadRequest,
		"req-retry": http.StatusServiceUnavailable, "req-missing": omitResult,
	}
	fake.script = func(requestID string, _ int) int { return statuses[requestID] }
	service := newReplica(db, baseURL, &clock{now: time.UnixMilli(1_000_000)})
	for requestID := range statuses {
		recordRequest(service, requestID)
	}

	if delivered := service.deliverBatch(context.Background()); delivered != 4 || len(fake.batches) != 1 {
		t.Fatalf("deliverBatch() = %d in %d requests, want 4 in one", delivered, len(fake.batches))
	}

	if _, exists := readRow(t, db, "req-ok"); exists {
		t.Fatal("accepted report is still in the outbox")
	}
	if row, _ := readRow(t, db, "req-invalid"); row.nextAttempt.Valid || row.lastError == "" {
		t.Fatalf("rejected report = %+v, want parked", row)
	}
	for _, requestID := range []string{"req-retry", "req-missing"} {
		row, _ := readRow(t, db, requestID)
		if row.attempts != 1 || row.nextAttempt.Int64 != 1_000_000+30_000+1_000 || row.lastError == "" {
			t.Fatalf("%s = %+v, want a retry at the time claim set", requestID, row)
		}
	}
}

// A batch Lingying does not answer with 200 is retried as a whole.
func TestDeliveryRetriesAFailedBatch(t *testing.T) {
	db := openOutboxDB(t)
	fake, baseURL := newFakeLingying(t)
	fake.batchStatus = http.StatusServiceUnavailable
	service := newReplica(db, baseURL, &clock{now: time.UnixMilli(1_000_000)})
	for _, requestID := range []string{"req-1", "req-2"} {
		recordRequest(service, requestID)
	}

	service.deliverBatch(context.Background())

	for _, requestID := range []string{"req-1", "req-2"} {
		row, _ := readRow(t, db, requestID)
		if row.attempts != 1 || !row.nextAttempt.Valid || !strings.Contains(row.lastError, "HTTP 503") {
			t.Fatalf("%s = %+v, want a retry recording HTTP 503", requestID, row)
		}
	}
}

// One delivery carries at most deliveryBatchSize reports.
func TestDeliveryClaimsAtMostTheBatchSize(t *testing.T) {
	db := openOutboxDB(t)
	_, baseURL := newFakeLingying(t)
	service := newReplica(db, baseURL, &clock{now: time.UnixMilli(1_000_000)})
	for index := range 150 {
		recordRequest(service, fmt.Sprintf("req-%03d", index))
	}

	var delivered []int
	for range 3 {
		delivered = append(delivered, service.deliverBatch(context.Background()))
	}

	if fmt.Sprint(delivered) != "[100 50 0]" {
		t.Fatalf("deliverBatch() returned %v, want [100 50 0]", delivered)
	}
}

// Run keeps delivering batches until nothing is due, without waiting a poll
// interval between them.
func TestRunDeliversEveryDueBatchAtOnce(t *testing.T) {
	db := openOutboxDB(t)
	fake, baseURL := newFakeLingying(t)
	service := newReplica(db, baseURL, &clock{now: time.UnixMilli(1_000_000)})
	for index := range 250 {
		recordRequest(service, fmt.Sprintf("req-%03d", index))
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { service.Run(ctx); close(stopped) }()
	defer func() { cancel(); <-stopped }()

	deadline := time.Now().Add(pollInterval / 2)
	for countOutbox(t, db) != 0 {
		if time.Now().After(deadline) {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			t.Fatalf("outbox not drained within half a poll interval; batches %v", fake.batches)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
