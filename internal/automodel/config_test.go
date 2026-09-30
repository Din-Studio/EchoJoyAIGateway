package automodel

import (
	"encoding/json"
	"testing"
)

func TestConfigUsesSingleEnableSwitchAndRejectsMissingTargets(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "secret"
	entry := Template()
	entry.Enabled = false
	config.Models = []Entry{entry}
	names := map[string]struct{}{"gpt-5.6-luna": {}, "gpt-5.6-terra": {}, "gpt-5.6-sol": {}, "gpt-6-astra": {}}
	compiled, err := Compile(config, names)
	if err != nil {
		t.Fatal(err)
	}
	if selected, exists := compiled.Lookup("auto"); !exists || !selected.Enabled {
		t.Fatal("automatic entry did not follow the system enable switch")
	}
	names["auto"] = struct{}{}
	if _, err := Compile(config, names); err == nil {
		t.Fatal("ordinary auto model was hijacked")
	}
	delete(names, "auto")
	delete(names, "gpt-5.6-sol")
	if _, err := Compile(config, names); err == nil {
		t.Fatal("missing enabled target accepted")
	}
	config.Enabled = false
	if _, err := Compile(config, names); err != nil {
		t.Fatalf("disabled draft cannot be edited: %v", err)
	}
}

func TestConfigRejectsProtectedContinuationOverrides(t *testing.T) {
	config := DefaultConfig()
	config.Models = []Entry{Template()}
	config.Models[0].Presets[0].ParameterOverrides = json.RawMessage(`[{"set":{"previous_response_id":"injected"}}]`)
	if _, err := Compile(config, nil); err == nil {
		t.Fatal("preset can rewrite continuation identity")
	}
}

func TestCompiledEntryFingerprintTracksOnlyItsOwnConfiguration(t *testing.T) {
	names := map[string]struct{}{"gpt-5.6-luna": {}, "gpt-5.6-terra": {}, "gpt-5.6-sol": {}, "gpt-6-astra": {}}
	build := func(edit func(*Config)) map[string]string {
		t.Helper()
		config := DefaultConfig()
		other := Template()
		other.ID, other.Name = "other", "other"
		config.Models = []Entry{Template(), other}
		if edit != nil {
			edit(&config)
		}
		compiled, err := Compile(config, names)
		if err != nil {
			t.Fatal(err)
		}
		fingerprints := map[string]string{}
		for _, name := range []string{"auto", "other"} {
			entry, exists := compiled.Lookup(name)
			if !exists || entry.Fingerprint == "" {
				t.Fatalf("entry %q fingerprint missing: %#v", name, entry)
			}
			fingerprints[name] = entry.Fingerprint
		}
		return fingerprints
	}

	base := build(nil)
	if again := build(nil); again["auto"] != base["auto"] || again["other"] != base["other"] {
		t.Fatalf("recompiled fingerprints = %v, want %v", again, base)
	}
	presetEdited := build(func(config *Config) { config.Models[0].Presets[0].Description = "changed" })
	if presetEdited["auto"] == base["auto"] || presetEdited["other"] != base["other"] {
		t.Fatalf("after editing auto's preset: %v, base %v", presetEdited, base)
	}
	otherEdited := build(func(config *Config) {
		config.Models[1].Presets[0].Description = "changed"
		config.TimeoutSeconds = 9
	})
	if otherEdited["auto"] != base["auto"] {
		t.Fatalf("auto fingerprint changed with an unrelated edit: %v, base %v", otherEdited, base)
	}
}
