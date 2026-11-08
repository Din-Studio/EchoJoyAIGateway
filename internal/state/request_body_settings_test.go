package state

import (
	"testing"

	"gpt-load/internal/platform/config"
)

func TestDefaultAccessKeyRecordRequestBodySetting(t *testing.T) {
	const key = SettingDefaultAccessKeyRecordRequestBody
	if !IsRuntimeSettingKey(key) || DefaultRuntimeSettings().DefaultAccessKeyRecordRequestBody {
		t.Fatal("recording default must be a public setting, disabled by default")
	}
	for _, enabled := range []bool{false, true} {
		if err := ValidateRuntimeSetting(key, enabled); err != nil {
			t.Fatal(err)
		}
		resolved, err := ResolveRuntimeSettings(config.Settings{key: enabled})
		if err != nil || resolved.DefaultAccessKeyRecordRequestBody != enabled {
			t.Fatalf("resolve %v: %+v, %v", enabled, resolved, err)
		}
		if _, err := ResolveGroupRuntimeSettings(DefaultRuntimeSettings(), config.Settings{key: enabled}); err == nil {
			t.Fatal("creation default must not be a group setting")
		}
	}
	for _, invalid := range []any{nil, "true", 1, []any{}, map[string]any{}} {
		if err := ValidateRuntimeSetting(key, invalid); err == nil {
			t.Fatalf("accepted invalid setting: %#v", invalid)
		}
		if _, err := ResolveRuntimeSettings(config.Settings{key: invalid}); err == nil {
			t.Fatalf("resolved invalid setting: %#v", invalid)
		}
	}
}
