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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
)

// =============================================================================
// Hub-level default GCP identity: admin server-config write/read paths
// (ptone/scion#1857). Covers both handler families — DB mode (postgres) and
// file mode (SQLite / single-node VM) — and the shared validator.
// =============================================================================

// gcpIdentitySettingsSA registers a service account for the settings tests.
func gcpIdentitySettingsSA(t *testing.T, s store.Store, scope, scopeID string, verified bool) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        uuid.New().String(),
		Scope:     scope,
		ScopeID:   scopeID,
		Email:     fmt.Sprintf("sa-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
		ProjectID: "gcp-proj",
		CreatedBy: "someone",
		Verified:  verified,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// newGCPIdentitySettingsStore returns a migrated in-memory store.
func newGCPIdentitySettingsStore(t *testing.T) store.Store {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Skipf("skipping: test store unavailable (%v)", err)
	}
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// gcpIdentityValidationCase is one PUT body and its expected outcome. The body
// is built per-case because some cases need the ID of a freshly-created SA.
type gcpIdentityValidationCase struct {
	name     string
	mode     string // saAssignCheckMode for the server
	body     func(hubSA, unverifiedSA, projectSA *store.GCPServiceAccount) string
	wantCode int
	wantMsg  string
}

func gcpIdentityValidationCases() []gcpIdentityValidationCase {
	return []gcpIdentityValidationCase{
		{
			name:     "assign without a service account is rejected",
			mode:     SAAssignCheckEnforce,
			body:     func(_, _, _ *store.GCPServiceAccount) string { return `{"default_gcp_identity_mode":"assign"}` },
			wantCode: http.StatusUnprocessableEntity,
			wantMsg:  "requires a service account",
		},
		{
			name: "unknown service account is rejected",
			mode: SAAssignCheckEnforce,
			body: func(_, _, _ *store.GCPServiceAccount) string {
				return `{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":"no-such-sa"}`
			},
			wantCode: http.StatusUnprocessableEntity,
			wantMsg:  "not found",
		},
		{
			name: "unverified service account is rejected",
			mode: SAAssignCheckEnforce,
			body: func(_, u, _ *store.GCPServiceAccount) string {
				return fmt.Sprintf(`{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":%q}`, u.ID)
			},
			wantCode: http.StatusUnprocessableEntity,
			wantMsg:  "not verified",
		},
		{
			name: "project-scoped service account is rejected",
			mode: SAAssignCheckEnforce,
			body: func(_, _, p *store.GCPServiceAccount) string {
				return fmt.Sprintf(`{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":%q}`, p.ID)
			},
			wantCode: http.StatusUnprocessableEntity,
			wantMsg:  "must be hub-scoped",
		},
		{
			name: "hub-scoped service account outside enforce mode is rejected",
			mode: SAAssignCheckOff,
			body: func(h, _, _ *store.GCPServiceAccount) string {
				return fmt.Sprintf(`{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":%q}`, h.ID)
			},
			wantCode: http.StatusUnprocessableEntity,
			wantMsg:  "gcpIamCheckMode=enforce",
		},
		{
			name:     "unknown mode is rejected",
			mode:     SAAssignCheckEnforce,
			body:     func(_, _, _ *store.GCPServiceAccount) string { return `{"default_gcp_identity_mode":"foo"}` },
			wantCode: http.StatusUnprocessableEntity,
		},
		{
			name: "verified hub-scoped service account in enforce mode is accepted",
			mode: SAAssignCheckEnforce,
			body: func(h, _, _ *store.GCPServiceAccount) string {
				return fmt.Sprintf(`{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":%q}`, h.ID)
			},
			wantCode: http.StatusOK,
		},
		{
			name:     "passthrough is accepted",
			mode:     SAAssignCheckEnforce,
			body:     func(_, _, _ *store.GCPServiceAccount) string { return `{"default_gcp_identity_mode":"passthrough"}` },
			wantCode: http.StatusOK,
		},
		{
			name: "clearing is always accepted",
			mode: SAAssignCheckOff,
			body: func(_, _, _ *store.GCPServiceAccount) string {
				return `{"default_gcp_identity_mode":"","default_gcp_identity_service_account_id":""}`
			},
			wantCode: http.StatusOK,
		},
	}
}

// TestPutServerConfigDB_HubDefaultGCPIdentity_Validation drives
// validateHubDefaultGCPIdentity through the DB-mode PUT handler.
func TestPutServerConfigDB_HubDefaultGCPIdentity_Validation(t *testing.T) {
	for _, tc := range gcpIdentityValidationCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, ops := newTestDBServer(t)
			s := newGCPIdentitySettingsStore(t)
			srv.store = s
			setMode(srv, tc.mode)
			hubSA := gcpIdentitySettingsSA(t, s, store.ScopeHub, "hub", true)
			unverified := gcpIdentitySettingsSA(t, s, store.ScopeHub, "hub", false)
			projectSA := gcpIdentitySettingsSA(t, s, store.ScopeProject, "some-project", true)

			rr := httptest.NewRecorder()
			srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
				tc.body(hubSA, unverified, projectSA)), ops)

			require.Equal(t, tc.wantCode, rr.Code, "body: %s", rr.Body.String())
			if tc.wantMsg != "" {
				assert.Contains(t, rr.Body.String(), tc.wantMsg)
			}
		})
	}
}

