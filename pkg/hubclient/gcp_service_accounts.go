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
	"fmt"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// GCPServiceAccountService handles GCP service account operations for a project.
type GCPServiceAccountService interface {
	// List returns all GCP service accounts for the project.
	List(ctx context.Context) ([]GCPServiceAccount, error)

	// Get returns a specific GCP service account by ID.
	Get(ctx context.Context, id string) (*GCPServiceAccount, error)

	// Create registers a new GCP service account.
	Create(ctx context.Context, req *CreateGCPServiceAccountRequest) (*GCPServiceAccount, error)

	// Delete removes a GCP service account registration.
	Delete(ctx context.Context, id string) error

	// Verify triggers verification that the Hub can impersonate the SA.
	Verify(ctx context.Context, id string) (*GCPServiceAccount, error)

	// Mint creates a new GCP service account in the Hub's GCP project.
	Mint(ctx context.Context, req *MintGCPServiceAccountRequest) (*GCPServiceAccount, error)
}

// GCPServiceAccount represents a registered GCP service account.
type GCPServiceAccount struct {
	ID                 string    `json:"id"`
	Scope              string    `json:"scope"`
	ScopeID            string    `json:"scopeId"`
	Email              string    `json:"email"`
	ProjectID          string    `json:"projectId"` // GCP Project ID
	DisplayName        string    `json:"displayName"`
	DefaultScopes      []string  `json:"defaultScopes,omitempty"`
	Verified           bool      `json:"verified"`
	VerifiedAt         time.Time `json:"verifiedAt,omitempty"`
	VerificationStatus string    `json:"verificationStatus,omitempty"`
	VerificationError  string    `json:"verificationError,omitempty"`
	CreatedBy          string    `json:"createdBy"`
	CreatedAt          time.Time `json:"createdAt"`
	Managed            bool      `json:"managed"`
	ManagedBy          string    `json:"managedBy,omitempty"`
}

// CreateGCPServiceAccountRequest is the request for registering a GCP SA.
type CreateGCPServiceAccountRequest struct {
	Email       string   `json:"email"`
	ProjectID   string   `json:"projectId"`
	DisplayName string   `json:"displayName,omitempty"`
	Scopes      []string `json:"defaultScopes,omitempty"`
}

// MintGCPServiceAccountRequest is the request for minting a new GCP SA.
type MintGCPServiceAccountRequest struct {
	AccountID   string `json:"account_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
}

// gcpServiceAccountService is the implementation of GCPServiceAccountService.
type gcpServiceAccountService struct {
	c         *client
	projectID string
}

func (s *gcpServiceAccountService) basePath() string {
	return fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", s.projectID)
}

func (s *gcpServiceAccountService) List(ctx context.Context) ([]GCPServiceAccount, error) {
	resp, err := s.c.get(ctx, s.basePath(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, apiclient.ParseErrorResponse(resp)
	}
	var result []GCPServiceAccount
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}

func (s *gcpServiceAccountService) Get(ctx context.Context, id string) (*GCPServiceAccount, error) {
	path := fmt.Sprintf("%s/%s", s.basePath(), id)
	resp, err := s.c.get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[GCPServiceAccount](resp)
}

func (s *gcpServiceAccountService) Create(ctx context.Context, req *CreateGCPServiceAccountRequest) (*GCPServiceAccount, error) {
	resp, err := s.c.post(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[GCPServiceAccount](resp)
}

func (s *gcpServiceAccountService) Delete(ctx context.Context, id string) error {
	path := fmt.Sprintf("%s/%s", s.basePath(), id)
	_, err := s.c.delete(ctx, path, nil)
	return err
}

func (s *gcpServiceAccountService) Verify(ctx context.Context, id string) (*GCPServiceAccount, error) {
	path := fmt.Sprintf("%s/%s/verify", s.basePath(), id)
	resp, err := s.c.post(ctx, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[GCPServiceAccount](resp)
}

func (s *gcpServiceAccountService) Mint(ctx context.Context, req *MintGCPServiceAccountRequest) (*GCPServiceAccount, error) {
	path := fmt.Sprintf("%s/mint", s.basePath())
	resp, err := s.c.post(ctx, path, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[GCPServiceAccount](resp)
}
