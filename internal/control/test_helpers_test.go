package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/catalog"
	"gpt-load/internal/channel"
	"gpt-load/internal/gateway"
	"gpt-load/internal/health"
	"gpt-load/internal/platform/encryption"
	"gpt-load/internal/platform/httproute"
	"gpt-load/internal/platform/i18n"
	"gpt-load/internal/requestlog"
	"gpt-load/internal/state"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/subscription"
	subscriptionproviders "gpt-load/internal/subscription/providers"
	subscriptionruntime "gpt-load/internal/subscription/runtime"
	"gpt-load/internal/testutil/encryptiontest"
	"gpt-load/internal/testutil/pgtest"
)

var (
	controlI18nOnce         sync.Once
	controlI18nErr          error
	testIdempotencySequence atomic.Uint64
)

type blockingDecryptService struct {
	encryption.Service
	started chan<- struct{}
	release <-chan struct{}
}

func (service blockingDecryptService) Decrypt(ciphertext string) (string, error) {
	close(service.started)
	<-service.release
	return service.Service.Decrypt(ciphertext)
}

func loadCreatedGroupModels(t *testing.T, fixture serviceFixture, groupID uint) []GroupModel {
	t.Helper()
	var group models.Group
	if err := fixture.db.First(&group, groupID).Error; err != nil {
		t.Fatalf("query group %d: %v", groupID, err)
	}
	var result []GroupModel
	if err := json.Unmarshal(group.Models, &result); err != nil {
		t.Fatalf("decode group %d models: %v", groupID, err)
	}
	if result == nil {
		result = make([]GroupModel, 0)
	}
	return result
}

func createGroupWithCredentials(t *testing.T, fixture serviceFixture, credentials string) uint {
	t.Helper()
	name := fmt.Sprintf("credential-group-%d", testIdempotencySequence.Add(1))
	result, err := fixture.service.CreateGroup(t.Context(), GroupCreateRequest{
		Name: &name, ChannelID: channel.OpenAI, Params: json.RawMessage(`{}`),
		Models:      optionalGroupModels{Set: true, Values: []GroupModel{{ID: "gpt-4o"}}},
		Credentials: credentials, ConnectionType: "api_key",
	})
	if err != nil {
		t.Fatalf("CreateGroup(%q) error = %v", name, err)
	}
	return result.GroupID
}

// RegisterRoutes keeps package tests concise while production registration
// remains exclusively owned by the shared HTTP route registry.
func (s *Server) RegisterRoutes(engine *gin.Engine) {
	registry, err := httproute.NewRegistry(s.HTTPModule())
	if err != nil {
		panic(err)
	}
	if err := registry.Bind(engine); err != nil {
		panic(err)
	}
}

func registerGatewayRoutes(t *testing.T, engine *gin.Engine, handler *gateway.Handler) {
	t.Helper()
	registry, err := httproute.NewRegistry(handler.HTTPModule())
	if err != nil {
		t.Fatalf("NewRegistry(gateway) error = %v", err)
	}
	if err := registry.Bind(engine); err != nil {
		t.Fatalf("Bind(gateway) error = %v", err)
	}
}

func setRequiredTestIdempotencyHeader(request *http.Request) {
	sequence := testIdempotencySequence.Add(1)
	request.Header.Set(
		"Idempotency-Key",
		fmt.Sprintf("00000000-0000-4000-8000-%012x", sequence),
	)
}

type serviceFixture struct {
	db              *gorm.DB
	manager         *state.Manager
	registry        *state.CredentialRegistry
	channelRegistry *channel.Registry
	priceRuntime    *PriceRuntime
	catalogRuntime  *catalog.Runtime
	encryption      encryption.Service
	stats           *health.StatsStore
	mutations       *health.MutationCoordinator
	requestLogStats *staticRequestLogStatsReader
	accessQuota     *accessquota.Runtime
	service         *Service
}

type staticRequestLogStatsReader struct {
	value requestlog.Stats
	fn    func() requestlog.Stats
}

