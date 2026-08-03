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

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// SecretIntakeService handles secret intake link operations.
type SecretIntakeService interface {
	// Create creates a new secret intake link and returns the URL.
	Create(ctx context.Context, req *CreateIntakeRequest) (*CreateIntakeResponse, error)
}

// secretIntakeService is the implementation of SecretIntakeService.
type secretIntakeService struct {
	c *client
}

// CreateIntakeRequest is the request body for creating a secret intake link.
type CreateIntakeRequest struct {
	Key            string `json:"key"`
	Scope          string `json:"scope,omitempty"`
	ScopeID        string `json:"scope_id,omitempty"`
	SecretType     string `json:"type,omitempty"`
	Target         string `json:"target,omitempty"`
	Description    string `json:"description,omitempty"`
	TTLSeconds     int    `json:"ttl_seconds,omitempty"`
	Channel        string `json:"channel,omitempty"`
	ChannelContext string `json:"channel_context,omitempty"`
}

// CreateIntakeResponse is the response from creating a secret intake link.
type CreateIntakeResponse struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
	IntakeID  string `json:"intake_id"`
}

// Create creates a new secret intake link.
func (s *secretIntakeService) Create(ctx context.Context, req *CreateIntakeRequest) (*CreateIntakeResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/secret-intake", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[CreateIntakeResponse](resp)
}
