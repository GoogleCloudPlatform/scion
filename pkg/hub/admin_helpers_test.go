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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/require"
)

// passthrough is a simple handler that writes 200 OK.
var passthrough = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
})

func newTestWebServerWithMaintenance(enabled bool, message string) *WebServer {
	ws := &WebServer{
		config:      WebServerConfig{},
		mux:         http.NewServeMux(),
		maintenance: NewMaintenanceState(enabled, message),
	}
	return ws
}

// setWebSessionUser is a test helper to set the web session user in context.
func setWebSessionUser(ctx context.Context, user *webSessionUser) context.Context {
	return context.WithValue(ctx, webUserContextKey{}, user)
}

func adminContext(r *http.Request) *http.Request {
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	return r.WithContext(contextWithIdentity(r.Context(), admin))
}

func getServerConfigDB(t *testing.T, srv *Server, ops *OperationalSettings) ServerConfigDBResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp ServerConfigDBResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp
}

func sectionRowRaw(t *testing.T, f *fakeHubSettingStore, section string) map[string]interface{} {
	t.Helper()
	f.mu.Lock()
	row := f.settings[section]
	f.mu.Unlock()
	require.NotNil(t, row, "%s row missing", section)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(row.Value, &m))
	return m
}

// githubAppRowRaw returns the stored github_app row as a generic map, so
// tests see keys the section struct does not model.
func githubAppRowRaw(t *testing.T, f *fakeHubSettingStore) map[string]interface{} {
	t.Helper()
	f.mu.Lock()
	row := f.settings["github_app"]
	f.mu.Unlock()
	require.NotNil(t, row, "github_app row missing")
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(row.Value, &m))
	return m
}

// newTestDBServer creates a test Server configured in postgres mode with a
// fakeHubSettingStore and OperationalSettings wired up for testing.
func newTestDBServer(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fileK := emptyKoanf()
	envK := emptyKoanf()

	ops := NewOperationalSettings(fakeStore, fileK, envK)

	srv := &Server{
		dbDriver:    "postgres",
		maintenance: NewMaintenanceState(false, ""),
	}
	srv.SetOperationalSettings(ops)

	return srv, fakeStore, ops
}

func adminRequest(method, url, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, url, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, url, nil)
	}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	// An interactive session, as the auth middleware records it for a
	// signed-in admin: settings writes refuse every other credential kind
	// for keys outside the configuration set.
	ctx := contextWithCredentialContext(contextWithIdentity(r.Context(), admin), CredentialContext{Kind: CredentialKindInteractive})
	return r.WithContext(ctx)
}

func putServerConfigDB(t *testing.T, srv *Server, ops *OperationalSettings, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
	return rr
}

// sdsWriteGlobalNFSBlock writes a global settings file whose
// server.shared_dir_storage carries a complete nfs block (backend local).
func sdsWriteGlobalNFSBlock(t *testing.T) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	dir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(`schema_version: "1"
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /srv/nfs
      shares:
        - id: share-1
          pv_name: pv-1
`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sdsGetProfilesDB(t *testing.T, srv *Server, ops *OperationalSettings) map[string]config.V1ProfileConfig {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp ServerConfigDBResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp.Profiles
}

const (
	rtWebhook    = "https://hooks.example.test/services/T000/B000/real"
	rtPrivateKey = "-----BEGIN RSA PRIVATE KEY-----\nreal\n-----END RSA PRIVATE KEY-----\n"
	rtWebhookSec = "whsec_real"
	rtOAuthSec   = "oauth-real-secret"
)

// setTempScionHome points HOME at a fresh temp dir with a .scion directory
// and returns the settings.yaml path.
func setTempScionHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".scion", "settings.yaml")
}

// getServerSection decodes the "server" object of a GET response body.
func getServerSection(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	server, ok := resp["server"].(map[string]interface{})
	if !ok {
		t.Fatalf("GET body has no server object: %s", body)
	}
	return server
}

// fileModePutServerConfig issues a file-mode PUT with HOME pointed at a temp
// dir and returns the recorder and the settings.yaml path.
func fileModePutServerConfig(t *testing.T, srv *Server, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	return rr, filepath.Join(tmpHome, ".scion", "settings.yaml")
}
