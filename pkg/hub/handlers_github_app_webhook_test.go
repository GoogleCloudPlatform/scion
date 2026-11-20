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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func TestGitHubOwnerRepo(t *testing.T) {
	githubCom := &Server{}
	ghes := &Server{}
	ghes.config.GitHubAppConfig.APIBaseURL = "https://github.example.com/api/v3"

	tests := []struct {
		name     string
		srv      *Server
		remote   string
		expected string
	}{
		{"https github", githubCom, "https://github.com/acme/widgets.git", "acme/widgets"},
		{"https github no .git", githubCom, "https://github.com/acme/widgets", "acme/widgets"},
		{"https github trailing slash", githubCom, "https://github.com/acme/widgets/", "acme/widgets"},
		{"ssh github", githubCom, "git@github.com:acme/widgets.git", "acme/widgets"},
		{"ssh github no .git", githubCom, "git@github.com:acme/widgets", "acme/widgets"},
		{"http github", githubCom, "http://github.com/acme/widgets.git", "acme/widgets"},
		{"case preserved", githubCom, "https://GitHub.com/Acme/Widgets.git", "Acme/Widgets"},
		{"github enterprise https", ghes, "https://github.example.com/org/repo.git", "org/repo"},
		{"github enterprise ssh with port", ghes, "ssh://git@github.example.com:2222/org/repo.git", "org/repo"},
		{"ssh with port, other hostname", ghes, "ssh://git@gitlab.example.com:2222/org/repo.git", ""},
		{"ssh userinfo names the GHES host", ghes, "ssh://git@github.example.com@other.example/org/repo.git", ""},
		{"scp-form userinfo names the GHES host", ghes, "git@github.example.com@other.example:org/repo.git", ""},
		{"scp-form path is not owner/repo", ghes, "git@github.example.com:other", ""},
		{"https with a non-default port", ghes, "https://github.example.com:8443/org/repo.git", ""},
		{"empty", githubCom, "", ""},
		{"just hostname", githubCom, "github.com", ""},
		{"no repo", githubCom, "https://github.com/acme", ""},
		{"too many parts", githubCom, "https://github.com/a/b/c", ""},

		// Host match: only the configured GitHub host.
		{"other host", githubCom, "https://gitlab.com/acme/widgets.git", ""},
		{"other host ssh", githubCom, "git@gitlab.com:acme/widgets.git", ""},
		{"lookalike host", githubCom, "https://github.com.other.example/acme/widgets", ""},
		{"port is a different host", githubCom, "https://github.com:8443/acme/widgets", ""},
		{"userinfo before another host", githubCom, "https://github.com@other.example/acme/widgets", ""},
		{"github.com when GHES is configured", ghes, "https://github.com/acme/widgets.git", ""},
		{"GHES host when github.com is configured", githubCom, "https://github.example.com/org/repo.git", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.srv.githubOwnerRepo(tc.remote)
			if result != tc.expected {
				t.Errorf("githubOwnerRepo(%q) = %q, want %q", tc.remote, result, tc.expected)
			}
		})
	}
}

// signWebhookPayload creates a webhook signature for testing.
func signWebhookPayload(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return fmt.Sprintf("sha256=%x", mac.Sum(nil))
}

func TestHandleGitHubWebhook_Ping(t *testing.T) {
	srv, _ := webhookTestServer(t)

	payload := []byte(`{"zen":"Practicality beats purity."}`)
	sig := signWebhookPayload(payload, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", "test-delivery-1")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp["status"] != "pong" {
		t.Errorf("expected pong, got %s", resp["status"])
	}
}

