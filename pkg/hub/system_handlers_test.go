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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	_ "github.com/GoogleCloudPlatform/scion/pkg/store/sqlite" // register the sqlite driver for the hub test binary
)

// doWorkstationRequest performs an authenticated request from a loopback address.
func doWorkstationRequest(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	req.RemoteAddr = "127.0.0.1:1234"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ============================================================================
// requireWorkstation middleware
// ============================================================================

func TestRequireWorkstation_Disabled(t *testing.T) {
	srv, _ := testServer(t)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/system/status", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 when workstation disabled, got %d", rec.Code)
	}
}

func TestRequireWorkstation_Enabled(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/status", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 when workstation enabled, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The onboarding status must not report image presence it never checks
// (ptone/scion#4640); the remaining fields keep their JSON names.
func TestSystemStatus_NoUncheckedImagesPresent(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if _, ok := body["imagesPresent"]; ok {
		t.Errorf("status reports imagesPresent, which is never checked: %s", rec.Body.String())
	}
	for _, key := range []string{"initialized", "identitySet", "runtimeOK", "harnessesSeeded", "hasWorkspace", "complete"} {
		if _, ok := body[key]; !ok {
			t.Errorf("status is missing %q: %s", key, rec.Body.String())
		}
	}
}

func TestSystemStatus_ImageTag(t *testing.T) {
	srv, _ := testWorkstationServer(t)

	setTestVersion(t, "v0.4.0-preview.3")
	status := getWorkstationOnboardingStatus(t, srv)
	assert.Equal(t, "v0.4.0-preview.3", status.ImageTag)

	setTestVersion(t, "dev")
	status = getWorkstationOnboardingStatus(t, srv)
	assert.Equal(t, "", status.ImageTag)
}

func getWorkstationOnboardingStatus(t *testing.T, srv *Server) OnboardingStatus {
	t.Helper()
	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/status", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var status OnboardingStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	return status
}

// setupCLIWorkstation prepares a workstation server mirroring `scion server start`
// before the web onboarding wizard has been opened: ~/.scion/settings.yaml exists
// without server.auth identity fields, a local container runtime is reachable,
// global harness configs are bootstrapped into the store, and the Hub's built-in
// global system project is present when withGlobalProject is true.
func setupCLIWorkstation(t *testing.T, withGlobalProject bool) (*Server, store.Store, string) {
	t.Helper()
	srv, s := testWorkstationServer(t)
	srv.SetStorage(newMockStorage("test-bucket"))
	ctx := context.Background()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\n"), 0o600))

	restore := config.OverrideRuntimeDetection(
		func(string) (string, error) { return "/usr/bin/docker", nil },
		func(string, []string) error { return nil },
	)
	t.Cleanup(restore)

	hcDir := writeHarnessConfigDirs(t, globalDir, "claude")
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, hcDir))

	if withGlobalProject {
		require.NoError(t, srv.ensureEmbeddedGlobalProject(ctx))
	}
	return srv, s, globalDir
}

type projectSummaryFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (w projectSummaryFaultStore) ListProjectSummaries(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	if !w.fault.Active() {
		return w.Store.ListProjectSummaries(ctx, f, o)
	}
	return nil, errors.New("database is locked")
}

