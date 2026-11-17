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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectService_GitHubTriggerSettings(t *testing.T) {
	checkedAt := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/projects/proj-1/trigger-sources/github", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{
				"unmatched_command_reply": false,
				"unmatched_command_text": "",
				"readiness": {
					"ready": false,
					"missing_events": ["issue_comment"],
					"missing_permissions": ["issues:write"],
					"app_slug": "scion-bot",
					"checked_at": "2026-11-17T03:00:00Z"
				},
				"routing": {
					"primary_project": null,
					"is_primary": false,
					"other_projects": ["widgets-b"]
				},
				"warnings": ["multi_project_repo"]
			}`))
		case http.MethodPut:
			var req UpdateProjectGitHubTriggerSettingsRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.True(t, req.UnmatchedCommandReply)
			assert.Equal(t, "Available: /triage, /review", req.UnmatchedCommandText)
			_, _ = w.Write([]byte(`{
				"unmatched_command_reply": true,
				"unmatched_command_text": "Available: /triage, /review",
				"readiness": {
					"ready": true,
					"missing_events": [],
					"missing_permissions": [],
					"app_slug": "scion-bot"
				},
				"routing": {
					"primary_project": {"id": "proj-1", "name": "widgets-a"},
					"is_primary": true,
					"other_projects": ["widgets-b"]
				}
			}`))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	got, err := client.Projects().GetGitHubTriggerSettings(context.Background(), "proj-1")
	require.NoError(t, err)
	assert.False(t, got.UnmatchedCommandReply)
	assert.Equal(t, "", got.UnmatchedCommandText)
	assert.False(t, got.Readiness.Ready)
	assert.Equal(t, []string{"issue_comment"}, got.Readiness.MissingEvents)
	assert.Equal(t, []string{"issues:write"}, got.Readiness.MissingPermissions)
	assert.Equal(t, "scion-bot", got.Readiness.AppSlug)
	require.NotNil(t, got.Readiness.CheckedAt)
	assert.Equal(t, checkedAt, *got.Readiness.CheckedAt)
	assert.Nil(t, got.Routing.PrimaryProject)
	assert.False(t, got.Routing.IsPrimary)
	assert.Equal(t, []string{"widgets-b"}, got.Routing.OtherProjects)
	assert.Equal(t, []string{"multi_project_repo"}, got.Warnings)

	updated, err := client.Projects().UpdateGitHubTriggerSettings(context.Background(), "proj-1", &UpdateProjectGitHubTriggerSettingsRequest{
		UnmatchedCommandReply: true,
		UnmatchedCommandText:  "Available: /triage, /review",
	})
	require.NoError(t, err)
	assert.True(t, updated.UnmatchedCommandReply)
	assert.Equal(t, "Available: /triage, /review", updated.UnmatchedCommandText)
	assert.True(t, updated.Readiness.Ready)
	assert.Nil(t, updated.Readiness.CheckedAt, "omitted checked_at before first fetch decodes to nil")
	require.NotNil(t, updated.Routing.PrimaryProject)
	assert.Equal(t, "proj-1", updated.Routing.PrimaryProject.ID)
	assert.Equal(t, "widgets-a", updated.Routing.PrimaryProject.Name)
	assert.True(t, updated.Routing.IsPrimary)
	assert.Empty(t, updated.Warnings)

	_, err = client.Projects().GetGitHubTriggerSettings(context.Background(), "")
	require.Error(t, err)
	_, err = client.Projects().UpdateGitHubTriggerSettings(context.Background(), "", &UpdateProjectGitHubTriggerSettingsRequest{})
	require.Error(t, err)
	_, err = client.Projects().UpdateGitHubTriggerSettings(context.Background(), "proj-1", nil)
	require.Error(t, err)
}
