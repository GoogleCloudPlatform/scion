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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provideHub is an httptest hub that serves the project providers API for
// one project and records every request it receives.
type provideHub struct {
	mu        sync.Mutex
	requests  []string
	added     []hubclient.AddProviderRequest
	providers []hubclient.ProjectProvider
}

func newProvideHub(t *testing.T, projectID string, providers []hubclient.ProjectProvider) (*provideHub, *httptest.Server) {
	t.Helper()
	h := &provideHub{providers: providers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.requests = append(h.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/providers":
			_ = json.NewEncoder(w).Encode(hubclient.ListProvidersResponse{Providers: h.providers})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/providers":
			var req hubclient.AddProviderRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			h.added = append(h.added, req)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.AddProviderResponse{
				Provider: &hubclient.ProjectProvider{BrokerID: req.BrokerID, LocalPath: req.LocalPath},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "provide-project", DefaultRuntimeBrokerID: "broker-1"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

func TestProvideBrokerToProject_UsesProvidersAPI(t *testing.T) {
	hub, srv := newProvideHub(t, "proj-1", nil)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	project, err := provideBrokerToProject(context.Background(), client, "proj-1", "broker-1", "/home/dev/repo")

	require.NoError(t, err)
	assert.Equal(t, "proj-1", project.ID)
	assert.Equal(t, "broker-1", project.DefaultRuntimeBrokerID)
	assert.Equal(t, []hubclient.AddProviderRequest{{BrokerID: "broker-1", LocalPath: "/home/dev/repo"}}, hub.added)
	for _, req := range hub.requests {
		assert.NotContains(t, req, "/projects/register", "provide links through the providers API")
	}
	assert.Contains(t, hub.requests, "POST /api/v1/projects/proj-1/providers")
}

func TestProvideBrokerToProject_EmptyPathSendsNoPath(t *testing.T) {
	hub, srv := newProvideHub(t, "proj-2", []hubclient.ProjectProvider{
		{BrokerID: "broker-1", LocalPath: "/home/dev/repo/.scion"},
	})
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	_, err = provideBrokerToProject(context.Background(), client, "proj-2", "broker-1", "")

	require.NoError(t, err)
	assert.Equal(t, []hubclient.AddProviderRequest{{BrokerID: "broker-1"}}, hub.added,
		"an empty local path is sent as no path; the hub decides what an existing provider keeps")
	assert.NotContains(t, hub.requests, "GET /api/v1/projects/proj-2/providers",
		"the client does not read the stored provider path")
}

func TestProvideBrokerToProject_ReturnsAddError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"only the broker's owner or a super-admin may associate this broker with a project"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(hubclient.ListProvidersResponse{})
	}))
	defer srv.Close()
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	_, err = provideBrokerToProject(context.Background(), client, "proj-3", "broker-1", "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner")
}

// provideMockHub records provider-add bodies and project updates, and
// resolves projects by ID.
type provideMockHub struct {
	mu        sync.Mutex
	registers []map[string]interface{} // POST /projects/register bodies (provide sends none)
	adds      []map[string]interface{} // POST /projects/{id}/providers bodies, with "projectId" added
	updates   []map[string]interface{} // PATCH /projects/{id} bodies, with "projectId" added
	projects  map[string]string        // id -> name
	defaults  map[string]string        // id -> defaultRuntimeBrokerId
}

func (m *provideMockHub) project(id string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "name": m.projects[id], "slug": m.projects[id],
		"defaultRuntimeBrokerId": m.defaults[id],
	}
}

