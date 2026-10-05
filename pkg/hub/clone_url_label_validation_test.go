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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// cloneURLLabelSentinel marks the secret part of the credential-bearing test
// URLs; a 400 response must never echo it back.
const cloneURLLabelSentinel = "FAKE-KEY-SENTINEL-not-a-real-credential"

// cloneURLLabelCases is the URL table shared by every write-path test. A
// non-empty wantMsg means the value must be refused with a 400 whose message
// contains it.
var cloneURLLabelCases = []struct {
	name    string
	value   string
	wantMsg string
}{
	{"https user and password", "https://user:" + cloneURLLabelSentinel + "@github.com/org/repo.git", "username, password or token"},
	{"https x-access-token", "https://x-access-token:" + cloneURLLabelSentinel + "@github.com/org/repo", "username, password or token"},
	{"https token-only userinfo", "https://" + cloneURLLabelSentinel + "@github.com/org/repo", "username, password or token"},
	{"http user without password", "http://deploy@internal.host/repo", "username, password or token"},
	{"https empty password", "https://user:@github.com/org/repo", "username, password or token"},
	{"schemeless user and password", "user:" + cloneURLLabelSentinel + "@github.com/org/repo", "username, password or token"},
	{"schemeless token-only", cloneURLLabelSentinel + "@github.com/org/repo", "username, password or token"},
	{"ssh with password", "ssh://git:" + cloneURLLabelSentinel + "@github.com/org/repo.git", "username, password or token"},
	{"query token", "https://github.com/org/repo.git?access_token=" + cloneURLLabelSentinel, "query string"},
	{"fragment", "https://github.com/org/repo.git#" + cloneURLLabelSentinel, "fragment"},
	{"password that looks like a port", "https://user:8443/" + cloneURLLabelSentinel + "@host/repo", "username, password or token"},
	{"at sign in path", "https://github.com/org/repo@v1", "username, password or token"},
	{"scheme-like suffix", "user:" + cloneURLLabelSentinel + "@host/org/repo://", "username, password or token"},
	{"single-slash scheme", "https:/user:" + cloneURLLabelSentinel + "@host/r", "username, password or token"},
	{"embedded newline", "https://host/r\nhttps://u:" + cloneURLLabelSentinel + "@h/x", "whitespace and control"},
	{"scp path with at sign", "git@host:repo@v1", "username, password or token"},
	{"local path with hash", "/tmp/repo#1", ""},
	{"clean https", "https://github.com/org/repo.git", ""},
	{"clean https with port", "https://git.example.com:8443/org/repo.git", ""},
	{"clean schemeless", "github.com/org/repo", ""},
	{"scp-style login", "git@github.com:org/repo.git", ""},
	{"scp-style custom login", "deploy@internal.host:team/project", ""},
	{"ssh login", "ssh://git@github.com/org/repo.git", ""},
	{"git scheme", "git://172.17.0.1:9418/org/repo", ""},
	{"local path", "/tmp/source-repo", ""},
}

func assertCloneURLLabelResult(t *testing.T, code int, body, wantMsg string) {
	t.Helper()
	if wantMsg == "" {
		assert.Less(t, code, 300, "clean clone-url must be accepted: %s", body)
		return
	}
	require.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, wantMsg)
	assert.Contains(t, body, "project secrets or the GitHub App")
	assert.Contains(t, body, "labels."+store.LabelCloneURL)
	assert.NotContains(t, body, cloneURLLabelSentinel, "the rejected value must not be echoed")
}

func TestCreateProject_CloneURLLabelValidation(t *testing.T) {
	srv, _ := testServer(t)
	for i, tc := range cloneURLLabelCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
				Name:      fmt.Sprintf("Clone URL Create %d", i),
				GitRemote: "github.com/org/repo",
				Labels:    map[string]string{store.LabelCloneURL: tc.value},
			})
			assertCloneURLLabelResult(t, rec.Code, rec.Body.String(), tc.wantMsg)
		})
	}
}

func TestRegisterProject_CloneURLLabelValidation(t *testing.T) {
	srv, _ := testServer(t)
	for i, tc := range cloneURLLabelCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
				Name:      fmt.Sprintf("Clone URL Register %d", i),
				GitRemote: fmt.Sprintf("github.com/org/register-%d", i),
				Labels:    map[string]string{store.LabelCloneURL: tc.value},
			})
			assertCloneURLLabelResult(t, rec.Code, rec.Body.String(), tc.wantMsg)
		})
	}
}