func TestHandleGitHubWebhook_NoSecretConfigured_UnsignedRejected(t *testing.T) {
	// The capture must run before testServer: testServer's New() call binds
	// the Server's subsystem loggers (logging.Subsystem) to whatever
	// slog.Default() is at that moment, and that binding does not follow a
	// later slog.SetDefault swap. Capturing afterward could leave the
	// "secret lookup failed" check below unable to observe a log line
	// written through a subsystem logger, making the absence check vacuous.
	//
	// A "not configured" secret lookup (store.ErrNotFound) must not be treated
	// as a backend error: only the once-per-process "not configured" WARN
	// should fire, not the per-request "secret lookup failed" WARN.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	srv, s := testServer(t)
	ctx := context.Background()

	// Positive control: New() unconditionally logs an Info line during
	// construction, so the capture must have observed something. This
	// guards against a misrouted capture making the absence check below
	// pass vacuously.
	requireLogCaptureLive(t, &logBuf, serverConstructionLogLine)

	payload := mustJSON(t, map[string]interface{}{
		"action": "created",
		"installation": map[string]interface{}{
			"id":     987654321,
			"app_id": 42,
			"account": map[string]interface{}{
				"login": "example-org",
				"type":  "Organization",
			},
			"repository_selection": "all",
		},
		"repositories": []map[string]interface{}{
			{"id": 1, "full_name": "example-org/example-repo", "name": "example-repo"},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-GitHub-Delivery", "test-delivery-no-secret-1")
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no Authorization header and no X-Hub-Signature-256 header.

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}

	if _, err := s.GetGitHubInstallation(ctx, 987654321); err == nil {
		t.Fatal("expected no installation row to be created when no webhook secret is configured")
	}

	if strings.Contains(logBuf.String(), "secret lookup failed") {
		t.Errorf("expected no \"secret lookup failed\" WARN for a not-configured secret, got log: %s", logBuf.String())
	}
}

func TestHandleGitHubWebhook_NoSecretConfigured_BogusSignatureRejected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	payload := mustJSON(t, map[string]interface{}{
		"action": "created",
		"installation": map[string]interface{}{
			"id":     987654322,
			"app_id": 42,
			"account": map[string]interface{}{
				"login": "example-org",
				"type":  "Organization",
			},
			"repository_selection": "all",
		},
		"repositories": []map[string]interface{}{
			{"id": 1, "full_name": "example-org/example-repo", "name": "example-repo"},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-GitHub-Delivery", "test-delivery-no-secret-2")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}

	if _, err := s.GetGitHubInstallation(ctx, 987654322); err == nil {
		t.Fatal("expected no installation row to be created when no webhook secret is configured")
	}
}

func TestHandleGitHubWebhook_NoSecretConfigured_PingRejected(t *testing.T) {
	srv, _ := testServer(t)

	payload := []byte(`{"zen":"Practicality beats purity."}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", "test-delivery-no-secret-3")
	// No signature header at all — even ping must not be let through.

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleGitHubWebhook_SecretConfigured_ValidSignatureStillProcessed guards
// against regressing the existing behavior: when a secret IS configured and
// the signature is valid, the request must still be processed normally.
func TestHandleGitHubWebhook_SecretConfigured_ValidSignatureStillProcessed(t *testing.T) {
	srv, _ := webhookTestServer(t)

	payload := []byte(`{"zen":"Practicality beats purity."}`)
	sig := signWebhookPayload(payload, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", "test-delivery-valid-sig")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp["status"] != "pong" {
		t.Errorf("expected pong, got %s", resp["status"])
	}
}

// TestHandleGitHubWebhook_SecretConfigured_WrongSignatureRejected guards
// against regressing the existing behavior: when a secret IS configured, a
// wrong signature must still be rejected with 401.
func TestHandleGitHubWebhook_SecretConfigured_WrongSignatureRejected(t *testing.T) {
	srv, _ := webhookTestServer(t)

	payload := []byte(`{"action":"created"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-Hub-Signature-256", "sha256=badsignature")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleGitHubWebhook_InvalidSignature(t *testing.T) {
	srv, _ := webhookTestServer(t)

	payload := []byte(`{"action":"created"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-Hub-Signature-256", "sha256=badsignature")

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleGitHubWebhook_InstallationCreated(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// The project's creator proves access to the repository with their own
	// GitHub token; a repo-name match alone does not associate.
	giveUserGitHubToken(t, srv, s, "creator-1", "ghp_creator_1")
	fakeCreatorAccess(srv, map[string]githubapp.RepoAccess{
		"ghp_creator_1|acme/widgets": {Push: true, Pull: true},
	})

	// Create a project with a matching git remote
	project := &store.Project{
		ID:        tid("project-1"),
		Name:      "Test Project",
		Slug:      "test-project",
		GitRemote: "https://github.com/acme/widgets.git",
		CreatedBy: "creator-1",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	payload := mustJSON(t, map[string]interface{}{
		"action": "created",
		"installation": map[string]interface{}{
			"id":     12345,
			"app_id": 42,
			"account": map[string]interface{}{
				"login": "acme",
				"type":  "Organization",
			},
			"repository_selection": "selected",
		},
		"repositories": []map[string]interface{}{
			{"id": 1, "full_name": "acme/widgets", "name": "widgets"},
			{"id": 2, "full_name": "acme/api", "name": "api"},
		},
	})

	sig := signWebhookPayload(payload, "test-webhook-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify installation was recorded
	installation, err := s.GetGitHubInstallation(ctx, 12345)
	if err != nil {
		t.Fatalf("installation not found: %v", err)
	}
	if installation.AccountLogin != "acme" {
		t.Errorf("expected account acme, got %s", installation.AccountLogin)
	}
	if installation.Status != store.GitHubInstallationStatusActive {
		t.Errorf("expected active status, got %s", installation.Status)
	}

	// Verify project was auto-associated
	updatedProject, err := s.GetProject(ctx, tid("project-1"))
	if err != nil {
		t.Fatalf("failed to get project: %v", err)
	}
	if updatedProject.GitHubInstallationID == nil {
		t.Fatal("expected project to be associated with installation")
	}
	if *updatedProject.GitHubInstallationID != 12345 {
		t.Errorf("expected installation ID 12345, got %d", *updatedProject.GitHubInstallationID)
	}
	if updatedProject.GitHubAppStatus == nil {
		t.Fatal("expected project to have GitHub App status")
	}
	if updatedProject.GitHubAppStatus.State != store.GitHubAppStateUnchecked {
		t.Errorf("expected unchecked state, got %s", updatedProject.GitHubAppStatus.State)
	}
}

func TestHandleGitHubWebhook_InstallationDeleted(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// Pre-create installation
	installationID := int64(12345)
	installation := &store.GitHubInstallation{
		InstallationID: installationID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Status:         store.GitHubInstallationStatusActive,
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	// Create a project associated with the installation
	project := &store.Project{
		ID:                   tid("project-1"),
		Name:                 "Test Project",
		Slug:                 "test-project",
		GitRemote:            "https://github.com/acme/widgets.git",
		GitHubInstallationID: &installationID,
		GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: time.Now()},
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	payload := mustJSON(t, map[string]interface{}{
		"action": "deleted",
		"installation": map[string]interface{}{
			"id":     12345,
			"app_id": 42,
			"account": map[string]interface{}{
				"login": "acme",
				"type":  "Organization",
			},
		},
	})

	sig := signWebhookPayload(payload, "test-webhook-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Verify installation was marked deleted
	updated, _ := s.GetGitHubInstallation(ctx, 12345)
	if updated.Status != store.GitHubInstallationStatusDeleted {
		t.Errorf("expected deleted status, got %s", updated.Status)
	}

	// Verify project was set to error state
	updatedProject, _ := s.GetProject(ctx, tid("project-1"))
	if updatedProject.GitHubAppStatus == nil || updatedProject.GitHubAppStatus.State != store.GitHubAppStateError {
		t.Errorf("expected project error state, got %v", updatedProject.GitHubAppStatus)
	}
}

func TestHandleGitHubWebhook_InstallationReposRemoved(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	installationID := int64(12345)
	installation := &store.GitHubInstallation{
		InstallationID: installationID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets", "acme/api"},
		Status:         store.GitHubInstallationStatusActive,
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                   tid("project-1"),
		Name:                 "Test Project",
		Slug:                 "test-project",
		GitRemote:            "https://github.com/acme/widgets.git",
		GitHubInstallationID: &installationID,
		GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: time.Now()},
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	payload := mustJSON(t, map[string]interface{}{
		"action":       "removed",
		"installation": map[string]interface{}{"id": 12345},
		"repositories_removed": []map[string]interface{}{
			{"id": 1, "full_name": "acme/widgets", "name": "widgets"},
		},
		"repositories_added": []map[string]interface{}{},
	})

	sig := signWebhookPayload(payload, "test-webhook-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation_repositories")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Verify repo was removed from installation
	updated, _ := s.GetGitHubInstallation(ctx, 12345)
	if len(updated.Repositories) != 1 || updated.Repositories[0] != "acme/api" {
		t.Errorf("expected [acme/api], got %v", updated.Repositories)
	}

	// Verify project was set to error
	updatedProject, _ := s.GetProject(ctx, tid("project-1"))
	if updatedProject.GitHubAppStatus == nil || updatedProject.GitHubAppStatus.State != store.GitHubAppStateError {
		t.Errorf("expected error state, got %v", updatedProject.GitHubAppStatus)
	}
	if updatedProject.GitHubAppStatus.ErrorCode != "repo_not_accessible" {
		t.Errorf("expected repo_not_accessible error code, got %s", updatedProject.GitHubAppStatus.ErrorCode)
	}
}

// TestHandleGitHubWebhook_InstallationReposRemoved_CaseInsensitive covers
// ptone/scion#7548: GitHub owner and repo names are case-insensitive, so a
// removed repository must match the installation's repo list and the
// project's git remote regardless of letter case.
func TestHandleGitHubWebhook_InstallationReposRemoved_CaseInsensitive(t *testing.T) {
	cases := []struct {
		name        string
		storedRepos []string
		gitRemote   string
		removed     string
	}{
		{"mixed-case remote, lowercase payload", []string{"acme/widgets", "acme/api"}, "https://github.com/Acme/Widgets.git", "acme/widgets"},
		{"lowercase remote, mixed-case payload", []string{"Acme/Widgets", "acme/api"}, "https://github.com/acme/widgets.git", "Acme/Widgets"},
		{"stored list differs from payload", []string{"ACME/WIDGETS", "acme/api"}, "git@github.com:aCmE/wIdGeTs.git", "Acme/Widgets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := webhookTestServer(t)
			ctx := context.Background()

			installationID := int64(12345)
			if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
				InstallationID: installationID,
				AccountLogin:   "acme",
				AccountType:    "Organization",
				AppID:          42,
				Repositories:   tc.storedRepos,
				Status:         store.GitHubInstallationStatusActive,
			}); err != nil {
				t.Fatalf("failed to create installation: %v", err)
			}
			if err := s.CreateProject(ctx, &store.Project{
				ID:                   tid("project-1"),
				Name:                 "Test Project",
				Slug:                 "test-project",
				GitRemote:            tc.gitRemote,
				GitHubInstallationID: &installationID,
				GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: time.Now()},
				Created:              time.Now(),
				Updated:              time.Now(),
			}); err != nil {
				t.Fatalf("failed to create project: %v", err)
			}

			payload := mustJSON(t, map[string]interface{}{
				"action":       "removed",
				"installation": map[string]interface{}{"id": installationID},
				"repositories_removed": []map[string]interface{}{
					{"id": 1, "full_name": tc.removed, "name": "widgets"},
				},
				"repositories_added": []map[string]interface{}{},
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
			req.Header.Set("X-GitHub-Event", "installation_repositories")
			req.Header.Set("X-Hub-Signature-256", signWebhookPayload(payload, "test-webhook-secret"))
			rec := httptest.NewRecorder()
			srv.handleGitHubWebhook(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}

			updated, err := s.GetGitHubInstallation(ctx, installationID)
			if err != nil {
				t.Fatalf("failed to get installation: %v", err)
			}
			if len(updated.Repositories) != 1 || updated.Repositories[0] != "acme/api" {
				t.Errorf("expected [acme/api], got %v", updated.Repositories)
			}

			updatedProject, err := s.GetProject(ctx, tid("project-1"))
			if err != nil {
				t.Fatalf("failed to get project: %v", err)
			}
			if updatedProject.GitHubAppStatus == nil || updatedProject.GitHubAppStatus.State != store.GitHubAppStateError {
				t.Fatalf("expected error state, got %v", updatedProject.GitHubAppStatus)
			}
			if updatedProject.GitHubAppStatus.ErrorCode != githubapp.ErrCodeRepoNotAccessible {
				t.Errorf("expected %s error code, got %s", githubapp.ErrCodeRepoNotAccessible, updatedProject.GitHubAppStatus.ErrorCode)
			}
		})
	}
}

func TestHandleGitHubWebhook_IgnoredEvent(t *testing.T) {
	srv, _ := webhookTestServer(t)

	payload := []byte(`{"action":"completed"}`)
	sig := signWebhookPayload(payload, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "check_run")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var resp map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp["status"] != "ignored" {
		t.Errorf("expected ignored status, got %s", resp["status"])
	}
}

func TestMatchProjectsToInstallation(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// The creator proves access to every repository with their own token.
	giveUserGitHubToken(t, srv, s, "creator-g", "ghp_creator_g")
	fakeCreatorAccess(srv, map[string]githubapp.RepoAccess{
		"ghp_creator_g|acme/widgets": {Push: true, Pull: true},
		"ghp_creator_g|acme/api":     {Push: true, Pull: true},
		"ghp_creator_g|other/repo":   {Push: true, Pull: true},
	})

	// Create projects with different git remotes
	projects := []*store.Project{
		{ID: tid("g1"), Name: "G1", Slug: tid("g1"), GitRemote: "https://github.com/acme/widgets.git", CreatedBy: "creator-g", Created: time.Now(), Updated: time.Now()},
		{ID: tid("g2"), Name: "G2", Slug: tid("g2"), GitRemote: "https://github.com/acme/api.git", CreatedBy: "creator-g", Created: time.Now(), Updated: time.Now()},
		{ID: tid("g3"), Name: "G3", Slug: tid("g3"), GitRemote: "https://github.com/other/repo.git", CreatedBy: "creator-g", Created: time.Now(), Updated: time.Now()},
		{ID: tid("g4"), Name: "G4", Slug: tid("g4"), Created: time.Now(), Updated: time.Now()}, // No git remote
	}

	for _, g := range projects {
		if err := s.CreateProject(ctx, g); err != nil {
			t.Fatalf("failed to create project %s: %v", g.ID, err)
		}
	}

	installation := &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "acme",
		Repositories:   []string{"acme/widgets", "acme/api"},
		Status:         store.GitHubInstallationStatusActive,
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	matched := srv.matchProjectsToInstallation(ctx, installation)

	if len(matched) != 2 {
		t.Fatalf("expected 2 matched projects, got %d: %v", len(matched), matched)
	}

	// Verify both matching projects were associated
	for _, gID := range []string{tid("g1"), tid("g2")} {
		project, _ := s.GetProject(ctx, gID)
		if project.GitHubInstallationID == nil {
			t.Errorf("project %s should be associated with installation", gID)
		} else if *project.GitHubInstallationID != 12345 {
			t.Errorf("project %s has wrong installation ID: %d", gID, *project.GitHubInstallationID)
		}
	}

	// Verify non-matching project was NOT associated
	g3, _ := s.GetProject(ctx, tid("g3"))
	if g3.GitHubInstallationID != nil {
		t.Error("project g3 should not be associated")
	}

	// Verify no-remote project was NOT associated
	g4, _ := s.GetProject(ctx, tid("g4"))
	if g4.GitHubInstallationID != nil {
		t.Error("project g4 should not be associated")
	}
}

func TestMatchProjectsToInstallation_SkipsAlreadyAssociated(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	otherInstallation := int64(99999)

	// Create the referenced installation so the project FK is satisfied
	if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: otherInstallation,
		AccountLogin:   "other-org",
		Status:         store.GitHubInstallationStatusActive,
	}); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                   tid("g1"),
		Name:                 "G1",
		Slug:                 tid("g1"),
		GitRemote:            "https://github.com/acme/widgets.git",
		GitHubInstallationID: &otherInstallation,
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: 12345,
		Repositories:   []string{"acme/widgets"},
	}

	matched := srv.matchProjectsToInstallation(ctx, installation)
	if len(matched) != 0 {
		t.Errorf("expected 0 matched projects (already associated), got %d", len(matched))
	}

	// Verify project still has the original installation
	updatedProject, _ := s.GetProject(ctx, tid("g1"))
	if *updatedProject.GitHubInstallationID != 99999 {
		t.Errorf("project should still have original installation")
	}
}

func TestHandleGitHubWebhook_MethodNotAllowed(t *testing.T) {
	srv, _ := webhookTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/github", nil)
	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestHandleGitHubWebhook_InstallationCreatedIdempotent(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// Pre-create the installation
	existing := &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Status:         store.GitHubInstallationStatusActive,
	}
	if err := s.CreateGitHubInstallation(ctx, existing); err != nil {
		t.Fatalf("failed to pre-create installation: %v", err)
	}

	payload := mustJSON(t, map[string]interface{}{
		"action": "created",
		"installation": map[string]interface{}{
			"id":     12345,
			"app_id": 42,
			"account": map[string]interface{}{
				"login": "acme",
				"type":  "Organization",
			},
		},
		"repositories": []map[string]interface{}{},
	})

	sig := signWebhookPayload(payload, "test-webhook-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	// Should succeed (idempotent)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent), got %d: %s", rec.Code, rec.Body.String())
	}

	// Installation should still exist
	inst, err := s.GetGitHubInstallation(ctx, 12345)
	if err != nil {
		t.Fatalf("installation not found after idempotent create: %v", err)
	}
	if inst.Status != store.GitHubInstallationStatusActive {
		t.Errorf("expected active, got %s", inst.Status)
	}
}

// recordingEventPublisher records calls to PublishProjectUpdated for test assertions.
type recordingEventPublisher struct {
	noopEventPublisher
	mu             sync.Mutex
	projectUpdates []*store.Project
}

func (r *recordingEventPublisher) PublishProjectUpdated(_ context.Context, project *store.Project) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.projectUpdates = append(r.projectUpdates, project)
}

func (r *recordingEventPublisher) getProjectUpdates() []*store.Project {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]*store.Project, len(r.projectUpdates))
	copy(result, r.projectUpdates)
	return result
}

func TestWebhook_PublishesProjectUpdatedOnInstallationDeleted(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// Replace the event publisher with a recording one
	recorder := &recordingEventPublisher{}
	srv.events = recorder

	// Pre-create installation
	installationID := int64(12345)
	installation := &store.GitHubInstallation{
		InstallationID: installationID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Status:         store.GitHubInstallationStatusActive,
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	// Create a project associated with the installation
	project := &store.Project{
		ID:                   tid("project-event-1"),
		Name:                 "Event Test Project",
		Slug:                 "event-test-project",
		GitRemote:            "https://github.com/acme/widgets.git",
		GitHubInstallationID: &installationID,
		GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: time.Now()},
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	payload := mustJSON(t, map[string]interface{}{
		"action": "deleted",
		"installation": map[string]interface{}{
			"id":     12345,
			"app_id": 42,
			"account": map[string]interface{}{
				"login": "acme",
				"type":  "Organization",
			},
		},
	})

	sig := signWebhookPayload(payload, "test-webhook-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Verify PublishProjectUpdated was called
	updates := recorder.getProjectUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 project updated event, got %d", len(updates))
	}
	if updates[0].ID != tid("project-event-1") {
		t.Errorf("expected project ID project-event-1, got %s", updates[0].ID)
	}
	if updates[0].GitHubAppStatus == nil || updates[0].GitHubAppStatus.State != store.GitHubAppStateError {
		t.Errorf("expected error state in published event, got %v", updates[0].GitHubAppStatus)
	}
}

func TestWebhook_PublishesProjectUpdatedOnRepoRemoved(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	recorder := &recordingEventPublisher{}
	srv.events = recorder

	installationID := int64(12345)
	installation := &store.GitHubInstallation{
		InstallationID: installationID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets", "acme/api"},
		Status:         store.GitHubInstallationStatusActive,
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                   tid("project-event-2"),
		Name:                 "Event Test Project 2",
		Slug:                 "event-test-project-2",
		GitRemote:            "https://github.com/acme/widgets.git",
		GitHubInstallationID: &installationID,
		GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: time.Now()},
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	payload := mustJSON(t, map[string]interface{}{
		"action":       "removed",
		"installation": map[string]interface{}{"id": 12345},
		"repositories_removed": []map[string]interface{}{
			{"id": 1, "full_name": "acme/widgets", "name": "widgets"},
		},
		"repositories_added": []map[string]interface{}{},
	})

	sig := signWebhookPayload(payload, "test-webhook-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation_repositories")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	updates := recorder.getProjectUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 project updated event, got %d", len(updates))
	}
	if updates[0].ID != tid("project-event-2") {
		t.Errorf("expected project ID project-event-2, got %s", updates[0].ID)
	}
	if updates[0].GitHubAppStatus == nil || updates[0].GitHubAppStatus.State != store.GitHubAppStateError {
		t.Errorf("expected error state in published event, got %v", updates[0].GitHubAppStatus)
	}
}

func TestWebhook_PublishesProjectUpdatedOnAutoMatch(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	recorder := &recordingEventPublisher{}
	srv.events = recorder

	giveUserGitHubToken(t, srv, s, "creator-3", "ghp_creator_3")
	fakeCreatorAccess(srv, map[string]githubapp.RepoAccess{
		"ghp_creator_3|acme/widgets": {Push: true, Pull: true},
	})

	// Create a project with a matching git remote but no installation yet
	project := &store.Project{
		ID:        tid("project-event-3"),
		Name:      "Event Test Project 3",
		Slug:      "event-test-project-3",
		CreatedBy: "creator-3",
		GitRemote: "https://github.com/acme/widgets.git",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	payload := mustJSON(t, map[string]interface{}{
		"action": "created",
		"installation": map[string]interface{}{
			"id":     12345,
			"app_id": 42,
			"account": map[string]interface{}{
				"login": "acme",
				"type":  "Organization",
			},
			"repository_selection": "selected",
		},
		"repositories": []map[string]interface{}{
			{"id": 1, "full_name": "acme/widgets", "name": "widgets"},
		},
	})

	sig := signWebhookPayload(payload, "test-webhook-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	updates := recorder.getProjectUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 project updated event from auto-match, got %d", len(updates))
	}
	if updates[0].ID != tid("project-event-3") {
		t.Errorf("expected project ID project-event-3, got %s", updates[0].ID)
	}
	if updates[0].GitHubInstallationID == nil || *updates[0].GitHubInstallationID != 12345 {
		t.Error("expected project to be associated with installation 12345")
	}
}

// pagedListProjectsStore wraps store.Store and caps every ListProjects call to
// pageSize items while its fault switch is armed, simulating the store's page
// cap across multiple cursor pages without inserting thousands of rows.
type pagedListProjectsStore struct {
	store.Store
	fault    *storeFaultSwitch
	pageSize int

	mu    sync.Mutex
	calls []store.ListOptions
}

func (p *pagedListProjectsStore) ListProjects(ctx context.Context, filter store.ProjectFilter, opts store.ListOptions) (*store.ListResult[store.Project], error) {
	if !p.fault.Active() {
		return p.Store.ListProjects(ctx, filter, opts)
	}
	p.mu.Lock()
	p.calls = append(p.calls, opts)
	p.mu.Unlock()

	capped := opts
	if p.pageSize > 0 && (capped.Limit <= 0 || capped.Limit > p.pageSize) {
		capped.Limit = p.pageSize
	}
	return p.Store.ListProjects(ctx, filter, capped)
}

func (p *pagedListProjectsStore) recordedCalls() []store.ListOptions {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]store.ListOptions, len(p.calls))
	copy(out, p.calls)
	return out
}

func assertCursorPaginationCalls(t *testing.T, calls []store.ListOptions, minPages int) {
	t.Helper()
	if len(calls) < minPages {
		t.Fatalf("expected at least %d ListProjects page calls, got %d: %+v", minPages, len(calls), calls)
	}
	if calls[0].Cursor != "" {
		t.Errorf("expected first ListProjects call to have empty cursor, got %q", calls[0].Cursor)
	}
	for i, c := range calls {
		if c.Limit <= 0 || c.Limit > 1000 {
			t.Errorf("call %d requested Limit=%d, want 1..1000", i, c.Limit)
		}
		if !c.SkipTotalCount {
			t.Errorf("call %d did not set SkipTotalCount=true", i)
		}
		if i > 0 && c.Cursor == "" {
			t.Errorf("call %d had empty Cursor, expected pagination cursor", i)
		}
	}
}

// scriptedListProjectsStore is a pure fake store returning scripted ListProjects
// pages (and/or errors) without touching a database.
type scriptedListProjectsStore struct {
	store.Store
	pages []func(opts store.ListOptions) (*store.ListResult[store.Project], error)
	calls []store.ListOptions
}

func (f *scriptedListProjectsStore) ListProjects(_ context.Context, _ store.ProjectFilter, opts store.ListOptions) (*store.ListResult[store.Project], error) {
	idx := len(f.calls)
	f.calls = append(f.calls, opts)
	if idx >= len(f.pages) {
		return &store.ListResult[store.Project]{}, nil
	}
	return f.pages[idx](opts)
}

func TestForEachGitHubAppProject_FakeStore(t *testing.T) {
	t.Run("follows cursors across pages until exhausted", func(t *testing.T) {
		fake := &scriptedListProjectsStore{
			pages: []func(store.ListOptions) (*store.ListResult[store.Project], error){
				func(opts store.ListOptions) (*store.ListResult[store.Project], error) {
					if opts.Cursor != "" {
						t.Errorf("page 1 cursor = %q, want empty", opts.Cursor)
					}
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p1"}, {ID: "p2"}},
						NextCursor: "cursor-1",
					}, nil
				},
				func(opts store.ListOptions) (*store.ListResult[store.Project], error) {
					if opts.Cursor != "cursor-1" {
						t.Errorf("page 2 cursor = %q, want cursor-1", opts.Cursor)
					}
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p3"}, {ID: "p4"}},
						NextCursor: "cursor-2",
					}, nil
				},
				func(opts store.ListOptions) (*store.ListResult[store.Project], error) {
					if opts.Cursor != "cursor-2" {
						t.Errorf("page 3 cursor = %q, want cursor-2", opts.Cursor)
					}
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p5"}},
						NextCursor: "",
					}, nil
				},
			},
		}
		testSrv := &Server{store: fake}
		var visited []string
		if err := testSrv.forEachGitHubAppProject(context.Background(), func(p *store.Project) {
			visited = append(visited, p.ID)
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"p1", "p2", "p3", "p4", "p5"}
		if strings.Join(visited, ",") != strings.Join(want, ",") {
			t.Fatalf("visited = %v, want %v", visited, want)
		}
		assertCursorPaginationCalls(t, fake.calls, 3)
	})

	t.Run("propagates error on subsequent page", func(t *testing.T) {
		boom := errors.New("page 2 query failed")
		fake := &scriptedListProjectsStore{
			pages: []func(store.ListOptions) (*store.ListResult[store.Project], error){
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p1"}},
						NextCursor: "cursor-1",
					}, nil
				},
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return nil, boom
				},
			},
		}
		testSrv := &Server{store: fake}
		var visited []string
		err := testSrv.forEachGitHubAppProject(context.Background(), func(p *store.Project) {
			visited = append(visited, p.ID)
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		if len(visited) != 1 || visited[0] != "p1" {
			t.Fatalf("visited = %v, want [p1]", visited)
		}
	})

	t.Run("terminates on stagnant cursor or empty page", func(t *testing.T) {
		fake := &scriptedListProjectsStore{
			pages: []func(store.ListOptions) (*store.ListResult[store.Project], error){
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p1"}},
						NextCursor: "stuck",
					}, nil
				},
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p2"}},
						NextCursor: "stuck",
					}, nil
				},
			},
		}
		testSrv := &Server{store: fake}
		var visited []string
		if err := testSrv.forEachGitHubAppProject(context.Background(), func(p *store.Project) {
			visited = append(visited, p.ID)
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(fake.calls) != 2 {
			t.Fatalf("expected loop to stop on repeated cursor after 2 calls, got %d", len(fake.calls))
		}
		if strings.Join(visited, ",") != "p1,p2" {
			t.Fatalf("visited = %v, want [p1 p2]", visited)
		}
	})
}

func TestGitHubAppHandlers_PaginateProjects(t *testing.T) {
	oldBase := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	t.Run("matchProjectsToInstallation", func(t *testing.T) {
		srv, s := webhookTestServer(t)
		paged, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *pagedListProjectsStore {
			return &pagedListProjectsStore{Store: inner, fault: f, pageSize: 2}
		})
		ctx := context.Background()

		giveUserGitHubToken(t, srv, s, "creator-paged", "ghp_creator_paged")
		fakeCreatorAccess(srv, map[string]githubapp.RepoAccess{
			"ghp_creator_paged|acme/widgets": {Push: true, Pull: true},
		})

		newestTime := time.Now().UTC().Add(10 * time.Minute)
		fixtures := []*store.Project{
			{
				ID: tid("match-oldest"), Name: "Match Oldest", Slug: tid("match-oldest"),
				GitRemote: "https://github.com/acme/widgets.git", CreatedBy: "creator-paged",
				Created: oldBase, Updated: oldBase,
			},
			{
				ID: tid("match-filler-1"), Name: "Filler 1", Slug: tid("match-filler-1"),
				GitRemote: "https://github.com/other/repo.git", CreatedBy: "creator-paged",
				Created: oldBase.Add(1 * time.Minute), Updated: oldBase.Add(1 * time.Minute),
			},
			{
				ID: tid("match-filler-2"), Name: "Filler 2", Slug: tid("match-filler-2"),
				GitRemote: "https://github.com/other/repo.git", CreatedBy: "creator-paged",
				Created: oldBase.Add(2 * time.Minute), Updated: oldBase.Add(2 * time.Minute),
			},
			{
				ID: tid("match-filler-3"), Name: "Filler 3", Slug: tid("match-filler-3"),
				Created: oldBase.Add(3 * time.Minute), Updated: oldBase.Add(3 * time.Minute),
			},
			{
				ID: tid("match-newest"), Name: "Match Newest", Slug: tid("match-newest"),
				GitRemote: "https://github.com/acme/widgets.git", CreatedBy: "creator-paged",
				Created: newestTime, Updated: newestTime,
			},
		}
		for _, p := range fixtures {
			if err := s.CreateProject(ctx, p); err != nil {
				t.Fatalf("CreateProject(%s): %v", p.ID, err)
			}
		}

		inst := &store.GitHubInstallation{
			InstallationID: 31001,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          42,
			Repositories:   []string{"acme/widgets"},
			Status:         store.GitHubInstallationStatusActive,
		}
		if err := s.CreateGitHubInstallation(ctx, inst); err != nil {
			t.Fatalf("CreateGitHubInstallation: %v", err)
		}

		fault.Arm()
		matched := srv.matchProjectsToInstallation(ctx, inst)

		if len(matched) != 2 {
			t.Fatalf("expected 2 matched projects across pages, got %d: %v", len(matched), matched)
		}
		assertCursorPaginationCalls(t, paged.recordedCalls(), 3)

		for _, id := range []string{tid("match-newest"), tid("match-oldest")} {
			got, err := s.GetProject(ctx, id)
			if err != nil {
				t.Fatalf("GetProject(%s): %v", id, err)
			}
			if got.GitHubInstallationID == nil || *got.GitHubInstallationID != inst.InstallationID {
				t.Errorf("project %s installation = %v, want %d", id, got.GitHubInstallationID, inst.InstallationID)
			}
		}
	})

	t.Run("updateProjectsForInstallation", func(t *testing.T) {
		srv, s := webhookTestServer(t)
		paged, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *pagedListProjectsStore {
			return &pagedListProjectsStore{Store: inner, fault: f, pageSize: 2}
		})
		ctx := context.Background()

		instID := int64(31002)
		if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: instID,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          42,
			Status:         store.GitHubInstallationStatusActive,
		}); err != nil {
			t.Fatalf("CreateGitHubInstallation: %v", err)
		}

		newestTime := time.Now().UTC().Add(10 * time.Minute)
		fixtures := []*store.Project{
			{
				ID: tid("upd-oldest"), Name: "Update Oldest", Slug: tid("upd-oldest"),
				GitRemote: "https://github.com/acme/widgets.git", GitHubInstallationID: &instID,
				GitHubAppStatus: &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: oldBase},
				Created:         oldBase, Updated: oldBase,
			},
			{
				ID: tid("upd-filler-1"), Name: "Filler 1", Slug: tid("upd-filler-1"),
				Created: oldBase.Add(1 * time.Minute), Updated: oldBase.Add(1 * time.Minute),
			},
			{
				ID: tid("upd-filler-2"), Name: "Filler 2", Slug: tid("upd-filler-2"),
				Created: oldBase.Add(2 * time.Minute), Updated: oldBase.Add(2 * time.Minute),
			},
			{
				ID: tid("upd-filler-3"), Name: "Filler 3", Slug: tid("upd-filler-3"),
				Created: oldBase.Add(3 * time.Minute), Updated: oldBase.Add(3 * time.Minute),
			},
			{
				ID: tid("upd-newest"), Name: "Update Newest", Slug: tid("upd-newest"),
				GitRemote: "https://github.com/acme/widgets.git", GitHubInstallationID: &instID,
				GitHubAppStatus: &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: oldBase},
				Created:         newestTime, Updated: newestTime,
			},
		}
		for _, p := range fixtures {
			if err := s.CreateProject(ctx, p); err != nil {
				t.Fatalf("CreateProject(%s): %v", p.ID, err)
			}
		}

		fault.Arm()
		payload := mustJSON(t, map[string]any{
			"action": "deleted",
			"installation": map[string]any{
				"id":     instID,
				"app_id": 42,
				"account": map[string]any{
					"login": "acme",
					"type":  "Organization",
				},
			},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Event", "installation")
		req.Header.Set("X-Hub-Signature-256", signWebhookPayload(payload, "test-webhook-secret"))
		rec := httptest.NewRecorder()
		srv.handleGitHubWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook returned %d: %s", rec.Code, rec.Body.String())
		}

		assertCursorPaginationCalls(t, paged.recordedCalls(), 3)

		for _, id := range []string{tid("upd-newest"), tid("upd-oldest")} {
			got, err := s.GetProject(ctx, id)
			if err != nil {
				t.Fatalf("GetProject(%s): %v", id, err)
			}
			if got.GitHubAppStatus == nil || got.GitHubAppStatus.State != store.GitHubAppStateError {
				t.Errorf("project %s status = %+v, want error state", id, got.GitHubAppStatus)
			}
		}
	})

	t.Run("checkProjectsForRemovedRepos", func(t *testing.T) {
		srv, s := webhookTestServer(t)
		paged, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *pagedListProjectsStore {
			return &pagedListProjectsStore{Store: inner, fault: f, pageSize: 2}
		})
		ctx := context.Background()

		instID := int64(31003)
		if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: instID,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          42,
			Repositories:   []string{"acme/widgets", "acme/api"},
			Status:         store.GitHubInstallationStatusActive,
		}); err != nil {
			t.Fatalf("CreateGitHubInstallation: %v", err)
		}

		newestTime := time.Now().UTC().Add(10 * time.Minute)
		fixtures := []*store.Project{
			{
				ID: tid("rem-oldest"), Name: "Removed Oldest", Slug: tid("rem-oldest"),
				GitRemote: "https://github.com/acme/widgets.git", GitHubInstallationID: &instID,
				GitHubAppStatus: &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: oldBase},
				Created:         oldBase, Updated: oldBase,
			},
			{
				ID: tid("rem-filler-1"), Name: "Filler 1", Slug: tid("rem-filler-1"),
				Created: oldBase.Add(1 * time.Minute), Updated: oldBase.Add(1 * time.Minute),
			},
			{
				ID: tid("rem-filler-2"), Name: "Filler 2", Slug: tid("rem-filler-2"),
				Created: oldBase.Add(2 * time.Minute), Updated: oldBase.Add(2 * time.Minute),
			},
			{
				ID: tid("rem-filler-3"), Name: "Filler 3", Slug: tid("rem-filler-3"),
				GitRemote: "https://github.com/acme/api.git", GitHubInstallationID: &instID,
				GitHubAppStatus: &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: oldBase},
				Created:         oldBase.Add(3 * time.Minute), Updated: oldBase.Add(3 * time.Minute),
			},
			{
				ID: tid("rem-newest"), Name: "Removed Newest", Slug: tid("rem-newest"),
				GitRemote: "https://github.com/acme/widgets.git", GitHubInstallationID: &instID,
				GitHubAppStatus: &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: oldBase},
				Created:         newestTime, Updated: newestTime,
			},
		}
		for _, p := range fixtures {
			if err := s.CreateProject(ctx, p); err != nil {
				t.Fatalf("CreateProject(%s): %v", p.ID, err)
			}
		}

		fault.Arm()
		payload := mustJSON(t, map[string]any{
			"action":       "removed",
			"installation": map[string]any{"id": instID},
			"repositories_removed": []map[string]any{
				{"id": 1, "full_name": "acme/widgets", "name": "widgets"},
			},
			"repositories_added": []map[string]any{},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Event", "installation_repositories")
		req.Header.Set("X-Hub-Signature-256", signWebhookPayload(payload, "test-webhook-secret"))
		rec := httptest.NewRecorder()
		srv.handleGitHubWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook returned %d: %s", rec.Code, rec.Body.String())
		}

		assertCursorPaginationCalls(t, paged.recordedCalls(), 3)

		for _, id := range []string{tid("rem-newest"), tid("rem-oldest")} {
			got, err := s.GetProject(ctx, id)
			if err != nil {
				t.Fatalf("GetProject(%s): %v", id, err)
			}
			if got.GitHubAppStatus == nil ||
				got.GitHubAppStatus.State != store.GitHubAppStateError ||
				got.GitHubAppStatus.ErrorCode != githubapp.ErrCodeRepoNotAccessible {
				t.Errorf("project %s status = %+v, want error/%s", id, got.GitHubAppStatus, githubapp.ErrCodeRepoNotAccessible)
			}
		}
	})

	t.Run("handleSyncAppPermissions", func(t *testing.T) {
		mockGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
			"id":     42,
			"slug":   "scion-app",
			"events": []string{},
			"permissions": map[string]string{
				"contents": "read",
				"metadata": "read",
			},
		})

		srv, s := webhookTestServer(t)
		paged, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *pagedListProjectsStore {
			return &pagedListProjectsStore{Store: inner, fault: f, pageSize: 2}
		})
		srv.mu.Lock()
		srv.config.GitHubAppConfig.PrivateKey = testGitHubAppPrivateKeyPEM(t)
		srv.config.GitHubAppConfig.APIBaseURL = mockGH.server.URL
		srv.mu.Unlock()

		ctx := context.Background()
		instID := int64(31004)
		if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: instID,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          42,
			Status:         store.GitHubInstallationStatusActive,
		}); err != nil {
			t.Fatalf("CreateGitHubInstallation: %v", err)
		}

		newestTime := time.Now().UTC().Add(10 * time.Minute)
		fixtures := []*store.Project{
			{
				ID: tid("sync-oldest"), Name: "Sync Oldest", Slug: tid("sync-oldest"),
				GitRemote: "https://github.com/acme/widgets.git", GitHubInstallationID: &instID,
				GitHubPermissions: &store.GitHubTokenPermissions{Contents: "write", Metadata: "read"},
				GitHubAppStatus:   &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: oldBase},
				Created:           oldBase, Updated: oldBase,
			},
			{
				ID: tid("sync-filler-1"), Name: "Filler 1", Slug: tid("sync-filler-1"),
				Created: oldBase.Add(1 * time.Minute), Updated: oldBase.Add(1 * time.Minute),
			},
			{
				ID: tid("sync-filler-2"), Name: "Filler 2", Slug: tid("sync-filler-2"),
				GitRemote: "https://github.com/acme/widgets.git", GitHubInstallationID: &instID,
				GitHubPermissions: &store.GitHubTokenPermissions{Contents: "read", Metadata: "read"},
				Created:           oldBase.Add(2 * time.Minute), Updated: oldBase.Add(2 * time.Minute),
			},
			{
				ID: tid("sync-filler-3"), Name: "Filler 3", Slug: tid("sync-filler-3"),
				Created: oldBase.Add(3 * time.Minute), Updated: oldBase.Add(3 * time.Minute),
			},
			{
				ID: tid("sync-newest"), Name: "Sync Newest", Slug: tid("sync-newest"),
				GitRemote: "https://github.com/acme/widgets.git", GitHubInstallationID: &instID,
				GitHubPermissions: &store.GitHubTokenPermissions{Contents: "write", Metadata: "read"},
				GitHubAppStatus:   &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: oldBase},
				Created:           newestTime, Updated: newestTime,
			},
		}
		for _, p := range fixtures {
			if err := s.CreateProject(ctx, p); err != nil {
				t.Fatalf("CreateProject(%s): %v", p.ID, err)
			}
		}

		fault.Arm()
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/github-app/sync-permissions", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("sync-permissions returned %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			AffectedCount    int              `json:"affected_count"`
			AffectedProjects []map[string]any `json:"affected_projects"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.AffectedCount != 2 {
			t.Fatalf("affected_count = %d, want 2 (body: %+v)", resp.AffectedCount, resp)
		}

		assertCursorPaginationCalls(t, paged.recordedCalls(), 3)

		for _, id := range []string{tid("sync-newest"), tid("sync-oldest")} {
			got, err := s.GetProject(ctx, id)
			if err != nil {
				t.Fatalf("GetProject(%s): %v", id, err)
			}
			if got.GitHubAppStatus == nil || got.GitHubAppStatus.State != store.GitHubAppStateDegraded {
				t.Errorf("project %s status = %+v, want degraded", id, got.GitHubAppStatus)
			}
		}
	})
}

func TestHandleGitHubWebhook_InstallationReposRemoved_PausesGitHubTriggers(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	instID := int64(12345)
	otherInstID := int64(99999)

	if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"Acme/Widgets", "acme/api"},
		Status:         store.GitHubInstallationStatusActive,
	}); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}
	if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: otherInstID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
	}); err != nil {
		t.Fatalf("failed to create other installation: %v", err)
	}

	projAffectedA := tid("repo-rem-aff-a")
	projAffectedB := tid("repo-rem-aff-b")
	projOtherRepo := tid("repo-rem-other-repo")
	projOtherInst := tid("repo-rem-other-inst")

	now := time.Now().UTC()
	for _, p := range []*store.Project{
		{
			ID:                   projAffectedA,
			Name:                 "Affected A",
			Slug:                 "affected-a",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &instID,
			GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: now},
			Created:              now,
			Updated:              now,
		},
		{
			ID:                   projAffectedB,
			Name:                 "Affected B",
			Slug:                 "affected-b",
			GitRemote:            "git@github.com:ACME/WIDGETS.git", // case-insensitive match
			GitHubInstallationID: &instID,
			GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: now},
			Created:              now,
			Updated:              now,
		},
		{
			ID:                   projOtherRepo,
			Name:                 "Other Repo",
			Slug:                 "other-repo",
			GitRemote:            "https://github.com/acme/api.git",
			GitHubInstallationID: &instID,
			GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: now},
			Created:              now,
			Updated:              now,
		},
		{
			ID:                   projOtherInst,
			Name:                 "Other Installation",
			Slug:                 "other-inst",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &otherInstID,
			GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: now},
			Created:              now,
			Updated:              now,
		},
	} {
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatalf("CreateProject(%s): %v", p.ID, err)
		}
	}

	// Set a primary routing entry on instID for acme/widgets; §8.4 requires
	// installation_repositories.removed NOT to edit trigger_routing.
	if err := s.SetGitHubInstallationTriggerRoute(ctx, instID, "acme/widgets", store.GitHubTriggerRoute{
		PrimaryProjectID: projAffectedA,
		SetBy:            "admin-1",
		SetAt:            now,
	}); err != nil {
		t.Fatalf("SetGitHubInstallationTriggerRoute: %v", err)
	}

	mkTrigger := func(id, projectID, name, sourceKind, sourceID, status, statusReason string) *store.Trigger {
		tr := &store.Trigger{
			ID:            tid(id),
			ProjectID:     projectID,
			Name:          name,
			SourceKind:    sourceKind,
			SourceID:      sourceID,
			MatchTypes:    []string{"github.issue_comment.created"},
			ActionType:    store.TriggerActionMessage,
			ActionPayload: `{"agentId":"agent-1","message":"hi"}`,
			Status:        status,
			StatusReason:  statusReason,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := s.CreateTrigger(ctx, tr); err != nil {
			t.Fatalf("CreateTrigger(%s): %v", id, err)
		}
		return tr
	}

	trAffActiveA := mkTrigger("tr-aff-a-active", projAffectedA, "gh-active-a", store.TriggerSourceKindGitHub, "", store.TriggerStatusActive, store.TriggerStatusReasonNone)
	trAffUserPausedA := mkTrigger("tr-aff-a-user-paused", projAffectedA, "gh-user-paused-a", store.TriggerSourceKindGitHub, "", store.TriggerStatusPaused, store.TriggerStatusReasonNone)
	trAffCreatorDelA := mkTrigger("tr-aff-a-creator-del", projAffectedA, "gh-creator-del-a", store.TriggerSourceKindGitHub, "", store.TriggerStatusPaused, store.TriggerStatusReasonCreatorDeleted)
	trAffWebhookA := mkTrigger("tr-aff-a-webhook", projAffectedA, "wh-active-a", store.TriggerSourceKindWebhook, tid("ep-1"), store.TriggerStatusActive, store.TriggerStatusReasonNone)
	trAffActiveB := mkTrigger("tr-aff-b-active", projAffectedB, "gh-active-b", store.TriggerSourceKindGitHub, "", store.TriggerStatusActive, store.TriggerStatusReasonNone)
	trOtherRepo := mkTrigger("tr-other-repo", projOtherRepo, "gh-other-repo", store.TriggerSourceKindGitHub, "", store.TriggerStatusActive, store.TriggerStatusReasonNone)
	trOtherInst := mkTrigger("tr-other-inst", projOtherInst, "gh-other-inst", store.TriggerSourceKindGitHub, "", store.TriggerStatusActive, store.TriggerStatusReasonNone)

	sendRemovedWebhook := func() {
		t.Helper()
		payload := mustJSON(t, map[string]interface{}{
			"action":       "removed",
			"installation": map[string]interface{}{"id": instID},
			"repositories_removed": []map[string]interface{}{
				{"id": 1, "full_name": "aCmE/wIdGeTs", "name": "widgets"},
			},
			"repositories_added": []map[string]interface{}{},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Event", "installation_repositories")
		req.Header.Set("X-Hub-Signature-256", signWebhookPayload(payload, "test-webhook-secret"))
		rec := httptest.NewRecorder()
		srv.handleGitHubWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	sendRemovedWebhook()

	assertTriggerState := func(id, wantStatus, wantReason string) {
		t.Helper()
		got, err := s.GetTrigger(ctx, id)
		if err != nil {
			t.Fatalf("GetTrigger(%s): %v", id, err)
		}
		if got.Status != wantStatus || got.StatusReason != wantReason {
			t.Errorf("trigger %s: got (%q, %q), want (%q, %q)", id, got.Status, got.StatusReason, wantStatus, wantReason)
		}
	}

	// Affected projects' github triggers paused with source_deleted.
	assertTriggerState(trAffActiveA.ID, store.TriggerStatusPaused, store.TriggerStatusReasonSourceDeleted)
	assertTriggerState(trAffUserPausedA.ID, store.TriggerStatusPaused, store.TriggerStatusReasonSourceDeleted)
	assertTriggerState(trAffActiveB.ID, store.TriggerStatusPaused, store.TriggerStatusReasonSourceDeleted)

	// Already system-paused trigger preserves its existing system reason.
	assertTriggerState(trAffCreatorDelA.ID, store.TriggerStatusPaused, store.TriggerStatusReasonCreatorDeleted)

	// Webhook trigger on affected project and github triggers on unaffected projects stay active.
	assertTriggerState(trAffWebhookA.ID, store.TriggerStatusActive, store.TriggerStatusReasonNone)
	assertTriggerState(trOtherRepo.ID, store.TriggerStatusActive, store.TriggerStatusReasonNone)
	assertTriggerState(trOtherInst.ID, store.TriggerStatusActive, store.TriggerStatusReasonNone)

	// Primary routing entry on the installation is NOT edited (§8.4).
	updatedInst, err := s.GetGitHubInstallation(ctx, instID)
	if err != nil {
		t.Fatalf("GetGitHubInstallation: %v", err)
	}
	route, ok := updatedInst.TriggerRouting["acme/widgets"]
	if !ok || route.PrimaryProjectID != projAffectedA {
		t.Errorf("expected trigger_routing[acme/widgets] to stay %q, got %+v", projAffectedA, updatedInst.TriggerRouting)
	}

	// Exactly 3 mutation audit records written (one per newly paused trigger).
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: mutationTypeTriggerPause,
		Limit:        100,
	})
	if err != nil {
		t.Fatalf("ListMutationAudits: %v", err)
	}
	if len(audits) != 3 {
		t.Fatalf("expected 3 trigger_pause audit records, got %d", len(audits))
	}
	auditedTargets := make(map[string]*store.MutationAuditRecord, len(audits))
	for _, rec := range audits {
		auditedTargets[rec.TargetID] = rec
	}
	for _, wantID := range []string{trAffActiveA.ID, trAffUserPausedA.ID, trAffActiveB.ID} {
		rec, exists := auditedTargets[wantID]
		if !exists {
			t.Errorf("missing trigger_pause audit for trigger %s", wantID)
			continue
		}
		if !strings.Contains(rec.AfterSummary, store.TriggerStatusReasonSourceDeleted) ||
			!strings.Contains(rec.AfterSummary, "installation_repositories.removed") {
			t.Errorf("audit for %s has unexpected AfterSummary: %s", wantID, rec.AfterSummary)
		}
	}

	// Redelivery must be idempotent: triggers stay paused, no duplicate audit records.
	sendRemovedWebhook()

	assertTriggerState(trAffActiveA.ID, store.TriggerStatusPaused, store.TriggerStatusReasonSourceDeleted)
	assertTriggerState(trAffUserPausedA.ID, store.TriggerStatusPaused, store.TriggerStatusReasonSourceDeleted)
	assertTriggerState(trAffActiveB.ID, store.TriggerStatusPaused, store.TriggerStatusReasonSourceDeleted)

	auditsAfterRedelivery, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: mutationTypeTriggerPause,
		Limit:        100,
	})
	if err != nil {
		t.Fatalf("ListMutationAudits after redelivery: %v", err)
	}
	if len(auditsAfterRedelivery) != 3 {
		t.Errorf("expected 3 trigger_pause audit records after idempotent redelivery, got %d", len(auditsAfterRedelivery))
	}
}

