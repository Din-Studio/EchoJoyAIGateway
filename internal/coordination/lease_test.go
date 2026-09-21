package coordination

import (
	"testing"
)

// Lease key names are a cross-instance contract: two instances running
// different builds must agree on them, so a later KeyPrefix change has to
// break this test rather than drift silently.
func TestLeaseKeyNamesAreStable(t *testing.T) {
	for task, want := range map[string]string{
		"requestlog_retention":     "gw:lease:requestlog_retention",
		"credential_stage_cleanup": "gw:lease:credential_stage_cleanup",
		"operation_compaction":     "gw:lease:operation_compaction",
	} {
		if got := leaseKey(task); got != want {
			t.Errorf("leaseKey(%q) = %q, want %q", task, got, want)
		}
	}
}
