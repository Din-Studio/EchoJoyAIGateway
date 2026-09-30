package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// pricePerMillionUSD makes one fixture completion (3 input + 2 output
// tokens) cost exactly 5 × 1000 / 1e6 = 0.005 USD.
const (
	pricePerMillionUSD = "1000"
	requestCostUSD     = 0.005
)

// adminClient talks to one instance's /api control plane.
type adminClient struct {
	baseURL string
	authKey string
	http    *http.Client
}

type envelope struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c adminClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.authKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodPost {
		request.Header.Set("Idempotency-Key", newUUIDv4())
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: status %d: %s", method, path, response.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	var decoded envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("%s %s: decode envelope: %w", method, path, err)
	}
	if err := json.Unmarshal(decoded.Data, out); err != nil {
		return fmt.Errorf("%s %s: decode data: %w", method, path, err)
	}
	return nil
}

type groupModel struct {
	ID           string `json:"id"`
	Alias        string `json:"alias"`
	AliasEnabled bool   `json:"alias_enabled"`
}

func (c adminClient) createGroup(ctx context.Context, name, baseURL string, models []string) (uint, error) {
	groupModels := make([]groupModel, 0, len(models))
	for _, model := range models {
		groupModels = append(groupModels, groupModel{ID: model})
	}
	var result struct {
		GroupID uint `json:"group_id"`
	}
	err := c.do(ctx, http.MethodPost, "/api/groups", map[string]any{
		"name":                name,
		"channel_id":          "openai",
		"connection_type":     "api_key",
		"params":              map[string]string{"base_url": baseURL},
		"models":              groupModels,
		"credentials":         "sk-clusterbench-" + name,
		"confirm_same_target": true,
	}, &result)
	return result.GroupID, err
}

func (c adminClient) setGroupModels(ctx context.Context, groupID uint, models []string) error {
	groupModels := make([]groupModel, 0, len(models))
	for _, model := range models {
		groupModels = append(groupModels, groupModel{ID: model})
	}
	return c.do(ctx, http.MethodPut, "/api/groups/"+strconv.FormatUint(uint64(groupID), 10)+"/models",
		map[string]any{"models": groupModels}, nil)
}

// priceModel pins the OpenAI price of model so every fixture completion costs
// requestCostUSD, independent of models.dev.
func (c adminClient) priceModel(ctx context.Context, model string) error {
	var list struct {
		Items []struct {
			ID        uint   `json:"id"`
			ChannelID string `json:"channel_id"`
			ModelID   string `json:"model_id"`
		} `json:"items"`
	}
	query := url.Values{"usage": {"all"}, "search": {model}}
	if err := c.do(ctx, http.MethodGet, "/api/model-prices?"+query.Encode(), nil, &list); err != nil {
		return err
	}
	for _, item := range list.Items {
		if item.ChannelID != "openai" || item.ModelID != model {
			continue
		}
		return c.do(ctx, http.MethodPut, "/api/model-prices/"+strconv.FormatUint(uint64(item.ID), 10), map[string]any{
			"input": pricePerMillionUSD, "output": pricePerMillionUSD,
			"cache_read": nil, "cache_write": nil,
			"context_tiers": []any{}, "mode_schedules": map[string]any{},
		}, nil)
	}
	return fmt.Errorf("no openai price row for model %q", model)
}

type accessKeySpec struct {
	Name     string
	RPMLimit int64
	// TotalLimitUSD adds one total cost-limit rule when non-empty.
	TotalLimitUSD string
}

type accessKey struct {
	ID     uint
	Secret string
}