func TestResumeGitHubTrigger_RefusedUntilRepoReassociated(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	gh := newFakeGitHubForAssoc(t)
	f.srv.mu.Lock()
	f.srv.config.GitHubAppConfig.APIBaseURL = gh.srv.URL
	f.srv.config.GitHubAppConfig.AppID = 778899
	f.srv.config.GitHubAppConfig.AppSlug = "scion-bot"
	f.srv.config.GitHubAppConfig.WebhookSecret = "test-webhook-secret"
	f.srv.config.GitHubAppConfig.WebhooksEnabled = true
	f.srv.mu.Unlock()

	setTriggerExperimentsForG1bTest(t, f.srv, true, true)

	sysAdmin := createAssocAdmin(t, f.store, "resume-sys-admin")
	const instID int64 = 6202
	createAssocInstallation(t, f.store, instID, "acme", "acme/widgets")

	// Associate the fixture project with instID + Acme/Widgets via admin binding.
	remote := gh.remote("Acme", "Widgets")
	proj, err := f.store.GetProject(ctx, f.projectID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	proj.GitRemote = remote
	idCopy := instID
	proj.GitHubInstallationID = &idCopy
	proj.GitHubAppStatus = &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: time.Now().UTC()}
	if err := f.store.UpdateProject(ctx, proj); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	setAdminBinding(t, f.store, proj, instID, util.NormalizeGitRemote(remote), sysAdmin.ID)
	f.srv.invalidateGitHubAssociation(proj.ID)

	// Create an active github trigger.
	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-resume-check",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.agentA.ID, Message: "ping"},
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create trigger: expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	var created TriggerResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created trigger: %v", err)
	}

	// Remove acme/widgets via installation_repositories.removed -> pauses trigger with source_deleted.
	removePayload := mustJSON(t, map[string]any{
		"action":       "removed",
		"installation": map[string]any{"id": instID},
		"repositories_removed": []map[string]any{
			{"id": 1, "full_name": "acme/widgets", "name": "widgets"},
		},
		"repositories_added": []map[string]any{},
	})
	removeReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(removePayload))
	removeReq.Header.Set("X-GitHub-Event", "installation_repositories")
	removeReq.Header.Set("X-Hub-Signature-256", signWebhookPayload(removePayload, "test-webhook-secret"))
	removeRec := httptest.NewRecorder()
	f.srv.handleGitHubWebhook(removeRec, removeReq)
	if removeRec.Code != http.StatusOK {
		t.Fatalf("removed webhook: expected 200, got %d: %s", removeRec.Code, removeRec.Body.String())
	}

	// Attempting to resume while repo is absent from installation.Repositories must return 409 repo_not_accessible.
	refusedRec := f.doTrigger(t, f.ownerAuth, created.ID+"/resume", http.MethodPost, nil)
	if refusedRec.Code != http.StatusConflict {
		t.Fatalf("resume before re-association: expected 409, got %d: %s", refusedRec.Code, refusedRec.Body.String())
	}
	if code := decodeAPIErrorCode(t, refusedRec); code != githubapp.ErrCodeRepoNotAccessible {
		t.Errorf("expected error code %q, got %q (body: %s)", githubapp.ErrCodeRepoNotAccessible, code, refusedRec.Body.String())
	}
	if !strings.Contains(refusedRec.Body.String(), "repository list") {
		t.Errorf("expected clear reason in 409 body, got: %s", refusedRec.Body.String())
	}

	// Trigger must still be paused with source_deleted.
	stillPaused, err := f.store.GetTrigger(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTrigger: %v", err)
	}
	if stillPaused.Status != store.TriggerStatusPaused || stillPaused.StatusReason != store.TriggerStatusReasonSourceDeleted {
		t.Fatalf("expected trigger still paused (source_deleted), got (%q, %q)", stillPaused.Status, stillPaused.StatusReason)
	}

	// Re-add the repository via installation_repositories.added (mixed case) -> resume now succeeds.
	addPayload := mustJSON(t, map[string]any{
		"action":       "added",
		"installation": map[string]any{"id": instID},
		"repositories_added": []map[string]any{
			{"id": 1, "full_name": "Acme/Widgets", "name": "widgets"},
		},
		"repositories_removed": []map[string]any{},
	})
	addReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(addPayload))
	addReq.Header.Set("X-GitHub-Event", "installation_repositories")
	addReq.Header.Set("X-Hub-Signature-256", signWebhookPayload(addPayload, "test-webhook-secret"))
	addRec := httptest.NewRecorder()
	f.srv.handleGitHubWebhook(addRec, addReq)
	if addRec.Code != http.StatusOK {
		t.Fatalf("added webhook: expected 200, got %d: %s", addRec.Code, addRec.Body.String())
	}

	resumedRec := f.doTrigger(t, f.ownerAuth, created.ID+"/resume", http.MethodPost, nil)
	if resumedRec.Code != http.StatusOK {
		t.Fatalf("resume after re-association: expected 200, got %d: %s", resumedRec.Code, resumedRec.Body.String())
	}
	resumed, err := f.store.GetTrigger(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTrigger after resume: %v", err)
	}
	if resumed.Status != store.TriggerStatusActive || resumed.StatusReason != store.TriggerStatusReasonNone {
		t.Fatalf("expected trigger active with empty reason, got (%q, %q)", resumed.Status, resumed.StatusReason)
	}
}
