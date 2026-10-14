package channel

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpt-load/internal/channel/spec"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
)

func TestVercelChannelContract(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	descriptor, ok := registry.Get(Vercel)
	if !ok {
		t.Fatal("Vercel channel is not registered")
	}
	if descriptor.Name != "Vercel AI Gateway" || descriptor.Mark != "VC" || descriptor.Icon != "vercel" {
		t.Fatalf("Vercel identity = %#v", descriptor)
	}
	if descriptor.Connection.Type != "api_key" || descriptor.Connection.CredentialInput != "entries" {
		t.Fatalf("Vercel connection = %#v", descriptor.Connection)
	}
	if len(descriptor.ParamFields) != 1 {
		t.Fatalf("Vercel params = %#v", descriptor.ParamFields)
	}
	if field := descriptor.ParamFields[0]; field.Key != "base_url" || field.InputKind != InputURL || field.Required || field.Sensitive {
		t.Fatalf("Vercel base_url field = %#v", field)
	}
	if len(descriptor.CredentialFields) != 1 {
		t.Fatalf("Vercel credentials = %#v", descriptor.CredentialFields)
	}
	if field := descriptor.CredentialFields[0]; field.Key != "api_key" || field.InputKind != InputSecret || !field.Required || !field.Sensitive {
		t.Fatalf("Vercel api_key field = %#v", field)
	}
	if got := descriptorIDs(registry.Search("ai gateway")); !reflect.DeepEqual(got, []ID{Vercel}) {
		t.Fatalf("Search(ai gateway) IDs = %v", got)
	}

	target, err := registry.Resolve(Vercel, nil)
	if err != nil {
		t.Fatalf("Resolve(Vercel) error = %v", err)
	}
	if target.ProviderKind != ProviderMultiProtocolGateway || target.CatalogProviderID != "vercel" {
		t.Fatalf("Vercel target = %#v", target)
	}
	if got := string(target.TargetConfig); got != `{"base_url":"https://ai-gateway.vercel.sh"}` {
		t.Fatalf("Vercel default target config = %s", got)
	}
	if target.SupportsResponsesLifecycle() {
		t.Fatal("Vercel must not advertise a complete Responses lifecycle")
	}
	overridden, err := registry.Resolve(Vercel, json.RawMessage(`{"base_url":"https://proxy.example/vercel/"}`))
	if err != nil {
		t.Fatalf("Resolve(Vercel override) error = %v", err)
	}
	if got := string(overridden.TargetConfig); got != `{"base_url":"https://proxy.example/vercel"}` {
		t.Fatalf("Vercel overridden target config = %s", got)
	}

	wantRoutes := map[protocol.Protocol]map[execution.Operation]RouteMode{
		protocol.OpenAICompletions: {
			execution.OperationChatCompletion: RouteNative,
			execution.OperationListModels:     RouteNative,
			execution.OperationProbe:          RouteNative,
		},
		protocol.OpenAIResponses: {
			execution.OperationResponsesCreate: RouteNative,
			execution.OperationProbe:           RouteNative,
		},
		protocol.OpenAIEmbeddings: {
			execution.OperationEmbeddingsCreate: RouteNative,
			execution.OperationProbe:            RouteNative,
		},
		protocol.Anthropic: {
			execution.OperationChatCompletion: RouteNative,
			execution.OperationCountTokens:    RouteNative,
			execution.OperationProbe:          RouteNative,
		},
	}
	gotRoutes := make(map[protocol.Protocol]map[execution.Operation]RouteMode)
	for _, route := range descriptor.Routes {
		if gotRoutes[route.ClientProtocol] == nil {
			gotRoutes[route.ClientProtocol] = make(map[execution.Operation]RouteMode)
		}
		gotRoutes[route.ClientProtocol][route.Operation] = route.RouteMode
	}
	if !reflect.DeepEqual(gotRoutes, wantRoutes) {
		t.Fatalf("Vercel routes = %#v, want %#v", gotRoutes, wantRoutes)
	}

	wantScrub := spec.ResponseScrub{
		AnyKeys:        []string{"provider_metadata", "generationId"},
		UsageKeys:      []string{"cost", "market_cost", "gateway_cost", "is_byok", "cost_details"},
		HeaderPrefixes: []string{"X-Vercel-", "X-Matched-Path"},
	}
	scrub, ok := registry.ResponseScrub(Vercel)
	if !ok || !reflect.DeepEqual(scrub, wantScrub) {
		t.Fatalf("Vercel response scrub = %#v, want %#v", scrub, wantScrub)
	}
	scrub.AnyKeys[0] = "mutated"
	if again, _ := registry.ResponseScrub(Vercel); !reflect.DeepEqual(again, wantScrub) {
		t.Fatalf("ResponseScrub returned shared state: %#v", again)
	}
}

func TestCompileResponseScrubRejectsBlankEntries(t *testing.T) {
	t.Parallel()

	for _, rules := range []spec.ResponseScrub{
		{AnyKeys: []string{""}},
		{UsageKeys: []string{" cost"}},
		{HeaderPrefixes: []string{"X-Vercel- "}},
	} {
		if _, err := compileResponseScrub("test", rules); err == nil {
			t.Fatalf("compileResponseScrub(%#v) accepted an invalid entry", rules)
		}
	}
}
