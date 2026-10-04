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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provideMockHub records register bodies and resolves projects by ID.
type provideMockHub struct {
	mu        sync.Mutex
	registers []map[string]interface{}
	projects  map[string]string // id -> name
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
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
			name, ok := m.projects[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "not_found", "message": "not found"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "name": name, "slug": name})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (m *provideMockHub) lastRegister(t *testing.T) map[string]interface{} {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NotEmpty(t, m.registers, "no register request reached the hub")
	return m.registers[len(m.registers)-1]
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

	mock := &provideMockHub{projects: map[string]string{"p-web": "web-app", "p-global": "global"}}
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

	body := mock.lastRegister(t)
	assert.Equal(t, "p-web", body["id"])
	assert.Equal(t, "broker-1", body["brokerId"])
	assert.NotContains(t, body, "path", "provide --project must not send the current directory's path")
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
	assert.Equal(t, wantPath, mock.lastRegister(t)["path"])
}

func TestRunBrokerProvide_PathToGlobalDirRefusedForProject(t *testing.T) {
	mock, globalDir := setupProvideTest(t)
	brokerProjectID = "p-web"
	brokerProvidePath = globalDir

	err := runBrokerProvide(brokerProvideCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "global scion directory")
	assert.Empty(t, mock.registers, "no register request may be sent")
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

	body := mock.lastRegister(t)
	assert.Equal(t, "p-global", body["id"])
	assert.Equal(t, "global", body["name"])
	assert.Equal(t, globalDir, body["path"])
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
	body := mock.lastRegister(t)
	assert.Equal(t, "p-web", body["id"])
	assert.Equal(t, wantPath, body["path"])
}