func (m *provideMockHub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/register" && r.Method == http.MethodPost:
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode register body: %v", err)
			}
			m.mu.Lock()
			m.registers = append(m.registers, body)
			m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]interface{}{"id": body["id"], "name": body["name"], "slug": body["name"]},
			})
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && strings.HasSuffix(r.URL.Path, "/providers") && r.Method == http.MethodPost:
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/projects/"), "/providers")
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode provider body: %v", err)
			}
			m.mu.Lock()
			body["projectId"] = id
			m.adds = append(m.adds, body)
			if m.defaults[id] == "" {
				// The hub makes the first provider the project default.
				m.defaults[id], _ = body["brokerId"].(string)
			}
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"provider": body})
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && (r.Method == http.MethodGet || r.Method == http.MethodPatch):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
			m.mu.Lock()
			defer m.mu.Unlock()
			if _, ok := m.projects[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "not_found", "message": "not found"},
				})
				return
			}
			if r.Method == http.MethodPatch {
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode update body: %v", err)
				}
				body["projectId"] = id
				m.updates = append(m.updates, body)
				if d, ok := body["defaultRuntimeBrokerId"].(string); ok && d != "" {
					m.defaults[id] = d
				}
			}
			_ = json.NewEncoder(w).Encode(m.project(id))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// lastAdd returns the last provider-add body and checks that provide sent
// no project register request.
func (m *provideMockHub) lastAdd(t *testing.T) map[string]interface{} {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Empty(t, m.registers, "provide links through the providers API, not project register")
	require.NotEmpty(t, m.adds, "no provider-add request reached the hub")
	return m.adds[len(m.adds)-1]
}

// setupProvideTest runs provide from a broker user's home directory, which
// holds the global scion directory and no project.
func setupProvideTest(t *testing.T) (*provideMockHub, string) {
	t.Helper()
	origProjectPath, origYes := projectPath, autoConfirm
	origProject, origBroker, origHub, origPath, origDefault := brokerProjectID, brokerBrokerID, brokerHubFlag, brokerProvidePath, brokerMakeDefault
	t.Cleanup(func() {
		projectPath, autoConfirm = origProjectPath, origYes
		brokerProjectID, brokerBrokerID, brokerHubFlag, brokerProvidePath, brokerMakeDefault = origProject, origBroker, origHub, origPath, origDefault
	})

	mock := &provideMockHub{
		projects: map[string]string{"p-web": "web-app", "p-global": "global"},
		defaults: map[string]string{},
	}
	server := httptest.NewServer(mock.handler(t))
	t.Cleanup(server.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_PROJECT", "")
	t.Setenv("SCION_PROJECT_ID", "")
	t.Setenv("SCION_PROJECT_PATH", "")

	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))
	settings, err := json.Marshal(map[string]interface{}{
		"hub": map[string]interface{}{"enabled": true, "endpoint": server.URL, "brokerId": "broker-1"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.json"), settings, 0644))

	t.Chdir(home)
	projectPath, autoConfirm = "", true
	brokerProjectID, brokerBrokerID, brokerHubFlag, brokerProvidePath, brokerMakeDefault = "", "", "", "", false
	return mock, globalDir
}

func TestRunBrokerProvide_ProjectFlagFromHomeSendsNoPath(t *testing.T) {
	mock, _ := setupProvideTest(t)
	brokerProjectID = "p-web"

	require.NoError(t, runBrokerProvide(brokerProvideCmd, nil))

	body := mock.lastAdd(t)
	assert.Equal(t, "p-web", body["projectId"])
	assert.Equal(t, "broker-1", body["brokerId"])
	assert.NotContains(t, body, "localPath", "provide --project must not send the current directory's path")
}

func TestRunBrokerProvide_ProjectFlagWithPathSendsPath(t *testing.T) {
	mock, _ := setupProvideTest(t)
	checkout := filepath.Join(t.TempDir(), "web-app")
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, ".scion"), 0755))
	brokerProjectID = "p-web"
	brokerProvidePath = checkout

	require.NoError(t, runBrokerProvide(brokerProvideCmd, nil))

	wantPath, err := filepath.EvalSymlinks(filepath.Join(checkout, ".scion"))
	require.NoError(t, err)
	assert.Equal(t, wantPath, mock.lastAdd(t)["localPath"])
}

