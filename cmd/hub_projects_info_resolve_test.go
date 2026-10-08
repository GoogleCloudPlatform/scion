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
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProjectsHub serves the project lookup endpoints used by
// `scion hub projects info`, mirroring the hub's semantics: GET by ID only,
// the name filter is a case-insensitive name match and the slug filter is an
// exact slug match. It records each request so tests can check which lookups
// were made.
type fakeProjectsHub struct {
	projects []hubclient.Project

	mu       sync.Mutex
	requests []string
}

func (f *fakeProjectsHub) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	req := r.URL.Path
	if r.URL.RawQuery != "" {
		req += "?" + r.URL.RawQuery
	}
	f.requests = append(f.requests, req)
}

func (f *fakeProjectsHub) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeProjectsHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/v1/projects" {
		name, slug := r.URL.Query().Get("name"), r.URL.Query().Get("slug")
		matches := []hubclient.Project{}
		for _, p := range f.projects {
			if (name != "" && strings.EqualFold(p.Name, name)) || (slug != "" && p.Slug == slug) {
				matches = append(matches, p)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"projects": matches})
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/api/v1/projects/"); ok {
		for _, p := range f.projects {
			if p.ID == id {
				_ = json.NewEncoder(w).Encode(p)
				return
			}
		}
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"project not found"}}`))
}

// TestResolveProjectForInfo covers ptone/scion#3772: `scion hub projects
// info` accepts a project UUID as well as a name, and keeps the existing
// name lookup and not-found error unchanged.
func TestResolveProjectForInfo(t *testing.T) {
	const (
		idA = "0b9a4c1e-2f3d-4e5a-8b6c-7d8e9f0a1b2c"
		idB = "5f1e2d3c-4b5a-4987-8a6b-1c2d3e4f5a6b"
	)
	hub := &fakeProjectsHub{projects: []hubclient.Project{
		{ID: idA, Name: "My Project", Slug: "my-project"},
		{ID: idB, Name: "tools", Slug: "tools"},
	}}

	tests := []struct {
		name     string
		arg      string
		wantID   string
		wantErr  string
		wantReqs []string
	}{
		{
			name:     "uuid resolves by ID",
			arg:      idA,
			wantID:   idA,
			wantReqs: []string{"/api/v1/projects/" + idA},
		},
		{
			name:     "name resolves by name lookup only",
			arg:      "My Project",
			wantID:   idA,
			wantReqs: []string{"/api/v1/projects?name=My+Project"},
		},
		{
			name:     "name match is case-insensitive",
			arg:      "my project",
			wantID:   idA,
			wantReqs: []string{"/api/v1/projects?name=my+project"},
		},
		{
			name:     "slug equal to name resolves as before",
			arg:      "tools",
			wantID:   idB,
			wantReqs: []string{"/api/v1/projects?name=tools"},
		},
		{
			name:     "unknown name reports not found",
			arg:      "missing",
			wantErr:  "project 'missing' not found",
			wantReqs: []string{"/api/v1/projects?name=missing"},
		},
		{
			name:    "unknown uuid reports not found",
			arg:     "9d8c7b6a-5f4e-4d3c-9b2a-1f0e9d8c7b6a",
			wantErr: "project '9d8c7b6a-5f4e-4d3c-9b2a-1f0e9d8c7b6a' not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub.mu.Lock()
			hub.requests = nil
			hub.mu.Unlock()
			srv := httptest.NewServer(hub)
			t.Cleanup(srv.Close)
			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)

			got, err := resolveProjectForInfo(context.Background(), client, tt.arg)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantID, got.ID)
			}
			if tt.wantReqs != nil {
				assert.Equal(t, tt.wantReqs, hub.recorded())
			}
		})
	}
}

// TestHubProjectsInfoHelpMentionsID checks the usage and help text advertise
// project ID lookup.
func TestHubProjectsInfoHelpMentionsID(t *testing.T) {
	assert.Contains(t, hubProjectsInfoCmd.Use, "project-name-or-id")
	assert.Contains(t, hubProjectsInfoCmd.Long, "project ID (UUID)")
	assert.Contains(t, hubProjectsCmd.Use, "project-name-or-id")
}
