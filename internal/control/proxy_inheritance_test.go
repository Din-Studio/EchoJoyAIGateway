package control

import (
	"encoding/json"
	"fmt"
	"testing"

	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/storage/models"
)

func TestCorruptProxyCiphertextNeverFallsBack(t *testing.T) {
	fixture := newServiceFixture(t)
	groupID := createGroupWithCredentials(t, fixture, "corrupt-proxy")
	item, err := fixture.service.SaveProxy(t.Context(), 0, ProxySaveRequest{URL: "http://corrupt-proxy.invalid:8080"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.UpdateGroupSettings(t.Context(), groupID, GroupSettingsUpdateRequest{Proxy: optionalField[outboundproxy.Config]{Set: true, Value: outboundproxy.Config{Mode: outboundproxy.ModeCustom, ProxyID: item.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate damaged storage. Production configuration writes must not allow
	// this value, and a disabled damaged entry is not a missing reference.
	if err := fixture.db.Model(&models.Proxy{}).Where("id = ?", item.ID).Updates(map[string]any{"config": "damaged-ciphertext", "enabled": false}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.GetGroupSettings(t.Context(), groupID); err == nil {
		t.Fatal("corrupt catalog silently inherited direct access")
	}
	if _, err := fixture.service.ListProxies(t.Context(), ProxyListQuery{}); err == nil {
		t.Fatal("corrupt catalog was exposed as an ordinary disabled entry")
	}
	if _, err := fixture.service.RevealProxy(t.Context(), item.ID); err == nil {
		t.Fatal("corrupt catalog returned a secret")
	}
}

func TestUnavailableProxyReferencesInheritEveryParent(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.service.environmentProxy = &outboundproxy.Config{Mode: outboundproxy.ModeEnvironment}
	groupID := createGroupWithCredentials(t, fixture, "proxy-inheritance")
	items := make([]ProxyItem, 0, 3)
	for _, name := range []string{"global", "group", "credential"} {
		item, err := fixture.service.SaveProxy(t.Context(), 0, ProxySaveRequest{Name: name, URL: fmt.Sprintf("http://%s-proxy.invalid:8080", name)})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	_, err := fixture.service.UpdateSettings(t.Context(), SettingsUpdateRequest{Settings: map[string]json.RawMessage{
		outboundproxy.SystemSettingKey: json.RawMessage(fmt.Sprintf(`{"mode":"custom","proxy_id":%d}`, items[0].ID)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.UpdateGroupSettings(t.Context(), groupID, GroupSettingsUpdateRequest{Proxy: optionalField[outboundproxy.Config]{Set: true, Value: outboundproxy.Config{Mode: outboundproxy.ModeCustom, ProxyID: items[1].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := fixture.service.ListGroupCredentials(t.Context(), groupID, CredentialCollectionQuery{Page: 1, PageSize: 20})
	if err != nil || len(rows.Items) != 1 {
		t.Fatalf("credentials: %+v %v", rows, err)
	}
	id := rows.Items[0].CredentialID
	_, err = fixture.service.UpdateGroupCredential(t.Context(), groupID, id, CredentialUpdateRequest{Proxy: optionalField[outboundproxy.Config]{Set: true, Value: outboundproxy.Config{Mode: outboundproxy.ModeCustom, ProxyID: items[2].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	assert := func(source outboundproxy.Source, mode outboundproxy.Mode, reference string) {
		t.Helper()
		rows, err := fixture.service.ListGroupCredentials(t.Context(), groupID, CredentialCollectionQuery{Page: 1, PageSize: 20})
		if err != nil || len(rows.Items) != 1 {
			t.Fatalf("credentials: %+v %v", rows, err)
		}
		view := rows.Items[0].Proxy
		if view.ProxyID != items[2].ID || view.EffectiveSource != source || view.EffectiveMode != mode || view.ReferenceState != reference {
			t.Fatalf("proxy inheritance: %+v, want source=%s mode=%s reference=%s", view, source, mode, reference)
		}
	}
	assert(outboundproxy.SourceCredential, outboundproxy.ModeCustom, "enabled")
	for _, action := range []string{"disable", "delete"} {
		if action == "delete" {
			// Restore the chain to prove deletion independently of disabling.
			if err := fixture.service.BatchProxies(t.Context(), ProxyBatchRequest{IDs: []uint{items[0].ID, items[1].ID, items[2].ID}, Action: "enable"}); err != nil {
				t.Fatal(err)
			}
		}
		state := "disabled"
		if action == "delete" {
			state = "deleted"
		}
		for index, source := range []outboundproxy.Source{outboundproxy.SourceGroup, outboundproxy.SourceGlobal, outboundproxy.SourceEnvironment} {
			item := items[2-index]
			if err := fixture.service.BatchProxies(t.Context(), ProxyBatchRequest{IDs: []uint{item.ID}, Action: action}); err != nil {
				t.Fatal(err)
			}
			mode := outboundproxy.ModeCustom
			if source == outboundproxy.SourceEnvironment {
				mode = outboundproxy.ModeEnvironment
			}
			assert(source, mode, state)
		}
	}
	fixture.service.environmentProxy = nil
	if err := fixture.service.BatchProxies(t.Context(), ProxyBatchRequest{IDs: []uint{items[0].ID}, Action: "disable"}); err != nil {
		t.Fatal(err)
	}
	assert(outboundproxy.SourceDefault, outboundproxy.ModeDirect, "deleted")
}