func mustBuildCompileInput(t *testing.T, db *gorm.DB) state.CompileInput {
	t.Helper()
	input, err := stateloader.BuildCompileInput(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func (reader *staticRequestLogStatsReader) Stats() requestlog.Stats {
	if reader.fn != nil {
		return reader.fn()
	}
	return reader.value
}

func initControlI18n(t *testing.T) {
	t.Helper()
	controlI18nOnce.Do(func() {
		gin.SetMode(gin.ReleaseMode)
		controlI18nErr = i18n.Init()
	})
	if controlI18nErr != nil {
		t.Fatalf("i18n.Init() error = %v", controlI18nErr)
	}
}

func newServiceFixture(t *testing.T) serviceFixture {
	t.Helper()
	return newServiceFixtureWithDSN(t, pgtest.NewDatabase(t))
}

func mustEnsureInitialPrices(t *testing.T, fixture serviceFixture) {
	t.Helper()
	if err := fixture.service.EnsureInitialState(t.Context()); err != nil {
		t.Fatalf("EnsureInitialState() error = %v", err)
	}
	if fixture.priceRuntime.Load() == nil {
		t.Fatal("EnsureInitialState() did not publish PriceTable")
	}
}

// newServiceFixtureWithSecondDSN returns a fixture and the DSN of its
// database, so a test can open a second connection to the same data.
func newServiceFixtureWithSecondDSN(t *testing.T) (serviceFixture, string) {
	t.Helper()
	dsn := pgtest.NewDatabase(t)
	return newServiceFixtureWithDSN(t, dsn), dsn
}

// newExternalServiceFixture migrates the shared external test database named
// by GPT_LOAD_DATABASE_TEST_DSN before opening a fixture on it.
func newExternalServiceFixture(t *testing.T, dsn string) serviceFixture {
	t.Helper()
	return newServiceFixtureWithDatabase(t, openControlTestDBWithDSN(t, dsn))
}

// newServiceFixtureWithDSN opens an already migrated database.
func newServiceFixtureWithDSN(t *testing.T, dsn string) serviceFixture {
	t.Helper()
	return newServiceFixtureWithDatabase(t, openControlTestDBWithoutMigration(t, dsn))
}

func newServiceFixtureWithDatabase(t *testing.T, db *gorm.DB) serviceFixture {
	t.Helper()
	manager := state.NewManager()
	accessQuota := accessquota.NewRuntime()
	manager.SetSnapshotReconciler(controlAccessQuotaReconciler{runtime: accessQuota})
	registry := state.NewCredentialRegistry()
	channelRegistry := channel.NewRegistry()
	keyService := encryptiontest.Service(t, "control-test-master-key-material-2026")
	if _, err := manager.Publish(state.CompileInput{}); err != nil {
		t.Fatalf("manager.Publish(empty) error = %v", err)
	}
	stats := health.NewStatsStore()
	mutations := health.NewMutationCoordinator()
	subscriptions, err := subscriptionruntime.NewRuntime(channelRegistry, subscriptionproviders.Implementations()...)
	if err != nil {
		t.Fatalf("subscriptionruntime.NewRuntime() error = %v", err)
	}
	subscriptionCredentials := subscription.NewCredentialManager(db, keyService, registry, mutations, subscriptions)
	requestLogStats := &staticRequestLogStatsReader{}
	priceRuntime := NewPriceRuntime()
	catalogRuntime := &catalog.Runtime{}
	service := NewService(
		db,
		manager,
		registry,
		priceRuntime,
		catalogRuntime,
		nil,
		nil,
		keyService,
		controlHTTPExecutor{},
		subscriptionCredentials,
		nil,
		nil,
		nil,
		stats,
		mutations,
		requestLogStats,
		accessQuota,
		nil,
		nil,
		nil,
		channelRegistry,
	)
	installCodexControlTestHooks(service)
	// Tests opt into reset-credit upstream calls explicitly; no fixture may
	// reach a real provider by accident.
	setCodexResetCreditObservation(service, nil)
	return serviceFixture{
		db: db, manager: manager, registry: registry, channelRegistry: channelRegistry, encryption: keyService,
		priceRuntime: priceRuntime, catalogRuntime: catalogRuntime,
		stats: stats, mutations: mutations, requestLogStats: requestLogStats, accessQuota: accessQuota,
		service: service,
	}
}

type controlAccessQuotaReconciler struct {
	runtime *accessquota.Runtime
}

func (reconciler controlAccessQuotaReconciler) ReconcileConfigSnapshot(snapshot *state.ConfigSnapshot) error {
	return reconciler.runtime.Reconcile(snapshot.AccessQuotaDefinitions())
}

func openControlTestDBWithDSN(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db := openControlTestDBWithoutMigration(t, dsn)
	if err := storage.AutoMigrate(db); err != nil {
		t.Fatalf("storage.AutoMigrate() error = %v", err)
	}
	return db
}

func openControlTestDBWithoutMigration(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatalf("storage.Open(%q) error = %v", dsn, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close control test database: %v", err)
		}
	})
	return db
}

func assertGroupCount(t *testing.T, db *gorm.DB, want int64) {
	t.Helper()
	var got int64
	if err := db.Table("groups").Count(&got).Error; err != nil {
		t.Fatalf("count groups: %v", err)
	}
	if got != want {
		t.Fatalf("group count = %d, want %d", got, want)
	}
}