// ptone/scion#7537: HasWorkspace excludes the Hub's built-in global project, and
// Complete is satisfied when core workstation setup is ready and either web
// identity is configured or a user project exists (e.g. CLI `scion init`).
func TestSystemStatus_WorkspaceAndComplete(t *testing.T) {
	t.Run("fresh install with only global project enters onboarding", func(t *testing.T) {
		srv, _, _ := setupCLIWorkstation(t, true)

		st := getWorkstationOnboardingStatus(t, srv)
		assert.True(t, st.Initialized)
		assert.False(t, st.IdentitySet)
		assert.True(t, st.RuntimeOK)
		assert.True(t, st.HarnessesSeeded)
		assert.False(t, st.HasWorkspace, "built-in global project alone must not count as a user workspace")
		assert.False(t, st.Complete, "fresh install without identity or user project must enter /onboarding")
	})

	t.Run("CLI-initialized workstation with user project skips onboarding", func(t *testing.T) {
		srv, _, _ := setupCLIWorkstation(t, true)

		reg := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/projects/register", map[string]interface{}{
			"name":      "cli-repo",
			"gitRemote": "https://github.com/example/cli-repo.git",
		})
		require.Equal(t, http.StatusOK, reg.Code, "register project: %s", reg.Body.String())

		st := getWorkstationOnboardingStatus(t, srv)
		assert.True(t, st.Initialized)
		assert.False(t, st.IdentitySet)
		assert.True(t, st.RuntimeOK)
		assert.True(t, st.HarnessesSeeded)
		assert.True(t, st.HasWorkspace)
		assert.True(t, st.Complete, "CLI-created user project must satisfy onboarding completeness")
	})

	t.Run("user project created before global project still counts as workspace", func(t *testing.T) {
		srv, _, _ := setupCLIWorkstation(t, false)

		reg := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/projects/register", map[string]interface{}{
			"name":      "older-cli-repo",
			"gitRemote": "https://github.com/example/older-cli-repo.git",
		})
		require.Equal(t, http.StatusOK, reg.Code, "register project: %s", reg.Body.String())
		// Create built-in global project afterwards so it sorts first under
		// ListProjectSummaries's DESC created ordering.
		require.NoError(t, srv.ensureEmbeddedGlobalProject(context.Background()))

		st := getWorkstationOnboardingStatus(t, srv)
		assert.True(t, st.HasWorkspace, "newer global project must not shadow an existing user project")
		assert.True(t, st.Complete)
	})

	t.Run("user project with client-supplied global labels still counts as workspace", func(t *testing.T) {
		srv, _, _ := setupCLIWorkstation(t, true)

		reg := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/projects/register", map[string]interface{}{
			"name": "labeled-user-project",
			"labels": map[string]string{
				store.LabelSystemProject: "true",
				store.LabelGlobalProject: "true",
			},
		})
		require.Equal(t, http.StatusOK, reg.Code, "register project: %s", reg.Body.String())

		st := getWorkstationOnboardingStatus(t, srv)
		assert.True(t, st.HasWorkspace, "client labels on a non-global slug do not make it the global project")
		assert.True(t, st.Complete)
	})

	t.Run("wizard path with IdentitySet true and only global project stays Complete", func(t *testing.T) {
		srv, _, _ := setupCLIWorkstation(t, true)

		put := doWorkstationRequest(t, srv, http.MethodPut, "/api/v1/system/identity", map[string]interface{}{
			"displayName": "Workstation Owner",
			"email":       "owner@example.com",
		})
		require.Equal(t, http.StatusOK, put.Code, "put identity: %s", put.Body.String())

		st := getWorkstationOnboardingStatus(t, srv)
		assert.True(t, st.Initialized)
		assert.True(t, st.IdentitySet)
		assert.True(t, st.RuntimeOK)
		assert.True(t, st.HarnessesSeeded)
		assert.False(t, st.HasWorkspace, "skipping workspace step leaves only global project -> HasWorkspace false")
		assert.True(t, st.Complete, "wizard completion with skipped workspace must stay Complete")
	})

	t.Run("user project does not mark Complete when required setup is missing", func(t *testing.T) {
		t.Run("uninitialized settings", func(t *testing.T) {
			srv, _, globalDir := setupCLIWorkstation(t, true)
			reg := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/projects/register", map[string]interface{}{
				"name":      "cli-repo",
				"gitRemote": "https://github.com/example/cli-repo.git",
			})
			require.Equal(t, http.StatusOK, reg.Code)
			require.NoError(t, os.Remove(filepath.Join(globalDir, "settings.yaml")))

			st := getWorkstationOnboardingStatus(t, srv)
			assert.False(t, st.Initialized)
			assert.True(t, st.HasWorkspace)
			assert.False(t, st.Complete)
		})

		t.Run("runtime unavailable", func(t *testing.T) {
			srv, _, _ := setupCLIWorkstation(t, true)
			reg := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/projects/register", map[string]interface{}{
				"name":      "cli-repo",
				"gitRemote": "https://github.com/example/cli-repo.git",
			})
			require.Equal(t, http.StatusOK, reg.Code)
			restore := config.OverrideRuntimeDetection(
				func(string) (string, error) { return "", errors.New("no runtime") },
				func(string, []string) error { return errors.New("no runtime") },
			)
			t.Cleanup(restore)

			st := getWorkstationOnboardingStatus(t, srv)
			assert.False(t, st.RuntimeOK)
			assert.True(t, st.HasWorkspace)
			assert.False(t, st.Complete)
		})

		t.Run("harnesses not seeded", func(t *testing.T) {
			srv, s := testWorkstationServer(t)
			srv.SetStorage(newMockStorage("test-bucket"))
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			globalDir := filepath.Join(tmpHome, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
				[]byte("schema_version: \"1\"\n"), 0o600))
			restore := config.OverrideRuntimeDetection(
				func(string) (string, error) { return "/usr/bin/docker", nil },
				func(string, []string) error { return nil },
			)
			t.Cleanup(restore)
			require.NoError(t, srv.ensureEmbeddedGlobalProject(context.Background()))
			require.NoError(t, s.CreateProject(context.Background(), &store.Project{
				ID: "11111111-1111-4111-8111-111111111111", Name: "cli-repo", Slug: "cli-repo",
			}))

			st := getWorkstationOnboardingStatus(t, srv)
			assert.False(t, st.HarnessesSeeded)
			assert.True(t, st.HasWorkspace)
			assert.False(t, st.Complete)
		})

		t.Run("store error listing projects", func(t *testing.T) {
			srv, s := testWorkstationServer(t)
			waitUserScopedDataSweep(t, srv)
			_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) projectSummaryFaultStore {
				return projectSummaryFaultStore{Store: inner, fault: f}
			})
			srv.SetStorage(newMockStorage("test-bucket"))
			ctx := context.Background()
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			globalDir := filepath.Join(tmpHome, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
				[]byte("schema_version: \"1\"\n"), 0o600))
			restore := config.OverrideRuntimeDetection(
				func(string) (string, error) { return "/usr/bin/docker", nil },
				func(string, []string) error { return nil },
			)
			t.Cleanup(restore)
			hcDir := writeHarnessConfigDirs(t, globalDir, "claude")
			require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, hcDir))
			require.NoError(t, s.CreateProject(ctx, &store.Project{
				ID: "22222222-2222-4222-8222-222222222222", Name: "cli-repo", Slug: "cli-repo",
			}))

			fault.Arm()
			st := getWorkstationOnboardingStatus(t, srv)
			assert.False(t, st.HasWorkspace)
			assert.False(t, st.Complete)
		})
	})
}

