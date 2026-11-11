package codexrecovery

import "testing"

func TestEncryptedContentDigestsCollectNestedValues(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"opaque-1"},
			{"type":"function_call_output","call_id":"call_1","output":[{"type":"encrypted_content","encrypted_content":"opaque-2"}]}
		]
	}`)
	digests := EncryptedContentDigests(body)
	if len(digests) != 2 {
		t.Fatalf("digests = %v, want 2", digests)
	}
}

func TestHistoryItemIDsCollectsReplayableItems(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"message","id":"msg_1","role":"assistant","content":"history"},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"summary"}]},
			{"type":"item_reference","id":"rs_server_only"},
			{"type":"web_search_call","id":"ws_unknown"},
			{"type":"message","id":"msg_no_content","role":"user"}
		]
	}`)
	ids := HistoryItemIDs(body)
	if len(ids) != 2 {
		t.Fatalf("history item IDs = %v, want only the 2 replayable items", ids)
	}
}
