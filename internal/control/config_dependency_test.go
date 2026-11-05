package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/automodel"
	"gpt-load/internal/channel"
	"gpt-load/internal/configdependency"
	"gpt-load/internal/platform/config"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/storage/models"
)

// These fixtures use local test PostgreSQL and in-process Redis. No credential
// validation or model discovery request is sent to an upstream provider.
func dependencyFixture(t *testing.T, auto, audit bool, explicit bool) (serviceFixture, uint, uint) {
	t.Helper()
	f := newServiceFixture(t)
	create := func(id channel.ID, name, model string) uint {
		t.Helper()
		result, err := f.service.CreateGroup(t.Context(), GroupCreateRequest{
			Name: &name, ChannelID: id, ConnectionType: "api_key", Params: json.RawMessage(`{}`),
			Models:      optionalGroupModels{Set: true, Values: []GroupModel{{ID: model}}},
			Credentials: apiKeyEntries("sk-local-dependency-test"),
		})
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		return result.GroupID
	}
	decision := create(channel.Jev, "decisions", "jev-latest")
	ordinary := create(channel.OpenAI, "ordinary", "target-model")
	groupID := decision
	if !explicit {
		groupID = 0
	}
	settings := map[string]json.RawMessage{
		"jev":           json.RawMessage(fmt.Sprintf(`{"group_id":%d,"model":"jev-latest","timeout_seconds":5}`, groupID)),
		"auto_model":    json.RawMessage(fmt.Sprintf(`{"enabled":%t,"models":[{"id":"auto-id","name":"auto","fallback":"p","presets":[{"id":"p","name":"Default preset","description":"test","model":"target-model","parameter_overrides":[]}]}]}`, auto)),
		"request_audit": json.RawMessage(fmt.Sprintf(`{"enabled":%t}`, audit)),
	}
	if _, err := f.service.UpdateSettings(t.Context(), SettingsUpdateRequest{Settings: settings}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	return f, decision, ordinary
}

func dependencyHTTP(t *testing.T, f serviceFixture, method, path, body, locale string) *httptest.ResponseRecorder {
	t.Helper()
	initControlI18n(t)
	engine := gin.New()
	newTestServer(t, &config.Config{AuthKey: "test-auth-key"}, f.service).RegisterRoutes(engine)
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-auth-key")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept-Language", locale)
	r.Header.Set("Idempotency-Key", "00000000-0000-4000-8000-00000000d001")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, r)
	return w
}

