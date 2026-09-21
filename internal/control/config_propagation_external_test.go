package control

import (
	"context"
	"errors"
	"hash/fnv"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/coordination"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
)

// doorbellOnlyPollInterval is long enough that only a Pub/Sub doorbell can
// wake an instance, which is what makes the doorbell path observable.
const doorbellOnlyPollInterval = time.Hour

// fastPollInterval stands in for the production configPollInterval when a test
// needs the polling fallback to be observable within a test's lifetime.
const fastPollInterval = 200 * time.Millisecond

const propagationTimeout = 15 * time.Second

// propagationInstance is one gateway process: its own database handle, runtime
// state and Redis connection, sharing only the database file and Redis with
// its peer.
type propagationInstance struct {
	serviceFixture
	version   *observedConfigVersion
	broadcast *interruptibleBroadcaster
}

// interruptibleBroadcaster simulates Redis being unreachable at the moment a
// control transaction commits.
type interruptibleBroadcaster struct {
	version *coordination.ConfigVersion
	failing atomic.Bool
}

func (broadcaster *interruptibleBroadcaster) Bump(ctx context.Context) (int64, error) {
	if broadcaster.failing.Load() {
		return 0, errors.New("redis is unreachable")
	}
	return broadcaster.version.Bump(ctx)
}

// observedConfigVersion exposes when the watch loop has actually subscribed and
// read, so a test can write only after its peer can hear the doorbell.
type observedConfigVersion struct {
	source ConfigVersionSource
	reads  atomic.Int64
	silent bool
}

func (observed *observedConfigVersion) Current(ctx context.Context) (int64, error) {
	version, err := observed.source.Current(ctx)
	if err == nil {
		observed.reads.Add(1)
	}
	return version, err
}

func (observed *observedConfigVersion) Subscribe(ctx context.Context) (<-chan struct{}, func()) {
	if observed.silent {
		// A doorbell that never rings: the instance has only its poll timer,
		// which is what a dropped Pub/Sub connection leaves behind.
		return nil, func() {}
	}
	return observed.source.Subscribe(ctx)
}

func TestExternalRedisConfigPropagationAppliesGroupChange(t *testing.T) {
	writer, reader := newPropagationPair(t, doorbellOnlyPollInterval, false)

	if _, err := writer.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		return tx.Create(validControlGroup("propagated-group")).Error
	}, nil); err != nil {
		t.Fatalf("writeConfig() error = %v", err)
	}

	waitFor(t, "peer applies the group change", func() bool {
		snapshot := reader.manager.Current()
		return snapshot != nil && len(snapshot.Groups) == 1
	})
}

func TestExternalRedisConfigPropagationAppliesModelPrice(t *testing.T) {
	writer, reader := newPropagationPair(t, doorbellOnlyPollInterval, false)

	price := int64(4_000_000)
	if _, err := writer.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		return tx.Create(&models.ModelPrice{
			ChannelID: "openai_compatible", ModelID: "gpt-4o", IsManual: true,
			InputPriceNanoUSDPerMillionTokens:  &price,
			OutputPriceNanoUSDPerMillionTokens: &price,
		}).Error
	}, nil); err != nil {
		t.Fatalf("writeConfig() error = %v", err)
	}

	identity, err := PriceIdentityForChannelModel("openai_compatible", "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "peer applies the model price", func() bool {
		table := reader.priceRuntime.Load()
		if table == nil {
			return false
		}
		_, ok := table.Lookup(pricing.Identity(identity))
		return ok
	})
}

func TestExternalRedisConfigPropagationPreservesPeerCredentialCooldown(t *testing.T) {
	writer, reader := newPropagationPair(t, doorbellOnlyPollInterval, false)

	group := validControlGroup("cooldown-holder")
	if _, err := writer.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		if err := tx.Create(group).Error; err != nil {
			return err
		}
		return tx.Create(&models.Credential{
			GroupID: group.ID, Data: "ciphertext", Fingerprint: "propagation-fingerprint",
			Status: models.CredentialStatusActive,
		}).Error
	}, nil); err != nil {
		t.Fatalf("writeConfig() error = %v", err)
	}
	waitFor(t, "peer loads the credential", func() bool {
		return len(reader.registry.Snapshot()) == 1
	})

	credentialID := reader.registry.Snapshot()[0].ID
	cooldown := time.Now().Add(time.Hour).UTC()
	if !reader.registry.SetCooldown(credentialID, cooldown) {
		t.Fatalf("SetCooldown(%d) = false", credentialID)
	}

	// An unrelated remote setting change must not cost the peer the credential
	// health only it knows about.
	if _, err := writer.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		return tx.Create(&models.SystemSetting{
			Key: state.SettingBlacklistThreshold, Value: "9",
		}).Error
	}, nil); err != nil {
		t.Fatalf("writeConfig(setting) error = %v", err)
	}
	waitFor(t, "peer applies the setting change", func() bool {
		snapshot := reader.manager.Current()
		return snapshot != nil && snapshot.Settings.BlacklistThreshold == 9
	})

	got, ok := reader.registry.CredentialCooldownUntil(credentialID)
	if !ok || !got.Equal(cooldown) {
		t.Fatalf("CredentialCooldownUntil(%d) = %v, %t, want %v, true", credentialID, got, ok, cooldown)
	}
}

func TestExternalRedisConfigPropagationConvergesWithoutTheDoorbell(t *testing.T) {
	writer, reader := newPropagationPair(t, fastPollInterval, true)

	if _, err := writer.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		return tx.Create(validControlGroup("polled-group")).Error
	}, nil); err != nil {
		t.Fatalf("writeConfig() error = %v", err)
	}

	waitFor(t, "peer converges through polling alone", func() bool {
		snapshot := reader.manager.Current()
		return snapshot != nil && len(snapshot.Groups) == 1
	})
}