func TestRunBrokerProvide_PathToGlobalDirRefusedForProject(t *testing.T) {
	mock, globalDir := setupProvideTest(t)
	brokerProjectID = "p-web"
	brokerProvidePath = globalDir

	err := runBrokerProvide(brokerProvideCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "global scion directory")
	assert.Empty(t, mock.registers, "no register request may be sent")
	assert.Empty(t, mock.adds, "no provider-add request may be sent")
}

// The global project keeps its existing flow: provide from the home
// directory without --project registers the global directory.
func TestRunBrokerProvide_GlobalProjectFromHomeSendsGlobalDir(t *testing.T) {
	mock, globalDir := setupProvideTest(t)
	settings, err := json.Marshal(map[string]interface{}{
		"hub": map[string]interface{}{
			"enabled": true, "endpoint": os.Getenv("SCION_HUB_ENDPOINT"), "brokerId": "broker-1", "projectId": "p-global",
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.json"), settings, 0644))

	require.NoError(t, runBrokerProvide(brokerProvideCmd, nil))

	body := mock.lastAdd(t)
	assert.Equal(t, "p-global", body["projectId"])
	assert.Equal(t, "broker-1", body["brokerId"])
	assert.Equal(t, globalDir, body["localPath"])
}

// hub link from a linked project checkout registers the local broker as a
// provider with that project's own path.
func TestRunHubLink_AddsProviderWithProjectPath(t *testing.T) {
	_, globalDir := setupProvideTest(t)
	origGlobal := globalMode
	t.Cleanup(func() { globalMode = origGlobal })
	globalMode = false
	t.Setenv("SCION_AUTH_TOKEN", "test-token")

	var (
		mu        sync.Mutex
		providers []map[string]interface{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/register" && r.Method == http.MethodPost:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]interface{}{"id": "p-linked", "name": body["name"], "slug": body["name"]},
				"created": true,
			})
		case strings.HasSuffix(r.URL.Path, "/providers") && r.Method == http.MethodPost:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			providers = append(providers, body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"provider": body})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "not_found", "message": "not found"},
			})
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	checkout := filepath.Join(t.TempDir(), "web-app")
	scionDir := filepath.Join(checkout, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.json"), []byte(`{"project_id":"local-web"}`), 0644))
	require.NotEmpty(t, globalDir)
	t.Chdir(checkout)

	require.NoError(t, runHubLink(hubLinkCmd, nil))

	wantPath, err := filepath.EvalSymlinks(scionDir)
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, providers, 1)
	assert.Equal(t, "broker-1", providers[0]["brokerId"])
	assert.Equal(t, wantPath, providers[0]["localPath"])
}

// provide with no --project from a linked project checkout sends that
// project's path.
func TestRunBrokerProvide_CurrentLinkedProjectSendsItsPath(t *testing.T) {
	mock, _ := setupProvideTest(t)
	checkout := filepath.Join(t.TempDir(), "web-app")
	scionDir := filepath.Join(checkout, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0755))
	settings, err := json.Marshal(map[string]interface{}{
		"hub": map[string]interface{}{"enabled": true, "endpoint": os.Getenv("SCION_HUB_ENDPOINT"), "projectId": "p-web"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.json"), settings, 0644))
	t.Chdir(checkout)

	require.NoError(t, runBrokerProvide(brokerProvideCmd, nil))

	wantPath, err := filepath.EvalSymlinks(scionDir)
	require.NoError(t, err)
	body := mock.lastAdd(t)
	assert.Equal(t, "p-web", body["projectId"])
	assert.Equal(t, wantPath, body["localPath"])
}

// --path may name the global directory for the global project (hub slug
// "global").
func TestRunBrokerProvide_PathToGlobalDirAllowedForGlobalProject(t *testing.T) {
	mock, globalDir := setupProvideTest(t)
	brokerProjectID = "p-global"
	brokerProvidePath = globalDir

	require.NoError(t, runBrokerProvide(brokerProvideCmd, nil))
	assert.Equal(t, globalDir, mock.lastAdd(t)["localPath"])
}
