package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpt-load/internal/cluster"
	"gpt-load/internal/outboundproxy"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/testutil/clustertest"
)

func awaitProxySignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy request did not reach the expected boundary")
	}
}

func TestProxyProbeRejectsOldConfigurationAndClearsOldResults(t *testing.T) {
	first, second := newPeerFixtures(t)
	started, finish := make(chan struct{}), make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(started)
			select {
			case <-finish:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	defer close(finish)
	item, err := first.service.SaveProxy(t.Context(), 0, ProxySaveRequest{URL: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.service.TestProxy(t.Context(), item.ID, "http://probe.invalid/fast"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.service.TestProxy(t.Context(), item.ID, "http://probe.invalid/slow")
		done <- err
	}()
	awaitProxySignal(t, started)
	// Change away and back to the same fingerprint: ciphertext identity must
	// also change, otherwise the first request could resurrect old results.
	for _, endpoint := range []string{"http://new-proxy.invalid:8080", proxy.URL} {
		updated, err := second.service.SaveProxy(t.Context(), item.ID, ProxySaveRequest{URL: endpoint})
		if err != nil || updated.LastTestAtMS != nil || updated.LastTestURL != "" {
			t.Fatalf("URL edit kept stale observations: result=%+v err=%v", updated, err)
		}
	}
	// Unblock without closing twice on a fatal path.
	finish <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale probe did not finish")
	}
	var row models.Proxy
	if err := first.db.Take(&row, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.LastTestAtMS != nil || row.LastTestURL != "" {
		t.Fatal("old address probe overwrote a new configuration")
	}
}

func TestProxyProbeDatabaseOrderAndBookkeeping(t *testing.T) {
	first, second := newPeerFixtures(t)
	started, finish := make(chan struct{}), make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/older" {
			close(started)
			select {
			case <-finish:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	defer close(finish)
	item, err := first.service.SaveProxy(t.Context(), 0, ProxySaveRequest{URL: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := readClusterConfigRevision(t.Context(), first.db)
	if err != nil {
		t.Fatal(err)
	}
	before := first.manager.Current()
	// Deliberately give the older request a later local clock. Ordering must
	// come from PostgreSQL, not instance clocks or equal millisecond times.
	first.service.now = func() time.Time { return time.Unix(2000000000, 0) }
	second.service.now = func() time.Time { return time.Unix(1800000000, 0) }
	done := make(chan error, 1)
	go func() {
		_, err := first.service.TestProxy(t.Context(), item.ID, "http://probe.invalid/older")
		done <- err
	}()
	awaitProxySignal(t, started)
	newer, err := second.service.TestProxy(t.Context(), item.ID, "http://probe.invalid/newer")
	if err != nil {
		t.Fatal(err)
	}
	finish <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("older probe did not finish")
	}
	var row models.Proxy
	if err := first.db.Take(&row, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.LastTestURL != newer.LastTestURL || row.LastTestAtMS == nil || *row.LastTestAtMS != *newer.LastTestAtMS {
		t.Fatal("late older probe replaced the newer observation")
	}
	if err := first.service.SaveProxyTestURL(t.Context(), "http://probe.invalid/preference"); err != nil {
		t.Fatal(err)
	}
	afterRevision, err := readClusterConfigRevision(t.Context(), first.db)
	if err != nil || afterRevision != revision || first.manager.Current() != before {
		t.Fatalf("bookkeeping republished configuration: revision=%d want=%d err=%v", afterRevision, revision, err)
	}
}

func TestProxyProbesShareEightSlotsAcrossServicesAndReleaseCancellation(t *testing.T) {
	first, second := newPeerFixtures(t)
	peerClient := clustertest.Connect(t, first.redis, "proxy-peer")
	second.service.clusterConcurrency = cluster.NewConcurrency(peerClient)
	var hits atomic.Int32
	started := make(chan struct{}, 8)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer proxy.Close()
	item, err := first.service.SaveProxy(t.Context(), 0, ProxySaveRequest{URL: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	var cancels []context.CancelFunc
	var wg sync.WaitGroup
	for index := range 8 {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		cancels = append(cancels, cancel)
		service := first.service
		if index%2 == 1 {
			service = second.service
		}
		wg.Go(func() {
			_, err := service.TestProxy(ctx, item.ID, "http://probe.invalid/test")
			if !errors.Is(err, context.Canceled) {
				t.Errorf("canceled probe: %v", err)
			}
		})
	}
	for range 8 {
		awaitProxySignal(t, started)
	}
	_, err = second.service.TestProxy(t.Context(), item.ID, "http://probe.invalid/rejected")
	if !errors.Is(err, errProxyTestsFull) || hits.Load() != 8 {
		t.Fatalf("full probe pool: err=%v outbound=%d", err, hits.Load())
	}
	for _, cancel := range cancels {
		cancel()
	}
	wg.Wait()
	// All eight slots must be available again after cancellation.
	for range 8 {
		release, acquired, err := second.service.clusterConcurrency.AcquireProxyTest(t.Context())
		if err != nil || !acquired {
			t.Fatalf("cancel leaked slot: acquired=%v err=%v", acquired, err)
		}
		defer release()
	}
	if count, err := first.concurrency.GlobalInflight(t.Context()); err != nil || count != 0 {
		t.Fatalf("manual probes consumed business capacity: count=%d err=%v", count, err)
	}
}

func TestProxyProbeFailsClosedWithoutRedis(t *testing.T) {
	fixture := newServiceFixture(t)
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer proxy.Close()
	item, err := fixture.service.SaveProxy(t.Context(), 0, ProxySaveRequest{URL: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	fixture.redis.Close()
	_, err = fixture.service.TestProxy(t.Context(), item.ID, "http://probe.invalid/test")
	if !errors.Is(err, errProxyTestStateUnavailable) || hits.Load() != 0 {
		t.Fatalf("Redis failure allowed probe: err=%v hits=%d", err, hits.Load())
	}
}

func TestProxyIdempotencyReplayRecoveryAndSecrets(t *testing.T) {
	first, second := newPeerFixtures(t)
	const createKey = "bb000000-0000-4000-8000-000000000001"
	request := ProxySaveRequest{Name: "secret", URL: "http://user:private-password@proxy.invalid:8080"}
	once := true
	first.service.beforeAdvanceOperationStage = func(_ context.Context, _ *models.ControlOperation, stage operationStage) error {
		if once && stage == operationStageRegistryApplied {
			once = false
			return errors.New("injected post-commit interruption")
		}
		return nil
	}
	_, err := first.service.SaveProxyIdempotent(t.Context(), createKey, 0, request)
	assertAPIErrorCode(t, err, app_errors.ErrControlOperationIncomplete.Code)
	created, err := second.service.SaveProxyIdempotent(t.Context(), createKey, 0, request)
	if err != nil {
		t.Fatal(err)
	}
	if count := mustCount(t, first, &models.Proxy{}, "id = ?", created.ID); count != 1 {
		t.Fatalf("recovery duplicated proxy: count=%d", count)
	}
	op := mustOperationRow(t, first, createKey)
	if op.CompletedAtMS == nil || string(op.OperationKind) != string(operationKindProxyCreate) {
		t.Fatal("proxy operation did not recover through the durable ledger")
	}
	if len(op.CanonicalResult) == 0 || !json.Valid(op.CanonicalResult) {
		t.Fatal("proxy result is not durable JSON")
	}
	encoded, _ := json.Marshal(op)
	if bytes.Contains(encoded, []byte("private-password")) {
		t.Fatal("operation ledger exposed authentication")
	}
	updated, err := second.service.SaveProxyIdempotent(t.Context(), "bb000000-0000-4000-8000-000000000002", created.ID, ProxySaveRequest{Name: "renamed"})
	if err != nil || updated.Name != "renamed" {
		t.Fatalf("idempotent update: result=%+v err=%v", updated, err)
	}
	if err := second.service.BatchProxies(t.Context(), ProxyBatchRequest{IDs: []uint{created.ID}, Action: "delete"}); err != nil {
		t.Fatal(err)
	}
	replayed, err := first.service.SaveProxyIdempotent(t.Context(), createKey, 0, request)
	if err != nil || !reflect.DeepEqual(replayed, created) {
		t.Fatalf("replay changed after delete: result=%+v err=%v", replayed, err)
	}
	request.Name = "different"
	_, err = first.service.SaveProxyIdempotent(t.Context(), createKey, 0, request)
	assertAPIErrorCode(t, err, app_errors.ErrIdempotencyKeyReused.Code)
	const importKey = "bb000000-0000-4000-8000-000000000003"
	entries := "http://user:private-password@proxy.invalid:8080\ninvalid\nhttp://other.invalid:8080"
	imported, err := first.service.ImportProxiesIdempotent(t.Context(), importKey, entries)
	if err != nil || imported.Imported != 2 || len(imported.InvalidLines) != 1 {
		t.Fatalf("import: %+v %v", imported, err)
	}
	importReplay, err := second.service.ImportProxiesIdempotent(t.Context(), importKey, entries)
	if err != nil || !reflect.DeepEqual(importReplay, imported) {
		t.Fatalf("import replay: %+v %v", importReplay, err)
	}
}

func TestProxyIdempotencyConcurrentInstances(t *testing.T) {
	first, second := newPeerFixtures(t)
	const key = "cc000000-0000-4000-8000-000000000001"
	request := ProxySaveRequest{URL: "http://concurrent.invalid:8080"}
	start := make(chan struct{})
	type result struct {
		item ProxyItem
		err  error
	}
	results := make(chan result, 2)
	for _, service := range []*Service{first.service, second.service} {
		go func() {
			<-start
			item, err := service.SaveProxyIdempotent(t.Context(), key, 0, request)
			results <- result{item, err}
		}()
	}
	close(start)
	left, right := <-results, <-results
	if left.err != nil || right.err != nil || !reflect.DeepEqual(left.item, right.item) {
		t.Fatalf("concurrent replay: left=%+v right=%+v", left, right)
	}
	if count := mustCount(t, first, &models.Proxy{}, "1=1"); count != 1 {
		t.Fatalf("concurrent create produced %d proxies", count)
	}
}

func TestProxyCatalogPropagatesAcrossInstances(t *testing.T) {
	first, second := newPeerFixtures(t)
	peer := clustertest.Connect(t, first.redis, "proxy-config-peer")
	first.service.clusterEvents = cluster.NewConfigEventBus(first.cluster)
	bus := cluster.NewConfigEventBus(peer)
	second.service.clusterEvents = bus
	groupID := createGroupWithCredentials(t, first, "proxy-propagation-credential")
	item, err := first.service.SaveProxyIdempotent(t.Context(), "dd000000-0000-4000-8000-000000000001", 0, ProxySaveRequest{URL: "http://before.invalid:8080"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = first.service.UpdateGroupSettings(t.Context(), groupID, GroupSettingsUpdateRequest{Proxy: optionalField[outboundproxy.Config]{Set: true, Value: outboundproxy.Config{Mode: outboundproxy.ModeCustom, ProxyID: item.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := first.service.ListGroupCredentials(t.Context(), groupID, CredentialCollectionQuery{Page: 1, PageSize: 20})
	if err != nil || len(rows.Items) != 1 {
		t.Fatalf("group credentials: %+v %v", rows, err)
	}
	credentialID := rows.Items[0].CredentialID
	_, err = first.service.UpdateGroupCredential(t.Context(), groupID, credentialID, CredentialUpdateRequest{Proxy: optionalField[outboundproxy.Config]{Set: true, Value: outboundproxy.Config{Mode: outboundproxy.ModeCustom, ProxyID: item.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingCredentialRuntimeExecutor{}
	second.service.executor = recorder
	coordinator := NewClusterConfigSync(second.service, bus)
	coordinator.pollInterval = 20 * time.Millisecond
	runClusterConfigSync(t, coordinator)
	await := func(expected string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if group, ok := second.manager.Current().Groups[groupID]; ok && group.Proxy.Config.URL == expected {
				ref, exists := second.registry.CredentialRef(credentialID)
				if !exists {
					continue
				}
				if expected == "" && ref.EncryptedProxy == "" {
					return
				}
				if expected != "" {
					resolved, err := decryptProxyOverride(second.encryption, &ref.EncryptedProxy)
					if err == nil && resolved != nil && resolved.URL == expected {
						return
					}
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("peer did not adopt proxy %q", expected)
	}
	await("http://before.invalid:8080")
	cooldown := time.Now().Add(10 * time.Minute).Truncate(time.Millisecond)
	if !second.registry.SetCooldown(credentialID, cooldown) {
		t.Fatal("could not set peer runtime health")
	}
	for index, endpoint := range []string{"http://after.invalid:8080", "http://final.invalid:8080"} {
		_, err := first.service.SaveProxyIdempotent(t.Context(), fmt.Sprintf("dd000000-0000-4000-8000-%012d", index+2), item.ID, ProxySaveRequest{URL: endpoint})
		if err != nil {
			t.Fatal(err)
		}
		await(endpoint)
	}
	for _, action := range []string{"disable", "enable", "delete"} {
		if err := first.service.BatchProxies(t.Context(), ProxyBatchRequest{IDs: []uint{item.ID}, Action: action}); err != nil {
			t.Fatal(err)
		}
		expected := ""
		if action == "enable" {
			expected = "http://final.invalid:8080"
		}
		await(expected)
	}
	view, exists := findRuntimeCredential(second.registry.Snapshot(), credentialID)
	if !exists || !view.CooldownUntil.Equal(cooldown) {
		t.Fatal("proxy propagation erased credential health")
	}
	if len(recorder.retiredCredentialIDs()) < 4 {
		t.Fatal("peer did not retire provider runtime after proxy changes")
	}
}
