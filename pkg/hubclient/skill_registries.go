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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// SkillRegistryService defines operations for skill registries.
type SkillRegistryService interface {
	List(ctx context.Context) (*ListSkillRegistriesResponse, error)
	Get(ctx context.Context, id string) (*SkillRegistry, error)
	Create(ctx context.Context, req *CreateSkillRegistryRequest) (*SkillRegistry, error)
	Update(ctx context.Context, id string, req *UpdateSkillRegistryRequest) (*SkillRegistry, error)
	Delete(ctx context.Context, id string) error
	Pin(ctx context.Context, id string, req *PinSkillHashRequest) error
}

type skillRegistryService struct {
	c *client
}

// SkillRegistry represents a skill registry from the Hub API.
type SkillRegistry struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Endpoint    string    `json:"endpoint"`
	Description string    `json:"description,omitempty"`
	Type        string    `json:"type"`
	TrustLevel  string    `json:"trustLevel"`
	ResolvePath string    `json:"resolvePath,omitempty"`
	Status      string    `json:"status"`
	CreatedBy   string    `json:"createdBy,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
}

// ListSkillRegistriesResponse is the response for listing skill registries.
type ListSkillRegistriesResponse struct {
	Items      []SkillRegistry `json:"items"`
	TotalCount int             `json:"totalCount"`
}

// CreateSkillRegistryRequest is the request body for creating a skill registry.
type CreateSkillRegistryRequest struct {
	Name        string `json:"name"`
	Endpoint    string `json:"endpoint"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	TrustLevel  string `json:"trustLevel,omitempty"`
	AuthToken   string `json:"authToken,omitempty"`
	ResolvePath string `json:"resolvePath,omitempty"`
}

// UpdateSkillRegistryRequest is the request body for updating a skill registry.
type UpdateSkillRegistryRequest struct {
	Endpoint    string `json:"endpoint,omitempty"`
	Description string `json:"description,omitempty"`
	TrustLevel  string `json:"trustLevel,omitempty"`
	AuthToken   string `json:"authToken,omitempty"`
	ResolvePath string `json:"resolvePath,omitempty"`
	Status      string `json:"status,omitempty"`
}

// PinSkillHashRequest is the request body for pinning a skill hash.
type PinSkillHashRequest struct {
	URI  string `json:"uri"`
	Hash string `json:"hash"`
}

func (s *skillRegistryService) List(ctx context.Context) (*ListSkillRegistriesResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/skill-registries", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListSkillRegistriesResponse](resp)
}

func (s *skillRegistryService) Get(ctx context.Context, id string) (*SkillRegistry, error) {
	resp, err := s.c.get(ctx, "/api/v1/skill-registries/"+id, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SkillRegistry](resp)
}

func (s *skillRegistryService) Create(ctx context.Context, req *CreateSkillRegistryRequest) (*SkillRegistry, error) {
	resp, err := s.c.post(ctx, "/api/v1/skill-registries", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SkillRegistry](resp)
}

func (s *skillRegistryService) Update(ctx context.Context, id string, req *UpdateSkillRegistryRequest) (*SkillRegistry, error) {
	resp, err := s.c.put(ctx, "/api/v1/skill-registries/"+id, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SkillRegistry](resp)
}

func (s *skillRegistryService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.delete(ctx, "/api/v1/skill-registries/"+id, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

func (s *skillRegistryService) Pin(ctx context.Context, id string, req *PinSkillHashRequest) error {
	resp, err := s.c.post(ctx, "/api/v1/skill-registries/"+id+"/pin", req, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
