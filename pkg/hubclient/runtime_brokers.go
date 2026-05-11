// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hubclient

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// RuntimeBrokerService handles runtime broker operations.
type RuntimeBrokerService interface {
	// Create creates a new broker registration and returns a join token.
	// The join token must be used with Join() to complete registration.
	Create(ctx context.Context, req *CreateBrokerRequest) (*CreateBrokerResponse, error)

	// Join completes broker registration using a join token.
	// Returns the HMAC secret key for future authentication.
	Join(ctx context.Context, req *JoinBrokerRequest) (*JoinBrokerResponse, error)

	// List returns runtime brokers matching the filter criteria.
	List(ctx context.Context, opts *ListBrokersOptions) (*ListBrokersResponse, error)

	// Get returns a single runtime broker by ID.
	Get(ctx context.Context, brokerID string) (*RuntimeBroker, error)

	// Update updates broker metadata.
	Update(ctx context.Context, brokerID string, req *UpdateBrokerRequest) (*RuntimeBroker, error)

	// Delete removes a broker from all projects.
	Delete(ctx context.Context, brokerID string) error

	// ListProjects returns projects this broker contributes to.
	ListProjects(ctx context.Context, brokerID string) (*ListBrokerProjectsResponse, error)

	// Heartbeat sends a heartbeat for a broker.
	Heartbeat(ctx context.Context, brokerID string, status *BrokerHeartbeat) error
}

// runtimeBrokerService is the implementation of RuntimeBrokerService.
type runtimeBrokerService struct {
	c *client
}

// ListBrokersOptions configures runtime broker list filtering.
type ListBrokersOptions struct {
	Status    string // Filter by status (online, offline)
	ProjectID string // Filter by project contribution
	Name      string // Exact match on broker name (case-insensitive)
	Page      apiclient.PageOptions
}

// ListBrokersResponse is the response from listing runtime brokers.
type ListBrokersResponse struct {
	Brokers []RuntimeBroker
	Page    apiclient.PageResult
}

