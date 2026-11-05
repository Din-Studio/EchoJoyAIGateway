package control

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gpt-load/internal/health"
	"gpt-load/internal/platform/config"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
)

func TestCustomErrorSettingsPersistPublishAndRespectEmptyOverride(t *testing.T) {
	pair := newClusterPair(t)
	fixture := pair.a
	assertRemote := func(groupID uint, want int) {
		t.Helper()
		pair.reloadB(t)
		if got := len(pair.b.manager.Current().Groups[groupID].ErrorRules.Rules()); got != want {
			t.Fatalf("remote rule count=%d, want %d", got, want)
		}
	}
	groupID := createGroupWithCredentials(t, fixture, "test-custom-error-key")
	const ruleJSON = `[{"status_codes":[403],"keywords":["欠费"],"retry":"next_candidate","effect":"record_credential_failure"}]`
	updated, err := fixture.service.UpdateSettings(t.Context(), SettingsUpdateRequest{Settings: map[string]json.RawMessage{
		state.SettingErrorRules: json.RawMessage(ruleJSON),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Values.ErrorRules) != 1 || len(fixture.manager.Current().Groups[groupID].ErrorRules.Rules()) != 1 {
		t.Fatal("system rules were not returned and inherited by the group")
	}
	assertRemote(groupID, 1)
	read, err := fixture.service.GetGroupSettings(t.Context(), groupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Effective.ErrorRules) != 1 {
		t.Fatal("effective group response omitted inherited rules")
	}
	_, err = fixture.service.UpdateGroupSettings(t.Context(), groupID, GroupSettingsUpdateRequest{
		Overrides: optionalField[config.Settings]{Set: true, Value: config.Settings{
			state.SettingErrorRules:         []health.ErrorRule{{Keywords: []string{"余额不足"}, Retry: health.RetryNone, Effect: health.EffectNone}},
			state.SettingBlacklistThreshold: 2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	read, err = fixture.service.GetGroupSettings(t.Context(), groupID)
	if err != nil || len(read.Effective.ErrorRules) != 1 || read.Effective.ErrorRules[0].Keywords[0] != "余额不足" {
		t.Fatalf("group rules did not round-trip: %v", err)
	}
	group, err := fixture.service.UpdateGroupSettings(t.Context(), groupID, GroupSettingsUpdateRequest{
		Overrides: optionalField[config.Settings]{Set: true, Value: config.Settings{
			state.SettingErrorRules: []any{}, state.SettingBlacklistThreshold: 2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := group.Overrides[state.SettingErrorRules]; !exists || len(group.Effective.ErrorRules) != 0 || len(group.DefaultErrorRules) != 1 || len(fixture.manager.Current().Groups[groupID].ErrorRules.Rules()) != 0 || group.Effective.BlacklistThreshold != 2 {
		t.Fatal("empty override did not suppress global rules")
	}
	assertRemote(groupID, 0)
	before := fixture.manager.Current()
	beforeRevision := pair.reloadB(t)
	_, err = fixture.service.UpdateSettings(t.Context(), SettingsUpdateRequest{Settings: map[string]json.RawMessage{
		state.SettingErrorRules: json.RawMessage(`[{"status_codes":[403],"retry":"refresh_credential","effect":"none"}]`),
	}})
	if !errors.Is(err, app_errors.ErrValidation) || fixture.manager.Current() != before {
		t.Fatalf("invalid system rules changed the snapshot: %v", err)
	}
	_, err = fixture.service.UpdateGroupSettings(t.Context(), groupID, GroupSettingsUpdateRequest{
		Overrides: optionalField[config.Settings]{Set: true, Value: config.Settings{state.SettingErrorRules: []any{map[string]any{
			"retry": "none", "effect": "none",
		}}}},
	})
	if !errors.Is(err, app_errors.ErrValidation) || fixture.manager.Current() != before {
		t.Fatalf("invalid group rules changed the snapshot: %v", err)
	}
	if revision := pair.reloadB(t); revision != beforeRevision {
		t.Fatalf("invalid update changed cluster revision: %d, want %d", revision, beforeRevision)
	}
	group, err = fixture.service.UpdateGroupSettings(t.Context(), groupID, GroupSettingsUpdateRequest{
		Overrides: optionalField[config.Settings]{Set: true, Value: config.Settings{}},
	})
	if err != nil || len(group.Effective.ErrorRules) != 1 {
		t.Fatalf("restoring inheritance failed: %v", err)
	}
	assertRemote(groupID, 1)
	reset, err := fixture.service.UpdateSettings(t.Context(), SettingsUpdateRequest{Settings: map[string]json.RawMessage{
		state.SettingErrorRules: json.RawMessage("null"),
	}})
	if err != nil || len(reset.Values.ErrorRules) != 0 {
		t.Fatalf("resetting system rules failed: %v", err)
	}
	assertRemote(groupID, 0)
}

func TestCustomErrorSettingsFitTextStorage(t *testing.T) {
	fixture := newServiceFixture(t)
	// 配置资源上限包含 JSON 及转义后的内容。
	const limit = 65535
	rules := []health.ErrorRule{{
		StatusCodes: []int{403}, Keywords: []string{"欠费🙂<>&\u2028\t"},
		Retry: health.RetryNone, Effect: health.EffectNone,
	}}
	raw, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	rules[0].Keywords[0] += strings.Repeat("x", limit-len(raw))
	raw, err = json.Marshal(rules)
	if err != nil || len(raw) != limit {
		t.Fatalf("boundary fixture size=%d error=%v", len(raw), err)
	}
	updated, err := fixture.service.UpdateSettings(t.Context(), SettingsUpdateRequest{Settings: map[string]json.RawMessage{
		state.SettingErrorRules: raw,
	}})
	if err != nil || len(updated.Values.ErrorRules) != 1 || updated.Values.ErrorRules[0].Keywords[0] != rules[0].Keywords[0] {
		t.Fatalf("boundary rules did not round-trip: %v", err)
	}
	var stored models.SystemSetting
	if err := fixture.db.Where(&models.SystemSetting{Key: state.SettingErrorRules}).Take(&stored).Error; err != nil || len(stored.Value) != limit {
		t.Fatalf("stored rules size=%d error=%v", len(stored.Value), err)
	}
	before := fixture.manager.Current()
	rules[0].Keywords[0] += "x"
	raw, err = json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.UpdateSettings(t.Context(), SettingsUpdateRequest{Settings: map[string]json.RawMessage{
		state.SettingErrorRules: raw,
	}})
	if !errors.Is(err, app_errors.ErrValidation) || fixture.manager.Current() != before {
		t.Fatalf("oversized rules must be rejected without publishing: %v", err)
	}
}
