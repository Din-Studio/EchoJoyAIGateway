package ly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	balancePath = "/internal/aigw/balance"
	usagePath   = "/internal/aigw/usage"

	balanceTimeout = 3 * time.Second
	// reportTimeout exceeds Lingying's 15-second usage write timeout.
	reportTimeout    = 20 * time.Second
	maxResponseBytes = 64 << 10
)

// client calls the Lingying billing callbacks. A response is classified by its
// HTTP status code only; the body's "code" field can disagree with it.
type client struct {
	baseURL string
	token   string
	http    *http.Client
}

func newClient(baseURL, token string) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// The endpoint is reached through the internal load balancer, never a proxy.
		http: &http.Client{Transport: &http.Transport{
			Proxy:               nil,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
		}},
	}
}

// balance is the subset of POST /internal/aigw/balance this gateway reads.
type balance struct {
	UserID    string `json:"user_id"`
	KeyID     string `json:"key_id"`
	Usable    bool   `json:"usable"`
	Reason    string `json:"reason"`
	Available string `json:"available"`
}

// balance returns the HTTP status and, for 200, the decoded balance.
func (c *client) balance(ctx context.Context, apiKey string) (int, balance, error) {
	body, err := json.Marshal(map[string]string{"api_key": apiKey})
	if err != nil {
		return 0, balance{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, balanceTimeout)
	defer cancel()
	status, response, err := c.post(ctx, balancePath, body)
	if err != nil || status != http.StatusOK {
		return status, balance{}, err
	}
	var decoded balance
	if err := json.Unmarshal(response, &decoded); err != nil {
		return status, balance{}, fmt.Errorf("decode balance: %w", err)
	}
	return status, decoded, nil
}

// reportUsage posts one usage report and returns the HTTP status and body.
func (c *client) reportUsage(ctx context.Context, payload []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	return c.post(ctx, usagePath, payload)
}

// post returns status 0 with an error when no response arrived.
func (c *client) post(ctx context.Context, path string, body []byte) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}
	return response.StatusCode, raw, nil
}
