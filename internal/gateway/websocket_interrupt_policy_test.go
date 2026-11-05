package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gpt-load/internal/channel"
	"gpt-load/internal/state"
)

func TestWebsocketInterruptRechecksPolicyAndConnectionOwnership(t *testing.T) {
	for _, scenario := range []struct {
		name string
		want reason
	}{
		{"revoked access key", reasonInvalidAccessKey},
		{"disabled group", reasonWebsocketDisabled},
		{"deleted credential", reasonConfigurationChanged},
		{"disabled credential", reasonConfigurationChanged},
		{"replaced credential", reasonConfigurationChanged},
		{"another connection", reasonWebsocketResponseNotActive},
		{"unsupported session", reasonWebsocketCapability},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, _, input := websocketTestHandler(t, "http://unused.invalid", channel.OpenAI)
			registry := h.registry.(*state.CredentialRegistry)
			ref, ok := registry.CredentialRef(1)
			if !ok {
				t.Fatal("missing fixture credential")
			}
			session := &interruptibleWebsocketFixture{interrupts: make(chan []byte, 1)}
			binding := &websocketBinding{session: session, ref: ref}
			parents := map[string]websocketParent{"resp_1": {}}
			switch scenario.name {
			case "revoked access key":
				input.AccessKeys[0].Status = state.AccessKeyStatusDisabled
			case "disabled group":
				input.Groups[0].Enabled = false
			case "deleted credential":
				if err := registry.ReplaceCredentials(nil); err != nil {
					t.Fatal(err)
				}
			case "disabled credential", "replaced credential":
				entry := testCredentialEntry(t, h.encryption, 1, 1, "replacement-key")
				entry.IdentityGeneration++
				entry.Fingerprint = "replacement-fingerprint"
				if scenario.name == "disabled credential" {
					entry.Status = state.CredentialStatusDisabled
				}
				if err := registry.ReplaceCredentials([]state.CredentialEntry{entry}); err != nil {
					t.Fatal(err)
				}
			case "another connection":
				parents = map[string]websocketParent{}
			case "unsupported session":
				binding.session = &websocketScriptSession{}
			}
			if _, err := h.manager.Publish(input); err != nil {
				t.Fatal(err)
			}
			finished := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				s := &websocketConnection{handler: h, conn: conn, request: r, ctx: ctx, cancel: cancel,
					keyID: 1, keyHash: input.AccessKeys[0].KeyHash, write: make(chan struct{}, 1), binding: binding, parents: parents}
				body := []byte(`{"type":"response.interrupt","response_id":"resp_1"}`)
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Error(err)
					return
				}
				s.handleInterrupt(websocketTurn{body: body}, fields)
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			var event struct {
				Type  string `json:"type"`
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := conn.ReadJSON(&event); err != nil || event.Type != "error" || event.Error.Code != scenario.want.Code {
				t.Fatalf("event=%+v err=%v, want %s", event, err, scenario.want.Code)
			}
			<-finished
			select {
			case body := <-session.interrupts:
				t.Fatalf("rejected interrupt reached upstream: %s", body)
			default:
			}
		})
	}
}
