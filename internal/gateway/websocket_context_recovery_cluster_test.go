package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"gpt-load/internal/channel"
	"gpt-load/internal/cluster"
	"gpt-load/internal/state"
	"gpt-load/internal/testutil/clustertest"
)

func TestWebsocketRecoverySharesRejectionsAcrossReplicas(t *testing.T) {
	for _, id := range []channel.ID{channel.CLIProxyAPI, channel.OpenAI} {
		t.Run(string(id), func(t *testing.T) {
			_, client := clustertest.NewClient(t)
			observed := &encryptedRecoveryBodies{}
			upstream := websocketContextRecoveryUpstream(t, observed, false, rejectEncryptedContent)
			a, engineA, _ := websocketTestHandler(t, upstream.URL, id)
			b, engineB, _ := websocketTestHandler(t, upstream.URL, id)
			a.invalidLineage = cluster.NewInvalidLineage(client)
			b.invalidLineage = cluster.NewInvalidLineage(client)
			serverA, serverB := httptest.NewServer(engineA), httptest.NewServer(engineB)
			defer serverA.Close()
			defer serverB.Close()
			conn := dialGatewayWebsocket(t, serverA.URL)
			websocketContextRecoveryExchange(t, conn, `{"type":"response.create","model":"public","store":false,"input":"warmup"}`)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(websocketContextRecoveryTurn)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
				t.Fatalf("want reconnect: %v", err)
			}
			if reply := websocketContextRecoveryExchange(t, dialGatewayWebsocket(t, serverB.URL), websocketContextRecoveryTurn); !strings.Contains(reply, "response.completed") {
				t.Fatal(reply)
			}
			_, bodies := observed.snapshot()
			if len(bodies) != 3 || strings.Contains(bodies[2], "opaque") || !strings.Contains(bodies[2], "rs_old") {
				t.Fatalf("cross-replica cleanup: %v", bodies)
			}
		})
	}
}

func TestWebsocketRecoveryDoesNotReplayChangedIdentity(t *testing.T) {
	observed := &encryptedRecoveryBodies{}
	var change atomic.Pointer[func()]
	upstream := websocketContextRecoveryUpstream(t, observed, false, func(key, body string) string {
		rejected := rejectEncryptedContent(key, body)
		if rejected != "" {
			if fn := change.Load(); fn != nil {
				(*fn)()
			}
		}
		return rejected
	})
	handler, engine, _ := websocketTestHandler(t, upstream.URL, channel.CLIProxyAPI)
	replaced := testCredentialEntry(t, handler.encryption, 1, 1, "other-account")
	replaced.IdentityGeneration++
	fn := func() {
		if err := handler.registry.(*state.CredentialRegistry).ReplaceCredentials([]state.CredentialEntry{replaced}); err != nil {
			t.Error(err)
		}
	}
	change.Store(&fn)
	server := httptest.NewServer(engine)
	defer server.Close()
	reply := websocketContextRecoveryExchange(t, dialGatewayWebsocket(t, server.URL), websocketContextRecoveryTurn)
	if !strings.Contains(reply, "invalid_encrypted_content") {
		t.Fatal(reply)
	}
	if _, bodies := observed.snapshot(); len(bodies) != 1 {
		t.Fatalf("identity change replayed %d turns", len(bodies))
	}
}

func TestWebsocketRecoveryDoesNotReplayDeliveredOutput(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			calls.Add(1)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","response_id":"resp_partial","delta":"delivered"}`)); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(websocketRejection("invalid_encrypted_content", "The encrypted content could not be verified."))); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	_, engine, _ := websocketTestHandler(t, upstream.URL, channel.CLIProxyAPI)
	server := httptest.NewServer(engine)
	defer server.Close()
	conn := dialGatewayWebsocket(t, server.URL)
	if reply := websocketContextRecoveryExchange(t, conn, websocketContextRecoveryTurn); !strings.Contains(reply, "delivered") {
		t.Fatal(reply)
	}
	_, reply, err := conn.ReadMessage()
	if err != nil || !strings.Contains(string(reply), "invalid_encrypted_content") {
		t.Fatalf("late error=%s %v", reply, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("delivered output replayed %d times", calls.Load())
	}
}

func TestWebsocketRecoveryContinuesWhenLineageRedisFails(t *testing.T) {
	observed := &encryptedRecoveryBodies{}
	upstream := websocketContextRecoveryUpstream(t, observed, false, rejectEncryptedContent)
	handler, engine, _ := websocketTestHandler(t, upstream.URL, channel.CLIProxyAPI)
	redis, client := clustertest.NewClient(t)
	redis.Close()
	handler.invalidLineage = cluster.NewInvalidLineage(client)
	server := httptest.NewServer(engine)
	defer server.Close()
	if reply := websocketContextRecoveryExchange(t, dialGatewayWebsocket(t, server.URL), websocketContextRecoveryTurn); !strings.Contains(reply, "response.completed") {
		t.Fatal(reply)
	}
	if keys, bodies := observed.snapshot(); len(keys) != 2 || keys[0] != keys[1] || strings.Contains(bodies[1], "opaque") {
		t.Fatalf("cache failure prevented recovery: %v %v", keys, bodies)
	}
}
