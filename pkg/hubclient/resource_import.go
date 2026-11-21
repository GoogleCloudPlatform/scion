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

// ImportResourcesRequest is the request body for POST /api/v1/resources/import.
type ImportResourcesRequest struct {
	Kind      string   `json:"kind"`
	Scope     string   `json:"scope"`
	ScopeID   string   `json:"scopeId,omitempty"`
	SourceURL string   `json:"sourceUrl"`
	Names     []string `json:"names,omitempty"`
}

// ResourceImportFailure names a resource that failed to import and why.
type ResourceImportFailure struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ImportResourcesResponse is the response from POST /api/v1/resources/import.
type ImportResourcesResponse struct {
	Kind     string                  `json:"kind"`
	Imported []string                `json:"imported"`
	Count    int                     `json:"count"`
	Failed   []ResourceImportFailure `json:"failed,omitempty"`
}

// DiscoverResourcesRequest is the request body for POST /api/v1/resources/discover.
type DiscoverResourcesRequest struct {
	Kind      string `json:"kind"`
	Scope     string `json:"scope"`
	ScopeID   string `json:"scopeId,omitempty"`
	SourceURL string `json:"sourceUrl"`
}

// DiscoverResourcesResponse is the response from POST /api/v1/resources/discover.
type DiscoverResourcesResponse struct {
	Resources []string `json:"resources"`
	Skipped   []string `json:"skipped,omitempty"`
	Count     int      `json:"count"`
}

// ImportResources imports resources from a remote URL through the Hub.
// It always requests plain application/json (never the NDJSON stream).
func (s *templateService) ImportResources(ctx context.Context, req *ImportResourcesRequest) (*ImportResourcesResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/resources/import", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ImportResourcesResponse](resp)
}

// DiscoverResources discovers resources at a remote URL through the Hub
// without writing anything.
func (s *templateService) DiscoverResources(ctx context.Context, req *DiscoverResourcesRequest) (*DiscoverResourcesResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/resources/discover", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[DiscoverResourcesResponse](resp)
}