func (c adminClient) createAccessKey(ctx context.Context, spec accessKeySpec) (accessKey, error) {
	body := map[string]any{"name": spec.Name}
	if spec.RPMLimit > 0 {
		body["rpm_limit"] = spec.RPMLimit
	}
	if spec.TotalLimitUSD != "" {
		body["cost_limit_rules"] = []map[string]string{{"kind": "total", "limit_usd": spec.TotalLimitUSD}}
	}
	var result struct {
		ID  uint   `json:"id"`
		Key string `json:"key"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/access-keys", body, &result); err != nil {
		return accessKey{}, err
	}
	if result.Key == "" {
		return accessKey{}, fmt.Errorf("access key %q was created without a returned secret", spec.Name)
	}
	return accessKey{ID: result.ID, Secret: result.Key}, nil
}

// costUsedUSD returns the used amount of the key's first cost-limit rule. The
// list is newest first, so keys created by this run are on the first page.
func (c adminClient) costUsedUSD(ctx context.Context, id uint) (float64, error) {
	var list struct {
		Items []struct {
			ID              uint `json:"id"`
			CostLimitStatus *struct {
				Rules []struct {
					UsedUSD string `json:"used_usd"`
				} `json:"rules"`
			} `json:"cost_limit_status"`
		} `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/access-keys", nil, &list); err != nil {
		return 0, err
	}
	for _, item := range list.Items {
		if item.ID != id {
			continue
		}
		if item.CostLimitStatus == nil || len(item.CostLimitStatus.Rules) == 0 {
			return 0, fmt.Errorf("access key %d has no cost-limit status", id)
		}
		return strconv.ParseFloat(item.CostLimitStatus.Rules[0].UsedUSD, 64)
	}
	return 0, fmt.Errorf("access key %d is not on the first page of /api/access-keys", id)
}

// fixtureSet is the control-plane state every check runs against.
type fixtureSet struct {
	groupID uint
	// propagationModel is unique per run, so reruns against a reused
	// database still start from a model no group serves yet.
	propagationModel string
	probe            accessKey // no limits: readiness polling and warm-up
	rpm              accessKey // RPM 60
	quota            accessKey // total limit of 10.5 requests
	bench            accessKey // limits high enough never to trigger
}

const (
	benchModel    = "gpt-4o-mini"
	quotaRequests = 10.5
	rpmLimit      = 60
)

// seed creates the group and access keys on one instance and waits until
// every data-plane target serves the last created key. Configuration reloads
// are full snapshots, so the last key being visible implies all of them are.
func seed(ctx context.Context, admin adminClient, dataPlanes []string, upstreamBaseURL, suffix string) (fixtureSet, error) {
	set := fixtureSet{propagationModel: "propagation-" + suffix}
	var err error
	if set.groupID, err = admin.createGroup(ctx, "clusterbench-"+suffix, upstreamBaseURL, []string{benchModel}); err != nil {
		return set, err
	}
	if err = admin.priceModel(ctx, benchModel); err != nil {
		return set, err
	}
	specs := []struct {
		target *accessKey
		spec   accessKeySpec
	}{
		{&set.rpm, accessKeySpec{Name: "rpm-" + suffix, RPMLimit: rpmLimit}},
		{&set.quota, accessKeySpec{Name: "quota-" + suffix, TotalLimitUSD: strconv.FormatFloat(quotaRequests*requestCostUSD, 'f', -1, 64)}},
		{&set.bench, accessKeySpec{Name: "bench-" + suffix, RPMLimit: 1_000_000, TotalLimitUSD: "1000000"}},
		{&set.probe, accessKeySpec{Name: "probe-" + suffix}},
	}
	for _, item := range specs {
		if *item.target, err = admin.createAccessKey(ctx, item.spec); err != nil {
			return set, err
		}
	}
	client := newDataPlaneClient()
	for _, target := range dataPlanes {
		if err := awaitStatus(ctx, client, target, set.probe.Secret, benchModel, http.StatusOK, 30*time.Second); err != nil {
			return set, err
		}
	}
	return set, nil
}

func newDataPlaneClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 4096
	transport.MaxIdleConnsPerHost = 1024
	transport.MaxConnsPerHost = 0
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

func chatBody(model string) []byte {
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"ping"}]}`)
}

// chat sends one chat completion and returns the HTTP status.
func chat(ctx context.Context, client *http.Client, baseURL, secret, model string) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(chatBody(model)))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.StatusCode, nil
}

func awaitStatus(ctx context.Context, client *http.Client, baseURL, secret, model string, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := "no response"
	for time.Now().Before(deadline) {
		status, err := chat(ctx, client, baseURL, secret, model)
		if err == nil && status == want {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = strconv.Itoa(status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s did not serve %s with %d within %s (last: %s)", baseURL, model, want, timeout, last)
}

func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, strings.TrimRight(item, "/"))
		}
	}
	return out
}
