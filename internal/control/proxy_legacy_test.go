package control

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/platform/encryption"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage/models"
)

func TestLegacyProxyConversionAtomicAndRepeatSafe(t *testing.T) {
	fixture := newServiceFixture(t)
	db, service, crypto := fixture.db, fixture.service, fixture.encryption
	encrypt := func(raw string) string {
		t.Helper()
		value, err := crypto.Encrypt(raw)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := encrypt(`{"mode":"custom","url":"http://proxy.example:8080"}`)
	second := encrypt(`{"mode":"custom","url":"http://PROXY.EXAMPLE:8080/"}`)
	direct := encrypt(`{"mode":"direct"}`)
	groups := []models.Group{
		{Name: "first", ChannelID: "openai", Params: models.JSON(`{}`), Models: models.JSON(`[]`), ProxyConfig: &first},
		{Name: "second", ChannelID: "openai", Params: models.JSON(`{}`), Models: models.JSON(`[]`), ProxyConfig: &second},
		{Name: "direct", ChannelID: "openai", Params: models.JSON(`{}`), Models: models.JSON(`[]`), ProxyConfig: &direct},
		{Name: "inherit", ChannelID: "openai", Params: models.JSON(`{}`), Models: models.JSON(`[]`)},
	}
	if err := db.Create(&groups).Error; err != nil {
		t.Fatal(err)
	}
	bad := "invalid-encrypted-proxy"
	credential := models.Credential{GroupID: groups[0].ID, Data: encrypt(`{"api_key":"fixture"}`), Fingerprint: "proxy-fixture", ProxyConfig: &bad}
	if err := db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.SystemSetting{Key: outboundproxy.SystemSettingKey, Value: first}).Error; err != nil {
		t.Fatal(err)
	}
	beforeFailure := readLegacyProxySnapshot(t, db)
	if err := service.migrateLegacyProxyOverrides(t.Context()); err == nil {
		t.Fatal("corrupt ciphertext was silently inherited")
	}
	if !reflect.DeepEqual(beforeFailure, readLegacyProxySnapshot(t, db)) {
		t.Fatal("failed conversion changed source data, catalog, or revision")
	}
	if err := db.Model(&credential).UpdateColumn("proxy_config", second).Error; err != nil {
		t.Fatal(err)
	}
	before := readLegacyProxySnapshot(t, db)
	if err := service.migrateLegacyProxyOverrides(t.Context()); err != nil {
		t.Fatal(err)
	}
	converted := readLegacyProxySnapshot(t, db)
	if len(converted.Proxies) != 1 {
		t.Fatalf("catalog count = %d, want 1", len(converted.Proxies))
	}
	proxy := converted.Proxies[0]
	if proxy.Config != first {
		t.Fatal("conversion re-encrypted or normalized the original catalog ciphertext")
	}
	for _, ciphertext := range []string{*converted.Groups[0].ProxyConfig, *converted.Groups[1].ProxyConfig, *converted.Credentials[0].ProxyConfig} {
		assertLegacyProxyReference(t, crypto, ciphertext, proxy.ID)
	}
	for _, row := range converted.Settings {
		if row.Key == outboundproxy.SystemSettingKey {
			assertLegacyProxyReference(t, crypto, row.Value, proxy.ID)
		}
	}
	for index := range before.Groups {
		if index < 2 {
			before.Groups[index].ProxyConfig = converted.Groups[index].ProxyConfig
		}
	}
	before.Credentials[0].ProxyConfig = converted.Credentials[0].ProxyConfig
	if !reflect.DeepEqual(before.Groups, converted.Groups) || !reflect.DeepEqual(before.Credentials, converted.Credentials) {
		t.Fatal("conversion changed fields other than the legacy proxy reference")
	}
	for range 3 {
		if err := service.migrateLegacyProxyOverrides(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(converted, readLegacyProxySnapshot(t, db)) {
			t.Fatal("repeat conversion changed identity, ciphertext, timestamps, or revision")
		}
	}
	if _, err := stateloader.LoadProxyCatalog(t.Context(), db, crypto); err != nil {
		t.Fatalf("converted catalog cannot be loaded: %v", err)
	}
}

func TestLegacyProxyConversionConcurrentCallers(t *testing.T) {
	first, dsn := newServiceFixtureWithSecondDSN(t)
	second := newServiceFixtureWithDSN(t, dsn)
	ciphertext, err := first.encryption.Encrypt(`{"mode":"custom","url":"socks5://proxy.example:1080"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.db.Create(&models.SystemSetting{Key: outboundproxy.SystemSettingKey, Value: ciphertext}).Error; err != nil {
		t.Fatal(err)
	}
	beforeRevision, err := readClusterConfigRevision(t.Context(), first.db)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var callers sync.WaitGroup
	for _, service := range []*Service{first.service, second.service} {
		callers.Go(func() {
			<-start
			errorsCh <- service.migrateLegacyProxyOverrides(t.Context())
		})
	}
	close(start)
	callers.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	result := readLegacyProxySnapshot(t, first.db)
	if len(result.Proxies) != 1 || result.Proxies[0].Config != ciphertext {
		t.Fatal("concurrent conversion did not preserve one shared identity and ciphertext")
	}
	afterRevision, err := readClusterConfigRevision(t.Context(), first.db)
	if err != nil || afterRevision != beforeRevision+1 {
		t.Fatalf("revision = %d, want %d: %v", afterRevision, beforeRevision+1, err)
	}
	if err := second.service.migrateLegacyProxyOverrides(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, readLegacyProxySnapshot(t, first.db)) {
		t.Fatal("restart changed committed conversion")
	}
}

func TestLegacyProxyConversionReusesExistingCatalogIdentity(t *testing.T) {
	fixture := newServiceFixture(t)
	config := outboundproxy.Config{Mode: outboundproxy.ModeCustom, URL: "http://proxy.example:8080"}
	raw, err := outboundproxy.Encode(config)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := fixture.encryption.Encrypt(raw)
	if err != nil {
		t.Fatal(err)
	}
	row := models.Proxy{Name: "existing", Config: ciphertext, Fingerprint: fixture.encryption.Hash(raw), Enabled: true}
	if err := fixture.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	// A disabled identity remains the same identity; its references inherit.
	if err := fixture.db.Model(&row).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	var before models.Proxy
	if err := fixture.db.First(&before, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&models.SystemSetting{Key: outboundproxy.SystemSettingKey, Value: ciphertext}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.migrateLegacyProxyOverrides(t.Context()); err != nil {
		t.Fatal(err)
	}
	result := readLegacyProxySnapshot(t, fixture.db)
	if len(result.Proxies) != 1 || !reflect.DeepEqual(before, result.Proxies[0]) {
		t.Fatal("existing catalog identity or ciphertext changed")
	}
	for _, setting := range result.Settings {
		if setting.Key == outboundproxy.SystemSettingKey {
			assertLegacyProxyReference(t, fixture.encryption, setting.Value, row.ID)
		}
	}
}

func TestEnsureInitialStateConvertsLegacyProxyBeforeLoader(t *testing.T) {
	fixture := newServiceFixture(t)
	ciphertext, err := fixture.encryption.Encrypt(`{"mode":"custom","url":"http://proxy.example:8080"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&models.SystemSetting{Key: outboundproxy.SystemSettingKey, Value: ciphertext}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.EnsureInitialState(t.Context()); err != nil {
		t.Fatal(err)
	}
	result := readLegacyProxySnapshot(t, fixture.db)
	if len(result.Proxies) != 1 {
		t.Fatal("bootstrap did not convert legacy proxy")
	}
	loader := stateloader.NewWithCredentialValidation(fixture.db, fixture.manager, fixture.registry, fixture.channelRegistry, nil, fixture.encryption)
	if err := loader.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, readLegacyProxySnapshot(t, fixture.db)) {
		t.Fatal("loader wrote proxy state after bootstrap")
	}
}

func TestLegacyProxyConversionRejectsChangedSource(t *testing.T) {
	fixture, dsn := newServiceFixtureWithSecondDSN(t)
	peer := newServiceFixtureWithDSN(t, dsn)
	legacy, err := fixture.encryption.Encrypt(`{"mode":"custom","url":"http://proxy.example:8080"}`)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := fixture.encryption.Encrypt(`{"mode":"direct"}`)
	if err != nil {
		t.Fatal(err)
	}
	group := models.Group{Name: "changing", ChannelID: "openai", Params: models.JSON(`{}`), Models: models.JSON(`[]`), ProxyConfig: &legacy}
	if err := fixture.db.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	before := readLegacyProxySnapshot(t, fixture.db)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	callbackName := "test:legacy_proxy_source_changed"
	if err := fixture.db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "groups" {
			once.Do(func() {
				close(started)
				<-release
			})
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.db.Callback().Update().Remove(callbackName) })
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- fixture.service.migrateLegacyProxyOverrides(t.Context()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("conversion did not reach the guarded write")
	}
	// Simulate an old/non-cooperating writer. New catalog writers take the lock.
	writeErr := peer.db.Model(&models.Group{}).Where("id = ?", group.ID).UpdateColumn("proxy_config", direct).Error
	close(release)
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if err := <-errorsCh; err == nil {
		t.Fatal("conversion overwrote a concurrently changed source")
	}
	before.Groups[0].ProxyConfig = &direct
	if !reflect.DeepEqual(before, readLegacyProxySnapshot(t, fixture.db)) {
		t.Fatal("source conflict did not roll back catalog and revision")
	}
}

func TestLegacyProxyConversionWrongKeyAndCorruptCatalog(t *testing.T) {
	fixture := newServiceFixture(t)
	ciphertext, err := fixture.encryption.Encrypt(`{"mode":"custom","url":"http://proxy.example:8080"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&models.SystemSetting{Key: outboundproxy.SystemSettingKey, Value: ciphertext}).Error; err != nil {
		t.Fatal(err)
	}
	before := readLegacyProxySnapshot(t, fixture.db)
	wrongKey, err := encryption.NewService("wrong-legacy-proxy-key")
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.encryption = wrongKey
	if err := fixture.service.migrateLegacyProxyOverrides(t.Context()); err == nil {
		t.Fatal("wrong encryption key was accepted")
	}
	if !reflect.DeepEqual(before, readLegacyProxySnapshot(t, fixture.db)) {
		t.Fatal("wrong-key conversion changed persistent state")
	}
	fixture.service.encryption = fixture.encryption
	if err := fixture.db.Create(&models.Proxy{Name: "corrupt", Config: "invalid", Fingerprint: "corrupt", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	before = readLegacyProxySnapshot(t, fixture.db)
	if err := fixture.service.EnsureInitialState(t.Context()); err == nil {
		t.Fatal("bootstrap accepted corrupt managed proxy ciphertext")
	}
	if !reflect.DeepEqual(before, readLegacyProxySnapshot(t, fixture.db)) {
		t.Fatal("failed bootstrap changed persistent state")
	}
}

func TestLegacyProxyConcurrentBootstrap(t *testing.T) {
	first, dsn := newServiceFixtureWithSecondDSN(t)
	second := newServiceFixtureWithDSN(t, dsn)
	ciphertext, err := first.encryption.Encrypt(`{"mode":"custom","url":"http://proxy.example:8080"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.db.Create(&models.SystemSetting{Key: outboundproxy.SystemSettingKey, Value: ciphertext}).Error; err != nil {
		t.Fatal(err)
	}
	start, errorsCh := make(chan struct{}), make(chan error, 2)
	for _, service := range []*Service{first.service, second.service} {
		go func() {
			<-start
			errorsCh <- service.EnsureInitialState(t.Context())
		}()
	}
	close(start)
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	result := readLegacyProxySnapshot(t, first.db)
	if len(result.Proxies) != 1 || result.Proxies[0].Config != ciphertext {
		t.Fatal("concurrent bootstrap changed proxy identity")
	}
	var keyCount int64
	if err := first.db.Model(&models.AccessKey{}).Count(&keyCount).Error; err != nil || keyCount != 1 {
		t.Fatalf("default access key count = %d: %v", keyCount, err)
	}
}

func assertLegacyProxyReference(t *testing.T, crypto encryption.Service, ciphertext string, id uint) {
	t.Helper()
	plaintext, err := crypto.Decrypt(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	config, err := outboundproxy.Decode(plaintext)
	if err != nil || config.ProxyID != id || config.URL != "" {
		t.Fatal("legacy override did not become the expected shared reference")
	}
}

type legacyProxySnapshot struct {
	Proxies     []models.Proxy
	Groups      []models.Group
	Credentials []models.Credential
	Settings    []models.SystemSetting
}

func readLegacyProxySnapshot(t *testing.T, db *gorm.DB) legacyProxySnapshot {
	t.Helper()
	var result legacyProxySnapshot
	for _, query := range []struct {
		rows  any
		order string
	}{{&result.Proxies, "id ASC"}, {&result.Groups, "id ASC"}, {&result.Credentials, "id ASC"}, {&result.Settings, "key ASC"}} {
		if err := db.Order(query.order).Find(query.rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	return result
}