func TestUpdateProject_CloneURLLabelValidation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{
		ID:        tid("clone-url-patch"),
		Name:      "Clone URL Patch",
		Slug:      "clone-url-patch",
		GitRemote: "github.com/org/repo",
		Labels:    map[string]string{store.LabelCloneURL: "https://github.com/org/repo.git"},
	}
	require.NoError(t, s.CreateProject(ctx, project))

	for _, tc := range cloneURLLabelCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID, map[string]interface{}{
				"labels": map[string]string{store.LabelCloneURL: tc.value},
			})
			assertCloneURLLabelResult(t, rec.Code, rec.Body.String(), tc.wantMsg)

			stored, err := s.GetProject(ctx, project.ID)
			require.NoError(t, err)
			if tc.wantMsg == "" {
				assert.Equal(t, tc.value, stored.Labels[store.LabelCloneURL])
			} else {
				assert.NotContains(t, stored.Labels[store.LabelCloneURL], cloneURLLabelSentinel,
					"a refused PATCH must not change the stored label")
			}
		})
	}
}

// TestUpdateProject_LegacyCloneURLLabelUnchangedAllowed covers a row written
// before write-time validation: PATCH resends the whole label map, so an
// unchanged legacy clone-url must not block an unrelated label edit, while
// changing the clone-url to another bad value is still refused.
func TestUpdateProject_LegacyCloneURLLabelUnchangedAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	legacy := "https://user:" + cloneURLLabelSentinel + "@github.com/org/repo.git"
	project := &store.Project{
		ID:        tid("clone-url-legacy-patch"),
		Name:      "Clone URL Legacy Patch",
		Slug:      "clone-url-legacy-patch",
		GitRemote: "github.com/org/repo",
		Labels:    map[string]string{store.LabelCloneURL: legacy},
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID, map[string]interface{}{
		"labels": map[string]string{store.LabelCloneURL: legacy, "team": "web"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	stored, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, "web", stored.Labels["team"])

	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID, map[string]interface{}{
		"labels": map[string]string{store.LabelCloneURL: "https://other:" + cloneURLLabelSentinel + "@github.com/org/repo.git", "team": "web"},
	})
	assertCloneURLLabelResult(t, rec.Code, rec.Body.String(), "username, password or token")
	stored, err = s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, legacy, stored.Labels[store.LabelCloneURL], "a refused PATCH must not change the stored label")
}

