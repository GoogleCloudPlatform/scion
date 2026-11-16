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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitHubAppService_RepoRouting(t *testing.T) {
	ctx := context.Background()

	var gotMethod, gotPath, gotQuery string
	var gotBody SetGitHubRepoPrimaryRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]GitHubRepoRoutingEntry{
				{
					InstallationID:   42,
					Repo:             "acme/widgets",
					Projects:         []GitHubRepoRoutingProject{{ID: "p1", Name: "alpha", HasGitHubTriggers: false}},
					PrimaryProjectID: "p1",
					Status:           "ok",
				},
			})
		case http.MethodPut:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
			_ = json.NewEncoder(w).Encode(GitHubRepoRoutingEntry{
				InstallationID:   gotBody.InstallationID,
				Repo:             "acme/widgets",
				Projects:         []GitHubRepoRoutingProject{{ID: gotBody.PrimaryProjectID, Name: "alpha"}},
				PrimaryProjectID: gotBody.PrimaryProjectID,
				Status:           "ok",
			})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := New(srv.URL)
	require.NoError(t, err)

	instID := int64(42)
	list, err := c.GitHubApp().ListRepoRouting(ctx, &instID)
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/api/v1/github-app/repo-routing", gotPath)
	assert.Equal(t, "installation=42", gotQuery)
	require.Len(t, list, 1)
	assert.Equal(t, "ok", list[0].Status)

	entry, err := c.GitHubApp().SetRepoPrimary(ctx, "Acme", "Widgets", &SetGitHubRepoPrimaryRequest{
		InstallationID:   42,
		PrimaryProjectID: "p1",
	})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, "/api/v1/github-app/repo-routing/Acme/Widgets", gotPath)
	assert.Equal(t, int64(42), gotBody.InstallationID)
	assert.Equal(t, "p1", entry.PrimaryProjectID)

	require.NoError(t, c.GitHubApp().DeleteRepoPrimary(ctx, "Acme", "Widgets", &instID))
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/api/v1/github-app/repo-routing/Acme/Widgets", gotPath)
	assert.Equal(t, "installation=42", gotQuery)
}
