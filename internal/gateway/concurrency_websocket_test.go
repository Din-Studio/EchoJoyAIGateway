package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gpt-load/internal/channel"
	"gpt-load/internal/platform/config"
)

func TestConcurrencyWebsocketTurnsAndHotLimits(t *testing.T) {
	for _, layer := range []string{"global", "key", "group"} {
		t.Run(layer, func(t *testing.T) {
			started, finish := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(finish) })
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				n := calls.Add(1)
				if n == 1 {
					close(started)
					<-finish
				}
				_ = conn.WriteMessage(websocket.TextMessage, websocketCompleted(fmt.Sprintf("resp_%d", n), ""))
				_, _, _ = conn.ReadMessage()
			}))
			defer upstream.Close()
			h, engine, input := websocketTestHandler(t, upstream.URL, channel.OpenAI)
			// 回合开始前就配置上限：未配上限的作用域不占槽，也就无法观察。
			input.SystemSettings = config.Settings{}
			switch layer {
			case "global":
				input.SystemSettings["global_concurrency_limit"] = 2
			case "key":
				input.SystemSettings["default_access_key_concurrency_limit"] = 2
			case "group":
				input.Groups[0].Settings = config.Settings{"concurrency_limit": 2}
			}
			if _, err := h.manager.Publish(input); err != nil {
				t.Fatal(err)
			}
			sink := &recordingRequestLogSink{}
			h.requestLogSink = sink
			server := httptest.NewServer(engine)
			defer server.Close()
			first := dialGatewayWebsocket(t, server.URL)
			defer first.Close()
			second := dialGatewayWebsocket(t, server.URL)
			defer second.Close()
			defer unblock()
			if !concurrencySnapshot(t, h).empty() {
				t.Fatal("idle socket counted as a request")
			}
			payload := []byte(`{"type":"response.create","model":"public","input":"hello"}`)
			if err := first.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("first turn did not start")
			}
			counts := concurrencySnapshot(t, h)
			if counts.Global+counts.AccessKeys[1]+counts.Groups[1] != 1 {
				t.Fatalf("active turn not counted: %+v", counts)
			}
			// 热更新上限到 1：旧连接的下一回合立即受新上限约束。
			switch layer {
			case "global":
				input.SystemSettings["global_concurrency_limit"] = 1
			case "key":
				input.SystemSettings["default_access_key_concurrency_limit"] = 1
			case "group":
				input.Groups[0].Settings = config.Settings{"concurrency_limit": 1}
			}
			if _, err := h.manager.Publish(input); err != nil {
				t.Fatal(err)
			}
			if err := second.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatal(err)
			}
			_ = second.SetReadDeadline(time.Now().Add(3 * time.Second))
			var event struct {
				Type  string
				Error struct{ Code string }
			}
			if err := second.ReadJSON(&event); err != nil {
				t.Fatal(err)
			}
			if event.Type != "error" || event.Error.Code != "concurrency_limit_exceeded" || calls.Load() != 1 {
				t.Fatalf("old connection ignored new capacity: %+v", event)
			}
			unblock()
			_ = first.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, _, err := first.ReadMessage(); err != nil {
				t.Fatal(err)
			}
			waitWebsocketLogs(t, sink, 2)
			if !concurrencySnapshot(t, h).empty() {
				t.Fatal("finished turn retained capacity")
			}
			if err := second.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatal(err)
			}
			if _, _, err := second.ReadMessage(); err != nil {
				t.Fatalf("capacity rejection broke connection: %v", err)
			}
			waitWebsocketLogs(t, sink, 3)
		})
	}
}

func TestConcurrencyWebsocketMultiplexRejectsOnlyNewTurn(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(finish) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		close(started)
		<-finish
		_ = conn.WriteMessage(websocket.TextMessage, websocketCompleted("resp_first", "first"))
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	h, engine, input := websocketTestHandler(t, upstream.URL, channel.GPTLoad)
	input.SystemSettings = config.Settings{"global_concurrency_limit": 1}
	input.Groups[0].Settings = config.Settings{"concurrency_limit": 1}
	if _, err := h.manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	sink := &recordingRequestLogSink{}
	h.requestLogSink = sink
	server := httptest.NewServer(engine)
	defer server.Close()
	conn := dialGatewayWebsocket(t, server.URL)
	defer conn.Close()
	defer unblock()
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","stream_id":"first","model":"public","input":"first"}`))
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("multiplex first turn did not start")
	}
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","stream_id":"second","model":"public","input":"second"}`))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var event struct {
		Type     string
		StreamID string `json:"stream_id"`
		Error    struct{ Code string }
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "error" || event.StreamID != "second" || event.Error.Code != "concurrency_limit_exceeded" {
		t.Fatalf("wrong turn rejected: %+v", event)
	}
	waitWebsocketLogs(t, sink, 1)
	if s := concurrencySnapshot(t, h); s.Global != 1 || s.Groups[1] != 1 {
		t.Fatalf("rejection ended active turn: %+v", s)
	}
	unblock()
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "response.completed" || event.StreamID != "first" {
		t.Fatalf("active turn corrupted: %+v", event)
	}
	waitWebsocketLogs(t, sink, 2)
}
