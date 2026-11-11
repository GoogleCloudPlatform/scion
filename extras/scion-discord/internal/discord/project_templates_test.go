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

package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectTemplatesHub serves global templates and answers project-scoped
// template requests with status and body.
func projectTemplatesHub(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("scope") == "project" {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		_ = json.NewEncoder(w).Encode(hubTemplatesResponse{Templates: []hubTemplate{{Slug: "default", Name: "Default"}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func templateSlugs(list TemplateList) []string {
	var slugs []string
	for _, tmpl := range list.Templates {
		slugs = append(slugs, tmpl.Slug)
	}
	return slugs
}

func TestHTTPHubClient_ListTemplatesProjectOutcome(t *testing.T) {
	projectOK, err := json.Marshal(hubTemplatesResponse{Templates: []hubTemplate{{Slug: "proj-tmpl", Name: "Project"}}})
	require.NoError(t, err)

	tests := []struct {
		name      string
		status    int
		body      string
		wantSlugs []string
		wantNote  string
	}{
		{"success", http.StatusOK, string(projectOK), []string{"default", "proj-tmpl"}, ""},
		{"forbidden", http.StatusForbidden, deniedBody("list", "template"), []string{"default"}, projectTemplatesDeniedNote},
		{"unauthorized", http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"no"}}`, []string{"default"}, projectTemplatesDeniedNote},
		{"server error", http.StatusInternalServerError, serverErrorBody, []string{"default"}, projectTemplatesFailedNote},
		{"bad body", http.StatusOK, `not json`, []string{"default"}, projectTemplatesFailedNote},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewHTTPHubClient(projectTemplatesHub(t, tt.status, tt.body), "", "", nil)

			list, err := c.ListTemplates(context.Background(), "p1", luPrincipal)

			require.NoError(t, err, "a failed project read keeps the global templates")
			assert.ElementsMatch(t, tt.wantSlugs, templateSlugs(list))
			if tt.wantNote == "" {
				assert.NoError(t, list.ProjectErr)
				return
			}
			require.Error(t, list.ProjectErr)
			assert.Equal(t, tt.wantNote, projectTemplatesNote(list.ProjectErr))
		})
	}
}

func TestHTTPHubClient_ListTemplatesWithoutProject(t *testing.T) {
	c := NewHTTPHubClient(projectTemplatesHub(t, http.StatusForbidden, deniedBody("list", "template")), "", "", nil)

	list, err := c.ListTemplates(context.Background(), "", luPrincipal)

	require.NoError(t, err)
	assert.NoError(t, list.ProjectErr, "no project read is made without a project ID")
	assert.Equal(t, []string{"default"}, templateSlugs(list))
}

func TestHandleThread_ReportsFailedProjectTemplateRead(t *testing.T) {
	// Each hub body carries distinctive text that must not reach the user.
	tests := []struct {
		name     string
		status   int
		body     string
		hubText  string
		wantNote string
	}{
		{"forbidden", http.StatusForbidden,
			`{"error":{"code":"forbidden","message":"hub-text-forbidden","details":{"denied_action":"list","resource_type":"template"}}}`,
			"hub-text-forbidden", projectTemplatesDeniedNote},
		{"unauthorized", http.StatusUnauthorized,
			`{"error":{"code":"unauthorized","message":"hub-text-unauthorized"}}`,
			"hub-text-unauthorized", projectTemplatesDeniedNote},
		{"server error", http.StatusInternalServerError, serverErrorBody, "boom", projectTemplatesFailedNote},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			e.linkChannel(t)
			e.hub.failRequest(http.MethodGet, "/api/v1/templates?scope=project", tt.status, tt.body)

			e.commands.HandleThread(e.session, luCommand("thread",
				luStringOpt("title", "Fix the build"),
				luStringOpt("template", "proj-tmpl"),
			))

			bodies := e.discord.allBodies()
			assert.Contains(t, bodies, "Template **proj-tmpl** not found")
			assert.Contains(t, bodies, jsonText(t, tt.wantNote))
			assert.NotContains(t, bodies, tt.hubText, "hub error text is not shown")
			assert.Empty(t, e.hub.callsTo(http.MethodPost, luAgentsPath), "no agent is created")
		})
	}
}

func TestHandleThread_GlobalTemplateStillUsableWhenProjectReadFails(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	e.hub.failRequest(http.MethodGet, "/api/v1/templates?scope=project", http.StatusForbidden, deniedBody("list", "template"))

	e.commands.HandleThread(e.session, luCommand("thread",
		luStringOpt("title", "Fix the build"),
		luStringOpt("template", "default"),
	))

	bodies := e.discord.allBodies()
	assert.Contains(t, bodies, "Thread created with agent **fix-the-build**")
	assert.NotContains(t, bodies, "Project templates could not be loaded")
}