func TestExternalRedisConfigPropagationRecoversFromBroadcastOutage(t *testing.T) {
	// The writer polls fast because its own watch loop owns the pending
	// broadcast retry; the reader stays doorbell-only so the retry is the only
	// thing that can make it converge.
	writer, reader := newPropagationPairWithIntervals(
		t, fastPollInterval, doorbellOnlyPollInterval, false,
	)

	writer.broadcast.failing.Store(true)
	if _, err := writer.service.writeConfig(t.Context(), func(tx *gorm.DB) error {
		return tx.Create(validControlGroup("outage-group")).Error
	}, nil); err != nil {
		t.Fatalf("writeConfig() error = %v, want success despite the broadcast failure", err)
	}
	if !writer.service.broadcastPending.Load() {
		t.Fatal("broadcastPending = false after a failed broadcast")
	}
	// The shared version never moved, so nothing can have reached the reader.
	time.Sleep(500 * time.Millisecond)
	if snapshot := reader.manager.Current(); snapshot != nil && len(snapshot.Groups) != 0 {
		t.Fatalf("peer applied %d groups while the broadcast was failing", len(snapshot.Groups))
	}

	writer.broadcast.failing.Store(false)
	waitFor(t, "peer applies the change after the broadcast retry", func() bool {
		snapshot := reader.manager.Current()
		return snapshot != nil && len(snapshot.Groups) == 1
	})
	if writer.service.broadcastPending.Load() {
		t.Fatal("broadcastPending = true after a successful retry")
	}
}

func newPropagationPair(
	t *testing.T,
	pollInterval time.Duration,
	silentDoorbell bool,
) (*propagationInstance, *propagationInstance) {
	t.Helper()
	return newPropagationPairWithIntervals(t, pollInterval, pollInterval, silentDoorbell)
}

// newPropagationPairWithIntervals builds two independent runtimes over one
// shared database file and one shared Redis.
func newPropagationPairWithIntervals(
	t *testing.T,
	writerPollInterval time.Duration,
	readerPollInterval time.Duration,
	silentDoorbell bool,
) (*propagationInstance, *propagationInstance) {
	t.Helper()
	redisDSN := requireExternalRedisDSN(t)
	databaseDSN := filepath.Join(t.TempDir(), "propagation.db")
	resetSharedConfigVersion(t, redisDSN)

	writer := startPropagationInstance(t, databaseDSN, redisDSN, writerPollInterval, false)
	reader := startPropagationInstance(t, databaseDSN, redisDSN, readerPollInterval, silentDoorbell)
	return writer, reader
}

func startPropagationInstance(
	t *testing.T,
	databaseDSN string,
	redisDSN string,
	pollInterval time.Duration,
	silentDoorbell bool,
) *propagationInstance {
	t.Helper()
	fixture := newServiceFixtureWithDSN(t, databaseDSN)
	client := openExternalRedis(t, redisDSN)
	broadcaster := &interruptibleBroadcaster{version: coordination.NewConfigVersion(client)}
	fixture.service.SetConfigBroadcaster(broadcaster.Bump)

	observed := &observedConfigVersion{source: broadcaster.version, silent: silentDoorbell}
	runtime := &Runtime{configVersion: observed, configReload: fixture.service}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runtime.runConfigWatch(ctx, standardRuntimeTicker{ticker: time.NewTicker(pollInterval)})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(propagationTimeout):
			t.Error("configuration watch did not stop")
		}
	})

	// The loop subscribes before its first version read, so a completed read
	// proves this instance can already hear the doorbell. Writing before that
	// point could race past the subscription.
	waitFor(t, "instance starts watching the shared configuration version", func() bool {
		return observed.reads.Load() > 0
	})
	return &propagationInstance{serviceFixture: fixture, version: observed, broadcast: broadcaster}
}

func requireExternalRedisDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("GPT_LOAD_REDIS_TEST_DSN"))
	if dsn == "" {
		t.Skip("GPT_LOAD_REDIS_TEST_DSN is not set")
	}
	return isolatedRedisDSN(t, dsn)
}

// isolatedRedisDSN gives each test its own Redis database index so concurrent
// runs cannot observe each other's configuration version. Pub/Sub is not
// database-scoped, but the doorbell carries no state, so a stray message costs
// a receiver at most one idempotent reload.
func isolatedRedisDSN(t *testing.T, dsn string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse GPT_LOAD_REDIS_TEST_DSN: %v", err)
	}
	digest := fnv.New32a()
	if _, err := digest.Write([]byte(t.Name())); err != nil {
		t.Fatalf("hash test name: %v", err)
	}
	parsed.Path = "/" + strconv.Itoa(1+int(digest.Sum32()%15))
	return parsed.String()
}

func openExternalRedis(t *testing.T, dsn string) *coordination.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := coordination.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("coordination.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close coordination client: %v", err)
		}
	})
	return client
}

// resetSharedConfigVersion clears this test's counter so both instances start
// from a known version, and removes it again once the test is done.
func resetSharedConfigVersion(t *testing.T, dsn string) {
	t.Helper()
	client := openExternalRedis(t, dsn)
	remove := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Redis().Del(ctx, coordination.Key("config", "version")).Err(); err != nil {
			t.Errorf("delete shared config version: %v", err)
		}
	}
	remove()
	t.Cleanup(remove)
}

func waitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(propagationTimeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
