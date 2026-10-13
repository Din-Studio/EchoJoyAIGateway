package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	casdoorTimeout      = 5 * time.Second
	maxCasdoorBodyBytes = 16 << 20
)

// casdoorClient calls the Casdoor API with the application's client
// credentials, which Casdoor treats as an administrator of its organization.
type casdoorClient struct {
	baseURL      string
	clientID     string
	clientSecret string
	organization string
	http         *http.Client
}

type casdoorEnvelope struct {
	Status string          `json:"status"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
}

func newCasdoorClient(baseURL, clientID, clientSecret, organization string) *casdoorClient {
	return &casdoorClient{
		baseURL:      baseURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		organization: organization,
		http:         &http.Client{Timeout: casdoorTimeout},
	}
}

// GetUser returns the organization's user with Casdoor id userID, or nil when
// the organization has no such user.
func (c *casdoorClient) GetUser(ctx context.Context, userID string) (*User, error) {
	var user *User
	query := url.Values{"userId": {userID}, "owner": {c.organization}}
	if err := c.call(ctx, http.MethodGet, "/api/get-user", query, nil, &user); err != nil {
		return nil, err
	}
	return user, nil
}

// BatchEnforce judges requests against every permission of the model.
func (c *casdoorClient) BatchEnforce(ctx context.Context, requests [][]string) ([][]bool, error) {
	var decisions [][]bool
	query := url.Values{"modelId": {c.organization + "/" + ModelName}}
	if err := c.call(ctx, http.MethodPost, "/api/batch-enforce", query, requests, &decisions); err != nil {
		return nil, err
	}
	return decisions, nil
}

// ListGroups returns every group of the organization.
func (c *casdoorClient) ListGroups(ctx context.Context) ([]Team, error) {
	var groups []struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/get-groups", url.Values{"owner": {c.organization}}, nil, &groups); err != nil {
		return nil, err
	}
	teams := make([]Team, 0, len(groups))
	for _, group := range groups {
		teams = append(teams, Team{Name: group.Name, DisplayName: group.DisplayName})
	}
	return teams, nil
}

// ListGroupUsers returns the direct members of one group.
func (c *casdoorClient) ListGroupUsers(ctx context.Context, group string) ([]User, error) {
	var users []User
	query := url.Values{"owner": {c.organization}, "groupName": {group}}
	if err := c.call(ctx, http.MethodGet, "/api/get-users", query, nil, &users); err != nil {
		return nil, err
	}
	return users, nil
}

func (c *casdoorClient) call(
	ctx context.Context,
	method string,
	path string,
	query url.Values,
	body any,
	target any,
) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("casdoor %s: encode request: %w", path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path+"?"+query.Encode(), reader)
	if err != nil {
		return fmt.Errorf("casdoor %s: %w", path, err)
	}
	request.SetBasicAuth(c.clientID, c.clientSecret)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("casdoor %s: %w", path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxCasdoorBodyBytes))
	if err != nil {
		return fmt.Errorf("casdoor %s: read response: %w", path, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("casdoor %s: HTTP %d", path, response.StatusCode)
	}
	var envelope casdoorEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("casdoor %s: decode response: %w", path, err)
	}
	if envelope.Status != "ok" {
		return fmt.Errorf("casdoor %s: %s", path, envelope.Msg)
	}
	if len(envelope.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, target); err != nil {
		return fmt.Errorf("casdoor %s: decode data: %w", path, err)
	}
	return nil
}
