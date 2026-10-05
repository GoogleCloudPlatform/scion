package slack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// AgentInfo holds an agent's slug and current activity state.
type AgentInfo struct {
	Slug     string `json:"slug"`
	Activity string `json:"activity,omitempty"`
}

// ProjectOption holds a project's identifiers for display in selection UI.
type ProjectOption struct {
	ID   string
	Name string
	Slug string
}

// DisplayName returns a human-readable label for the project.
func (p ProjectOption) DisplayName() string {
	if p.Name != "" {
		return p.Name
	}
	if p.Slug != "" {
		return p.Slug
	}
	return p.ID
}

// HubClient provides access to the Scion hub API for project and agent listing.
type HubClient interface {
	// ListUserProjects lists the projects visible to the linked user.
	// linkedUser ("user:<email>") is the Slack user's linked Scion account,
	// sent with the request. Use this for every user-facing project picker.
	ListUserProjects(ctx context.Context, linkedUser string) ([]ProjectOption, error)
	// ListAgents lists the agents of a project. linkedUser ("user:<email>")
	// is the Slack user's linked Scion account, sent with the request; the
	// hub denies a project agent list without it.
	ListAgents(ctx context.Context, projectID, linkedUser string) ([]AgentInfo, error)
}

// headerOnBehalfOf names the linked Scion user a request is made for.
const headerOnBehalfOf = "X-Scion-On-Behalf-Of"

// setLinkedUser adds the linked user to a hub request. It must be called
// before the request is signed.
func setLinkedUser(req *http.Request, linkedUser string) {
	if linkedUser == "" {
		return
	}
	req.Header.Set(headerOnBehalfOf, linkedUser)
	req.Header.Set(apiclient.HeaderSignedHeaders, "x-scion-on-behalf-of")
}

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

func (c *httpHubClient) ListUserProjects(ctx context.Context, linkedUser string) ([]ProjectOption, error) {
	url := c.hubURL + "/api/v1/projects"

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list user projects request: %w", err)
	}
	setLinkedUser(req, linkedUser)

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list user projects request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list user projects: %w", parseHubError(resp))
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

func (c *httpHubClient) ListAgents(ctx context.Context, projectID, linkedUser string) ([]AgentInfo, error) {
	url := fmt.Sprintf("%s/api/v1/projects/%s/agents", c.hubURL, projectID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list agents request: %w", err)
	}
	setLinkedUser(req, linkedUser)

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list agents request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list agents: %w", parseHubError(resp))
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

// decodeBase64 tries standard and URL-safe base64 decoding.
func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("invalid base64 encoding")
}
