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
	balancePath    = "/internal/aigw/balance"
	usageBatchPath = "/internal/aigw/usage/batch"

	balanceTimeout = 3 * time.Second
	// reportTimeout exceeds Lingying's 25-second deadline for a whole batch.
	reportTimeout = 30 * time.Second
	// maxResponseBytes holds the results of a full batch with their errors.
	maxResponseBytes = 1 << 20
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
func (c *client) balance(ctx context.Context, credential Credential) (int, balance, error) {
	body, err := json.Marshal(credential)
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

// batchResult is Lingying's outcome for one report of a batch. Status has
// the meaning the single-report endpoint gives its HTTP status.
type batchResult struct {
	RequestID string `json:"request_id"`
	Status    int    `json:"status"`
	Error     string `json:"error"`
}

// reportUsageBatch posts usage reports in one request, each payload as
// stored, and returns the per-report results. Any answer other than 200 is an
// error that fails the whole batch.
func (c *client) reportUsageBatch(ctx context.Context, payloads []string) ([]batchResult, error) {
	reports := make([]json.RawMessage, len(payloads))
	for index, payload := range payloads {
		reports[index] = json.RawMessage(payload)
	}
	body, err := json.Marshal(struct {
		Reports []json.RawMessage `json:"reports"`
	}{reports})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	status, response, err := c.post(ctx, usageBatchPath, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", status, response)
	}
	var decoded struct {
		Results []batchResult `json:"results"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		return nil, fmt.Errorf("decode usage batch results: %w", err)
	}
	return decoded.Results, nil
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