// TestPopulateAgentConfig_LegacyCloneURLLabelUserinfoStripped covers rows
// written before write-time validation: the clone URL handed to the broker
// (and from there to the agent's start environment) carries no userinfo,
// query string or fragment.
func TestPopulateAgentConfig_LegacyCloneURLLabelUserinfoStripped(t *testing.T) {
	srv, _ := testServer(t)

	tests := []struct {
		name, label, want string
	}{
		{"https user and password", "https://user:" + cloneURLLabelSentinel + "@github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"https token-only", "https://" + cloneURLLabelSentinel + "@github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"schemeless user and password", "user:" + cloneURLLabelSentinel + "@github.com/org/repo", "https://github.com/org/repo.git"},
		{"ssh keeps login drops password", "ssh://git:" + cloneURLLabelSentinel + "@github.com/org/repo.git", "ssh://git@github.com/org/repo.git"},
		{"scp-style unchanged", "git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"https query token", "https://github.com/org/repo.git?access_token=" + cloneURLLabelSentinel, "https://github.com/org/repo.git"},
		{"https fragment", "https://github.com/org/repo.git#" + cloneURLLabelSentinel, "https://github.com/org/repo.git"},
		{"https userinfo, query and fragment", "https://" + cloneURLLabelSentinel + "@github.com/org/repo.git?t=" + cloneURLLabelSentinel + "#" + cloneURLLabelSentinel, "https://github.com/org/repo.git"},
		{"ssh query keeps login", "ssh://git@github.com/org/repo.git?t=" + cloneURLLabelSentinel, "ssh://git@github.com/org/repo.git"},
		{"scp-style query", "git@github.com:org/repo.git?t=" + cloneURLLabelSentinel, "git@github.com:org/repo.git"},
		{"schemeless query", "github.com/org/repo?access_token=" + cloneURLLabelSentinel, "https://github.com/org/repo.git"},
		{"leading whitespace", " https://u:" + cloneURLLabelSentinel + "@github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"other scheme userinfo", "git+ssh://u:" + cloneURLLabelSentinel + "@github.com/org/repo", "https://git+ssh://github.com/org/repo.git"},
		{"ambiguous value falls back to git remote", "https://user:8443/" + cloneURLLabelSentinel + "@host/repo", "https://github.com/org/repo.git"},
		{"embedded newline falls back to git remote", "https://host/r\nhttps://u:" + cloneURLLabelSentinel + "@h/x", "https://github.com/org/repo.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := &store.Project{
				ID:        tid("legacy-clone-url"),
				Name:      "Legacy Clone URL",
				Slug:      "legacy-clone-url",
				GitRemote: "github.com/org/repo",
				Labels:    map[string]string{store.LabelCloneURL: tt.label},
			}
			agent := &store.Agent{ID: "agent-legacy-clone-url", AppliedConfig: &store.AgentAppliedConfig{}}

			srv.populateAgentConfig(context.Background(), agent, project, nil)

			require.NotNil(t, agent.AppliedConfig.GitClone)
			got := agent.AppliedConfig.GitClone.URL
			assert.Equal(t, tt.want, got)
			assert.False(t, strings.Contains(got, cloneURLLabelSentinel), "credential leaked into clone URL")
			raw, err := json.Marshal(agent.AppliedConfig)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), cloneURLLabelSentinel)
		})
	}
}

// sourceURLLabelCases is the URL table shared by the source-url write-path
// tests: the label is sanitized on write, never refused.
var sourceURLLabelCases = []struct {
	name, value, want string
}{
	{"https user and password", "https://user:" + cloneURLLabelSentinel + "@github.com/org/repo.git", "https://github.com/org/repo.git"},
	{"https token-only", "https://" + cloneURLLabelSentinel + "@github.com/org/repo", "https://github.com/org/repo"},
	{"schemeless user and password", "user:" + cloneURLLabelSentinel + "@github.com/org/repo", "github.com/org/repo"},
	{"ssh password", "ssh://git:" + cloneURLLabelSentinel + "@github.com/org/repo.git", "ssh://git@github.com/org/repo.git"},
	{"query token", "https://github.com/org/repo.git?access_token=" + cloneURLLabelSentinel, "https://github.com/org/repo.git"},
	{"fragment", "https://github.com/org/repo.git#" + cloneURLLabelSentinel, "https://github.com/org/repo.git"},
	{"password that looks like a port removed", "https://user:8443/" + cloneURLLabelSentinel + "@host/repo", ""},
	{"single-slash scheme removed", "https:/user:" + cloneURLLabelSentinel + "@host/r", ""},
	{"embedded newline removed", "https://host/r\nhttps://u:" + cloneURLLabelSentinel + "@h/x", ""},
	{"scheme-like suffix", "user:" + cloneURLLabelSentinel + "@host/org/repo://", "host/org/repo://"},
	{"scp path with at sign kept", "git@host:repo@v1", "git@host:repo@v1"},
	{"clean https", "https://github.com/org/repo.git", "https://github.com/org/repo.git"},
	{"clean schemeless", "github.com/org/repo", "github.com/org/repo"},
	{"scp-style login", "git@github.com:org/repo.git", "git@github.com:org/repo.git"},
	{"ssh login", "ssh://git@github.com/org/repo.git", "ssh://git@github.com/org/repo.git"},
}

func assertStoredSourceURL(t *testing.T, s store.Store, projectID, want string) {
	t.Helper()
	stored, err := s.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	got, present := stored.Labels[store.LabelSourceURL]
	assert.Equal(t, want, got)
	assert.Equal(t, want != "", present, "a source-url that cannot be sanitized is removed")
	assert.NotContains(t, stored.Labels[store.LabelSourceURL], cloneURLLabelSentinel)
}

