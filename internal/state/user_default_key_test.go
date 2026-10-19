package state

import (
	"encoding/json"
	"testing"

	"gpt-load/internal/channel"
)

func TestCompileIndexesActiveUserDefaultKeysByNote(t *testing.T) {
	t.Parallel()
	key := func(id uint, name, note string, status AccessKeyStatus) AccessKeyConfig {
		return AccessKeyConfig{ID: id, Name: name, Note: note, KeyHash: name, Status: status}
	}
	snapshot, err := Compile(CompileInput{
		ChannelRegistry: channel.NewRegistry(),
		Groups: []GroupConfig{{
			ID: 1, ChannelID: channel.OpenAI, ConnectionType: "api_key", Params: json.RawMessage(`{}`), Enabled: true,
		}},
		AccessKeys: []AccessKeyConfig{
			key(3, "ly:default:k3", "user-1", AccessKeyStatusActive),
			key(4, "ly:personal", "user-2", AccessKeyStatusActive),
			key(5, "ly:default:off", "user-3", AccessKeyStatusDisabled),
			key(6, "ly:default:nobody", "", AccessKeyStatusActive),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint{"user-1": 3}
	if len(snapshot.UserDefaultKeys) != len(want) || snapshot.UserDefaultKeys["user-1"] != want["user-1"] {
		t.Fatalf("UserDefaultKeys = %+v, want %+v", snapshot.UserDefaultKeys, want)
	}
}
