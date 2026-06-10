package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// httpHubClient implements HubClient using HTTP calls to the Hub API.
type httpHubClient struct {
	hubURL     string
	hmacKey    string
	brokerID   string
	httpClient *http.Client
}

// NewHTTPHubClient creates a new HubClient that calls the Scion Hub API.
func NewHTTPHubClient(hubURL, hmacKey, brokerID string) HubClient {
	return &httpHubClient{
		hubURL:     hubURL,
		hmacKey:    hmacKey,
		brokerID:   brokerID,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

type hubProjectsResponse struct {
	Projects []hubProject `json:"projects"`
}

type hubProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type hubAgentsResponse struct {
	Agents []hubAgent `json:"agents"`
}

type hubAgent struct {
	Slug     string `json:"slug"`
	Activity string `json:"activity"`
}

func (c *httpHubClient) ListProjects(ctx context.Context) ([]ProjectOption, error) {
	url := c.hubURL + "/api/v1/projects"

	slog.Debug("Listing projects from hub", "url", url, "broker_id", c.brokerID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list projects request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list projects request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Debug("Hub returned non-OK for list projects", "status", resp.StatusCode, "url", url)
		return nil, fmt.Errorf("list projects returned status %d", resp.StatusCode)
	}

	var result hubProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list projects response: %w", err)
	}

	slog.Debug("Hub returned projects", "count", len(result.Projects))

	projects := make([]ProjectOption, len(result.Projects))
	for i, p := range result.Projects {
		projects[i] = ProjectOption{ID: p.ID, Name: p.Name, Slug: p.Slug}
	}
	return projects, nil
}

func (c *httpHubClient) ListProjectsFresh(ctx context.Context) ([]ProjectOption, error) {
	url := c.hubURL + "/api/v1/broker/projects"

	slog.Debug("Listing fresh projects from hub broker endpoint", "url", url, "broker_id", c.brokerID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list fresh projects request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list fresh projects request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Debug("Hub returned non-OK for list fresh projects", "status", resp.StatusCode, "url", url)
		return nil, fmt.Errorf("list fresh projects returned status %d", resp.StatusCode)
	}

	var result hubProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list fresh projects response: %w", err)
	}

	slog.Debug("Hub returned fresh projects", "count", len(result.Projects))

	projects := make([]ProjectOption, len(result.Projects))
	for i, p := range result.Projects {
		projects[i] = ProjectOption{ID: p.ID, Name: p.Name, Slug: p.Slug}
	}
	return projects, nil
}

func (c *httpHubClient) ListProjectsForUser(ctx context.Context, ownerID string) ([]ProjectOption, error) {
	url := c.hubURL + "/api/v1/projects?ownerId=" + ownerID

	slog.Debug("Listing projects for user from hub", "url", url, "owner_id", ownerID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list user projects request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list user projects request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list user projects returned status %d", resp.StatusCode)
	}

	var result hubProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list user projects response: %w", err)
	}

	projects := make([]ProjectOption, len(result.Projects))
	for i, p := range result.Projects {
		projects[i] = ProjectOption{ID: p.ID, Name: p.Name, Slug: p.Slug}
	}
	return projects, nil
}

func (c *httpHubClient) ListAgents(ctx context.Context, projectID string) ([]AgentInfo, error) {
	url := fmt.Sprintf("%s/api/v1/projects/%s/agents", c.hubURL, projectID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list agents request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list agents request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list agents returned status %d", resp.StatusCode)
	}

	var result hubAgentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list agents response: %w", err)
	}

	agents := make([]AgentInfo, len(result.Agents))
	for i, a := range result.Agents {
		agents[i] = AgentInfo{Slug: a.Slug, Activity: a.Activity}
	}
	return agents, nil
}

func (c *httpHubClient) signRequest(req *http.Request) error {
	if c.brokerID == "" || c.hmacKey == "" {
		return nil
	}

	secretKey, err := decodeBase64(c.hmacKey)
	if err != nil {
		return fmt.Errorf("decode HMAC key: %w", err)
	}

	auth := &apiclient.HMACAuth{
		BrokerID:  c.brokerID,
		SecretKey: secretKey,
	}
	return auth.ApplyAuth(req)
}
