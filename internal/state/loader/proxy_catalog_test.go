package loader_test

import (
	"testing"

	"gpt-load/internal/channel"
	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/platform/encryption"
	"gpt-load/internal/state"
	"gpt-load/internal/state/loader"
	"gpt-load/internal/storage/models"
)

func TestProxyReferencesResolveBeforeRuntimeCompilation(t *testing.T) {
	db := openMigratedDatabase(t)
	crypto, err := encryption.NewService("managed-proxy-loader-test-key")
	if err != nil {
		t.Fatal(err)
	}
	encode := func(raw string) string {
		t.Helper()
		result, err := crypto.Encrypt(raw)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	raw := `{"mode":"custom","url":"socks5://proxy.example:1080"}`
	proxy := models.Proxy{Name: "shared", Config: encode(raw), Fingerprint: crypto.Hash(raw), Enabled: true}
	mustCreate(t, db, &proxy)
	reference, err := outboundproxy.Encode(outboundproxy.Config{Mode: outboundproxy.ModeCustom, ProxyID: proxy.ID})
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, db, &models.SystemSetting{Key: outboundproxy.SystemSettingKey, Value: encode(reference)})
	for _, phase := range []string{"enabled", "disabled", "enabled_again", "deleted"} {
		switch phase {
		case "disabled":
			if err := db.Model(&proxy).Update("enabled", false).Error; err != nil {
				t.Fatal(err)
			}
		case "enabled_again":
			if err := db.Model(&proxy).Update("enabled", true).Error; err != nil {
				t.Fatal(err)
			}
		case "deleted":
			if err := db.Delete(&proxy).Error; err != nil {
				t.Fatal(err)
			}
		}
		for _, environment := range []*outboundproxy.Config{nil, {Mode: outboundproxy.ModeEnvironment}} {
			input, err := loader.BuildCompileInputWithProxy(t.Context(), db, crypto, environment, channel.NewRegistry())
			if err != nil {
				t.Fatalf("%s: %v", phase, err)
			}
			snapshot, err := state.Compile(input)
			if err != nil {
				t.Fatalf("%s: %v", phase, err)
			}
			if phase == "enabled" || phase == "enabled_again" {
				if snapshot.GlobalProxy.Config.URL != "socks5://proxy.example:1080" {
					t.Fatalf("%s did not resolve proxy", phase)
				}
			} else {
				want := outboundproxy.ModeDirect
				if environment != nil {
					want = outboundproxy.ModeEnvironment
				}
				if snapshot.GlobalProxy.Config.Mode != want {
					t.Fatalf("%s inherited %s, want %s", phase, snapshot.GlobalProxy.Config.Mode, want)
				}
			}
		}
	}
}

func TestLoaderRejectsCorruptDisabledProxy(t *testing.T) {
	db := openMigratedDatabase(t)
	crypto, err := encryption.NewService("managed-proxy-loader-test-key")
	if err != nil {
		t.Fatal(err)
	}
	proxy := models.Proxy{Name: "corrupt", Config: "invalid-ciphertext", Fingerprint: "invalid", Enabled: true}
	mustCreate(t, db, &proxy)
	if err := db.Model(&proxy).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := loader.LoadProxyCatalog(t.Context(), db, crypto); err == nil {
		t.Fatal("catalog treated corrupt ciphertext as a missing reference")
	}
	if _, err := loader.BuildCompileInputWithProxy(t.Context(), db, crypto, nil, channel.NewRegistry()); err == nil {
		t.Fatal("compile input silently ignored corrupt catalog ciphertext")
	}
}

func TestLoaderOnlyReadsLegacyProxyOverrides(t *testing.T) {
	db := openMigratedDatabase(t)
	crypto, err := encryption.NewService("managed-proxy-migration-test-key")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := crypto.Encrypt(`{"mode":"custom","url":"http://same.example:8080"}`)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, db, &models.SystemSetting{Key: outboundproxy.SystemSettingKey, Value: encoded})
	for _, name := range []string{"first", "second"} {
		mustCreate(t, db, &models.Group{Name: name, ChannelID: string(channel.OpenAI), Params: models.JSON(`{}`), Models: models.JSON(`[]`), Overrides: models.JSON(`{}`), ProxyConfig: &encoded, Enabled: true})
	}
	value := loader.NewWithCredentialValidation(db, state.NewManager(), state.NewCredentialRegistry(), channel.NewRegistry(), nil, crypto)
	for range 2 {
		if err := value.Load(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var proxies []models.Proxy
	if err := db.Find(&proxies).Error; err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 0 {
		t.Fatalf("loader wrote %d catalog rows", len(proxies))
	}
	var setting models.SystemSetting
	if err := db.Where("key = ?", outboundproxy.SystemSettingKey).Take(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.Value != encoded {
		t.Fatal("loader rewrote the encrypted global proxy")
	}
	var groups []models.Group
	if err := db.Find(&groups).Error; err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if group.ProxyConfig == nil || *group.ProxyConfig != encoded {
			t.Fatal("loader rewrote an encrypted group proxy")
		}
	}
}
