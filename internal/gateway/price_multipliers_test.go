package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
	"gpt-load/internal/usage"
)

func TestHandlerFreezesPriceMultipliersAndAccountsTheSameEstimate(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "ordinary"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			forwarder := &scriptedForwarder{results: []UpstreamResult{{
				StatusCode: http.StatusOK, Header: make(http.Header), RequestWritten: true,
				Body:  []byte(`{"ok":true}`),
				Usage: usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1, Output: 1}},
			}}}
			if stream {
				forwarder.results[0].Committed = true
				forwarder.results[0].Stream = StreamObservation{EndReason: StreamEndCleanEOF}
				forwarder.streamResults = forwarder.results
			}
			sink := &recordingRequestLogSink{}
			engine, handler, manager, _ := newRequestLogHandlerTestRuntime(
				t, forwarder, &recordingAccessKeyRPMLimiter{}, sink, "sk-first",
			)
			rules := []accessquota.Rule{{ID: 901, Revision: 1, Kind: accessquota.KindTotal, Currency: "USD", LimitNano: 10_000_000}}
			quota := useSharedAccessQuota(t, handler)
			table := mustGatewayTableWithClientPrices(t, []pricing.Rule{{
				Identity: pricing.Identity{ChannelID: "openai", ModelID: "gpt-4o"},
				Prices: pricing.Prices{
					Input:  pricing.Price{NanoUSDPerMillion: 600_000, Set: true},
					Output: pricing.Price{NanoUSDPerMillion: 600_000, Set: true},
				},
			}}, "gpt-4o")
			handler.priceTables = &mutableGatewayPriceTableProvider{table: table}
			input := gatewayAccessQuotaCompileInput(handler, rules)
			setGatewayPriceMultipliers(t, &input, "2", "3")
			if _, err := manager.Publish(input); err != nil {
				t.Fatal(err)
			}
			forwarder.onCall = func(int) {
				setGatewayPriceMultipliers(t, &input, "7", "9")
				if _, err := manager.Publish(input); err != nil {
					t.Fatal(err)
				}
			}
			forwarder.onStreamCall = func(index int, _ http.ResponseWriter) { forwarder.onCall(index) }
			body := `{"model":"gpt-4o"}`
			if stream {
				body = `{"model":"gpt-4o","stream":true}`
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer gl-client")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			// 费用只乘密钥倍率，上游费用只乘分组倍率，两者都用转发前冻结的倍率。
			events := sink.snapshot()
			if len(events) != 1 || events[0].Usage.Pricing.EstimatedCostNanoUSD != 6 || events[0].Usage.Pricing.UpstreamCostNanoUSD != 4 {
				t.Fatalf("frozen multiplier estimate = %#v, want base 2 charged 6 and costed 4", events)
			}
			type frozenReceipt struct {
				SchemaVersion    int    `json:"schema_version"`
				BaseTotalNanoUSD *int64 `json:"base_total_nano_usd"`
				PriceMultipliers struct {
					Group     string `json:"group"`
					AccessKey string `json:"access_key"`
				} `json:"price_multipliers"`
			}
			var charge, upstream frozenReceipt
			if err := json.Unmarshal([]byte(events[0].Usage.Pricing.ReceiptJSON), &charge); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(events[0].Usage.Pricing.UpstreamReceiptJSON), &upstream); err != nil {
				t.Fatal(err)
			}
			if charge.SchemaVersion != 7 || charge.PriceMultipliers.Group != "1" || charge.PriceMultipliers.AccessKey != "3" || charge.BaseTotalNanoUSD == nil || *charge.BaseTotalNanoUSD != 2 {
				t.Fatalf("frozen charge receipt = %#v", charge)
			}
			if upstream.SchemaVersion != 6 || upstream.PriceMultipliers.Group != "2" || upstream.PriceMultipliers.AccessKey != "1" || upstream.BaseTotalNanoUSD == nil || *upstream.BaseTotalNanoUSD != 2 {
				t.Fatalf("frozen upstream receipt = %#v", upstream)
			}
			view, err := quota.View(t.Context(), manager.Current(), 1, time.Now())
			if err != nil || len(view.Rules) != 1 || view.Rules[0].UsedNano != 6 {
				t.Fatalf("quota = %#v, want the charge, not the upstream cost", view)
			}
		})
	}
}

func setGatewayPriceMultipliers(t *testing.T, input *state.CompileInput, group, accessKey string) {
	t.Helper()
	groupMultiplier, err := pricing.ParsePriceMultiplier(group)
	if err != nil {
		t.Fatal(err)
	}
	keyMultiplier, err := pricing.ParsePriceMultiplier(accessKey)
	if err != nil {
		t.Fatal(err)
	}
	input.Groups[0].PriceMultiplier = &groupMultiplier
	input.AccessKeys[0].PriceMultiplier = &keyMultiplier
}

// 跨分组重试后，费用与路由到哪个分组无关；上游费用使用最终计量分组的倍率。
func TestHandlerCrossGroupRetryChargesByKeyAndCostsByFinalGroup(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{
		{
			StatusCode: http.StatusTooManyRequests, Header: make(http.Header), RequestWritten: true,
			Body:               []byte(`{"error":{"type":"rate_limit_error"}}`),
			ClassificationBody: []byte(`{"error":{"type":"rate_limit_error"}}`),
			Usage:              usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1_000_000}},
		},
		{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: []byte(`{"ok":true}`), RequestWritten: true,
			Usage: usage.Result{State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1000}},
		},
	}}
	sink := &recordingRequestLogSink{}
	engine, handler, manager, registry := newRequestLogHandlerTestRuntime(
		t, forwarder, &recordingAccessKeyRPMLimiter{}, sink, "sk-first", "sk-second",
	)
	entries, err := registry.SnapshotGroupCredentialEntriesExact(1, []uint{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	entries[1].GroupID = 2
	if err := registry.ReplaceCredentials(entries); err != nil {
		t.Fatal(err)
	}
	input := gatewayAccessQuotaCompileInput(handler, nil)
	setGatewayPriceMultipliers(t, &input, "7", "1.5")
	second := input.Groups[0]
	second.ID, second.Name = 2, "second"
	secondMultiplier := pricing.PriceMultiplier(800_000)
	second.PriceMultiplier = &secondMultiplier
	input.Groups = append(input.Groups, second)
	input.Credentials = append(input.Credentials, state.CredentialConfig{
		ID: 2, GroupID: 2, Version: 1, IdentityGeneration: 2, Fingerprint: "credential-2", Status: state.CredentialStatusActive,
	})
	if _, err := manager.Publish(input); err != nil {
		t.Fatal(err)
	}

	handler.priceTables = &mutableGatewayPriceTableProvider{table: mustGatewayPriceTable(t, 2_000_000_000, true)}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	request.Header.Set("Authorization", "Bearer gl-client")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	events := sink.snapshot()
	if response.Code != http.StatusOK || len(events) != 1 || len(events[0].Attempts) != 2 ||
		events[0].Usage.GroupID != 2 || events[0].Usage.AttemptSequence != 2 ||
		events[0].Usage.Pricing.EstimatedCostNanoUSD != 3_000_000 || events[0].Usage.Pricing.UpstreamCostNanoUSD != 1_600_000 {
		t.Fatalf("retry response = %d; events = %#v", response.Code, events)
	}
}