// TestServerConfigDB_HubDefaultGCPIdentity_RoundTrip covers the DB-mode write
// and read paths end to end: extractKoanfKeysFromRequest/buildSingleSectionDoc
// on PUT, buildSnapshotFromKoanf/ApplySnapshot into the running server, and
// applySnapshotToResponse on GET.
func TestServerConfigDB_HubDefaultGCPIdentity_RoundTrip(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	s := newGCPIdentitySettingsStore(t)
	srv.store = s
	setMode(srv, SAAssignCheckEnforce)
	sa := gcpIdentitySettingsSA(t, s, store.ScopeHub, "hub", true)

	body := fmt.Sprintf(`{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":%q}`, sa.ID)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
	require.Equal(t, http.StatusOK, rr.Code, "PUT body: %s", rr.Body.String())

	// Snapshot built from the stored koanf doc.
	snap := ops.Snapshot()
	assert.Equal(t, store.GCPMetadataModeAssign, snap.DefaultGCPIdentityMode)
	assert.Equal(t, sa.ID, snap.DefaultGCPIdentityServiceAccountID)

	// Applied to the running server's agent defaults.
	ApplySnapshot(srv, snap)
	got := srv.hubAgentDefaults()
	assert.Equal(t, store.GCPMetadataModeAssign, got.DefaultGCPIdentityMode)
	assert.Equal(t, sa.ID, got.DefaultGCPIdentityServiceAccountID)

	// Reported back on GET.
	rr = httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, "GET body: %s", rr.Body.String())
	var resp ServerConfigResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, store.GCPMetadataModeAssign, resp.DefaultGCPIdentityMode)
	assert.Equal(t, sa.ID, resp.DefaultGCPIdentityServiceAccountID)

	// Clearing removes both.
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"default_gcp_identity_mode":"","default_gcp_identity_service_account_id":""}`), ops)
	require.Equal(t, http.StatusOK, rr.Code, "clear body: %s", rr.Body.String())
	snap = ops.Snapshot()
	assert.Empty(t, snap.DefaultGCPIdentityMode)
	assert.Empty(t, snap.DefaultGCPIdentityServiceAccountID)
}

// ---- File / SQLite mode ----

// fileModeGCPIdentityServer points the global settings directory at a temp
// HOME seeded with a settings.yaml that has a server key (as every hub's does)
// and returns a file-mode server backed by a real store.
func fileModeGCPIdentityServer(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	require.NoError(t, os.WriteFile(settingsPath, []byte(
		"schema_version: \"1\"\ndefault_timezone: UTC\nserver:\n  hub:\n    port: 9810\n"), 0o644))

	s := newGCPIdentitySettingsStore(t)
	srv := &Server{
		dbDriver:    "sqlite",
		maintenance: NewMaintenanceState(false, ""),
		store:       s,
	}
	return srv, s, settingsPath
}

func readSettingsYAML(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var raw map[string]interface{}
	require.NoError(t, yamlv3.Unmarshal(data, &raw))
	return raw
}

// TestServerConfigFile_HubDefaultGCPIdentity_RoundTrip is the single-node VM
// case (review C1): on a SQLite hub the admin UI goes through the file-mode
// handlers, and the setting must be persisted to settings.yaml, reported on
// GET, and reach hubAgentDefaults() without a restart.
func TestServerConfigFile_HubDefaultGCPIdentity_RoundTrip(t *testing.T) {
	srv, _, settingsPath := fileModeGCPIdentityServer(t)

	rr := httptest.NewRecorder()
	srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"default_gcp_identity_mode":"passthrough"}`))
	require.Equal(t, http.StatusOK, rr.Code, "PUT body: %s", rr.Body.String())

	raw := readSettingsYAML(t, settingsPath)
	assert.Equal(t, "passthrough", raw["default_gcp_identity_mode"])
	assert.Equal(t, "UTC", raw["default_timezone"], "unrelated keys must be preserved")

	rr = httptest.NewRecorder()
	srv.handleGetServerConfig(rr)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp ServerConfigResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "passthrough", resp.DefaultGCPIdentityMode)

	assert.Equal(t, store.GCPMetadataModePassthrough, srv.hubAgentDefaults().DefaultGCPIdentityMode,
		"a file-mode save must reach the running server via reloadSettings")

	// Clearing deletes the key and the runtime value.
	rr = httptest.NewRecorder()
	srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"default_gcp_identity_mode":"","default_gcp_identity_service_account_id":""}`))
	require.Equal(t, http.StatusOK, rr.Code, "clear body: %s", rr.Body.String())
	raw = readSettingsYAML(t, settingsPath)
	assert.NotContains(t, raw, "default_gcp_identity_mode")
	assert.NotContains(t, raw, "default_gcp_identity_service_account_id")
	assert.Empty(t, srv.hubAgentDefaults().DefaultGCPIdentityMode)
}

// TestServerConfigFile_HubDefaultGCPIdentity_AssignRoundTrip covers the SA ID
// key through the file-mode path.
func TestServerConfigFile_HubDefaultGCPIdentity_AssignRoundTrip(t *testing.T) {
	srv, s, settingsPath := fileModeGCPIdentityServer(t)
	setMode(srv, SAAssignCheckEnforce)
	sa := gcpIdentitySettingsSA(t, s, store.ScopeHub, "hub", true)

	rr := httptest.NewRecorder()
	srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		fmt.Sprintf(`{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":%q}`, sa.ID)))
	require.Equal(t, http.StatusOK, rr.Code, "PUT body: %s", rr.Body.String())

	raw := readSettingsYAML(t, settingsPath)
	assert.Equal(t, sa.ID, raw["default_gcp_identity_service_account_id"])

	rr = httptest.NewRecorder()
	srv.handleGetServerConfig(rr)
	var resp ServerConfigResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, sa.ID, resp.DefaultGCPIdentityServiceAccountID)

	got := srv.hubAgentDefaults()
	assert.Equal(t, store.GCPMetadataModeAssign, got.DefaultGCPIdentityMode)
	assert.Equal(t, sa.ID, got.DefaultGCPIdentityServiceAccountID)
}

