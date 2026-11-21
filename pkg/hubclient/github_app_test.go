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
	var lastGetJSON []byte
	p1 := "p1"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			payload, err := json.Marshal([]GitHubRepoRoutingEntry{
				{
					InstallationID:   42,
					Repo:             "acme/widgets",
					Projects:         []GitHubRepoRoutingProject{{ID: "p1", Name: "alpha", HasGitHubTriggers: false}},
					PrimaryProjectID: &p1,
					Status:           "ok",
				},
				{
					InstallationID:   42,
					Repo:             "acme/gadgets",
					Projects:         []GitHubRepoRoutingProject{{ID: "p2", Name: "beta", HasGitHubTriggers: false}},
					PrimaryProjectID: nil,
					Status:           "none",
				},
			})
			require.NoError(t, err)
			lastGetJSON = payload
			_, _ = w.Write(payload)
		case http.MethodPut:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
			primaryID := gotBody.PrimaryProjectID
			_ = json.NewEncoder(w).Encode(GitHubRepoRoutingEntry{
				InstallationID:   gotBody.InstallationID,
				Repo:             "acme/widgets",
				Projects:         []GitHubRepoRoutingProject{{ID: gotBody.PrimaryProjectID, Name: "alpha"}},
				PrimaryProjectID: &primaryID,
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
	require.Len(t, list, 2)
	assert.Equal(t, "ok", list[0].Status)
	require.NotNil(t, list[0].PrimaryProjectID)
	assert.Equal(t, "p1", *list[0].PrimaryProjectID)
	assert.Equal(t, "none", list[1].Status)
	assert.Nil(t, list[1].PrimaryProjectID)

	var rawWire []map[string]any
	require.NoError(t, json.Unmarshal(lastGetJSON, &rawWire))
	require.Len(t, rawWire, 2)
	unsetVal, hasUnsetKey := rawWire[1]["primary_project_id"]
	assert.True(t, hasUnsetKey, "GitHubRepoRoutingEntry must always serialize primary_project_id (null when unset)")
	assert.Nil(t, unsetVal)

	entry, err := c.GitHubApp().SetRepoPrimary(ctx, "Acme", "Widgets", &SetGitHubRepoPrimaryRequest{
		InstallationID:   42,
		PrimaryProjectID: "p1",
	})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, "/api/v1/github-app/repo-routing/Acme/Widgets", gotPath)
	assert.Equal(t, int64(42), gotBody.InstallationID)
	require.NotNil(t, entry.PrimaryProjectID)
	assert.Equal(t, "p1", *entry.PrimaryProjectID)

	require.NoError(t, c.GitHubApp().DeleteRepoPrimary(ctx, "Acme", "Widgets", &instID))
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/api/v1/github-app/repo-routing/Acme/Widgets", gotPath)
	assert.Equal(t, "installation=42", gotQuery)
}

func TestGitHubAppService_GetConfig(t *testing.T) {
	ctx := context.Background()

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitHubAppConfig{
			AppID:            98765,
			WebhooksEnabled:  true,
			Configured:       true,
			HasPrivateKey:    true,
			HasWebhookSecret: true,
			Triggers: &GitHubTriggersReadiness{
				Ready:              false,
				WebhooksEnabled:    true,
				HasWebhookSecret:   true,
				MissingEvents:      []string{"issue_comment"},
				MissingPermissions: []string{"issues:write"},
				AppSlug:            "scion-bot",
			},
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	cfg, err := c.GitHubApp().GetConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/api/v1/github-app", gotPath)
	assert.Equal(t, int64(98765), cfg.AppID)
	assert.True(t, cfg.Configured)
	assert.True(t, cfg.WebhooksEnabled)
	assert.True(t, cfg.HasWebhookSecret)
	require.NotNil(t, cfg.Triggers)
	assert.False(t, cfg.Triggers.Ready)
	assert.Equal(t, []string{"issue_comment"}, cfg.Triggers.MissingEvents)
	assert.Equal(t, []string{"issues:write"}, cfg.Triggers.MissingPermissions)
	assert.Equal(t, "scion-bot", cfg.Triggers.AppSlug)
}
