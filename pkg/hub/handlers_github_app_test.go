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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ============================================================================
// GitHub App Config Endpoints
// ============================================================================

func TestHandleGetGitHubApp(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp GitHubAppConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Configured {
		t.Error("expected configured=false for default config")
	}
}

func TestHandleUpdateGitHubApp(t *testing.T) {
	srv, _ := testServer(t)

	appID := int64(12345)
	apiURL := "https://github.example.com/api/v3"
	webhooksEnabled := true

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/github-app", GitHubAppConfigUpdateRequest{
		AppID:           &appID,
		APIBaseURL:      &apiURL,
		WebhooksEnabled: &webhooksEnabled,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp GitHubAppConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.AppID != 12345 {
		t.Errorf("expected app_id 12345, got %d", resp.AppID)
	}
	if resp.APIBaseURL != apiURL {
		t.Errorf("expected api_base_url %s, got %s", apiURL, resp.APIBaseURL)
	}
	if !resp.WebhooksEnabled {
		t.Error("expected webhooks_enabled true")
	}
}

func TestHandleGitHubApp_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/github-app", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

// ============================================================================
// GitHub App Installation Endpoints
// ============================================================================

func TestHandleGitHubAppInstallations_CRUD(t *testing.T) {
	srv, _ := testServer(t)

	// Create installation
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/github-app/installations", map[string]interface{}{
		"installation_id": 12345,
		"account_login":   "acme-org",
		"account_type":    "Organization",
		"app_id":          42,
		"repositories":    []string{"widgets", "api"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// List installations
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/github-app/installations", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var listResp struct {
		Installations []store.GitHubInstallation `json:"installations"`
		Total         int                        `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("failed to decode list response: %v", err)
	}
	if listResp.Total != 1 {
		t.Errorf("expected 1 installation, got %d", listResp.Total)
	}
	if listResp.Installations[0].AccountLogin != "acme-org" {
		t.Errorf("expected acme-org, got %s", listResp.Installations[0].AccountLogin)
	}

	// Get by ID
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/github-app/installations/12345", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var inst store.GitHubInstallation
	if err := json.NewDecoder(rec.Body).Decode(&inst); err != nil {
		t.Fatalf("failed to decode get response: %v", err)
	}
	if inst.InstallationID != 12345 {
		t.Errorf("expected installation_id 12345, got %d", inst.InstallationID)
	}

	// Delete
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/github-app/installations/12345", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify deleted
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/github-app/installations/12345", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", rec.Code)
	}
}

func TestHandleGitHubAppInstallations_ValidationErrors(t *testing.T) {
	srv, _ := testServer(t)

	// Missing installation_id
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/github-app/installations", map[string]interface{}{
		"account_login": "acme",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for missing installation_id, got %d", rec.Code)
	} else if e := settingsErrorEntries(t, rec); e[0].Field != "installation_id" {
		t.Errorf("field = %q, want installation_id", e[0].Field)
	}

	// Missing account_login
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/github-app/installations", map[string]interface{}{
		"installation_id": 123,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for missing account_login, got %d", rec.Code)
	} else if e := settingsErrorEntries(t, rec); e[0].Field != "account_login" {
		t.Errorf("field = %q, want account_login", e[0].Field)
	}
}

// ============================================================================
// Project GitHub Installation Association
// ============================================================================

func TestHandleProjectGitHubInstallation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:        tid("project_gh_test"),
		Slug:      "gh-test-project",
		Name:      "GH Test Project",
		GitRemote: "https://github.com/acme/widgets",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create an installation
	inst := &store.GitHubInstallation{
		InstallationID: 54321,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Status:         store.GitHubInstallationStatusActive,
	}
	if err := s.CreateGitHubInstallation(ctx, inst); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	// Associate project with installation
	rec := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/github-installation", tid("project_gh_test")), map[string]interface{}{
		"installation_id": 54321,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify project has installation ID
	updatedProject, err := s.GetProject(ctx, tid("project_gh_test"))
	if err != nil {
		t.Fatalf("failed to get project: %v", err)
	}
	if updatedProject.GitHubInstallationID == nil || *updatedProject.GitHubInstallationID != 54321 {
		t.Errorf("expected installation_id 54321, got %v", updatedProject.GitHubInstallationID)
	}
	if updatedProject.GitHubAppStatus == nil || updatedProject.GitHubAppStatus.State != store.GitHubAppStateUnchecked {
		t.Error("expected unchecked status after association")
	}

	// Get status
	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/github-status", tid("project_gh_test")), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Remove association
	rec = doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/github-installation", tid("project_gh_test")), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify removed
	clearedProject, err := s.GetProject(ctx, tid("project_gh_test"))
	if err != nil {
		t.Fatalf("failed to get project: %v", err)
	}
	if clearedProject.GitHubInstallationID != nil {
		t.Error("expected nil installation_id after removal")
	}
}

func TestHandleProjectGitHubStatus_PostNoInstallation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_gh_status_check"), Slug: "gh-status-check", Name: "GH Status Check",
		GitRemote: "https://github.com/acme/widgets",
		Created:   time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// POST without installation should return 400
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/github-status", tid("project_gh_status_check")), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for project without installation, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleProjectGitHubStatus_PostWithInstallation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create the installation record first (project has FK to installation)
	instID := int64(77777)
	inst := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Status:         store.GitHubInstallationStatusActive,
	}
	if err := s.CreateGitHubInstallation(ctx, inst); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID: tid("project_gh_status_check2"), Slug: "gh-status-check2", Name: "GH Status Check 2",
		GitRemote: "https://github.com/acme/widgets",
		Created:   time.Now(), Updated: time.Now()}
	project.GitHubInstallationID = &instID
	project.GitHubAppStatus = &store.GitHubAppProjectStatus{
		State:       store.GitHubAppStateUnchecked,
		LastChecked: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// POST should succeed (though minting will fail because no GitHub App
	// is configured — the endpoint should still return 200 with the error
	// captured in the response and project status updated to error)
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/github-status", tid("project_gh_status_check2")), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should have a check_error since GitHub App is not configured
	if _, ok := resp["check_error"]; !ok {
		t.Error("expected check_error in response since GitHub App is not configured")
	}

	// Project status should now be updated (to error since minting failed)
	statusMap, ok := resp["status"].(map[string]interface{})
	if !ok {
		t.Fatal("expected status object in response")
	}
	if statusMap["state"] != store.GitHubAppStateError {
		t.Errorf("expected state=%s after failed check, got %v", store.GitHubAppStateError, statusMap["state"])
	}
}

func TestHandleProjectGitHubInstallation_NotFoundInstallation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_gh_notfound"), Slug: "gh-nf", Name: "GH NF",
		Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/github-installation", tid("project_gh_notfound")), map[string]interface{}{
		"installation_id": 99999,
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent installation, got %d", rec.Code)
	}
}

// ============================================================================
// Project GitHub Permissions
// ============================================================================

func TestHandleProjectGitHubPermissions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_gh_perms"), Slug: "gh-perms", Name: "GH Perms",
		Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Get defaults
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/github-permissions", tid("project_gh_perms")), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var perms store.GitHubTokenPermissions
	if err := json.NewDecoder(rec.Body).Decode(&perms); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	wantDefaults := store.GitHubTokenPermissions(githubapp.DefaultTokenPermissions())
	if perms != wantDefaults {
		t.Errorf("expected default permissions %+v, got %+v", wantDefaults, perms)
	}

	// Set custom permissions
	rec = doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/github-permissions", tid("project_gh_perms")), map[string]interface{}{
		"contents": "read",
		"metadata": "read",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify stored
	updatedProject, err := s.GetProject(ctx, tid("project_gh_perms"))
	if err != nil {
		t.Fatalf("failed to get project: %v", err)
	}
	if updatedProject.GitHubPermissions == nil || updatedProject.GitHubPermissions.Contents != "read" {
		t.Error("expected custom contents:read permission")
	}

	// Reset to defaults
	rec = doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/github-permissions", tid("project_gh_perms")), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}

	clearedProject, err := s.GetProject(ctx, tid("project_gh_perms"))
	if err != nil {
		t.Fatalf("failed to get project: %v", err)
	}
	if clearedProject.GitHubPermissions != nil {
		t.Error("expected nil permissions after reset")
	}

	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/github-permissions", tid("project_gh_perms")), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after reset, got %d", rec.Code)
	}
	perms = store.GitHubTokenPermissions{}
	if err := json.NewDecoder(rec.Body).Decode(&perms); err != nil {
		t.Fatalf("failed to decode after reset: %v", err)
	}
	if perms != wantDefaults {
		t.Errorf("expected default permissions after reset %+v, got %+v", wantDefaults, perms)
	}
}

// ============================================================================
// GitHub App Token Refresh Endpoint (Phase 3)
// ============================================================================

// doRequestWithAgentTokenGH performs an HTTP request with an agent JWT token.
func doRequestWithAgentTokenGH(t *testing.T, srv *Server, method, path string, body interface{}, token string) *httptest.ResponseRecorder {
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
	req.Header.Set("X-Scion-Agent-Token", token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleAgentGitHubTokenRefresh_NoAuth(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// Create a project and agent
	project := &store.Project{
		ID:   tid("project_gh_refresh"),
		Name: "Test Project",
		Slug: "test-project",
	}
	if err := srv.store.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:        tid("agent_gh_refresh"),
		Name:      "test-agent",
		Slug:      "test-agent",
		ProjectID: project.ID,
	}
	if err := srv.store.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Request without any auth should fail with 401
	rec := doRequestNoAuth(t, srv, http.MethodPost,
		"/api/v1/agents/agent_gh_refresh/refresh-token", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentGitHubTokenRefresh_DevAuth(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// Create a project and agent
	project := &store.Project{
		ID:   tid("project_gh_refresh2"),
		Name: "Test Project 2",
		Slug: "test-project-2",
	}
	if err := srv.store.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:        tid("agent_gh_refresh2"),
		Name:      "test-agent-2",
		Slug:      "test-agent-2",
		ProjectID: project.ID,
	}
	if err := srv.store.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Dev auth creates a user identity, not an agent identity — should fail with 401
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/agents/agent_gh_refresh2/refresh-token", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 (needs agent auth, not user auth), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentGitHubTokenRefresh_SelfAccess(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// Create a project and agent
	project := &store.Project{
		ID:   tid("project_gh_refresh3"),
		Name: "Test Project 3",
		Slug: "test-project-3",
	}
	if err := srv.store.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:        tid("agent_gh_refresh3"),
		Name:      "test-agent-3",
		Slug:      "test-agent-3",
		ProjectID: project.ID,
	}
	if err := srv.store.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	if srv.agentTokenService == nil {
		t.Skip("agent token service not available")
	}

	// Generate an agent token with refresh scope
	agentToken := srv.generateRecordedAgentToken(t,
		tid("agent_gh_refresh3"), project.ID,
		[]AgentTokenScope{ScopeAgentTokenRefresh}, nil)

	// Agent trying to refresh another agent's token should fail
	rec := doRequestWithAgentTokenGH(t, srv, http.MethodPost,
		"/api/v1/agents/some_other_agent/refresh-token", nil, agentToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 (wrong agent), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentGitHubTokenRefresh_NoInstallation(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// Create a project WITHOUT a GitHub App installation
	project := &store.Project{
		ID:   tid("project_gh_refresh4"),
		Name: "Test Project 4",
		Slug: "test-project-4",
	}
	if err := srv.store.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:        tid("agent_gh_refresh4"),
		Name:      "test-agent-4",
		Slug:      "test-agent-4",
		ProjectID: project.ID,
	}
	if err := srv.store.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	if srv.agentTokenService == nil {
		t.Skip("agent token service not available")
	}

	agentToken := srv.generateRecordedAgentToken(t,
		tid("agent_gh_refresh4"), project.ID,
		[]AgentTokenScope{ScopeAgentTokenRefresh}, nil)

	// Request should fail because project has no GitHub App installation
	rec := doRequestWithAgentTokenGH(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/agents/%s/refresh-token", agent.ID), nil, agentToken)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 (no installation), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGitHubAppHealthCheck_LiveConfigChanges(t *testing.T) {
	srv, _ := testServer(t)
	srv.scheduler = NewScheduler(srv.store, slog.Default())
	srv.scheduler.srv = srv
	srv.registerSchedulerHandlers()

	countAndInterval := func() (int, int) {
		var count, interval int
		for _, h := range srv.scheduler.Status().Recurring {
			if h.Name == "github-app-health-check" {
				count++
				interval = h.Interval
			}
		}
		return count, interval
	}

	// Initially AppID == 0: health check should not be registered.
	if count, _ := countAndInterval(); count != 0 {
		t.Fatalf("expected 0 health-check handlers initially, got %d", count)
	}

	// Setting app_id live registers the health check at 6h (360 min).
	appID := int64(42)
	webhooksDisabled := false
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/github-app", GitHubAppConfigUpdateRequest{
		AppID:           &appID,
		WebhooksEnabled: &webhooksDisabled,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if count, interval := countAndInterval(); count != 1 || interval != 360 {
		t.Fatalf("after setting app_id live: got count=%d interval=%d, want count=1 interval=360", count, interval)
	}

	// Enabling webhooks reschedules to 24h (1440 min) without duplicating.
	webhooksEnabled := true
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/github-app", GitHubAppConfigUpdateRequest{
		WebhooksEnabled: &webhooksEnabled,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if count, interval := countAndInterval(); count != 1 || interval != 1440 {
		t.Fatalf("after enabling webhooks: got count=%d interval=%d, want count=1 interval=1440", count, interval)
	}

	// Clearing app_id unregisters the health check.
	zeroAppID := int64(0)
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/github-app", GitHubAppConfigUpdateRequest{
		AppID: &zeroAppID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if count, _ := countAndInterval(); count != 0 {
		t.Fatalf("after clearing app_id: got count=%d, want 0", count)
	}

	// ApplySnapshot setting app_id live also registers the health check.
	ApplySnapshot(srv, Layer1Snapshot{
		FromOperationalSettings: true,
		GitHubAppID:             99,
		GitHubWebhooksEnabled:   false,
	})
	if count, interval := countAndInterval(); count != 1 || interval != 360 {
		t.Fatalf("after ApplySnapshot setting app_id: got count=%d interval=%d, want count=1 interval=360", count, interval)
	}

	// ApplySnapshot clearing app_id unregisters the health check.
	ApplySnapshot(srv, Layer1Snapshot{
		FromOperationalSettings: true,
	})
	if count, _ := countAndInterval(); count != 0 {
		t.Fatalf("after ApplySnapshot clearing app_id: got count=%d, want 0", count)
	}
}

func TestScheduler_UpsertRecurringSingleton_PromptFirstRunAndShutdownSafety(t *testing.T) {
	sched := NewScheduler(nil, slog.Default(), WithTickInterval(time.Hour))
	sched.MaxJitter = 0

	var preStartCalls atomic.Int32
	// Registration before Start does not trigger an extra async run beyond tick-0.
	sched.UpsertRecurringSingleton("pre-start", 360, store.LockGitHubAppHealthCheck, func(ctx context.Context) {
		preStartCalls.Add(1)
	})
	if got := preStartCalls.Load(); got != 0 {
		t.Fatalf("pre-Start UpsertRecurringSingleton fired immediately (%d calls), want 0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	// Wait for tick-0 of pre-start handler.
	deadline := time.Now().Add(2 * time.Second)
	for preStartCalls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := preStartCalls.Load(); got != 1 {
		t.Fatalf("expected 1 tick-0 call for pre-start handler, got %d", got)
	}

	// New live registration fires once asynchronously promptly.
	var liveCalls atomic.Int32
	liveFired := make(chan struct{}, 2)
	sched.UpsertRecurringSingleton("live-check", 360, store.LockGitHubAppHealthCheck, func(ctx context.Context) {
		liveCalls.Add(1)
		liveFired <- struct{}{}
	})
	select {
	case <-liveFired:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for immediate async run on new live registration")
	}

	// Rescheduling an existing handler updates interval without firing an extra run.
	sched.UpsertRecurringSingleton("live-check", 1440, store.LockGitHubAppHealthCheck, func(ctx context.Context) {
		liveCalls.Add(1)
		liveFired <- struct{}{}
	})
	select {
	case <-liveFired:
		t.Fatal("rescheduling an existing handler must not trigger an extra run")
	case <-time.After(100 * time.Millisecond):
	}
	if got := liveCalls.Load(); got != 1 {
		t.Fatalf("expected 1 total live call after reschedule, got %d", got)
	}

	// Stopping the scheduler prevents any further live registrations from firing.
	sched.Stop()
	var afterStopCalls atomic.Int32
	sched.UpsertRecurringSingleton("after-stop", 360, store.LockGitHubAppHealthCheck, func(ctx context.Context) {
		afterStopCalls.Add(1)
	})
	time.Sleep(100 * time.Millisecond)
	if got := afterStopCalls.Load(); got != 0 {
		t.Fatalf("UpsertRecurringSingleton after Stop fired %d times, want 0", got)
	}
}

func TestGitHubAppHealthCheck_ConcurrentSyncMatchesFinalState(t *testing.T) {
	srv, _ := testServer(t)
	srv.scheduler = NewScheduler(srv.store, slog.Default())
	srv.scheduler.srv = srv

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			srv.mu.Lock()
			if i%2 == 0 {
				srv.config.GitHubAppConfig.AppID = 42
				srv.config.GitHubAppConfig.WebhooksEnabled = true
			} else {
				srv.config.GitHubAppConfig.AppID = 0
				srv.config.GitHubAppConfig.WebhooksEnabled = false
			}
			srv.mu.Unlock()
			srv.syncGitHubAppHealthCheckSchedule()
		}(i)
	}
	wg.Wait()

	// Final explicit state must always win cleanly with no duplicates.
	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 77
	srv.config.GitHubAppConfig.WebhooksEnabled = false
	srv.mu.Unlock()
	srv.syncGitHubAppHealthCheckSchedule()

	var count, interval int
	for _, h := range srv.scheduler.Status().Recurring {
		if h.Name == githubAppHealthCheckName {
			count++
			interval = h.Interval
		}
	}
	if count != 1 || interval != 360 {
		t.Fatalf("after concurrent syncs + final sync: got count=%d interval=%d, want count=1 interval=360", count, interval)
	}
}

func TestGitHubAppPrivateKeySourceAndOverrideWarning(t *testing.T) {
	t.Run("startup_config_key_overrides_stored_secret", func(t *testing.T) {
		srv, _ := testServer(t)
		ctx := context.Background()

		const startupSecretVal = "SUPER_SECRET_STARTUP_PEM_MATERIAL"
		const storedSecretVal = "SUPER_SECRET_STORED_PEM_MATERIAL"

		if err := srv.setGitHubAppSecret(ctx, GitHubAppSecretPrivateKey, storedSecretVal, "test", "admin"); err != nil {
			t.Fatalf("failed to store secret: %v", err)
		}
		srv.mu.Lock()
		srv.config.GitHubAppConfig.AppID = 42
		srv.config.GitHubAppConfig.PrivateKey = startupSecretVal
		srv.startupGitHubAppPrivateKey = true
		srv.mu.Unlock()

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp GitHubAppConfigResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if !resp.HasPrivateKey {
			t.Fatal("expected has_private_key=true")
		}
		if resp.PrivateKeySource != GitHubAppPrivateKeySourceConfig {
			t.Errorf("private_key_source = %q, want %q", resp.PrivateKeySource, GitHubAppPrivateKeySourceConfig)
		}
		if !resp.PrivateKeyOverridesSecret {
			t.Errorf("expected private_key_overrides_secret=true")
		}

		// Capture warning log and verify no key material is logged.
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		defer slog.SetDefault(prev)

		srv.warnGitHubAppKeyOverride(ctx)
		if !srv.githubAppKeyOverrideWarned.Load() {
			t.Errorf("expected githubAppKeyOverrideWarned to be true after first warning")
		}
		firstLen := buf.Len()
		// Subsequent call hits the atomic fast-path and logs nothing further.
		srv.warnGitHubAppKeyOverride(ctx)
		if buf.Len() != firstLen {
			t.Errorf("expected second warnGitHubAppKeyOverride call to be a no-op, got extra output: %q", buf.String()[firstLen:])
		}
		logged := buf.String()
		if !bytes.Contains([]byte(logged), []byte("overriding the stored hub secret")) {
			t.Errorf("expected override warning log, got: %q", logged)
		}
		if !bytes.Contains([]byte(logged), []byte("active_source=config")) {
			t.Errorf("expected active_source=config in log, got: %q", logged)
		}
		if bytes.Contains([]byte(logged), []byte(startupSecretVal)) || bytes.Contains([]byte(logged), []byte(storedSecretVal)) {
			t.Errorf("log must never contain private key material, got: %q", logged)
		}
	})

	t.Run("file_key_overrides_stored_secret", func(t *testing.T) {
		srv, _ := testServer(t)
		ctx := context.Background()

		if err := srv.setGitHubAppSecret(ctx, GitHubAppSecretPrivateKey, "stored-key", "test", "admin"); err != nil {
			t.Fatalf("failed to store secret: %v", err)
		}
		srv.mu.Lock()
		srv.config.GitHubAppConfig.AppID = 42
		srv.config.GitHubAppConfig.PrivateKeyPath = "/etc/ghapp/key.pem"
		srv.mu.Unlock()

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp GitHubAppConfigResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.PrivateKeySource != GitHubAppPrivateKeySourceFile {
			t.Errorf("private_key_source = %q, want %q", resp.PrivateKeySource, GitHubAppPrivateKeySourceFile)
		}
		if !resp.PrivateKeyOverridesSecret {
			t.Errorf("expected private_key_overrides_secret=true")
		}
	})

	t.Run("stored_secret_only_has_no_override", func(t *testing.T) {
		srv, _ := testServer(t)
		ctx := context.Background()

		if err := srv.setGitHubAppSecret(ctx, GitHubAppSecretPrivateKey, "stored-key", "test", "admin"); err != nil {
			t.Fatalf("failed to store secret: %v", err)
		}
		srv.mu.Lock()
		srv.config.GitHubAppConfig.AppID = 42
		srv.mu.Unlock()

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp GitHubAppConfigResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if !resp.HasPrivateKey || resp.PrivateKeySource != GitHubAppPrivateKeySourceSecret {
			t.Errorf("got has_private_key=%v source=%q, want true/%q", resp.HasPrivateKey, resp.PrivateKeySource, GitHubAppPrivateKeySourceSecret)
		}
		if resp.PrivateKeyOverridesSecret {
			t.Errorf("expected private_key_overrides_secret=false")
		}
	})

	t.Run("ui_upload_reports_secret_source_and_warns_if_file_path_set", func(t *testing.T) {
		srv, _ := testServer(t)
		srv.mu.Lock()
		srv.config.GitHubAppConfig.PrivateKeyPath = "/etc/ghapp/key.pem"
		srv.mu.Unlock()

		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		defer slog.SetDefault(prev)

		const uploadedKey = "UPLOADED_PEM_SECRET_DO_NOT_LOG"
		appID := int64(42)
		key := uploadedKey
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/github-app", GitHubAppConfigUpdateRequest{
			AppID:      &appID,
			PrivateKey: &key,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp GitHubAppConfigResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.PrivateKeySource != GitHubAppPrivateKeySourceSecret {
			t.Errorf("private_key_source = %q, want %q", resp.PrivateKeySource, GitHubAppPrivateKeySourceSecret)
		}
		if resp.PrivateKeyOverridesSecret {
			t.Errorf("expected private_key_overrides_secret=false while uploaded key is active in memory")
		}
		logged := buf.String()
		if !bytes.Contains([]byte(logged), []byte("take precedence after restart")) {
			t.Errorf("expected restart precedence warning log, got: %q", logged)
		}
		if bytes.Contains([]byte(logged), []byte(uploadedKey)) {
			t.Errorf("log must never contain private key material, got: %q", logged)
		}
	})
}
