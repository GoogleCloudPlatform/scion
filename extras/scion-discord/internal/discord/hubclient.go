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
// If httpClient is nil, a default client with a 15s timeout is used.
func NewHTTPHubClient(hubURL, hmacKey, brokerID string, httpClient *http.Client) HubClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &httpHubClient{
		hubURL:     hubURL,
		hmacKey:    hmacKey,
		brokerID:   brokerID,
		httpClient: httpClient,
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

type hubTemplatesResponse struct {
	Templates []hubTemplate `json:"templates"`
}

type hubTemplate struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Scope       string `json:"scope"`
	ScopeID     string `json:"scopeId,omitempty"`
	Status      string `json:"status"`
}

func (c *httpHubClient) ListTemplates(ctx context.Context, projectID string) ([]Template, error) {
	// Fetch global templates.
	globalURL := c.hubURL + "/api/v1/templates?scope=global&status=active"

	slog.Debug("Listing global templates from hub", "url", globalURL, "broker_id", c.brokerID)

	globalReq, err := http.NewRequestWithContext(ctx, "GET", globalURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create list global templates request: %w", err)
	}
	if err := c.signRequest(globalReq); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	globalResp, err := c.httpClient.Do(globalReq)
	if err != nil {
		return nil, fmt.Errorf("list global templates request failed: %w", err)
	}
	defer globalResp.Body.Close()

	if globalResp.StatusCode != http.StatusOK {
		slog.Debug("Hub returned non-OK for list global templates", "status", globalResp.StatusCode, "url", globalURL)
		return nil, fmt.Errorf("list global templates returned status %d", globalResp.StatusCode)
	}

	var globalResult hubTemplatesResponse
	if err := json.NewDecoder(globalResp.Body).Decode(&globalResult); err != nil {
		return nil, fmt.Errorf("decode list global templates response: %w", err)
	}

	slog.Debug("Hub returned global templates", "count", len(globalResult.Templates))

	// Merge into a map keyed by slug; project-scoped templates take precedence.
	bySlug := make(map[string]hubTemplate, len(globalResult.Templates))
	for _, t := range globalResult.Templates {
		bySlug[t.Slug] = t
	}

	// Fetch project-scoped templates if a project ID is provided.
	if projectID != "" {
		projectURL := fmt.Sprintf("%s/api/v1/templates?scope=project&projectId=%s&status=active", c.hubURL, projectID)

		slog.Debug("Listing project templates from hub", "url", projectURL, "project_id", projectID)

		projectReq, err := http.NewRequestWithContext(ctx, "GET", projectURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create list project templates request: %w", err)
		}
		if err := c.signRequest(projectReq); err != nil {
			return nil, fmt.Errorf("sign request: %w", err)
		}

		projectResp, err := c.httpClient.Do(projectReq)
		if err != nil {
			slog.Warn("Failed to list project templates, using global only", "error", err, "project_id", projectID)
		} else {
			defer projectResp.Body.Close()
			if projectResp.StatusCode == http.StatusOK {
				var projectResult hubTemplatesResponse
				if err := json.NewDecoder(projectResp.Body).Decode(&projectResult); err != nil {
					slog.Warn("Failed to decode project templates response, using global only", "error", err)
				} else {
					slog.Debug("Hub returned project templates", "count", len(projectResult.Templates))
					// Project-scoped templates override global ones with the same slug.
					for _, t := range projectResult.Templates {
						bySlug[t.Slug] = t
					}
				}
			} else {
				slog.Debug("Hub returned non-OK for list project templates", "status", projectResp.StatusCode)
			}
		}
	}

	// Convert map to slice.
	templates := make([]Template, 0, len(bySlug))
	for _, t := range bySlug {
		name := t.DisplayName
		if name == "" {
			name = t.Name
		}
		templates = append(templates, Template{Slug: t.Slug, Name: name})
	}

	return templates, nil
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
