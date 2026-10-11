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
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// TestIdentityService handles hub test identities (ptone/scion#4240): short
// lived synthetic member or viewer users for testing. The hub must run with
// --enable-test-identities, otherwise every call returns a 404 API error.
// The caller needs the test_identity.issue permission.
type TestIdentityService interface {
	// Issue creates a test identity and returns one access token for it.
	Issue(ctx context.Context, req *IssueTestIdentityRequest) (*TestIdentityTokenResponse, error)

	// Token issues a new access token for a live test identity.
	Token(ctx context.Context, id string, req *TestIdentityTokenRequest) (*TestIdentityTokenResponse, error)

	// List returns the caller's test identities (every identity for an
	// admin session).
	List(ctx context.Context, opts *ListTestIdentitiesOptions) (*ListTestIdentitiesResponse, error)

	// Delete deletes a test identity. It returns a 404 API error when the
	// identity does not exist (for example, it was already deleted) and a
	// 409 API error while the identity owns agents or is the last owner of
	// a project (see TestIdentityDeleteConflict).
	Delete(ctx context.Context, id string) error

	// Purge deletes a test identity together with the agents, schedules,
	// groups and projects it created (DELETE ...?purge=true). A complete
	// purge returns the report and a nil error. One that did not complete
	// returns the report and a 409 API error (run it again); see
	// TestIdentityPurgeIncomplete. An identity that is already gone, or one
	// the caller may not purge, returns an empty report.
	Purge(ctx context.Context, id string) (*TestIdentityPurgeReport, error)
}

// TestIdentityPurgeCounts counts what a purge removed.
type TestIdentityPurgeCounts struct {
	Agents    int `json:"agents"`
	Schedules int `json:"schedules"`
	Groups    int `json:"groups"`
	Projects  int `json:"projects"`
}

// TestIdentityPurgeItem is a resource a purge did not remove.
type TestIdentityPurgeItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ProjectID string `json:"projectId,omitempty"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

// TestIdentityPurgeKeptProject is a project a purge kept.
type TestIdentityPurgeKeptProject struct {
	ID              string `json:"id"`
	Reason          string `json:"reason"`
	BindingsRemoved int    `json:"bindingsRemoved"`
	Ownerless       bool   `json:"ownerless,omitempty"`
}

// TestIdentityPurgeReport is the hub's report of a purge.
type TestIdentityPurgeReport struct {
	IdentityID         string                         `json:"identityId"`
	Complete           bool                           `json:"complete"`
	IdentityDeleted    bool                           `json:"identityDeleted"`
	Deleted            TestIdentityPurgeCounts        `json:"deleted"`
	NotDone            []TestIdentityPurgeItem        `json:"notDone,omitempty"`
	Unproven           []TestIdentityPurgeItem        `json:"unproven,omitempty"`
	KeptProjects       []TestIdentityPurgeKeptProject `json:"keptProjects,omitempty"`
	IdentityKeptReason string                         `json:"identityKeptReason,omitempty"`
	Truncated          bool                           `json:"truncated,omitempty"`
}

// testIdentityService is the implementation of TestIdentityService.
type testIdentityService struct {
	c *client
}

// IssueTestIdentityRequest is the request for POST /api/v1/test-identities.
type IssueTestIdentityRequest struct {
	// Role is "member" or "viewer"; empty means member.
	Role string `json:"role,omitempty"`
	// Purpose is a short free-text label recorded on the identity.
	Purpose string `json:"purpose,omitempty"`
	// LifetimeSeconds is the identity lifetime; 0 means the hub default.
	LifetimeSeconds int64 `json:"lifetimeSeconds,omitempty"`
	// TokenTTLSeconds is the access token lifetime; 0 means the hub
	// default. The token never outlives the identity.
	TokenTTLSeconds int64 `json:"tokenTtlSeconds,omitempty"`
}

// TestIdentityTokenRequest is the request for POST
// /api/v1/test-identities/{id}/token.
type TestIdentityTokenRequest struct {
	TokenTTLSeconds int64 `json:"tokenTtlSeconds,omitempty"`
}

// TestIdentity is a test identity as the hub reports it. It never carries a
// token.
type TestIdentity struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	Purpose     string    `json:"purpose,omitempty"`
	IssuedBy    string    `json:"issuedBy"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Created     time.Time `json:"created"`
	Live        bool      `json:"live"`
}

// TestIdentityTokenResponse is the response of Issue and Token. AccessToken
// is a secret: write it only where the caller asked for it, never to logs
// or standard output.
type TestIdentityTokenResponse struct {
	Identity       TestIdentity `json:"identity"`
	AccessToken    string       `json:"accessToken"`
	TokenType      string       `json:"tokenType"`
	ExpiresIn      int64        `json:"expiresIn"`
	TokenExpiresAt time.Time    `json:"tokenExpiresAt"`
}

