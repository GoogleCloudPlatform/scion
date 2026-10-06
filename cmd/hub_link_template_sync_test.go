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

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/require"
)

// TestSplitTemplatesByHubPresence verifies that hub link offers only
// templates the Hub does not have yet in the project scope. It lists the
// project's active project-scoped templates once per page (no per-template
// lookups), follows the pagination cursor to the last page, and matches
// names exactly, the same criteria syncTemplateToHub uses.
func TestSplitTemplatesByHubPresence(t *testing.T) {
	const projectID = "proj-123"
	var mu sync.Mutex
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		if r.URL.Path != "/api/v1/templates" || r.Method != http.MethodGet ||
			q.Get("scope") != "project" || q.Get("projectId") != projectID || q.Get("status") != "active" ||
			q.Has("name") {
			http.Error(w, "unexpected request "+r.URL.String(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		cursors = append(cursors, q.Get("cursor"))
		mu.Unlock()
		switch q.Get("cursor") {
		case "":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"templates": []map[string]interface{}{
					// A similar name does not count as the same template.
					{"id": "t2", "name": "prefix-only-other"},
				},
				"nextCursor": "page-2",
			})
		case "page-2":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"templates": []map[string]interface{}{{"id": "t1", "name": "on-hub"}},
			})
		default:
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	local := []*config.Template{{Name: "on-hub"}, {Name: "new-one"}, {Name: "prefix-only"}}
	missing, existing, err := splitTemplatesByHubPresence(context.Background(), hubCtx, local)
	require.NoError(t, err)

	names := func(ts []*config.Template) []string {
		var out []string
		for _, tpl := range ts {
			out = append(out, tpl.Name)
		}
		return out
	}
	require.Equal(t, []string{"new-one", "prefix-only"}, names(missing))
	require.Equal(t, []string{"on-hub"}, names(existing), "a template on a later page must be found")
	require.Equal(t, []string{"", "page-2"}, cursors, "one list call per page, not per template")
}

