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

// DiscoverSkillsDirectoryRequest is the request body for POST /api/v1/skills/discover-directory.
type DiscoverSkillsDirectoryRequest struct {
	SourceURL string `json:"sourceUrl"`
	ProjectID string `json:"projectId,omitempty"`
}

// DiscoveredSkill is one entry in a discover-directory response.
type DiscoveredSkill struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// DiscoverSkillsDirectoryResponse is the response body for POST /api/v1/skills/discover-directory.
type DiscoverSkillsDirectoryResponse struct {
	Skills  []DiscoveredSkill `json:"skills"`
	Count   int               `json:"count"`
	Skipped []string          `json:"skipped,omitempty"`
}

// DiscoverSkillsDirectory calls POST /api/v1/skills/discover-directory and returns
// the list of skills found at the given GitHub directory URL.
func (c *client) DiscoverSkillsDirectory(ctx context.Context, req DiscoverSkillsDirectoryRequest) (*DiscoverSkillsDirectoryResponse, error) {
	resp, err := c.post(ctx, "/api/v1/skills/discover-directory", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[DiscoverSkillsDirectoryResponse](resp)
}
