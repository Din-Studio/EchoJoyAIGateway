package control

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpt-load/internal/platform/version"
)

// The exact-map comparison also proves no DSN, AUTH_KEY, or encryption key
// value is part of the response.
func TestSystemInfoResponseReportsOnlyDeploymentFacts(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(newSystemInfoResponse())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	want := map[string]any{
		"version":    version.Version,
		"deployment": map[string]any{"database": "postgres"},
		"auth_key":   map[string]any{"source": "environment"},
		"encryption": map[string]any{"source": "environment"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("system info = %#v, want %#v", got, want)
	}
}
