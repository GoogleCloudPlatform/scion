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

package hubsync

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// projectByIDHub is a fake hub that knows one project by its ID.
func projectByIDHub(t *testing.T, projectID string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/api/v1/projects/" + projectID:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": projectID, "name": "Foo", "slug": "foo"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestEnsureHubReady_ExplicitHub checks that a hub project reference from
// the CLI's current context (ExplicitHub) resolves without hub.enabled, and
// that without the option the hub-mode error is unchanged.
func TestEnsureHubReady_ExplicitHub(t *testing.T) {
	const projectID = "1dfdd6c7-0000-4000-8000-000000000001"

	t.Run("explicit hub bypasses the hub.enabled gate", func(t *testing.T) {
		home := clearHubCredentialEnv(t)
		t.Chdir(home)
		server := projectByIDHub(t, projectID)
		hubCtx, err := EnsureHubReady(projectID, EnsureHubReadyOptions{
			EndpointOverride: server.URL,
			ExplicitHub:      true,
			SkipSync:         true,
		})
		if err != nil {
			t.Fatalf("EnsureHubReady: %v", err)
		}
		if hubCtx == nil || hubCtx.ProjectID != projectID || hubCtx.Endpoint != server.URL {
			t.Fatalf("hub context = %+v, want project %s on %s", hubCtx, projectID, server.URL)
		}
	})

	t.Run("without it the gate error is unchanged", func(t *testing.T) {
		home := clearHubCredentialEnv(t)
		t.Chdir(home)
		server := projectByIDHub(t, projectID)
		_, err := EnsureHubReady(projectID, EnsureHubReadyOptions{
			EndpointOverride: server.URL,
			SkipSync:         true,
		})
		want := "hub project references (slugs, names, git URLs) require hub mode to be enabled\n\nEnable with: scion hub enable"
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})
}

func TestResolveProjectOnHub_TypedErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		}))
		t.Cleanup(server.Close)
		client, err := hubclient.New(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ResolveProjectOnHub(t.Context(), client, "nope")
		var nf *ProjectNotFoundError
		if !errors.As(err, &nf) || nf.Ref != "nope" {
			t.Fatalf("err = %v, want a ProjectNotFoundError for 'nope'", err)
		}
		if err.Error() != "project 'nope' not found on hub" {
			t.Fatalf("message changed: %q", err.Error())
		}
	})

	t.Run("ambiguous name", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("name") == "" {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []map[string]string{
				{"id": "id-1", "name": "dupe", "slug": "dupe-1"},
				{"id": "id-2", "name": "dupe", "slug": "dupe-2"},
			}})
		}))
		t.Cleanup(server.Close)
		client, err := hubclient.New(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ResolveProjectOnHub(t.Context(), client, "dupe")
		var amb *AmbiguousProjectError
		if !errors.As(err, &amb) || len(amb.Projects) != 2 || amb.Projects[0].Slug != "dupe-1" || amb.Projects[1].Slug != "dupe-2" {
			t.Fatalf("err = %v, want an AmbiguousProjectError with both slugs", err)
		}
		if err.Error() != "multiple projects found with name 'dupe' — please use a project ID or slug instead" {
			t.Fatalf("message changed: %q", err.Error())
		}
	})
}
