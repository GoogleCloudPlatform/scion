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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsLocalWorkstationEndpoint_CLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_abc")

	assert.True(t, isLocalWorkstationEndpoint("http://127.0.0.1:8080"), "loopback plus dev token")
	assert.False(t, isLocalWorkstationEndpoint("https://hub.example.com"), "remote plus dev token")

	t.Setenv("SCION_HUB_TOKEN", "scion_pat_xyz")
	assert.False(t, isLocalWorkstationEndpoint("http://127.0.0.1:8080"), "loopback with a non-dev token")
}

// setupNoEndpointLink prepares a temp HOME whose global project has no hub
// endpoint, and targets it with hub link. It returns the global dir.
func setupNoEndpointLink(t *testing.T) string {
	t.Helper()
	origProjectPath, origGlobal, origYes := projectPath, globalMode, autoConfirm
	origOffer := offerTemplateSyncOnLinkFn
	t.Cleanup(func() {
		projectPath, globalMode, autoConfirm = origProjectPath, origGlobal, origYes
		offerTemplateSyncOnLinkFn = origOffer
	})
	offerTemplateSyncOnLinkFn = func(_, _, _ string, _ bool) {}

	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\n"), 0o644))
	t.Chdir(home)
	projectPath, globalMode, autoConfirm = "", true, true
	return globalDir
}

func TestRunHubLink_NoEndpoint(t *testing.T) {
	globalDir := setupNoEndpointLink(t)

	err := runHubLink(hubLinkCmd, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errHubLinkNoEndpoint), "error = %v", err)
	assert.Equal(t, "No hub configured. Start the local hub with 'scion server start' (first run opens setup), "+
		"or set a remote one with 'scion config set hub.endpoint <url>'.", err.Error())
	assert.NotContains(t, err.Error(), "\n", "the error is one line")
	_, statErr := os.Stat(filepath.Join(globalDir, "server.pid"))
	assert.True(t, os.IsNotExist(statErr), "no server is started")
}

// fakeLinkHub is a minimal hub where nothing is registered and no project
// matches by name. It reports whether a project was registered.
func fakeLinkHub(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()
	registered := new(bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/register" && r.Method == http.MethodPost:
			*registered = true
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]interface{}{"id": body["id"], "name": body["name"], "slug": body["name"]},
				"created": true,
			})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		case r.URL.Path == "/api/v1/runtime-brokers":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"brokers": []interface{}{}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "not_found", "message": "not found"},
			})
		}
	}))
	t.Cleanup(server.Close)
	return server, registered
}

// TestRunHubLink_LocalHubSkipsLinkPrompt: without -y and without a
// terminal, the link confirmation would decline. On the local workstation
// hub it is skipped, so the link succeeds; with a non-dev credential the
// same loopback hub is not a local workstation hub and keeps the prompt.
func TestRunHubLink_LocalHubSkipsLinkPrompt(t *testing.T) {
	tests := []struct {
		name     string
		hubToken string
		wantErr  string
	}{
		{name: "local workstation hub: no prompt"},
		{name: "loopback hub with a non-dev credential: prompt kept", hubToken: "bearer-xyz", wantErr: "linking cancelled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			globalDir := setupNoEndpointLink(t)
			autoConfirm = false
			server, registered := fakeLinkHub(t)
			require.NoError(t, config.UpdateVersionedSetting(globalDir, "hub.endpoint", server.URL))
			t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
			t.Setenv("SCION_HUB_TOKEN", tc.hubToken)

			err := runHubLink(hubLinkCmd, nil)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.False(t, *registered)
				return
			}
			require.NoError(t, err)
			assert.True(t, *registered)
		})
	}
}
