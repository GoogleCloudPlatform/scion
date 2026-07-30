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

package hubclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverSkillsDirectory_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/api/v1/skills/discover-directory" && r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(hubclient.DiscoverSkillsDirectoryResponse{
				Skills: []hubclient.DiscoveredSkill{
					{URI: "skill://org/skill-a", Name: "skill-a"},
					{URI: "skill://org/skill-b", Name: "skill-b"},
				},
				Count: 2,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	resp, err := c.DiscoverSkillsDirectory(context.Background(), hubclient.DiscoverSkillsDirectoryRequest{
		SourceURL: "https://github.com/org/repo/tree/main/skills",
	})
	require.NoError(t, err)
	assert.Len(t, resp.Skills, 2)
	assert.Equal(t, "skill://org/skill-a", resp.Skills[0].URI)
	assert.Equal(t, "skill-a", resp.Skills[0].Name)
	assert.Equal(t, "skill://org/skill-b", resp.Skills[1].URI)
	assert.Equal(t, "skill-b", resp.Skills[1].Name)
	assert.Equal(t, 2, resp.Count)
}

func TestDiscoverSkillsDirectory_ErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/api/v1/skills/discover-directory" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    "bad_request",
				"message": "invalid source URL",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	_, err = c.DiscoverSkillsDirectory(context.Background(), hubclient.DiscoverSkillsDirectoryRequest{
		SourceURL: "not-a-valid-url",
	})
	assert.Error(t, err)
}

func TestDiscoverSkillsDirectory_ProjectIDForwarded(t *testing.T) {
	var receivedProjectID string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/api/v1/skills/discover-directory" && r.Method == http.MethodPost {
			var req hubclient.DiscoverSkillsDirectoryRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			receivedProjectID = req.ProjectID
			_ = json.NewEncoder(w).Encode(hubclient.DiscoverSkillsDirectoryResponse{
				Skills: []hubclient.DiscoveredSkill{
					{URI: "skill://org/skill-a", Name: "skill-a"},
				},
				Count: 1,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	c, err := hubclient.New(server.URL)
	require.NoError(t, err)

	_, err = c.DiscoverSkillsDirectory(context.Background(), hubclient.DiscoverSkillsDirectoryRequest{
		SourceURL: "https://github.com/org/repo/tree/main/skills",
		ProjectID: "my-project-id",
	})
	require.NoError(t, err)
	assert.Equal(t, "my-project-id", receivedProjectID)
}
