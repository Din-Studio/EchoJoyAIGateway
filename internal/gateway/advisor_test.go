package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/channel"
	"gpt-load/internal/dialect"
	"gpt-load/internal/pricing"
	"gpt-load/internal/state"
	"gpt-load/internal/usage"
)

func TestHandlerAccountsAdvisorUsageInCostLimit(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{{
		StatusCode: http.StatusOK, Header: make(http.Header), RequestWritten: true,
		Body: []byte(`{"ok":true}`),
		Usage: usage.Result{
			State: usage.StateComplete, Tokens: usage.Tokens{UncachedInput: 1, Output: 1},
			Advisor: &usage.AdvisorUsage{Calls: []usage.ModelTokens{
				{Model: "advisor-model", Tokens: usage.Tokens{UncachedInput: 10, Output: 10}},
			}},
		},
	}}}
	sink := &recordingRequestLogSink{}
	engine, handler, manager, _ := newRequestLogHandlerTestRuntime(
		t, forwarder, &recordingAccessKeyRPMLimiter{}, sink, "sk-first",
	)
	quota := useSharedAccessQuota(t, handler)
	rules := []accessquota.Rule{{ID: 901, Revision: 1, Kind: accessquota.KindTotal, Currency: "CNY", LimitNano: 40_000}}
	table, err := pricing.NewTableWithClientRules([]pricing.Rule{
		{Identity: pricing.Identity{ChannelID: "openai", ModelID: "gpt-4o"}, Prices: pricing.Prices{
			Input:  pricing.Price{NanoUSDPerMillion: 600_000, Set: true},
			Output: pricing.Price{NanoUSDPerMillion: 600_000, Set: true},
		}},
		{Identity: pricing.Identity{ChannelID: "openai", ModelID: "advisor-model"}, Prices: pricing.Prices{
			Input:  pricing.Price{NanoUSDPerMillion: 100_000_000, Set: true},
			Output: pricing.Price{NanoUSDPerMillion: 100_000_000, Set: true},
		}},
	}, []pricing.ClientRule{
		{ClientModel: "gpt-4o", Prices: pricing.Prices{Input: pricing.Price{NanoUSDPerMillion: 600_000, Set: true}, Output: pricing.Price{NanoUSDPerMillion: 600_000, Set: true}}},
		{ClientModel: "advisor-public", Prices: pricing.Prices{Input: pricing.Price{NanoUSDPerMillion: 100_000_000, Set: true}, Output: pricing.Price{NanoUSDPerMillion: 100_000_000, Set: true}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler.priceTables = &mutableGatewayPriceTableProvider{table: table}
	input := gatewayAccessQuotaCompileInput(handler, rules)
	input.Groups[0].Models = append(input.Groups[0].Models, state.ModelConfig{ID: "advisor-model", Alias: "advisor-public"})
	input.SystemSettings = cnySettings("7")
	setGatewayPriceMultipliers(t, &input, "2", "3")
	if _, err := manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	request.Header.Set("Authorization", "Bearer gl-client")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	// 统一价基价2002 × 密钥3 = 6006；渠道成本2002 × 分组2 = 4004。
	events := sink.snapshot()
	if len(events) != 1 || events[0].Usage.Pricing.EstimatedCostNanoUSD != 6006 || events[0].Usage.Pricing.UpstreamCostNanoUSD != 4004 ||
		events[0].Usage.Pricing.PricingCompleteness != string(pricing.CompletenessComplete) ||
		!strings.Contains(events[0].Usage.Pricing.ReceiptJSON, `"code":"advisor_input","model_id":"advisor-public"`) {
		t.Fatalf("request log pricing = %#v", events)
	}
	view, err := quota.View(t.Context(), manager.Current(), 1, time.Now())
	if err != nil || len(view.Rules) != 1 || view.Rules[0].UsedNano != 42042 {
		t.Fatalf("quota = %#v, want the advisor-inclusive CNY charge", view)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	request.Header.Set("Authorization", "Bearer gl-client")
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || len(forwarder.inputs) != 1 {
		t.Fatalf("quota rejection=%d attempts=%d body=%s", response.Code, len(forwarder.inputs), response.Body.String())
	}
}

func TestHandlerRequiresAdvisorModelInGroup(t *testing.T) {
	for _, test := range []struct {
		name         string
		advisor      string
		wantStatus   int
		wantCode     string
		wantAttempts int
	}{
		{name: "advisor model outside the group", advisor: "claude-opus-4-8",
			wantStatus: http.StatusBadRequest, wantCode: "advisor_model_unavailable"},
		{name: "advisor model in the group", advisor: "claude-sonnet-5-5", wantStatus: http.StatusOK, wantAttempts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarder := &scriptedForwarder{results: []UpstreamResult{{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       []byte(`{"id":"msg","type":"message","model":"claude-haiku-4-5","content":[]}`),
			}}}
			handler, manager, registry := newHandlerForTest(t, forwarder)
			if _, err := manager.Publish(state.CompileInput{
				ChannelRegistry: channel.NewRegistry(),
				Groups: []state.GroupConfig{{ConnectionType: "api_key", ID: 1, Name: "anthropic", ChannelID: channel.Anthropic,
					Params: json.RawMessage(`{}`), Enabled: true,
					Models: []state.ModelConfig{{ID: "claude-haiku-4-5"}, {ID: "claude-sonnet-5-5"}},
				}},
				Credentials: []state.CredentialConfig{testCredentialConfig(1, 1)},
				AccessKeys: []state.AccessKeyConfig{{
					ID: 1, Name: "client", KeyHash: handler.encryption.Hash("gl-client"),
					Status: state.AccessKeyStatusActive,
				}},
			}); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}
			if err := registry.ReplaceCredentials([]state.CredentialEntry{
				testCredentialEntry(t, handler.encryption, 1, 1, "sk-anthropic"),
			}); err != nil {
				t.Fatalf("ReplaceCredentials() error = %v", err)
			}
			handler.dialects = dialect.NewSet(dialect.NewAnthropic())
			engine := gin.New()
			bindGatewayRoutesForTest(t, engine, handler)
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(
				`{"model":"claude-haiku-4-5","max_tokens":16,"messages":[{"role":"user","content":"ping"}],`+
					`"tools":[{"type":"advisor_20260301","name":"advisor","model":"`+test.advisor+`"}]}`,
			))
			request.Header.Set("x-api-key", "gl-client")
			request.Header.Set("anthropic-version", "2023-06-01")
			response := httptest.NewRecorder()

			engine.ServeHTTP(response, request)

			if response.Code != test.wantStatus || len(forwarder.inputs) != test.wantAttempts {
				t.Fatalf("status/attempts = %d/%d, want %d/%d; body=%s",
					response.Code, len(forwarder.inputs), test.wantStatus, test.wantAttempts, response.Body.String())
			}
			if test.wantCode == "" {
				return
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Code != test.wantCode {
				t.Fatalf("reason body = %s, want code %q", response.Body.String(), test.wantCode)
			}
		})
	}
}
