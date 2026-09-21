package coordination

import "testing"

// The key and channel names are a cross-instance contract: instances running
// different builds must agree on them, so a later KeyPrefix change has to
// break this test rather than drift silently.
func TestConfigVersionNamesAreStable(t *testing.T) {
	version := NewConfigVersion(nil)
	if version.key != "gw:config:version" {
		t.Errorf("key = %q, want %q", version.key, "gw:config:version")
	}
	if version.channel != "gw:config" {
		t.Errorf("channel = %q, want %q", version.channel, "gw:config")
	}
}
