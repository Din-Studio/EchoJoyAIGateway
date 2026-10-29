package gateway

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/reasoning"
	"gpt-load/internal/telemetry"
)

func TestWebsocketLogsAppliedReasoning(t *testing.T) {
	h, engine, _ := websocketTestHandler(t, "http://127.0.0.1:1", channel.CLIProxyAPI)
	sink := &recordingRequestLogSink{}
	h.requestLogSink = sink
	h.forwarder = websocketScriptForwarder{AttemptForwarder: h.forwarder, open: func(context.Context, ForwardInput) (execution.WebsocketSession, execution.WebsocketResult) {
		session := &websocketScriptSession{done: make(chan struct{})}
		session.turn = func(ctx context.Context, _ []byte, emit func(context.Context, []byte) error) execution.WebsocketResult {
			// 上游响应报告请求基线，执行结果报告配置更新后的实际强度。
			if err := emit(ctx, []byte(`{"type":"response.completed","response":{"id":"resp_effort","object":"response","status":"completed","model":"upstream","reasoning":{"effort":"high"},"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`)); err != nil {
				return execution.WebsocketResult{DispatchState: execution.DispatchMaybeSent, Error: &execution.ErrorEvidence{Kind: execution.ErrorKindCanceled}}
			}
			return execution.WebsocketResult{DispatchState: execution.DispatchMaybeSent, AppliedReasoning: &reasoning.Config{Effort: "max"}}
		}
		return session, execution.WebsocketResult{DispatchState: execution.DispatchNotSent}
	}}
	server := httptest.NewServer(engine)
	defer server.Close()
	conn := dialGatewayWebsocket(t, server.URL)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"public","input":"hello","reasoning":{"effort":"high"},"store":false}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	event := waitWebsocketLogs(t, sink, 1)[0]
	if event.Status != telemetry.RequestStatusSuccess || len(event.Attempts) != 1 {
		t.Fatalf("请求未成功完成：%+v", event)
	}
	if event.Reasoning.Effort != "max" || event.Attempts[0].Reasoning.Effort != "max" {
		t.Fatalf("实际强度未写入日志：request=%q attempt=%q", event.Reasoning.Effort, event.Attempts[0].Reasoning.Effort)
	}
}