// TestPutServerConfigFile_HubDefaultGCPIdentity_Validation runs the same
// validator cases through the file-mode handler. A rejected PUT must leave
// settings.yaml untouched.
func TestPutServerConfigFile_HubDefaultGCPIdentity_Validation(t *testing.T) {
	for _, tc := range gcpIdentityValidationCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, settingsPath := fileModeGCPIdentityServer(t)
			setMode(srv, tc.mode)
			hubSA := gcpIdentitySettingsSA(t, s, store.ScopeHub, "hub", true)
			unverified := gcpIdentitySettingsSA(t, s, store.ScopeHub, "hub", false)
			projectSA := gcpIdentitySettingsSA(t, s, store.ScopeProject, "some-project", true)
			before, err := os.ReadFile(settingsPath)
			require.NoError(t, err)

			rr := httptest.NewRecorder()
			srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
				tc.body(hubSA, unverified, projectSA)))

			require.Equal(t, tc.wantCode, rr.Code, "body: %s", rr.Body.String())
			if tc.wantMsg != "" {
				assert.Contains(t, rr.Body.String(), tc.wantMsg)
			}
			if tc.wantCode != http.StatusOK {
				after, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after), "a rejected PUT must not write settings.yaml")
			}
		})
	}
}

// TestPutServerConfigFile_HubDefaultGCPIdentity_ValidatesMergedResult pins
// that file-mode validation checks the effective pair, not just the fields in
// the request: switching mode to assign against a previously-stored SA ID is
// accepted, and clearing only the SA while mode stays assign is rejected.
func TestPutServerConfigFile_HubDefaultGCPIdentity_ValidatesMergedResult(t *testing.T) {
	srv, s, _ := fileModeGCPIdentityServer(t)
	setMode(srv, SAAssignCheckEnforce)
	sa := gcpIdentitySettingsSA(t, s, store.ScopeHub, "hub", true)

	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		return rr
	}

	rr := put(fmt.Sprintf(`{"default_gcp_identity_service_account_id":%q}`, sa.ID))
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	rr = put(`{"default_gcp_identity_mode":"assign"}`)
	require.Equal(t, http.StatusOK, rr.Code, "assign against the stored SA must pass; body: %s", rr.Body.String())
	rr = put(`{"default_gcp_identity_service_account_id":""}`)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code,
		"removing the SA while the stored mode is assign must be rejected; body: %s", rr.Body.String())
}