func TestCreateProject_SourceURLLabelSanitized(t *testing.T) {
	srv, s := testServer(t)
	for i, tc := range sourceURLLabelCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
				Name:      fmt.Sprintf("Source URL Create %d", i),
				GitRemote: "github.com/org/repo",
				Labels:    map[string]string{store.LabelSourceURL: tc.value},
			})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), cloneURLLabelSentinel)
			var project store.Project
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
			assertStoredSourceURL(t, s, project.ID, tc.want)
		})
	}
}

func TestRegisterProject_SourceURLLabelSanitized(t *testing.T) {
	srv, s := testServer(t)
	for i, tc := range sourceURLLabelCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
				Name:      fmt.Sprintf("Source URL Register %d", i),
				GitRemote: fmt.Sprintf("github.com/org/source-register-%d", i),
				Labels:    map[string]string{store.LabelSourceURL: tc.value},
			})
			require.Less(t, rec.Code, 300, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), cloneURLLabelSentinel)
			var resp RegisterProjectResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			require.NotNil(t, resp.Project)
			assertStoredSourceURL(t, s, resp.Project.ID, tc.want)
		})
	}
}

func TestUpdateProject_SourceURLLabelSanitized(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{
		ID:        tid("source-url-patch"),
		Name:      "Source URL Patch",
		Slug:      "source-url-patch",
		GitRemote: "github.com/org/repo",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	for _, tc := range sourceURLLabelCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID, map[string]interface{}{
				"labels": map[string]string{store.LabelSourceURL: tc.value},
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), cloneURLLabelSentinel)
			assertStoredSourceURL(t, s, project.ID, tc.want)
		})
	}
}

// TestProjectClone_SanitizesCopiedGitSourceLabels covers a clone without a
// remote override: the source project's legacy clone-url and source-url
// labels are not copied with credentials, a query or a fragment.
func TestProjectClone_SanitizesCopiedGitSourceLabels(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	tests := []struct {
		name                  string
		cloneURL, sourceURL   string
		wantClone, wantSource string
		cloneKept, sourceKept bool
	}{
		{
			name:       "credentials and query stripped",
			cloneURL:   "https://user:" + cloneURLLabelSentinel + "@github.com/org/repo.git?t=" + cloneURLLabelSentinel,
			sourceURL:  "https://" + cloneURLLabelSentinel + "@github.com/org/repo#" + cloneURLLabelSentinel,
			wantClone:  "https://github.com/org/repo.git",
			wantSource: "https://github.com/org/repo",
			cloneKept:  true, sourceKept: true,
		},
		{
			name:      "ambiguous values removed",
			cloneURL:  "https://user:8443/" + cloneURLLabelSentinel + "@host/repo",
			sourceURL: "https:/user:" + cloneURLLabelSentinel + "@host/r",
		},
		{
			name:       "clean values copied unchanged",
			cloneURL:   "git@github.com:org/repo.git",
			sourceURL:  "ssh://git@github.com/org/repo.git",
			wantClone:  "git@github.com:org/repo.git",
			wantSource: "ssh://git@github.com/org/repo.git",
			cloneKept:  true, sourceKept: true,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &store.Project{
				ID:        tid(fmt.Sprintf("clone-src-labels-%d", i)),
				Name:      fmt.Sprintf("Clone Source Labels %d", i),
				Slug:      fmt.Sprintf("clone-source-labels-%d", i),
				GitRemote: "github.com/org/repo",
				OwnerID:   DevUserID,
				CreatedBy: DevUserID,
				Labels: map[string]string{
					store.LabelCloneURL:  tt.cloneURL,
					store.LabelSourceURL: tt.sourceURL,
				},
			}
			require.NoError(t, s.CreateProject(ctx, src))

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
				map[string]string{"name": fmt.Sprintf("Cloned Labels %d", i)})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), cloneURLLabelSentinel)

			var clone store.Project
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
			stored, err := s.GetProject(ctx, clone.ID)
			require.NoError(t, err)
			gotClone, cloneOK := stored.Labels[store.LabelCloneURL]
			gotSource, sourceOK := stored.Labels[store.LabelSourceURL]
			assert.Equal(t, tt.cloneKept, cloneOK)
			assert.Equal(t, tt.sourceKept, sourceOK)
			assert.Equal(t, tt.wantClone, gotClone)
			assert.Equal(t, tt.wantSource, gotSource)
		})
	}
}