// ============================================================================
// assertLoopback
// ============================================================================

func TestAssertLoopback(t *testing.T) {
	tests := []struct {
		remoteAddr string
		wantErr    bool
	}{
		{"127.0.0.1:1234", false},
		{"[::1]:1234", false},
		{"192.168.1.1:1234", true},
		{"10.0.0.1:5555", true},
		{"192.0.2.1:1234", true},
	}

	for _, tt := range tests {
		t.Run(tt.remoteAddr, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			err := assertLoopback(req)
			if tt.wantErr && err == nil {
				t.Errorf("expected error for %s", tt.remoteAddr)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for %s: %v", tt.remoteAddr, err)
			}
		})
	}
}

func TestAssertLoopback_RejectViaHandler(t *testing.T) {
	srv, _ := testWorkstationServer(t)

	// doRequest uses httptest default RemoteAddr (192.0.2.1:1234) — non-loopback
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/system/status", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-loopback request, got %d", rec.Code)
	}
}

// ============================================================================
// POST /system/init
// ============================================================================

func TestSystemInit_ValidHarnesses(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	restore := config.OverrideRuntimeDetection(
		func(string) (string, error) { return "/usr/bin/docker", nil },
		func(string, []string) error { return nil },
	)
	defer restore()

	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/init", map[string]interface{}{
		"harnesses": []string{"claude"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp systemInitResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !resp.OK || !resp.Initialized {
		t.Errorf("expected ok=true initialized=true, got %+v", resp)
	}

	settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		t.Error("expected settings.yaml to be created")
	}
}

