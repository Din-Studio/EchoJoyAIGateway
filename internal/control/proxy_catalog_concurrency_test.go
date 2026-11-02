package control

import (
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/outboundproxy"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/storage/models"
)

func TestLegacyProxyWritesDeduplicateAcrossInstances(t *testing.T) {
	first, second := newPeerFixtures(t)
	firstGroup := createCompatibleGroup(t, first, "https://first-legacy-group.invalid/v1", "proxy-legacy-first")
	secondGroup := createCompatibleGroup(t, second, "https://second-legacy-group.invalid/v1", "proxy-legacy-second")
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, target := range []struct {
		service *Service
		group   uint
	}{{first.service, firstGroup}, {second.service, secondGroup}} {
		go func() {
			<-start
			_, err := target.service.UpdateGroupSettings(t.Context(), target.group, GroupSettingsUpdateRequest{
				Proxy: optionalField[outboundproxy.Config]{Set: true, Value: outboundproxy.Config{Mode: outboundproxy.ModeCustom, URL: "http://shared-legacy.invalid:8080"}},
			})
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if count := mustCount(t, first, &models.Proxy{}, "1=1"); count != 1 {
		t.Fatalf("concurrent legacy writes created %d proxies", count)
	}
	var groups []models.Group
	if err := first.db.Order("id").Find(&groups).Error; err != nil {
		t.Fatal(err)
	}
	var selected uint
	for _, group := range groups {
		config, err := decryptProxyOverride(first.encryption, group.ProxyConfig)
		if err != nil || config == nil || config.ProxyID == 0 {
			t.Fatal("legacy write did not store a reference")
		}
		if selected != 0 && selected != config.ProxyID {
			t.Fatal("legacy writes selected different identities")
		}
		selected = config.ProxyID
	}
}

func TestProxyImportsDeduplicateAcrossInstances(t *testing.T) {
	first, second := newPeerFixtures(t)
	start := make(chan struct{})
	type result struct {
		value ProxyImportResult
		err   error
	}
	results := make(chan result, 2)
	for _, service := range []*Service{first.service, second.service} {
		go func() {
			<-start
			value, err := service.ImportProxies(t.Context(), "http://shared-import.invalid:8080")
			results <- result{value, err}
		}()
	}
	close(start)
	left, right := <-results, <-results
	if left.err != nil || right.err != nil || left.value.Imported+right.value.Imported != 1 || left.value.Duplicates+right.value.Duplicates != 1 {
		t.Fatalf("concurrent import: left=%+v right=%+v", left, right)
	}
	if count := mustCount(t, first, &models.Proxy{}, "1=1"); count != 1 {
		t.Fatalf("catalog contains %d proxies", count)
	}
}

func TestProxyReferenceSelectionWaitsForCatalogMutation(t *testing.T) {
	first, second := newPeerFixtures(t)
	group := createGroupWithCredentials(t, second, "proxy-reference-lock")
	item, err := first.service.SaveProxy(t.Context(), 0, ProxySaveRequest{URL: "http://reference-lock.invalid:8080"})
	if err != nil {
		t.Fatal(err)
	}
	mutated, commit := make(chan struct{}), make(chan struct{})
	var closeOnce sync.Once
	unblock := func() { closeOnce.Do(func() { close(commit) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		done <- first.service.withControlTransaction(t.Context(), func(tx *gorm.DB) error {
			if err := lockProxyCatalog(tx); err != nil {
				return err
			}
			if err := tx.Model(&models.Proxy{}).Where("id = ?", item.ID).Update("enabled", false).Error; err != nil {
				return err
			}
			close(mutated)
			<-commit
			return nil
		})
	}()
	awaitProxySignal(t, mutated)
	selection := make(chan error, 1)
	go func() {
		_, err := second.service.UpdateGroupSettings(t.Context(), group, GroupSettingsUpdateRequest{
			Proxy: optionalField[outboundproxy.Config]{Set: true, Value: outboundproxy.Config{Mode: outboundproxy.ModeCustom, ProxyID: item.ID}},
		})
		selection <- err
	}()
	select {
	case err := <-selection:
		t.Fatalf("selection ignored the catalog lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-selection:
		if !errors.Is(err, app_errors.ErrValidation) {
			t.Fatalf("disabled reference accepted: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("selection did not finish after catalog commit")
	}
}