// ListTestIdentitiesOptions are the query options of List.
type ListTestIdentitiesOptions struct {
	// Limit caps the number of identities returned; 0 means the hub
	// default.
	Limit int
	// IncludeExpired lists expired identities too.
	IncludeExpired bool
}

// ListTestIdentitiesResponse is the response of List.
type ListTestIdentitiesResponse struct {
	Items []TestIdentity `json:"items"`
	// Truncated is true when more identities match than were returned.
	Truncated bool `json:"truncated"`
}

// Issue creates a test identity. It is sent once, never retried, since a
// retry could create a second identity.
func (s *testIdentityService) Issue(ctx context.Context, req *IssueTestIdentityRequest) (*TestIdentityTokenResponse, error) {
	if req == nil {
		req = &IssueTestIdentityRequest{}
	}
	resp, err := s.c.postNoRetry(ctx, "/api/v1/test-identities", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[TestIdentityTokenResponse](resp)
}

// Token issues a new access token for a live test identity. It is sent
// once, never retried.
func (s *testIdentityService) Token(ctx context.Context, id string, req *TestIdentityTokenRequest) (*TestIdentityTokenResponse, error) {
	if id == "" {
		return nil, fmt.Errorf("test identity id is required")
	}
	if req == nil {
		req = &TestIdentityTokenRequest{}
	}
	resp, err := s.c.postNoRetry(ctx, "/api/v1/test-identities/"+url.PathEscape(id)+"/token", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[TestIdentityTokenResponse](resp)
}

// List returns test identities.
func (s *testIdentityService) List(ctx context.Context, opts *ListTestIdentitiesOptions) (*ListTestIdentitiesResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.IncludeExpired {
			query.Set("includeExpired", "true")
		}
	}
	resp, err := s.c.getWithQuery(ctx, "/api/v1/test-identities", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListTestIdentitiesResponse](resp)
}

// Delete deletes a test identity.
func (s *testIdentityService) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("test identity id is required")
	}
	resp, err := s.c.delete(ctx, "/api/v1/test-identities/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Purge deletes a test identity and what it created. It is sent once,
// never retried by the transport; running it again is safe.
func (s *testIdentityService) Purge(ctx context.Context, id string) (*TestIdentityPurgeReport, error) {
	if id == "" {
		return nil, fmt.Errorf("test identity id is required")
	}
	resp, err := s.c.delete(ctx, "/api/v1/test-identities/"+url.PathEscape(id)+"?purge=true", nil)
	if err != nil {
		return nil, err
	}
	report, err := apiclient.DecodeRequired[TestIdentityPurgeReport](resp)
	if err != nil {
		if r, ok := TestIdentityPurgeIncomplete(err); ok {
			return r, err
		}
		return nil, err
	}
	return report, nil
}

// TestIdentityPurgeIncomplete returns the report carried by a purge answer
// that did not complete (details.reason purge_incomplete). ok is false when
// err is not such an answer.
func TestIdentityPurgeIncomplete(err error) (*TestIdentityPurgeReport, bool) {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Details == nil {
		return nil, false
	}
	if reason, _ := apiErr.Details["reason"].(string); reason != "purge_incomplete" {
		return nil, false
	}
	raw, err2 := json.Marshal(apiErr.Details["report"])
	if err2 != nil {
		return nil, false
	}
	var report TestIdentityPurgeReport
	if json.Unmarshal(raw, &report) != nil {
		return nil, false
	}
	return &report, true
}

// TestIdentityBlockingAgent is an agent that blocks a test identity delete.
type TestIdentityBlockingAgent struct {
	ID        string
	Slug      string
	ProjectID string
}

// TestIdentityBlockingProject is a project the test identity is the last
// owner of, which blocks its delete.
type TestIdentityBlockingProject struct {
	ID   string
	Name string
}

// TestIdentityDeleteConflict reports the resources listed by a 409 answer
// to Delete: the agents the identity still owns, or the projects it is the
// last owner of. ok is false when err is not such an answer.
func TestIdentityDeleteConflict(err error) (agents []TestIdentityBlockingAgent, projects []TestIdentityBlockingProject, ok bool) {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || !apiErr.IsConflict() {
		return nil, nil, false
	}
	for _, raw := range detailList(apiErr.Details, "agents") {
		agents = append(agents, TestIdentityBlockingAgent{
			ID: detailString(raw, "id"), Slug: detailString(raw, "slug"), ProjectID: detailString(raw, "projectId"),
		})
	}
	for _, raw := range detailList(apiErr.Details, "projects") {
		projects = append(projects, TestIdentityBlockingProject{
			ID: detailString(raw, "id"), Name: detailString(raw, "name"),
		})
	}
	return agents, projects, true
}

func detailList(details map[string]interface{}, key string) []map[string]interface{} {
	items, _ := details[key].([]interface{})
	out := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out
}

func detailString(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}