func TestSystemInit_UnknownHarness(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/init", map[string]interface{}{
		"harnesses": []string{"bogus"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown harness, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSystemInit_EmptyHarnesses(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/init", map[string]interface{}{
		"harnesses": []string{},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty harnesses, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSystemInit_SelectiveHarnessConfig(t *testing.T) {
	srv, s := testWorkstationServer(t)
	srv.SetStorage(newMockStorage("test-bucket"))
	ctx := context.Background()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".scion")

	restore := config.OverrideRuntimeDetection(
		func(string) (string, error) { return "/usr/bin/docker", nil },
		func(string, []string) error { return nil },
	)
	defer restore()

	// workstationStart mirrors a workstation hub start: every bundled
	// resource is re-materialized on disk (UpdateDefaultTemplates(true)),
	// then imported by the *FromDir bootstrap.
	workstationStart := func() {
		t.Helper()
		if err := config.MaterializeBundledResources(globalDir, config.MaterializeOptions{Force: true}); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if err := srv.BootstrapTemplatesFromDir(ctx, filepath.Join(globalDir, "templates")); err != nil {
			t.Fatalf("template bootstrap: %v", err)
		}
		if err := srv.BootstrapHarnessConfigsFromDir(ctx, filepath.Join(globalDir, "harness-configs")); err != nil {
			t.Fatalf("harness config bootstrap: %v", err)
		}
	}
	workstationStart()
	if _, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, ""); err != nil {
		t.Fatalf("claude not seeded by startup bootstrap: %v", err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/init", map[string]interface{}{
		"harnesses": []string{"codex"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Codex harness-config should be seeded.
	codexConfig := filepath.Join(tmpHome, ".scion", "harness-configs", "codex", "config.yaml")
	if _, err := os.Stat(codexConfig); err != nil {
		t.Errorf("expected codex config.yaml to be created: %v", err)
	}

	// Claude should NOT be seeded (selective materialization).
	claudeConfig := filepath.Join(tmpHome, ".scion", "harness-configs", "claude", "config.yaml")
	if _, err := os.Stat(claudeConfig); err == nil {
		t.Error("expected claude config.yaml to NOT be created in selective mode")
	}

	// Default template should always be seeded.
	templateFile := filepath.Join(tmpHome, ".scion", "templates", "default", "scion-agent.yaml")
	if _, err := os.Stat(templateFile); err != nil {
		t.Errorf("expected default template scion-agent.yaml to be created: %v", err)
	}

	// ptone/scion#3544 (D3): unselected built-ins count as deleted. They are
	// removed from the hub by init and stay absent across restarts, even
	// though each restart re-materializes them on disk.
	assertSelection := func(when string) {
		t.Helper()
		configs, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
			Scope: store.HarnessConfigScopeGlobal,
		}, store.ListOptions{Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, hc := range configs.Items {
			names = append(names, hc.Name)
		}
		if len(names) != 1 || names[0] != "codex" {
			t.Errorf("%s: global harness configs = %v, want [codex]", when, names)
		}
		if _, err := s.GetTemplateBySlug(ctx, "default", string(store.TemplateScopeGlobal), ""); err != nil {
			t.Errorf("%s: default template missing: %v", when, err)
		}
	}
	assertSelection("after init")
	for i := range 2 {
		workstationStart()
		assertSelection(fmt.Sprintf("after restart %d", i))
	}
}

func TestSystemInit_EmbedOnlyHarness(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	restore := config.OverrideRuntimeDetection(
		func(string) (string, error) { return "/usr/bin/docker", nil },
		func(string, []string) error { return nil },
	)
	defer restore()

	// gemini-cli is the correct harness name (not the retired "gemini" alias).
	// This must succeed and seed the harness-config on disk.
	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/init", map[string]interface{}{
		"harnesses": []string{"gemini-cli"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// gemini-cli harness-config should be seeded on disk.
	geminiConfig := filepath.Join(tmpHome, ".scion", "harness-configs", "gemini-cli", "config.yaml")
	if _, err := os.Stat(geminiConfig); err != nil {
		t.Errorf("expected gemini-cli config.yaml to be created: %v", err)
	}
}

// ============================================================================
// PUT /system/identity
// ============================================================================

func TestSystemIdentity_PUT(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Seed a minimal settings.yaml so UpdateSetting has a file to update
	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("version: \"1\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodPut, "/api/v1/system/identity", map[string]interface{}{
		"displayName": "Test User",
		"email":       "test@example.com",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp systemIdentityResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.DisplayName != "Test User" {
		t.Errorf("expected displayName 'Test User', got %q", resp.DisplayName)
	}
	if resp.Email != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got %q", resp.Email)
	}
}

func TestSystemIdentity_MethodNotAllowed(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/identity", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

// ============================================================================
// POST /system/fs/validate-path
// ============================================================================

func TestFSValidatePath_ManagedOverlap(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	managedDir := filepath.Join(tmpHome, ".scion", "projects", "something")
	if err := os.MkdirAll(managedDir, 0755); err != nil {
		t.Fatal(err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/fs/validate-path", map[string]interface{}{
		"path": managedDir,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp fsValidatePathResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !resp.IsManaged {
		t.Error("expected IsManaged=true for path inside managed directory")
	}
	if resp.Error == "" {
		t.Error("expected non-empty error message for managed path")
	}
}

func TestFSValidatePath_NormalPath(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(tmpHome, "my-project")
	if err := os.MkdirAll(filepath.Join(projectDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/fs/validate-path", map[string]interface{}{
		"path": projectDir,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp fsValidatePathResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.IsManaged {
		t.Error("expected IsManaged=false for normal path")
	}
	if !resp.Exists {
		t.Error("expected Exists=true")
	}
	if !resp.IsDir {
		t.Error("expected IsDir=true")
	}
	if !resp.IsGit {
		t.Error("expected IsGit=true for dir with .git")
	}
	if resp.Error != "" {
		t.Errorf("expected no error, got %q", resp.Error)
	}
}

// ============================================================================
// GET /system/fs/list
// ============================================================================

func TestFSList_HomeDir(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := os.Mkdir(filepath.Join(tmpHome, "visible-dir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tmpHome, ".hidden-dir"), 0755); err != nil {
		t.Fatal(err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/fs/list?path="+tmpHome, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp fsListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	found := false
	for _, e := range resp.Entries {
		if e.Name == "visible-dir" {
			found = true
		}
		if strings.HasPrefix(e.Name, ".") {
			t.Errorf("hidden entry %q should be filtered", e.Name)
		}
	}
	if !found {
		t.Error("expected 'visible-dir' in entries")
	}
}

func TestFSList_OutsideHome(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/fs/list?path=/tmp", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for path outside home, got %d: %s", rec.Code, rec.Body.String())
	}

	// Sibling-prefix bypass: a path like /home/alice-backup must not pass
	// when home is /home/alice.
	siblingDir := tmpHome + "-backup"
	if err := os.MkdirAll(siblingDir, 0755); err != nil {
		t.Fatal(err)
	}
	rec = doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/fs/list?path="+siblingDir, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for sibling-prefix path, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFSList_DefaultsToHome(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := os.Mkdir(filepath.Join(tmpHome, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/fs/list", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp fsListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Path != tmpHome {
		t.Errorf("expected path=%q, got %q", tmpHome, resp.Path)
	}

	found := false
	for _, e := range resp.Entries {
		if e.Name == "subdir" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'subdir' in entries")
	}
}

func TestSystemCheck_RuntimeDaemonReachability(t *testing.T) {
	findRuntime := func(t *testing.T, results []DiagnosticResult) DiagnosticResult {
		t.Helper()
		for _, r := range results {
			if r.Name == "runtime" {
				return r
			}
		}
		t.Fatalf("runtime diagnostic missing from results: %+v", results)
		return DiagnosticResult{}
	}

	setupHome := func(t *testing.T) {
		t.Helper()
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		globalDir := filepath.Join(tmpHome, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
			[]byte("schema_version: \"1\"\n"), 0o600))
	}

	t.Run("binary present and daemon reachable", func(t *testing.T) {
		srv, _ := testWorkstationServer(t)
		setupHome(t)
		restore := config.OverrideRuntimeDetection(
			func(bin string) (string, error) {
				if bin == "docker" {
					return "/usr/bin/docker", nil
				}
				return "", errors.New("not found")
			},
			func(bin string, args []string) error {
				if bin == "docker" {
					return nil
				}
				return errors.New("not found")
			},
		)
		t.Cleanup(restore)

		rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/check", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp systemCheckResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		rt := findRuntime(t, resp.Results)
		assert.Equal(t, "pass", rt.Status)
		assert.Equal(t, "detected runtime: docker", rt.Message)
		assert.True(t, resp.Ready)
	})

	t.Run("binary present and daemon unreachable then re-checked after daemon starts", func(t *testing.T) {
		srv, _ := testWorkstationServer(t)
		setupHome(t)
		daemonRunning := false
		restore := config.OverrideRuntimeDetection(
			func(bin string) (string, error) {
				if bin == "docker" {
					return "/usr/bin/docker", nil
				}
				return "", errors.New("not found")
			},
			func(bin string, args []string) error {
				if bin != "docker" {
					return errors.New("not found")
				}
				if len(args) == 1 && args[0] == "info" && !daemonRunning {
					return errors.New("Cannot connect to the Docker daemon")
				}
				return nil
			},
		)
		t.Cleanup(restore)

		// First check: daemon stopped -> fail with actionable message and ready=false.
		rec1 := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/check", nil)
		require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
		var resp1 systemCheckResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))

		rt1 := findRuntime(t, resp1.Results)
		assert.Equal(t, "fail", rt1.Status)
		assert.Contains(t, rt1.Message, "docker daemon is not running")
		assert.Contains(t, rt1.Message, "Re-check")
		assert.False(t, resp1.Ready)

		// Start daemon and re-check -> immediately reflects live state (no caching).
		daemonRunning = true
		rec2 := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/check", nil)
		require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
		var resp2 systemCheckResponse
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))

		rt2 := findRuntime(t, resp2.Results)
		assert.Equal(t, "pass", rt2.Status)
		assert.Equal(t, "detected runtime: docker", rt2.Message)
		assert.True(t, resp2.Ready)
	})

	t.Run("no runtime binary present preserves existing error", func(t *testing.T) {
		srv, _ := testWorkstationServer(t)
		setupHome(t)
		restore := config.OverrideRuntimeDetection(
			func(string) (string, error) { return "", errors.New("not found") },
			func(string, []string) error { return errors.New("not found") },
		)
		t.Cleanup(restore)

		rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/check", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp systemCheckResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		rt := findRuntime(t, resp.Results)
		assert.Equal(t, "fail", rt.Status)
		assert.Contains(t, rt.Message, config.ErrNoLocalRuntime.Error())
		assert.False(t, resp.Ready)
	})

	t.Run("falls back to secondary runtime whose daemon is reachable", func(t *testing.T) {
		srv, _ := testWorkstationServer(t)
		setupHome(t)
		restore := config.OverrideRuntimeDetection(
			func(bin string) (string, error) {
				if bin == "podman" || bin == "docker" {
					return "/usr/bin/" + bin, nil
				}
				return "", errors.New("not found")
			},
			func(bin string, args []string) error {
				if bin == "podman" && len(args) == 1 && args[0] == "info" {
					return errors.New("podman machine not running")
				}
				return nil
			},
		)
		t.Cleanup(restore)

		rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/check", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp systemCheckResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		rt := findRuntime(t, resp.Results)
		assert.Equal(t, "pass", rt.Status)
		assert.Equal(t, "detected runtime: docker", rt.Message)
		assert.True(t, resp.Ready)
	})
}
