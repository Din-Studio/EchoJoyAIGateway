package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/provideradapter"
)

func TestVercelNativeProtocolsUseGatewayRootAndKeepRoutingOptions(t *testing.T) {
	t.Parallel()

	const model = "anthropic/claude-sonnet-4.5"
	const providerOptions = `{"gateway":{"order":["anthropic","bedrock"],"only":["anthropic"]}}`
	tests := []struct {
		name             string
		clientProtocol   protocol.Protocol
		operation        execution.Operation
		path             string
		body             string
		credentialHeader string
		response         string
	}{
		{
			name: "completions", clientProtocol: protocol.OpenAICompletions,
			operation: execution.OperationChatCompletion, path: "/v1/chat/completions",
			body:             `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"providerOptions":` + providerOptions + `}`,
			credentialHeader: "Authorization",
			response:         `{"id":"chat_1","object":"chat.completion","created":1,"model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		},
		{
			name: "responses", clientProtocol: protocol.OpenAIResponses,
			operation: execution.OperationResponsesCreate, path: "/v1/responses",
			body:             `{"model":"` + model + `","input":"hello","store":false,"providerOptions":` + providerOptions + `}`,
			credentialHeader: "Authorization",
			response:         `{"id":"resp_1","object":"response","status":"completed","model":"` + model + `","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
		},
		{
			name: "embeddings", clientProtocol: protocol.OpenAIEmbeddings,
			operation: execution.OperationEmbeddingsCreate, path: "/v1/embeddings",
			body:             `{"model":"` + model + `","input":"hello","providerOptions":` + providerOptions + `}`,
			credentialHeader: "Authorization",
			response:         `{"object":"list","model":"` + model + `","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		},
		{
			name: "anthropic messages", clientProtocol: protocol.Anthropic,
			operation: execution.OperationChatCompletion, path: "/v1/messages",
			body:             `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"providerOptions":` + providerOptions + `}`,
			credentialHeader: "X-Api-Key",
			response:         `{"id":"msg_1","type":"message","role":"assistant","model":"` + model + `","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			name: "anthropic count tokens", clientProtocol: protocol.Anthropic,
			operation: execution.OperationCountTokens, path: "/v1/messages/count_tokens",
			body:             `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"providerOptions":` + providerOptions + `}`,
			credentialHeader: "X-Api-Key",
			response:         `{"input_tokens":7}`,
		},
	}

	var wantOptions any
	if err := json.Unmarshal([]byte(providerOptions), &wantOptions); err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != test.path {
					t.Errorf("upstream target = %s %s", request.Method, request.URL.Path)
				}
				wantCredential := testAPIKey
				if test.credentialHeader == "Authorization" {
					wantCredential = "Bearer " + testAPIKey
				}
				if request.Header.Get(test.credentialHeader) != wantCredential {
					t.Errorf("credential %s = %q", test.credentialHeader, request.Header.Get(test.credentialHeader))
				}
				if test.clientProtocol == protocol.Anthropic && request.Header.Get("Anthropic-Version") == "" {
					t.Error("Anthropic-Version is missing")
				}
				body, _ := io.ReadAll(request.Body)
				var payload map[string]any
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Errorf("decode upstream request: %v", err)
				}
				if payload["model"] != model {
					t.Errorf("upstream model = %#v", payload["model"])
				}
				if !reflect.DeepEqual(payload["providerOptions"], wantOptions) {
					t.Errorf("upstream providerOptions = %#v", payload["providerOptions"])
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, test.response)
			}))
			defer server.Close()

			registry := channel.NewRegistry()
			resolved, err := registry.Resolve(channel.Vercel, json.RawMessage(`{"base_url":"`+server.URL+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			manager, err := newRuntimeManager(runtimeOptions{allowPrivateNetwork: true}, registry)
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer manager.Shutdown()
			if err := manager.Reconcile([]provideradapter.RuntimeTarget{{Target: resolved}}); err != nil {
				t.Fatal(err)
			}

			spec := freezeTestAttempt(execution.AttemptSpec{
				RequestID: "vercel-request", AttemptID: "vercel-attempt", Sequence: 1,
				ChannelID: string(channel.Vercel), ClientProtocol: test.clientProtocol,
				Operation: test.operation, ClientModel: model, UpstreamModel: model,
				Method: http.MethodPost, Path: test.path,
				Header:       http.Header{"Content-Type": {"application/json"}},
				Body:         []byte(test.body),
				TargetConfig: resolved.TargetConfig,
				Credential: execution.NewCredentialSnapshot(
					17, 1, 1, []byte(`{"api_key":"`+testAPIKey+`"}`),
				),
			})
			result := manager.Execute(context.Background(), spec)
			if err := result.Validate(); err != nil {
				t.Fatalf("result validation: %v; result=%+v", err, result)
			}
			if result.Error != nil || result.StatusCode != http.StatusOK || result.UpstreamProtocol != test.clientProtocol {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestVercelDefaultTargetUsesFixedGatewayRoot(t *testing.T) {
	t.Parallel()

	registry := channel.NewRegistry()
	resolved, err := registry.Resolve(channel.Vercel, nil)
	if err != nil {
		t.Fatal(err)
	}
	for clientProtocol, wantProvider := range map[protocol.Protocol]schemas.ModelProvider{
		protocol.OpenAICompletions: schemas.OpenAI,
		protocol.OpenAIResponses:   schemas.OpenAI,
		protocol.OpenAIEmbeddings:  schemas.OpenAI,
		protocol.Anthropic:         schemas.Anthropic,
	} {
		config, err := buildEffectiveProviderConfigForAttempt(
			resolved,
			execution.AttemptSpec{ClientProtocol: clientProtocol, RouteMode: execution.RouteNative},
			false,
		)
		if err != nil {
			t.Fatalf("%s: buildEffectiveProviderConfigForAttempt() error = %v", clientProtocol, err)
		}
		if config.provider != wantProvider || config.custom ||
			config.targetBaseURL != "https://ai-gateway.vercel.sh" ||
			config.providerConfig.NetworkConfig.BaseURL != "https://ai-gateway.vercel.sh" {
			t.Fatalf("%s: config = provider %q custom %t target %q network %q", clientProtocol,
				config.provider, config.custom, config.targetBaseURL, config.providerConfig.NetworkConfig.BaseURL)
		}
	}
}

func TestVercelRouteCapabilityMatchesDeclaredOperations(t *testing.T) {
	t.Parallel()

	registry := channel.NewRegistry()
	manager, err := newRuntimeManager(runtimeOptions{}, registry)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, ok := registry.Get(channel.Vercel)
	if !ok {
		t.Fatal("Vercel channel is missing")
	}
	for _, route := range descriptor.Routes {
		if err := manager.ValidateRouteCapability(channel.ProviderMultiProtocolGateway, route); err != nil {
			t.Errorf("route %s/%s/%s rejected: %v", route.ClientProtocol, route.Operation, route.RouteMode, err)
		}
	}
}

const vercelProviderMetadata = `{"gateway":{"routing":{"resolvedProvider":"baseten"},"cost":"0.0000336","generationId":"gen_1"}}`

func TestVercelNativeResponsesScrubPlatformMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		clientProtocol protocol.Protocol
		operation      execution.Operation
		path           string
		body           string
		stream         bool
		response       string
	}{
		{
			name: "completions", clientProtocol: protocol.OpenAICompletions,
			operation: execution.OperationChatCompletion, path: "/v1/chat/completions",
			body:     `{"model":"provider-model","messages":[{"role":"user","content":"hello"}]}`,
			response: `{"id":"gen_1","generationId":"gen_1","object":"chat.completion","created":1,"model":"provider-model","choices":[{"index":0,"message":{"role":"assistant","content":"pong","provider_metadata":` + vercelProviderMetadata + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"cost":0.0000336,"market_cost":0.0000336,"gateway_cost":0.0000336,"is_byok":false,"cost_details":{"upstream_inference_cost":null}}}`,
		},
		{
			name: "responses", clientProtocol: protocol.OpenAIResponses,
			operation: execution.OperationResponsesCreate, path: "/v1/responses",
			body:     `{"model":"provider-model","input":"hello","store":false}`,
			response: `{"id":"gen_1","object":"response","status":"completed","model":"provider-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}],"provider_metadata":` + vercelProviderMetadata + `,"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
		},
		{
			name: "embeddings", clientProtocol: protocol.OpenAIEmbeddings,
			operation: execution.OperationEmbeddingsCreate, path: "/v1/embeddings",
			body:     `{"model":"provider-model","input":"pong"}`,
			response: `{"object":"list","model":"provider-model","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"provider_metadata":` + vercelProviderMetadata + `,"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		},
		{
			name: "anthropic messages", clientProtocol: protocol.Anthropic,
			operation: execution.OperationChatCompletion, path: "/v1/messages",
			body:     `{"model":"provider-model","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
			response: `{"id":"gen_1","type":"message","role":"assistant","model":"provider-model","content":[{"type":"text","text":"pong"}],"provider_metadata":` + vercelProviderMetadata + `,"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			name: "completions stream", clientProtocol: protocol.OpenAICompletions,
			operation: execution.OperationChatCompletion, path: "/v1/chat/completions", stream: true,
			body: `{"model":"provider-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			response: "data: {\"id\":\"gen_1\",\"object\":\"chat.completion.chunk\",\"model\":\"provider-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"}}]}\n\n" +
				"data: {\"id\":\"gen_1\",\"generationId\":\"gen_1\",\"object\":\"chat.completion.chunk\",\"model\":\"provider-model\",\"choices\":[{\"index\":0,\"delta\":{\"provider_metadata\":" + vercelProviderMetadata + "},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2,\"cost\":0.0000336}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "responses stream", clientProtocol: protocol.OpenAIResponses,
			operation: execution.OperationResponsesCreate, path: "/v1/responses", stream: true,
			body: `{"model":"provider-model","input":"hello","store":false,"stream":true}`,
			response: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"gen_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"provider-model\",\"output\":[],\"provider_metadata\":" + vercelProviderMetadata + ",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
		},
		{
			name: "anthropic messages stream", clientProtocol: protocol.Anthropic,
			operation: execution.OperationChatCompletion, path: "/v1/messages", stream: true,
			body: `{"model":"provider-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			response: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"gen_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"provider-model\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1},\"provider_metadata\":" + vercelProviderMetadata + "}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != test.path {
					t.Errorf("upstream path = %s", request.URL.Path)
				}
				contentType := "application/json"
				if test.stream {
					contentType = "text/event-stream"
				}
				writer.Header().Set("Content-Type", contentType)
				writer.Header().Set("X-Vercel-Id", "sin1::fra1::abc")
				writer.Header().Set("X-Vercel-Cache", "MISS")
				writer.Header().Set("X-Matched-Path", test.path)
				writer.Header().Set("X-Request-Id", "req_1")
				_, _ = io.WriteString(writer, test.response)
			}))
			defer server.Close()

			manager, resolved := newChannelManagerForTest(t, channel.Vercel, server.URL)
			spec := freezeTestAttempt(execution.AttemptSpec{
				RequestID: "vercel-request", AttemptID: "vercel-attempt", Sequence: 1,
				ChannelID: string(channel.Vercel), ClientProtocol: test.clientProtocol,
				Operation: test.operation, ClientModel: "public-model", UpstreamModel: "provider-model",
				Method: http.MethodPost, Path: test.path,
				Header:       http.Header{"Content-Type": {"application/json"}},
				Body:         []byte(test.body),
				TargetConfig: resolved.TargetConfig,
				Credential: execution.NewCredentialSnapshot(
					17, 1, 1, []byte(`{"api_key":"`+testAPIKey+`"}`),
				),
			})
			var body []byte
			var headers http.Header
			if test.stream {
				var data bytes.Buffer
				result := manager.ExecuteStream(context.Background(), spec, func(event execution.StreamEvent) error {
					switch event.Kind {
					case execution.StreamEventReady:
						headers = event.Header
					case execution.StreamEventData:
						data.Write(event.Data)
					}
					return nil
				})
				if err := result.Validate(); err != nil || result.Error != nil {
					t.Fatalf("stream result = %+v err=%v", result, err)
				}
				body = data.Bytes()
			} else {
				result := manager.Execute(context.Background(), spec)
				if err := result.Validate(); err != nil || result.Error != nil || result.StatusCode != http.StatusOK {
					t.Fatalf("result = %+v err=%v", result, err)
				}
				body, headers = result.Body, result.Header
			}

			for _, leaked := range []string{"provider_metadata", "generationId", "baseten", `"cost"`, "gateway_cost", "market_cost", "is_byok", "cost_details", "provider-model"} {
				if bytes.Contains(body, []byte(leaked)) {
					t.Errorf("response leaks %s: %s", leaked, body)
				}
			}
			keep := "pong"
			if test.clientProtocol == protocol.OpenAIEmbeddings {
				keep = "[0.1,0.2]"
			}
			if !bytes.Contains(body, []byte(`"public-model"`)) || !bytes.Contains(body, []byte(keep)) {
				t.Errorf("response lost content or alias: %s", body)
			}
			if headers.Get("X-Vercel-Id") != "" || headers.Get("X-Vercel-Cache") != "" || headers.Get("X-Matched-Path") != "" ||
				headers.Get("X-Request-Id") != "req_1" {
				t.Errorf("response headers = %#v", headers)
			}
		})
	}
}

func TestGatewayWithoutResponseScrubForwardsMetadataUnchanged(t *testing.T) {
	t.Parallel()

	response := `{"id":"gen_1","object":"chat.completion","created":1,"model":"same-model","choices":[{"index":0,"message":{"role":"assistant","content":"pong","provider_metadata":` + vercelProviderMetadata + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"cost":0.1}}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-Vercel-Id", "sin1::fra1::abc")
		_, _ = io.WriteString(writer, response)
	}))
	defer server.Close()

	manager, resolved := newChannelManagerForTest(t, channel.NewAPI, server.URL)
	result := manager.Execute(context.Background(), freezeTestAttempt(execution.AttemptSpec{
		RequestID: "newapi-request", AttemptID: "newapi-attempt", Sequence: 1,
		ChannelID: string(channel.NewAPI), ClientProtocol: protocol.OpenAICompletions,
		Operation: execution.OperationChatCompletion, ClientModel: "same-model", UpstreamModel: "same-model",
		Method: http.MethodPost, Path: "/v1/chat/completions",
		Header:       http.Header{"Content-Type": {"application/json"}},
		Body:         []byte(`{"model":"same-model","messages":[{"role":"user","content":"hello"}]}`),
		TargetConfig: resolved.TargetConfig,
		Credential: execution.NewCredentialSnapshot(
			17, 1, 1, []byte(`{"api_key":"`+testAPIKey+`"}`),
		),
	}))
	if err := result.Validate(); err != nil || result.Error != nil {
		t.Fatalf("result = %+v err=%v", result, err)
	}
	if string(result.Body) != response || result.Header.Get("X-Vercel-Id") == "" {
		t.Fatalf("unscrubbed channel changed response: headers=%#v body=%s", result.Header, result.Body)
	}
}

func newChannelManagerForTest(t *testing.T, id channel.ID, baseURL string) (*RuntimeManager, channel.ResolvedTarget) {
	t.Helper()

	registry := channel.NewRegistry()
	resolved, err := registry.Resolve(id, json.RawMessage(`{"base_url":"`+baseURL+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := newRuntimeManager(runtimeOptions{allowPrivateNetwork: true}, registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Shutdown)
	if err := manager.Reconcile([]provideradapter.RuntimeTarget{{Target: resolved}}); err != nil {
		t.Fatal(err)
	}
	return manager, resolved
}