func assertDependencyResponse(t *testing.T, w *httptest.ResponseRecorder, reason configdependency.Reason) configdependency.Error {
	t.Helper()
	var b struct {
		Code    string                 `json:"code"`
		Message string                 `json:"message"`
		Data    configdependency.Error `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusConflict || b.Code != "CONFIG_DEPENDENCY_CONFLICT" || b.Data.Reason != reason {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
	}
	if b.Message == "" || strings.Contains(b.Message, "{{") || strings.Contains(b.Message, "config_dependency.") {
		t.Fatalf("missing localized message: %s", w.Body.String())
	}
	for _, secret := range []string{"sk-local-dependency-test", "test-auth-key", "control-test-master", "base_url", "encrypted"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("response exposed %q", secret)
		}
	}
	return b.Data
}

func TestJevDependencyHTTPRejectsGroupMutationsAndRollsBack(t *testing.T) {
	for _, feature := range []string{"auto_model", "request_audit", "both"} {
		for _, action := range []string{"delete", "disable", "channel", "remove_model", "rename_alias"} {
			t.Run(feature+"/"+action, func(t *testing.T) {
				f, id, _ := dependencyFixture(t, feature != "request_audit", feature != "auto_model", true)
				var before models.Group
				if err := f.db.First(&before, id).Error; err != nil {
					t.Fatal(err)
				}
				snapshot := f.manager.Current()
				registry := f.registry.Snapshot()
				prices := f.priceRuntime.Load()
				method, path, body := "PUT", fmt.Sprintf("/api/groups/%d/settings", id), `{"enabled":false}`
				switch action {
				case "delete":
					method, path, body = "DELETE", fmt.Sprintf("/api/groups/%d", id), ""
				case "channel":
					path, body = fmt.Sprintf("/api/groups/%d/channel", id), `{"channel_id":"openai","confirm_same_target":true}`
				case "remove_model":
					path, body = fmt.Sprintf("/api/groups/%d/models", id), `{"models":[]}`
				case "rename_alias":
					path, body = fmt.Sprintf("/api/groups/%d/models", id), `{"models":[{"id":"jev-latest","alias":"different","alias_enabled":true}]}`
				}
				w := dependencyHTTP(t, f, method, path, body, "zh-CN")
				data := assertDependencyResponse(t, w, configdependency.JevRouteUnavailable)
				if data.GroupID != id || data.Model != "jev-latest" || !strings.Contains(w.Body.String(), "JEV 路由不可用") {
					t.Fatalf("missing route detail: %s", w.Body.String())
				}
				want := []string{feature}
				if feature == "both" {
					want = []string{"auto_model", "request_audit"}
				}
				if !reflect.DeepEqual(data.Features, want) {
					t.Fatalf("features=%v want %v", data.Features, want)
				}
				var after models.Group
				if err := f.db.First(&after, id).Error; err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) || snapshot != f.manager.Current() || !reflect.DeepEqual(registry, f.registry.Snapshot()) || prices != f.priceRuntime.Load() {
					t.Fatal("rejected mutation changed persisted or published state")
				}
			})
		}
	}
}

func TestAutoPresetDependencyHTTPAndSharedNameSemantics(t *testing.T) {
	for _, action := range []string{"delete", "remove_model", "rename_alias", "disabled_target", "remaining_disabled_target", "auto_disabled"} {
		t.Run(action, func(t *testing.T) {
			f, _, id := dependencyFixture(t, action != "auto_disabled", false, true)
			if action == "remaining_disabled_target" {
				duplicate := validControlGroup("remaining-target")
				duplicate.Models = models.JSON(`[{"id":"target-model"}]`)
				duplicate.Enabled = false
				if err := f.db.Create(duplicate).Error; err != nil {
					t.Fatal(err)
				}
			}
			method, path, body := "DELETE", fmt.Sprintf("/api/groups/%d", id), ""
			switch action {
			case "remove_model":
				method, path, body = "PUT", path+"/models", `{"models":[]}`
			case "rename_alias":
				method, path, body = "PUT", path+"/models", `{"models":[{"id":"target-model","alias":"new-target","alias_enabled":true}]}`
			case "disabled_target":
				method, path, body = "PUT", path+"/settings", `{"enabled":false}`
			}
			before := f.manager.Current()
			w := dependencyHTTP(t, f, method, path, body, "en-US")
			if action == "disabled_target" || action == "remaining_disabled_target" || action == "auto_disabled" {
				if w.Code != 200 {
					t.Fatalf("allowed operation rejected: %d %s", w.Code, w.Body.String())
				}
				return
			}
			data := assertDependencyResponse(t, w, configdependency.AutoModelTargetMissing)
			if data.AutoModel != "auto" || data.Preset != "Default preset" || data.Model != "target-model" || !strings.Contains(w.Body.String(), "Change the preset target") {
				t.Fatalf("missing preset detail: %s", w.Body.String())
			}
			if f.manager.Current() != before {
				t.Fatal("snapshot changed on rejection")
			}
		})
	}
}

func TestJevDependencyPreservesAllowedDeletion(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		auto, explicit, duplicate, allowed bool
	}{
		{"disabled features", false, true, false, true},
		{"implicit last route", true, false, false, false},
		{"implicit shared route", true, false, true, true},
		{"explicit shared name", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, id, _ := dependencyFixture(t, tc.auto, false, tc.explicit)
			if tc.duplicate {
				group := models.Group{Name: "other-decisions", ChannelID: "jev", ConnectionType: "api_key", Params: models.JSON(`{}`), Models: models.JSON(`[{"id":"jev-latest"}]`), Overrides: models.JSON(`{}`), Enabled: true}
				if err := f.db.Create(&group).Error; err != nil {
					t.Fatal(err)
				}
			}
			w := dependencyHTTP(t, f, "DELETE", fmt.Sprintf("/api/groups/%d", id), "", "ja-JP")
			if tc.allowed {
				if w.Code != 200 {
					t.Fatalf("allowed delete: %d %s", w.Code, w.Body.String())
				}
				return
			}
			data := assertDependencyResponse(t, w, configdependency.JevRouteUnavailable)
			if !tc.explicit && data.GroupID != 0 {
				t.Fatalf("implicit route invented group: %+v", data)
			}
			if !strings.Contains(w.Body.String(), "保存できません") {
				t.Fatal(w.Body.String())
			}
		})
	}
}

func TestDependencySettingsAndModelNameConflictHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reason     configdependency.Reason
	}{
		{"guardrail missing route", `{"settings":{"jev":{"model":"","group_id":0,"timeout_seconds":5},"request_audit":{"enabled":true}}}`, configdependency.JevRouteRequired},
		{"missing implicit route", `{"settings":{"jev":{"model":"absent","group_id":0,"timeout_seconds":5}}}`, configdependency.JevRouteUnavailable},
		{"missing fallback", `{"settings":{"auto_model":{"enabled":false,"models":[{"id":"a","name":"auto","fallback":"missing","presets":[{"id":"p","name":"P","description":"D","model":"target-model","parameter_overrides":[]}]}]}}}`, configdependency.AutoModelFallbackMissing},
		{"recursive target", `{"settings":{"auto_model":{"enabled":false,"models":[{"id":"a","name":"auto","fallback":"p","presets":[{"id":"p","name":"P","description":"D","model":"auto","parameter_overrides":[]}]}]}}}`, configdependency.AutoModelRecursiveTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, _ := dependencyFixture(t, true, false, true)
			before := f.manager.Current()
			settingsBefore, err := f.service.GetSettings(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			w := dependencyHTTP(t, f, "PUT", "/api/settings", tc.body, "zh-CN")
			assertDependencyResponse(t, w, tc.reason)
			settingsAfter, err := f.service.GetSettings(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if before != f.manager.Current() || !reflect.DeepEqual(settingsBefore, settingsAfter) {
				t.Fatal("rejected settings persisted")
			}
		})
	}
	for _, auto := range []bool{false, true} {
		t.Run(fmt.Sprintf("name conflict auto=%t", auto), func(t *testing.T) {
			f, _, id := dependencyFixture(t, auto, false, true)
			w := dependencyHTTP(t, f, "PUT", fmt.Sprintf("/api/groups/%d/models", id), `{"models":[{"id":"target-model","alias_enabled":false},{"id":"new","alias":"auto","alias_enabled":true}]}`, "zh-CN")
			data := assertDependencyResponse(t, w, configdependency.AutoModelNameConflict)
			if data.AutoModel != "auto" || data.Model != "auto" {
				t.Fatal(data)
			}
			// POST uses the idempotent transaction path, not writeGroupConfig.
			body := `{"name":"collision","channel_id":"openai","connection_type":"api_key","params":{},"confirm_same_target":true,"models":[{"id":"auto","alias_enabled":false}],"credentials":[{"fields":{"api_key":"sk-local-dependency-test"}}]}`
			for attempt := 0; attempt < 2; attempt++ {
				w = dependencyHTTP(t, f, "POST", "/api/groups", body, "en-US")
				assertDependencyResponse(t, w, configdependency.AutoModelNameConflict)
			}
		})
	}
}

func TestMapConfigCompileErrorPreservesUnknownFailures(t *testing.T) {
	unknown := errors.New("private compiler diagnostics")
	if mapConfigCompileError(unknown) != unknown {
		t.Fatal("unknown failure misclassified")
	}
	if mapConfigCompileError(fmt.Errorf("wrapped: %w", automodel.ErrInvalidConfig)) != app_errors.ErrValidation {
		t.Fatal("invalid auto config not mapped")
	}
}
