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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/require"
)

// TestSplitTemplatesByHubPresence verifies that hub link offers only
// templates the Hub does not have yet in the project scope: the lookup uses
// the exact name, project scope, project ID and active status, the same
// lookup syncTemplateToHub uses to decide between create and update.
func TestSplitTemplatesByHubPresence(t *testing.T) {
	const projectID = "proj-123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		if r.URL.Path != "/api/v1/templates" || r.Method != http.MethodGet ||
			q.Get("scope") != "project" || q.Get("projectId") != projectID || q.Get("status") != "active" {
			http.Error(w, "unexpected request "+r.URL.String(), http.StatusBadRequest)
			return
		}
		templates := []map[string]interface{}{}
		switch q.Get("name") {
		case "on-hub":
			templates = append(templates, map[string]interface{}{"id": "t1", "name": "on-hub"})
		case "prefix-only":
			// A fuzzy match with a different name does not count.
			templates = append(templates, map[string]interface{}{"id": "t2", "name": "prefix-only-other"})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"templates": templates})
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
	require.Equal(t, []string{"on-hub"}, names(existing))
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
	require.Contains(t, err.Error(), `template "a"`)
}
