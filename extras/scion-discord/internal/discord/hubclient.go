package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// httpHubClient implements HubClient using HTTP calls to the Hub API.
type httpHubClient struct {
	hubURL         string
	hmacKey        string
	brokerID       string
	httpClient     *http.Client
	longHTTPClient *http.Client // no global timeout — used for long-running calls like CreateAgent
}

// NewHTTPHubClient creates a new HubClient that calls the Scion Hub API.
// If httpClient is nil, a default client with a 15s timeout is used.
// A separate longHTTPClient with no global timeout is created for long-running
// operations like CreateAgent (which synchronously dispatches container create+start
// and routinely takes 30–120s). The longHTTPClient inherits the Transport from
// httpClient so that IAP-authenticated transport is preserved on long-running calls.
func NewHTTPHubClient(hubURL, hmacKey, brokerID string, httpClient *http.Client) HubClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &httpHubClient{
		hubURL:     hubURL,
		hmacKey:    hmacKey,
		brokerID:   brokerID,
		httpClient: httpClient,
		longHTTPClient: &http.Client{
			Transport: httpClient.Transport, // inherit IAP/transport auth
		},
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
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Activity string `json:"activity"`
	Phase    string `json:"phase"`
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
		agents[i] = AgentInfo{ID: a.ID, Slug: a.Slug, Activity: a.Activity, Phase: a.Phase}
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

// hubCreateAgentResponse mirrors the relevant fields of the hub's CreateAgentResponse.
// The hub returns {"agent": {...}, "warnings": [...], ...}; we only need the agent.
type hubCreateAgentResponse struct {
	Agent struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"agent"`
}

func (c *httpHubClient) CreateAgent(ctx context.Context, projectID string, req CreateAgentRequest, onBehalfOf string) (*CreateAgentResponse, error) {
	url := fmt.Sprintf("%s/api/v1/projects/%s/agents", c.hubURL, projectID)

	slog.Debug("Creating agent via hub", "url", url, "name", req.Name, "template", req.Template, "on_behalf_of", onBehalfOf)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal create agent request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create agent request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Set the delegated identity header so the hub attributes the agent to the
	// invoking user rather than leaving it ownerless.
	if onBehalfOf != "" {
		httpReq.Header.Set("X-Scion-On-Behalf-Of", onBehalfOf)
		httpReq.Header.Set("X-Scion-Signed-Headers", "x-scion-on-behalf-of")
	}

	if err := c.signRequest(httpReq); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	// Use the long-timeout client — agent creation synchronously dispatches
	// container create+start and routinely takes 30–120s. The default httpClient
	// has a 15s timeout which would cause every create to fail.
	resp, err := c.longHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("create agent request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated: // 201 — success
		var result hubCreateAgentResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("decode create agent response: %w", err)
		}
		return &CreateAgentResponse{
			Slug: result.Agent.Slug,
			Name: result.Agent.Name,
		}, nil

	case http.StatusOK: // 200 — hub resumed/started an existing agent; treat as conflict per design decision 7b
		return nil, fmt.Errorf("an agent with this name already exists and was resumed by the hub — use a different title")

	case http.StatusConflict: // 409 — slug conflict
		return nil, fmt.Errorf("an agent with this slug already exists — try a different title")

	case http.StatusNotFound: // 404 — template or project not found
		he := parseHubError(resp)
		return nil, fmt.Errorf("not found: %s", he.Message)

	case http.StatusBadRequest: // 400 — validation error
		he := parseHubError(resp)
		return nil, fmt.Errorf("validation error: %s", he.Message)

	case http.StatusForbidden: // 403 — permission denied
		return nil, fmt.Errorf("you don't have permission to create agents in this project")

	default:
		he := parseHubError(resp)
		return nil, fmt.Errorf("create agent returned status %d: %s", resp.StatusCode, he.Message)
	}
}

func (c *httpHubClient) HubBaseURL() string {
	return c.hubURL
}

// --- Secrets API ---

// SecretInfo holds metadata about a secret for display in Discord.
// Values are never included.
type SecretInfo struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Scope       string `json:"scope"`
	ScopeID     string `json:"scopeId"`
	Description string `json:"description,omitempty"`
	Updated     string `json:"updated,omitempty"`
	Version     int    `json:"version"`
}

// setSecretPayload is the JSON body sent to PUT /api/v1/secrets/{key}.
type setSecretPayload struct {
	Value    string `json:"value"`
	Encoding string `json:"encoding"`
	Scope    string `json:"scope"`
	ScopeID  string `json:"scopeId"`
}

// hubListSecretsResponse mirrors the hub's list-secrets JSON envelope.
type hubListSecretsResponse struct {
	Secrets []SecretInfo `json:"secrets"`
}

func (c *httpHubClient) ListSecrets(ctx context.Context, scope, scopeID string) ([]SecretInfo, error) {
	u := fmt.Sprintf("%s/api/v1/secrets?scope=%s&scopeId=%s",
		c.hubURL, url.QueryEscape(scope), url.QueryEscape(scopeID))

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("create list secrets request: %w", err)
	}
	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list secrets request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		he := parseHubError(resp)
		return nil, fmt.Errorf("list secrets returned status %d: %s", resp.StatusCode, he.Message)
	}

	var result hubListSecretsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list secrets response: %w", err)
	}
	return result.Secrets, nil
}

func (c *httpHubClient) GetSecret(ctx context.Context, key, scope, scopeID string) (*SecretInfo, error) {
	u := fmt.Sprintf("%s/api/v1/secrets/%s?scope=%s&scopeId=%s",
		c.hubURL, url.PathEscape(key), url.QueryEscape(scope), url.QueryEscape(scopeID))

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("create get secret request: %w", err)
	}
	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get secret request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("secret %q not found", key)
	}
	if resp.StatusCode != http.StatusOK {
		he := parseHubError(resp)
		return nil, fmt.Errorf("get secret returned status %d: %s", resp.StatusCode, he.Message)
	}

	var info SecretInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode get secret response: %w", err)
	}
	return &info, nil
}

func (c *httpHubClient) SetSecret(ctx context.Context, key, value, scope, scopeID, onBehalfOf string) error {
	u := fmt.Sprintf("%s/api/v1/secrets/%s", c.hubURL, url.PathEscape(key))

	payload := setSecretPayload{
		Value:    value,
		Encoding: "raw",
		Scope:    scope,
		ScopeID:  scopeID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal set secret request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create set secret request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if onBehalfOf != "" {
		httpReq.Header.Set("X-Scion-On-Behalf-Of", onBehalfOf)
		httpReq.Header.Set("X-Scion-Signed-Headers", "x-scion-on-behalf-of")
	}
	if err := c.signRequest(httpReq); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("set secret request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		he := parseHubError(resp)
		return fmt.Errorf("set secret returned status %d: %s", resp.StatusCode, he.Message)
	}
	return nil
}

func (c *httpHubClient) DeleteSecret(ctx context.Context, key, scope, scopeID, onBehalfOf string) error {
	u := fmt.Sprintf("%s/api/v1/secrets/%s?scope=%s&scopeId=%s",
		c.hubURL, url.PathEscape(key), url.QueryEscape(scope), url.QueryEscape(scopeID))

	httpReq, err := http.NewRequestWithContext(ctx, "DELETE", u, nil)
	if err != nil {
		return fmt.Errorf("create delete secret request: %w", err)
	}
	if onBehalfOf != "" {
		httpReq.Header.Set("X-Scion-On-Behalf-Of", onBehalfOf)
		httpReq.Header.Set("X-Scion-Signed-Headers", "x-scion-on-behalf-of")
	}
	if err := c.signRequest(httpReq); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("delete secret request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		he := parseHubError(resp)
		return fmt.Errorf("delete secret returned status %d: %s", resp.StatusCode, he.Message)
	}
	return nil
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
