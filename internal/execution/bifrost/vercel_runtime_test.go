package bifrost

import (
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
