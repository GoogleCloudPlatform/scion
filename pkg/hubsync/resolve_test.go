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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsHubProjectRef(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{name: "empty string", input: "", expected: false},
		{name: "global keyword", input: "global", expected: false},
		{name: "home keyword", input: "home", expected: false},
		{name: "absolute path", input: "/tmp/project/.scion", expected: false},
		{name: "relative path dot", input: "./project", expected: false},
		{name: "relative path dotdot", input: "../project", expected: false},

		// Git URLs → hub references
		{name: "https git URL", input: "https://github.com/org/repo.git", expected: true},
		{name: "ssh git URL", input: "git@github.com:org/repo.git", expected: true},
		{name: "http git URL", input: "http://github.com/org/repo.git", expected: true},

		// Slug-like values (no matching local dir) → hub references
		{name: "simple slug", input: "my-project", expected: true},
		{name: "slug with numbers", input: "scion-agent-42", expected: true},
		{name: "UUID", input: "550e8400-e29b-41d4-a716-446655440000", expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsHubProjectRef(tt.input)
			assert.Equal(t, tt.expected, result, "IsHubProjectRef(%q)", tt.input)
		})
	}
}

func TestIsHubProjectRef_LocalDirExists(t *testing.T) {
	// Create a temporary directory that matches a slug-like name
	tmpDir := t.TempDir()
	dirName := "my-local-project"
	localDir := filepath.Join(tmpDir, dirName)
	require.NoError(t, os.MkdirAll(localDir, 0755))

	// Change to the parent directory so the relative path resolves
	origDir, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { os.Chdir(origDir) })
	require.NoError(t, os.Chdir(tmpDir))

	// The slug-like name should resolve as a local path since the dir exists
	assert.False(t, IsHubProjectRef(dirName), "should be false when local directory exists")
}

func TestIsHubProjectRef_LocalScionDirExists(t *testing.T) {
	// Create a temporary directory with a .scion subdirectory
	tmpDir := t.TempDir()
	dirName := "my-project"
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, dirName, ".scion"), 0755))

	origDir, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { os.Chdir(origDir) })
	require.NoError(t, os.Chdir(tmpDir))

	assert.False(t, IsHubProjectRef(dirName), "should be false when .scion subdirectory exists")
}

func TestIsHubProjectRef_PathSeparator(t *testing.T) {
	// Paths with separators (even without leading ./ or /) are filesystem paths
	assert.False(t, IsHubProjectRef("path/to/project"))
}

func TestResolveGroveOnHub_ByUUID(t *testing.T) {
	projectID := "550e8400-e29b-41d4-a716-446655440000"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves/"+projectID {
			json.NewEncoder(w).Encode(hubclient.Project{
				ID:   projectID,
				Name: "Test Project",
				Slug: "test-grove",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	grove, err := resolveProjectOnHub(context.Background(), client, projectID)
	require.NoError(t, err)
	assert.Equal(t, projectID, grove.ID)
	assert.Equal(t, "Test Project", grove.Name)
}

func TestResolveGroveOnHub_BySlug(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves" {
			slug := r.URL.Query().Get("slug")
			if slug == "my-project" {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"groves": []hubclient.Project{
						{ID: "abc-123", Name: "My Project", Slug: "my-project"},
					},
					"totalCount": 1,
				})
				return
			}
			// Empty for name fallback
			json.NewEncoder(w).Encode(map[string]interface{}{
				"groves":     []hubclient.Project{},
				"totalCount": 0,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	grove, err := resolveProjectOnHub(context.Background(), client, "my-project")
	require.NoError(t, err)
	assert.Equal(t, "abc-123", grove.ID)
	assert.Equal(t, "my-project", grove.Slug)
}

func TestResolveGroveOnHub_ByName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves" {
			name := r.URL.Query().Get("name")
			slug := r.URL.Query().Get("slug")
			if slug != "" {
				// Slug query returns nothing
				json.NewEncoder(w).Encode(map[string]interface{}{
					"groves":     []hubclient.Project{},
					"totalCount": 0,
				})
				return
			}
			if name == "My Project" {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"groves": []hubclient.Project{
						{ID: "abc-456", Name: "My Project", Slug: "my-project"},
					},
					"totalCount": 1,
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"groves":     []hubclient.Project{},
				"totalCount": 0,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	grove, err := resolveProjectOnHub(context.Background(), client, "My Project")
	require.NoError(t, err)
	assert.Equal(t, "abc-456", grove.ID)
}

func TestResolveGroveOnHub_ByGitURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves" {
			gitRemote := r.URL.Query().Get("gitRemote")
			if gitRemote != "" {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"groves": []hubclient.Project{
						{ID: "git-grove-1", Name: "Git Project", Slug: "git-project"},
					},
					"totalCount": 1,
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"groves":     []hubclient.Project{},
				"totalCount": 0,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	grove, err := resolveProjectOnHub(context.Background(), client, "https://github.com/org/repo.git")
	require.NoError(t, err)
	assert.Equal(t, "git-grove-1", grove.ID)
}

func TestResolveGroveOnHub_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"groves":     []hubclient.Project{},
				"totalCount": 0,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	_, err = resolveProjectOnHub(context.Background(), client, "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestResolveGroveOnHub_MultipleByName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves" {
			slug := r.URL.Query().Get("slug")
			if slug != "" {
				// No slug match
				json.NewEncoder(w).Encode(map[string]interface{}{
					"groves":     []hubclient.Project{},
					"totalCount": 0,
				})
				return
			}
			// Name returns multiple
			json.NewEncoder(w).Encode(map[string]interface{}{
				"groves": []hubclient.Project{
					{ID: "id-1", Name: "dupe", Slug: "dupe-1"},
					{ID: "id-2", Name: "dupe", Slug: "dupe-2"},
				},
				"totalCount": 2,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	_, err = resolveProjectOnHub(context.Background(), client, "dupe")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple projects found")
}