// TestSplitTemplatesByHubPresence_ListError verifies that a failed Hub lookup
// is returned as an error, so hub link offers nothing rather than guessing.
func TestSplitTemplatesByHubPresence_ListError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"internal","message":"boom"}}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "proj-123"}

	_, _, err = splitTemplatesByHubPresence(context.Background(), hubCtx, []*config.Template{{Name: "a"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to list project templates on the Hub")
}

// TestSplitTemplatesByHubPresence_RepeatedCursor verifies that a Hub that
// keeps returning the same cursor ends in an error instead of looping.
func TestSplitTemplatesByHubPresence_RepeatedCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"templates":  []map[string]interface{}{},
			"nextCursor": "same",
		})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "proj-123"}

	_, _, err = splitTemplatesByHubPresence(context.Background(), hubCtx, []*config.Template{{Name: "a"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cursor repeated")
}

// linkSyncHubCalls records the mutating calls syncNewTemplatesOnLink made.
type linkSyncHubCalls struct {
	mu        sync.Mutex
	created   []string // template names passed to create
	finalized []string // template IDs finalized
	touched   []string // requests against the existing template's ID
}

// newLinkSyncMockHub serves a project where "on-hub" already exists (ID
// t-existing) and any other name is missing. Creating a template returns ID
// t-new. When listFails is set, every template lookup fails.
func newLinkSyncMockHub(t *testing.T, calls *linkSyncHubCalls, listFails bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls.mu.Lock()
		defer calls.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "t-existing"):
			calls.touched = append(calls.touched, r.Method+" "+r.URL.Path)
			http.Error(w, "existing template must not be touched", http.StatusBadRequest)
		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
			if listFails {
				http.Error(w, `{"error":{"code":"internal","message":"boom"}}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"templates": []map[string]interface{}{
				{"id": "t-existing", "name": "on-hub"},
			}})
		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
			var req hubclient.CreateTemplateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			calls.created = append(calls.created, req.Name)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"template": map[string]interface{}{"id": "t-new", "name": req.Name},
			})
		case r.URL.Path == "/api/v1/templates/t-new/upload" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []map[string]interface{}{}})
		case r.URL.Path == "/api/v1/templates/t-new/finalize" && r.Method == http.MethodPost:
			calls.finalized = append(calls.finalized, "t-new")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "t-new", "status": "active"})
		default:
			http.NotFound(w, r)
		}
	}))
}

// linkSyncTemplates writes two local project templates: "on-hub", which the
// mock Hub already has, and "new-one", which it does not.
func linkSyncTemplates(t *testing.T) []*config.Template {
	t.Helper()
	var out []*config.Template
	for _, name := range []string{"on-hub", "new-one"} {
		dir := filepath.Join(t.TempDir(), name)
		writeTemplateFile(t, dir, "scion-agent.yaml", "harness_config: claude\n")
		out = append(out, &config.Template{Name: name, Path: dir, Scope: "project"})
	}
	return out
}

// TestSyncNewTemplatesOnLink_SyncsOnlyMissing drives hub link's template
// offer: only the template missing from the Hub is offered and created, the
// existing one is listed as skipped and never touched.
func TestSyncNewTemplatesOnLink_SyncsOnlyMissing(t *testing.T) {
	var calls linkSyncHubCalls
	server := newLinkSyncMockHub(t, &calls, false)
	defer server.Close()
	hubCtx := newTemplateSyncHubCtx(t, server)
	hubCtx.ProjectID = "proj-123"

	var prompts []string
	out := captureStdout(t, func() {
		syncNewTemplatesOnLink(hubCtx, linkSyncTemplates(t), func(p string) bool {
			prompts = append(prompts, p)
			return true
		})
	})

	require.Len(t, prompts, 1)
	require.Equal(t, []string{"new-one"}, calls.created)
	require.Equal(t, []string{"t-new"}, calls.finalized)
	require.Empty(t, calls.touched, "the template already on the Hub must not be synced")
	require.Contains(t, out, "Skipping 1 project template(s) already on the Hub:\n  - on-hub\n")
	require.Contains(t, out, "Found 1 project template(s) not yet on the Hub:\n  - new-one\n")
	require.Contains(t, out, "1 template(s) synced to project scope.")
}

// TestSyncNewTemplatesOnLink_DeclinedUploadsNothing checks that declining the
// prompt syncs nothing.
func TestSyncNewTemplatesOnLink_DeclinedUploadsNothing(t *testing.T) {
	var calls linkSyncHubCalls
	server := newLinkSyncMockHub(t, &calls, false)
	defer server.Close()
	hubCtx := newTemplateSyncHubCtx(t, server)
	hubCtx.ProjectID = "proj-123"

	captureStdout(t, func() {
		syncNewTemplatesOnLink(hubCtx, linkSyncTemplates(t), func(string) bool { return false })
	})

	require.Empty(t, calls.created)
	require.Empty(t, calls.finalized)
	require.Empty(t, calls.touched)
}

// TestSyncNewTemplatesOnLink_LookupErrorOffersNothing checks that a failed
// Hub lookup skips the offer: no prompt and no sync.
func TestSyncNewTemplatesOnLink_LookupErrorOffersNothing(t *testing.T) {
	var calls linkSyncHubCalls
	server := newLinkSyncMockHub(t, &calls, true)
	defer server.Close()
	hubCtx := newTemplateSyncHubCtx(t, server)
	hubCtx.ProjectID = "proj-123"

	prompted := false
	out := captureStdout(t, func() {
		syncNewTemplatesOnLink(hubCtx, linkSyncTemplates(t), func(string) bool {
			prompted = true
			return true
		})
	})

	require.False(t, prompted, "a failed lookup must not prompt")
	require.Empty(t, calls.created)
	require.Empty(t, calls.finalized)
	require.Contains(t, out, "Warning: skipping template sync")
}