// UpdateBrokerRequest is the request for updating a runtime broker.
type UpdateBrokerRequest struct {
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ListBrokerProjectsResponse is the response from listing broker projects.
type ListBrokerProjectsResponse struct {
	Projects []BrokerProjectInfo `json:"projects"`
}

// MarshalJSON implements custom marshaling to support legacy grove fields.
func (r ListBrokerProjectsResponse) MarshalJSON() ([]byte, error) {
	type Alias ListBrokerProjectsResponse
	return json.Marshal(&struct {
		Alias
		Groves []BrokerProjectInfo `json:"groves,omitempty"`
	}{
		Alias:  Alias(r),
		Groves: r.Projects,
	})
}

// UnmarshalJSON implements custom unmarshaling to support legacy grove fields.
func (r *ListBrokerProjectsResponse) UnmarshalJSON(data []byte) error {
	type Alias ListBrokerProjectsResponse
	aux := &struct {
		Groves []BrokerProjectInfo `json:"groves,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(r.Projects) == 0 && len(aux.Groves) > 0 {
		r.Projects = aux.Groves
	}
	return nil
}

// BrokerHeartbeat is the heartbeat payload.
type BrokerHeartbeat struct {
	Status   string             `json:"status"`
	Projects []ProjectHeartbeat `json:"projects,omitempty"`
}

// MarshalJSON implements custom marshaling to support legacy grove fields.
func (h BrokerHeartbeat) MarshalJSON() ([]byte, error) {
	type Alias BrokerHeartbeat
	return json.Marshal(&struct {
		Alias
		Groves []ProjectHeartbeat `json:"groves,omitempty"`
	}{
		Alias:  Alias(h),
		Groves: h.Projects,
	})
}

// ProjectHeartbeat is per-project status in a heartbeat.
type ProjectHeartbeat struct {
	ProjectID  string           `json:"projectId"`
	AgentCount int              `json:"agentCount"`
	Agents     []AgentHeartbeat `json:"agents,omitempty"`
}

// MarshalJSON implements custom marshaling to support legacy grove fields.
func (h ProjectHeartbeat) MarshalJSON() ([]byte, error) {
	type Alias ProjectHeartbeat
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(h),
		GroveID: h.ProjectID,
	})
}

// AgentHeartbeat is per-agent status in a heartbeat.
type AgentHeartbeat struct {
	Slug            string `json:"slug"` // Agent's URL-safe identifier
	Status          string `json:"status"`
	Phase           string `json:"phase,omitempty"`
	Activity        string `json:"activity,omitempty"`
	ContainerStatus string `json:"containerStatus,omitempty"`
	Message         string `json:"message,omitempty"`     // Error or status message from agent-info.json
	HarnessAuth     string `json:"harnessAuth,omitempty"` // Resolved auth method from container labels
	Profile         string `json:"profile,omitempty"`     // Settings profile used
}

// CreateBrokerRequest is the request to create a new broker registration.
type CreateBrokerRequest struct {
	BrokerID     string            `json:"brokerId,omitempty"` // Optional stable broker UUID supplied by the client
	Name         string            `json:"name"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	AutoProvide  bool              `json:"autoProvide,omitempty"` // Automatically add as provider for new projects
}

// CreateBrokerResponse is returned when creating a new broker.
type CreateBrokerResponse struct {
	BrokerID     string `json:"brokerId"`
	JoinToken    string `json:"joinToken"`
	ExpiresAt    string `json:"expiresAt"`
	Reregistered bool   `json:"reregistered,omitempty"`
}

// JoinBrokerRequest is the request to complete broker registration.
type JoinBrokerRequest struct {
	BrokerID     string          `json:"brokerId"`
	JoinToken    string          `json:"joinToken"`
	Hostname     string          `json:"hostname"`
	Version      string          `json:"version"`
	Capabilities []string        `json:"capabilities,omitempty"`
	Profiles     []BrokerProfile `json:"profiles,omitempty"`
}

// JoinBrokerResponse is returned after completing broker registration.
type JoinBrokerResponse struct {
	SecretKey   string `json:"secretKey"` // Base64-encoded HMAC secret
	HubEndpoint string `json:"hubEndpoint"`
	BrokerID    string `json:"brokerId"`
}

// Create creates a new broker registration and returns a join token.
func (s *runtimeBrokerService) Create(ctx context.Context, req *CreateBrokerRequest) (*CreateBrokerResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/brokers", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[CreateBrokerResponse](resp)
}

// Join completes broker registration using a join token.
func (s *runtimeBrokerService) Join(ctx context.Context, req *JoinBrokerRequest) (*JoinBrokerResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/brokers/join", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[JoinBrokerResponse](resp)
}

// List returns runtime brokers matching the filter criteria.
func (s *runtimeBrokerService) List(ctx context.Context, opts *ListBrokersOptions) (*ListBrokersResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		if opts.ProjectID != "" {
			query.Set("projectId", opts.ProjectID)
			query.Set("groveId", opts.ProjectID)
		}
		if opts.Name != "" {
			query.Set("name", opts.Name)
		}
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/runtime-brokers", query, nil)
	if err != nil {
		return nil, err
	}

	type listResponse struct {
		Brokers    []RuntimeBroker `json:"brokers"`
		NextCursor string          `json:"nextCursor,omitempty"`
		TotalCount int             `json:"totalCount,omitempty"`
	}

	result, err := apiclient.DecodeResponse[listResponse](resp)
	if err != nil {
		return nil, err
	}

	return &ListBrokersResponse{
		Brokers: result.Brokers,
		Page: apiclient.PageResult{
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		},
	}, nil
}

// Get returns a single runtime broker by ID.
func (s *runtimeBrokerService) Get(ctx context.Context, brokerID string) (*RuntimeBroker, error) {
	resp, err := s.c.get(ctx, "/api/v1/runtime-brokers/"+brokerID, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[RuntimeBroker](resp)
}

// Update updates broker metadata.
func (s *runtimeBrokerService) Update(ctx context.Context, brokerID string, req *UpdateBrokerRequest) (*RuntimeBroker, error) {
	resp, err := s.c.patch(ctx, "/api/v1/runtime-brokers/"+brokerID, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[RuntimeBroker](resp)
}

// Delete removes a broker from all projects.
func (s *runtimeBrokerService) Delete(ctx context.Context, brokerID string) error {
	resp, err := s.c.delete(ctx, "/api/v1/runtime-brokers/"+brokerID, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// ListProjects returns projects this broker contributes to.
func (s *runtimeBrokerService) ListProjects(ctx context.Context, brokerID string) (*ListBrokerProjectsResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/runtime-brokers/"+brokerID+"/projects", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListBrokerProjectsResponse](resp)
}

// Heartbeat sends a heartbeat for a broker.
func (s *runtimeBrokerService) Heartbeat(ctx context.Context, brokerID string, status *BrokerHeartbeat) error {
	resp, err := s.c.post(ctx, "/api/v1/runtime-brokers/"+brokerID+"/heartbeat", status, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
