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
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestExtractOwnerRepo(t *testing.T) {
	tests := []struct {
		name     string
		remote   string
		expected string
	}{
		{"https github", "https://github.com/acme/widgets.git", "acme/widgets"},
		{"https github no .git", "https://github.com/acme/widgets", "acme/widgets"},
		{"https github trailing slash", "https://github.com/acme/widgets/", "acme/widgets"},
		{"ssh github", "git@github.com:acme/widgets.git", "acme/widgets"},
		{"ssh github no .git", "git@github.com:acme/widgets", "acme/widgets"},
		{"http github", "http://github.com/acme/widgets.git", "acme/widgets"},
		{"github enterprise https", "https://github.example.com/org/repo.git", "org/repo"},
		{"ssh with slash prefix", "git@github.com:/acme/widgets.git", "acme/widgets"},
		{"empty", "", ""},
		{"just hostname", "github.com", ""},
		{"no repo", "https://github.com/acme", ""},
		{"too many parts", "https://github.com/a/b/c", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := extractOwnerRepo(tc.remote)
			if result != tc.expected {
				t.Errorf("extractOwnerRepo(%q) = %q, want %q", tc.remote, result, tc.expected)
			}
		})
	}
}

func TestIsValidOwnerRepo(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"acme/widgets", true},
		{"a/b", true},
		{"", false},
		{"acme", false},
		{"/repo", false},
		{"owner/", false},
		{"a/b/c", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := isValidOwnerRepo(tc.input)
			if result != tc.expected {
				t.Errorf("isValidOwnerRepo(%q) = %v, want %v", tc.input, result, tc.expected)
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

// webhookTestServer creates a test server with the webhook secret configured.
func webhookTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	srv.mu.Lock()
	srv.config.GitHubAppConfig.WebhookSecret = "test-webhook-secret"
	srv.config.GitHubAppConfig.WebhooksEnabled = true
	srv.config.GitHubAppConfig.AppID = 42
	srv.mu.Unlock()
	return srv, s
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
	srv, s := testServer(t)
	ctx := context.Background()

	// A "not configured" secret lookup (store.ErrNotFound) must not be treated
	// as a backend error: only the once-per-process "not configured" WARN
	// should fire, not the per-request "secret lookup failed" WARN.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

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

	// Create a project with a matching git remote
	project := &store.Project{
		ID:        tid("project-1"),
		Name:      "Test Project",
		Slug:      "test-project",
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

	// Create projects with different git remotes
	projects := []*store.Project{
		{ID: tid("g1"), Name: "G1", Slug: tid("g1"), GitRemote: "https://github.com/acme/widgets.git", Created: time.Now(), Updated: time.Now()},
		{ID: tid("g2"), Name: "G2", Slug: tid("g2"), GitRemote: "https://github.com/acme/api.git", Created: time.Now(), Updated: time.Now()},
		{ID: tid("g3"), Name: "G3", Slug: tid("g3"), GitRemote: "https://github.com/other/repo.git", Created: time.Now(), Updated: time.Now()},
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

	// Create a project with a matching git remote but no installation yet
	project := &store.Project{
		ID:        tid("project-event-3"),
		Name:      "Event Test Project 3",
		Slug:      "event-test-project-3",
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

func TestHandleGitHubWebhook_CommentMention(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// Create a project with a matching git remote
	project := &store.Project{
		ID:        tid("proj-mention-1"),
		Name:      "Proj Mention 1",
		Slug:      "proj-mention-1",
		GitRemote: "https://github.com/acme/widgets.git",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	tests := []struct {
		name          string
		eventType     string
		payload       map[string]interface{}
		expectedCode  int
		expectIgnored bool
	}{
		{
			name:      "issue comment with mention on PR",
			eventType: "issue_comment",
			payload: map[string]interface{}{
				"action": "created",
				"issue": map[string]interface{}{
					"number": 101,
					"pull_request": map[string]interface{}{
						"url": "https://api.github.com/repos/acme/widgets/pulls/101",
					},
				},
				"comment": map[string]interface{}{
					"id":   111,
					"body": "Hey @scion, please look at this!",
				},
				"repository": map[string]interface{}{
					"full_name": "acme/widgets",
				},
			},
			expectedCode: http.StatusOK,
		},
		{
			name:      "issue comment on PR case insensitive mention",
			eventType: "issue_comment",
			payload: map[string]interface{}{
				"action": "created",
				"issue": map[string]interface{}{
					"number": 101,
					"pull_request": map[string]interface{}{
						"url": "https://api.github.com/repos/acme/widgets/pulls/101",
					},
				},
				"comment": map[string]interface{}{
					"id":   112,
					"body": "Please review @SCION",
				},
				"repository": map[string]interface{}{
					"full_name": "acme/widgets",
				},
			},
			expectedCode: http.StatusOK,
		},
		{
			name:      "issue comment without mention",
			eventType: "issue_comment",
			payload: map[string]interface{}{
				"action": "created",
				"issue": map[string]interface{}{
					"number": 101,
					"pull_request": map[string]interface{}{
						"url": "https://api.github.com/repos/acme/widgets/pulls/101",
					},
				},
				"comment": map[string]interface{}{
					"id":   113,
					"body": "No mention here.",
				},
				"repository": map[string]interface{}{
					"full_name": "acme/widgets",
				},
			},
			expectedCode: http.StatusOK,
		},
		{
			name:      "issue comment not on PR",
			eventType: "issue_comment",
			payload: map[string]interface{}{
				"action": "created",
				"issue": map[string]interface{}{
					"number": 101,
				},
				"comment": map[string]interface{}{
					"id":   114,
					"body": "Hey @scion on a normal issue",
				},
				"repository": map[string]interface{}{
					"full_name": "acme/widgets",
				},
			},
			expectedCode:  http.StatusOK,
			expectIgnored: true,
		},
		{
			name:      "pull request review comment",
			eventType: "pull_request_review_comment",
			payload: map[string]interface{}{
				"action": "created",
				"pull_request": map[string]interface{}{
					"number": 102,
				},
				"comment": map[string]interface{}{
					"id":   222,
					"body": "Line comment for @scion",
				},
				"repository": map[string]interface{}{
					"full_name": "acme/widgets",
				},
			},
			expectedCode: http.StatusOK,
		},
		{
			name:      "pull request review comment non-create",
			eventType: "pull_request_review_comment",
			payload: map[string]interface{}{
				"action": "edited",
				"pull_request": map[string]interface{}{
					"number": 102,
				},
				"comment": map[string]interface{}{
					"id":   222,
					"body": "Line comment for @scion edited",
				},
				"repository": map[string]interface{}{
					"full_name": "acme/widgets",
				},
			},
			expectedCode:  http.StatusOK,
			expectIgnored: true,
		},
		{
			name:      "pull request review submitted with mention",
			eventType: "pull_request_review",
			payload: map[string]interface{}{
				"action": "submitted",
				"pull_request": map[string]interface{}{
					"number": 103,
				},
				"review": map[string]interface{}{
					"id":   333,
					"body": "Review comment for @scion",
				},
				"repository": map[string]interface{}{
					"full_name": "acme/widgets",
				},
			},
			expectedCode: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payloadBytes := mustJSON(t, tc.payload)
			sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

			req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
			req.Header.Set("X-GitHub-Event", tc.eventType)
			req.Header.Set("X-Hub-Signature-256", sig)

			rec := httptest.NewRecorder()
			srv.handleGitHubWebhook(rec, req)

			if rec.Code != tc.expectedCode {
				t.Errorf("expected status %d, got %d: %s", tc.expectedCode, rec.Code, rec.Body.String())
			}

			if tc.expectIgnored {
				var resp map[string]string
				json.Unmarshal(rec.Body.Bytes(), &resp)
				if resp["status"] != "ignored" {
					t.Errorf("expected status ignored, got %v", resp)
				}
			} else if tc.expectedCode == http.StatusOK {
				var resp map[string]string
				json.Unmarshal(rec.Body.Bytes(), &resp)
				if resp["status"] != "ok" {
					t.Errorf("expected status ok, got %v", resp)
				}
			}
		})
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal JSON: %v", err)
	}
	return data
}

func TestHandleGitHubWebhook_StrategyD_ActiveAgent(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// 1. Create a project with a matching git remote
	project := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Proj Active 1",
		Slug:      "proj-active-1",
		GitRemote: "https://github.com/acme/widgets.git",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// 2. Create an active running agent with the correct labels
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Agent Active 1",
		Slug:      "agent-active-1",
		ProjectID: project.ID,
		Phase:     "running",
		Labels: map[string]string{
			"github-pr":   "101",
			"github-repo": "acme/widgets",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// 3. Start Message Broker on the Server
	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	b := eventbus.NewInProcessEventBus(slog.Default())
	srv.StartMessageBroker(b)

	// 4. Subscribe to the agent's message topic on the broker
	msgCh := make(chan *messages.StructuredMessage, 10)
	topic := eventbus.TopicAgentMessages(project.ID, agent.Slug)
	_, err := b.Subscribe(topic, func(ctx context.Context, top string, msg *messages.StructuredMessage) {
		msgCh <- msg
	})
	if err != nil {
		t.Fatalf("failed to subscribe to agent messages: %v", err)
	}

	// 5. Build and send the webhook payload with the "@scion" mention
	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion, please re-run tests!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "octocat",
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 6. Assert message was published to the active agent
	select {
	case msg := <-msgCh:
		if msg.Sender != "user:github-octocat" {
			t.Errorf("expected sender 'user:github-octocat', got %q", msg.Sender)
		}
		if msg.Recipient != "agent:agent-active-1" {
			t.Errorf("expected recipient 'agent:agent-active-1', got %q", msg.Recipient)
		}
		if msg.RecipientID != agent.ID {
			t.Errorf("expected recipient ID %q, got %q", agent.ID, msg.RecipientID)
		}
		if msg.Msg != "Hey @scion, please re-run tests!" {
			t.Errorf("expected message body 'Hey @scion, please re-run tests!', got %q", msg.Msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for routed message on broker")
	}
}

func TestHandleGitHubWebhook_StrategyD_Fallback(t *testing.T) {
	// Setup mock GitHub API server
	expiresAt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_access_token_abc",
				"expires_at": expiresAt,
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/pulls/101" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 101,
				"head": map[string]interface{}{
					"ref": "my-cool-feature-branch",
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// Configure GitHub App settings on server to point to our mock server
	pemBytes := generateTestWebhookKey(t)
	instID := int64(123)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	// 1. Create a project with matching remote, installation id, and a runtime broker (so create agent succeeds)
	brokerObj := &store.RuntimeBroker{
		ID:          tid("broker-fallback-1"),
		Slug:        "broker-fallback-1",
		Name:        "Broker Fallback 1",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	if err := s.CreateTemplate(ctx, &store.Template{
		ID: tid("tmpl_default"), Slug: "default", Name: "Default Template",
		Harness: "gemini", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		ContentHash: "abc123",
		Created:     time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("failed to create default template: %v", err)
	}

	// Create a mock installation first to satisfy the project's foreign key constraint
	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                     tid("proj-fallback-1"),
		Name:                   "Proj Fallback 1",
		Slug:                   "proj-fallback-1",
		GitRemote:              "https://github.com/acme/widgets.git",
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerObj.ID,
		BrokerName: brokerObj.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// 2. Send webhook payload
	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion, please fix that typo!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "coder123",
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. Since agent creation happens in a background goroutine, poll the database for a few seconds to verify the new agent is created
	var spawnedAgent *store.Agent
	for i := 0; i < 20; i++ {
		agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{Limit: 100})
		if err == nil && len(agents.Items) > 0 {
			spawnedAgent = &agents.Items[0]
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	if spawnedAgent == nil {
		t.Fatal("timed out waiting for fallback agent to be spawned in background")
	}

	// 4. Assert correctness of the spawned agent
	if spawnedAgent.Labels == nil {
		t.Fatal("expected spawned agent to have labels, got nil")
	}
	if spawnedAgent.Labels["github-pr"] != "101" {
		t.Errorf("expected label github-pr='101', got %q", spawnedAgent.Labels["github-pr"])
	}
	if spawnedAgent.Labels["github-repo"] != "acme/widgets" {
		t.Errorf("expected label github-repo='acme/widgets', got %q", spawnedAgent.Labels["github-repo"])
	}
}

func TestHandleGitHubWebhook_ReviewCommand_ActiveAgent(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("proj-review-active-1"),
		Name:      "Proj Review Active 1",
		Slug:      "proj-review-active-1",
		GitRemote: "https://github.com/acme/widgets.git",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:        tid("agent-review-active-1"),
		Name:      "Agent Review Active 1",
		Slug:      "agent-review-active-1",
		ProjectID: project.ID,
		Phase:     "running",
		Labels: map[string]string{
			"github-pr":   "101",
			"github-repo": "acme/widgets",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	b := eventbus.NewInProcessEventBus(slog.Default())
	srv.StartMessageBroker(b)

	msgCh := make(chan *messages.StructuredMessage, 10)
	topic := eventbus.TopicAgentMessages(project.ID, agent.Slug)
	_, err := b.Subscribe(topic, func(ctx context.Context, top string, msg *messages.StructuredMessage) {
		msgCh <- msg
	})
	if err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion /review please!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "octocat",
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case msg := <-msgCh:
		t.Fatalf("unexpectedly received routed message for /review: %v", msg)
	case <-time.After(200 * time.Millisecond):
		// Expected: active agent is bypassed for /review
	}
}

func TestHandleGitHubWebhook_ReviewCommand_Fallback(t *testing.T) {
	expiresAt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_access_token_abc",
				"expires_at": expiresAt,
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/pulls/101" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 101,
				"head": map[string]interface{}{
					"ref": "my-cool-feature-branch",
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, s := webhookTestServer(t)
	ctx := context.Background()

	pemBytes := generateTestWebhookKey(t)
	instID := int64(123)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	brokerObj := &store.RuntimeBroker{
		ID:          tid("broker-review-fallback-1"),
		Slug:        "broker-review-fallback-1",
		Name:        "Broker Review Fallback 1",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	if err := s.CreateTemplate(ctx, &store.Template{
		ID: tid("tmpl_default"), Slug: "default", Name: "Default Template",
		Harness: "gemini", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		ContentHash: "abc123",
		Created:     time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("failed to create default template: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                     tid("proj-review-fallback-1"),
		Name:                   "Proj Review Fallback 1",
		Slug:                   "proj-review-fallback-1",
		GitRemote:              "https://github.com/acme/widgets.git",
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerObj.ID,
		BrokerName: brokerObj.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion /review please!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "coder123",
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var spawnedAgent *store.Agent
	for i := 0; i < 20; i++ {
		agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{Limit: 100})
		if err == nil && len(agents.Items) > 0 {
			spawnedAgent = &agents.Items[0]
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	if spawnedAgent == nil {
		t.Fatal("timed out waiting for fallback agent to be spawned")
	}

	expectedTask := "Perform a complete code review for Pull Request #101 on repository acme/widgets. Inspect the changes on branch my-cool-feature-branch, identify bugs, style issues, or architectural improvements, and post review comments back to GitHub."
	if spawnedAgent.AppliedConfig == nil {
		t.Fatal("expected spawned agent to have AppliedConfig, got nil")
	}
	if spawnedAgent.AppliedConfig.Task != expectedTask {
		t.Errorf("expected task %q, got %q", expectedTask, spawnedAgent.AppliedConfig.Task)
	}
}

func TestHandleGitHubWebhook_UnmatchedProjectCommentBack(t *testing.T) {
	var commentBody string
	var commentMutex sync.Mutex

	expiresAt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/456/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_unmatched_access_token",
				"expires_at": expiresAt,
			})
			return
		}
		if r.URL.Path == "/repos/acme/unmatched/issues/101/comments" {
			var reqData map[string]string
			if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			commentMutex.Lock()
			commentBody = reqData["body"]
			commentMutex.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 789,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, _ := webhookTestServer(t)

	pemBytes := generateTestWebhookKey(t)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/unmatched/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion can you review?",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/unmatched",
		},
		"sender": map[string]interface{}{
			"login": "coder123",
		},
		"installation": map[string]interface{}{
			"id": 456,
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var finalComment string
	commentMutex.Lock()
	finalComment = commentBody
	commentMutex.Unlock()

	expectedMsg := "No matching project or grove was found in Scion configured with this repository's Git remote. Please ensure the repository is linked to a Scion project."
	if finalComment != expectedMsg {
		t.Errorf("expected comment %q, got %q", expectedMsg, finalComment)
	}
}

func TestHandleGitHubWebhook_ValidateCommand_ActiveAgent(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("proj-validate-active-1"),
		Name:      "Proj Validate Active 1",
		Slug:      "proj-validate-active-1",
		GitRemote: "https://github.com/acme/widgets.git",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:        tid("agent-validate-active-1"),
		Name:      "Agent Validate Active 1",
		Slug:      "agent-validate-active-1",
		ProjectID: project.ID,
		Phase:     "running",
		Labels: map[string]string{
			"github-pr":   "101",
			"github-repo": "acme/widgets",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	b := eventbus.NewInProcessEventBus(slog.Default())
	srv.StartMessageBroker(b)

	msgCh := make(chan *messages.StructuredMessage, 10)
	topic := eventbus.TopicAgentMessages(project.ID, agent.Slug)
	_, err := b.Subscribe(topic, func(ctx context.Context, top string, msg *messages.StructuredMessage) {
		msgCh <- msg
	})
	if err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion /validate please!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "octocat",
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case msg := <-msgCh:
		t.Fatalf("unexpectedly received routed message for /validate: %v", msg)
	case <-time.After(200 * time.Millisecond):
		// Expected: active agent is bypassed for /validate
	}
}

func TestHandleGitHubWebhook_ValidateCommand_Fallback(t *testing.T) {
	expiresAt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_access_token_abc",
				"expires_at": expiresAt,
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/pulls/101" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 101,
				"head": map[string]interface{}{
					"ref": "my-cool-feature-branch",
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, s := webhookTestServer(t)
	ctx := context.Background()

	pemBytes := generateTestWebhookKey(t)
	instID := int64(123)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	brokerObj := &store.RuntimeBroker{
		ID:          tid("broker-validate-fallback-1"),
		Slug:        "broker-validate-fallback-1",
		Name:        "Broker Validate Fallback 1",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	if err := s.CreateTemplate(ctx, &store.Template{
		ID: tid("tmpl_default"), Slug: "default", Name: "Default Template",
		Harness: "gemini", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		ContentHash: "abc123",
		Created:     time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("failed to create default template: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                     tid("proj-validate-fallback-1"),
		Name:                   "Proj Validate Fallback 1",
		Slug:                   "proj-validate-fallback-1",
		GitRemote:              "https://github.com/acme/widgets.git",
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerObj.ID,
		BrokerName: brokerObj.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion /validate please!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "coder123",
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var spawnedAgent *store.Agent
	for i := 0; i < 20; i++ {
		agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{Limit: 100})
		if err == nil && len(agents.Items) > 0 {
			spawnedAgent = &agents.Items[0]
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	if spawnedAgent == nil {
		t.Fatal("timed out waiting for fallback agent to be spawned")
	}

	expectedTask := "Validate the changes for Pull Request #101 on repository acme/widgets and report the validation results back to GitHub."
	if spawnedAgent.AppliedConfig == nil {
		t.Fatal("expected spawned agent to have AppliedConfig, got nil")
	}
	if spawnedAgent.AppliedConfig.Task != expectedTask {
		t.Errorf("expected task %q, got %q", expectedTask, spawnedAgent.AppliedConfig.Task)
	}
	if spawnedAgent.Labels == nil || spawnedAgent.Labels["github-action"] != "validate" {
		t.Errorf("expected github-action label to be validate, got %v", spawnedAgent.Labels)
	}
}

func TestHandleGitHubWebhook_TemplateResolution(t *testing.T) {
	t.Setenv("SCION_REVIEW_TEMPLATE", "my-custom-review-tpl")
	t.Setenv("SCION_VALIDATE_TEMPLATE", "my-custom-validate-tpl")

	expiresAt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_access_token_abc",
				"expires_at": expiresAt,
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/pulls/101" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 101,
				"head": map[string]interface{}{
					"ref": "my-cool-feature-branch",
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, s := webhookTestServer(t)
	ctx := context.Background()

	now := time.Now()
	if err := s.CreateTemplate(ctx, &store.Template{
		ID: tid("tmpl_review1"), Slug: "my-custom-review-tpl", Name: "My Custom Review Template",
		Harness: "claude", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		ContentHash: "abc123",
		Created:     now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create custom review template: %v", err)
	}

	if err := s.CreateTemplate(ctx, &store.Template{
		ID: tid("tmpl_validate1"), Slug: "my-custom-validate-tpl", Name: "My Custom Validate Template",
		Harness: "gemini", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		ContentHash: "abc123",
		Created:     now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create custom validate template: %v", err)
	}

	pemBytes := generateTestWebhookKey(t)
	instID := int64(123)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	brokerObj := &store.RuntimeBroker{
		ID:          tid("broker-tpl-1"),
		Slug:        "broker-tpl-1",
		Name:        "Broker Tpl 1",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                     tid("proj-tpl-1"),
		Name:                   "Proj Tpl 1",
		Slug:                   "proj-tpl-1",
		GitRemote:              "https://github.com/acme/widgets.git",
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerObj.ID,
		BrokerName: brokerObj.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// 1. Trigger /review
	payloadReview := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion /review please!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "coder123",
		},
	}

	payloadBytesReview := mustJSON(t, payloadReview)
	sigReview := signWebhookPayload(payloadBytesReview, "test-webhook-secret")

	reqReview := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytesReview))
	reqReview.Header.Set("X-GitHub-Event", "issue_comment")
	reqReview.Header.Set("X-Hub-Signature-256", sigReview)

	recReview := httptest.NewRecorder()
	srv.handleGitHubWebhook(recReview, reqReview)

	if recReview.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", recReview.Code, recReview.Body.String())
	}

	var spawnedReviewAgent *store.Agent
	for i := 0; i < 20; i++ {
		agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID, Phase: "created"}, store.ListOptions{Limit: 100})
		if err == nil {
			for _, item := range agents.Items {
				if item.Labels["github-action"] == "review" {
					spawnedReviewAgent = &item
					break
				}
			}
		}
		if spawnedReviewAgent != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	if spawnedReviewAgent == nil {
		t.Fatal("timed out waiting for review agent to be spawned")
	}

	if spawnedReviewAgent.Template != "my-custom-review-tpl" {
		t.Errorf("expected template to be %q, got %q", "my-custom-review-tpl", spawnedReviewAgent.Template)
	}

	// Sleep to ensure a different Unix timestamp for /validate
	time.Sleep(1100 * time.Millisecond)

	// 2. Trigger /validate
	payloadValidate := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   112,
			"body": "Hey @scion /validate please!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "coder123",
		},
	}

	payloadBytesValidate := mustJSON(t, payloadValidate)
	sigValidate := signWebhookPayload(payloadBytesValidate, "test-webhook-secret")

	reqValidate := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytesValidate))
	reqValidate.Header.Set("X-GitHub-Event", "issue_comment")
	reqValidate.Header.Set("X-Hub-Signature-256", sigValidate)

	recValidate := httptest.NewRecorder()
	srv.handleGitHubWebhook(recValidate, reqValidate)

	if recValidate.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", recValidate.Code, recValidate.Body.String())
	}

	var spawnedValidateAgent *store.Agent
	for i := 0; i < 20; i++ {
		agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID, Phase: "created"}, store.ListOptions{Limit: 100})
		if err == nil {
			for _, item := range agents.Items {
				if item.Labels["github-action"] == "validate" {
					spawnedValidateAgent = &item
					break
				}
			}
		}
		if spawnedValidateAgent != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	if spawnedValidateAgent == nil {
		t.Fatal("timed out waiting for validate agent to be spawned")
	}

	if spawnedValidateAgent.Template != "my-custom-validate-tpl" {
		t.Errorf("expected template to be %q, got %q", "my-custom-validate-tpl", spawnedValidateAgent.Template)
	}
}

func TestHandleGitHubWebhook_TemplateResolution_ServerConfig(t *testing.T) {
	// Isolate HOME so config.LoadEffectiveSettings finds our temporary settings.yaml
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	globalDir := filepath.Join(tempHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatalf("failed to create global settings dir: %v", err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
profiles:
  local:
    default_template: my-server-config-review-tpl
`
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	expiresAt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_access_token_abc",
				"expires_at": expiresAt,
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/pulls/101" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 101,
				"head": map[string]interface{}{
					"ref": "my-cool-feature-branch",
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, s := webhookTestServer(t)
	ctx := context.Background()

	now := time.Now()
	if err := s.CreateTemplate(ctx, &store.Template{
		ID: tid("tmpl_review_server"), Slug: "my-server-config-review-tpl", Name: "My Server Config Review Template",
		Harness: "claude", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		ContentHash: "abc123",
		Created:     now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create custom review template: %v", err)
	}

	pemBytes := generateTestWebhookKey(t)
	instID := int64(123)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	brokerObj := &store.RuntimeBroker{
		ID:          tid("broker-srvtpl-1"),
		Slug:        "broker-srvtpl-1",
		Name:        "Broker SrvTpl 1",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                     tid("proj-srvtpl-1"),
		Name:                   "Proj SrvTpl 1",
		Slug:                   "proj-srvtpl-1",
		GitRemote:              "https://github.com/acme/widgets.git",
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerObj.ID,
		BrokerName: brokerObj.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	payloadReview := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion /review please!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "coder123",
		},
	}

	payloadBytesReview := mustJSON(t, payloadReview)
	sigReview := signWebhookPayload(payloadBytesReview, "test-webhook-secret")

	reqReview := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytesReview))
	reqReview.Header.Set("X-GitHub-Event", "issue_comment")
	reqReview.Header.Set("X-Hub-Signature-256", sigReview)

	recReview := httptest.NewRecorder()
	srv.handleGitHubWebhook(recReview, reqReview)

	if recReview.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", recReview.Code, recReview.Body.String())
	}

	var spawnedReviewAgent *store.Agent
	for i := 0; i < 20; i++ {
		agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID, Phase: "created"}, store.ListOptions{Limit: 100})
		if err == nil {
			for _, item := range agents.Items {
				if item.Labels["github-action"] == "review" {
					spawnedReviewAgent = &item
					break
				}
			}
		}
		if spawnedReviewAgent != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	if spawnedReviewAgent == nil {
		t.Fatal("timed out waiting for review agent to be spawned")
	}

	if spawnedReviewAgent.Template != "my-server-config-review-tpl" {
		t.Errorf("expected template to be %q, got %q", "my-server-config-review-tpl", spawnedReviewAgent.Template)
	}
}

func TestHandleGitHubWebhook_TemplateResolution_Database(t *testing.T) {
	expiresAt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_access_token_abc",
				"expires_at": expiresAt,
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/pulls/101" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 101,
				"head": map[string]interface{}{
					"ref": "my-cool-feature-branch",
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, s := webhookTestServer(t)
	ctx := context.Background()

	now := time.Now()
	if err := s.CreateTemplate(ctx, &store.Template{
		ID: tid("tmpl_val_db"), Slug: "my-db-validate-tpl", Name: "My DB Validate Template",
		Harness: "claude", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		ContentHash: "abc123",
		Created:     now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create custom validate template: %v", err)
	}

	// Create the hub-scoped env var under the "hub" scopeID
	if err := s.CreateEnvVar(ctx, &store.EnvVar{
		ID:      tid("envvar-val-db-1"),
		Key:     "SCION_VALIDATE_TEMPLATE",
		Value:   "my-db-validate-tpl",
		Scope:   store.ScopeHub,
		ScopeID: "hub",
		Created: now,
		Updated: now,
	}); err != nil {
		t.Fatalf("failed to create hub-scoped env var: %v", err)
	}

	pemBytes := generateTestWebhookKey(t)
	instID := int64(123)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	brokerObj := &store.RuntimeBroker{
		ID:          tid("broker-srvtpl-2"),
		Slug:        "broker-srvtpl-2",
		Name:        "Broker SrvTpl 2",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                     tid("proj-srvtpl-2"),
		Name:                   "Proj SrvTpl 2",
		Slug:                   "proj-srvtpl-2",
		GitRemote:              "https://github.com/acme/widgets.git",
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerObj.ID,
		BrokerName: brokerObj.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	payloadVal := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion /validate please!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "coder123",
		},
	}

	payloadBytesVal := mustJSON(t, payloadVal)
	sigVal := signWebhookPayload(payloadBytesVal, "test-webhook-secret")

	reqVal := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytesVal))
	reqVal.Header.Set("X-GitHub-Event", "issue_comment")
	reqVal.Header.Set("X-Hub-Signature-256", sigVal)

	recVal := httptest.NewRecorder()
	srv.handleGitHubWebhook(recVal, reqVal)

	if recVal.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", recVal.Code, recVal.Body.String())
	}

	var spawnedValAgent *store.Agent
	for i := 0; i < 20; i++ {
		agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID, Phase: "created"}, store.ListOptions{Limit: 100})
		if err == nil {
			for _, item := range agents.Items {
				if item.Labels["github-action"] == "validate" {
					spawnedValAgent = &item
					break
				}
			}
		}
		if spawnedValAgent != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	if spawnedValAgent == nil {
		t.Fatal("timed out waiting for validate agent to be spawned")
	}

	if spawnedValAgent.Template != "my-db-validate-tpl" {
		t.Errorf("expected template to be %q, got %q", "my-db-validate-tpl", spawnedValAgent.Template)
	}
}

func generateTestWebhookKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate test key: %v", err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return pemData
}

func TestResolveBestProject_BranchMatch(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	pA := &store.Project{
		ID:        tid("project-a"),
		Name:      "Project A",
		Slug:      "project-a",
		GitRemote: "https://github.com/acme/widgets.git",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	pB := &store.Project{
		ID:        tid("project-b"),
		Name:      "Project B",
		Slug:      "project-b",
		GitRemote: "https://github.com/acme/widgets.git",
		Created:   time.Now(),
		Updated:   time.Now(),
	}

	if err := s.CreateProject(ctx, pA); err != nil {
		t.Fatalf("failed to create project A: %v", err)
	}
	if err := s.CreateProject(ctx, pB); err != nil {
		t.Fatalf("failed to create project B: %v", err)
	}

	// Create an agent under pB with AppliedConfig.Branch matching target branch
	agent := &store.Agent{
		ID:        tid("agent-b"),
		Name:      "Agent B",
		Slug:      "agent-b",
		ProjectID: pB.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			Branch: "feature-xyz",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	projects := []store.Project{*pA, *pB}
	best := srv.resolveBestProjectForPR(ctx, projects, "feature-xyz")

	if best.ID != pB.ID {
		t.Errorf("expected best project to be %s, got %s", pB.ID, best.ID)
	}
}

func TestResolveBestProject_PublicIsolatedFallback(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// pA has a shared workspace (IsSharedWorkspace() is true)
	pA := &store.Project{
		ID:        tid("project-a"),
		Name:      "Project A",
		Slug:      "project-a",
		GitRemote: "https://github.com/acme/widgets.git",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
		},
		Created: time.Now(),
		Updated: time.Now(),
	}

	// pB has isolated workspaces enabled (!IsSharedWorkspace() i.e. not shared)
	pB := &store.Project{
		ID:        tid("project-b"),
		Name:      "Project B",
		Slug:      "project-b",
		GitRemote: "https://github.com/acme/widgets.git",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModePerAgent,
		},
		Created: time.Now(),
		Updated: time.Now(),
	}

	// pC has a shared workspace (IsSharedWorkspace() is true)
	pC := &store.Project{
		ID:        tid("project-c"),
		Name:      "Project C",
		Slug:      "project-c",
		GitRemote: "https://github.com/acme/widgets.git",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
		},
		Created: time.Now(),
		Updated: time.Now(),
	}

	if err := s.CreateProject(ctx, pA); err != nil {
		t.Fatalf("failed to create project A: %v", err)
	}
	if err := s.CreateProject(ctx, pB); err != nil {
		t.Fatalf("failed to create project B: %v", err)
	}
	if err := s.CreateProject(ctx, pC); err != nil {
		t.Fatalf("failed to create project C: %v", err)
	}

	projects := []store.Project{*pA, *pC, *pB}
	best := srv.resolveBestProjectForPR(ctx, projects, "feature-xyz")

	if best.ID != pB.ID {
		t.Errorf("expected best project to be %s (isolated), got %s", pB.ID, best.ID)
	}
}

func TestHandleGitHubWebhook_FixAndSpawnBypass(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// Create installation
	inst := &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Status:         store.GitHubInstallationStatusActive,
	}
	if err := s.CreateGitHubInstallation(ctx, inst); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	// Create project
	p := &store.Project{
		ID:                   tid("project-fix-1"),
		Name:                 "Project Fix 1",
		Slug:                 "project-fix-1",
		GitRemote:            "https://github.com/acme/widgets.git",
		GitHubInstallationID: &inst.InstallationID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create an active running agent
	agent := &store.Agent{
		ID:        tid("agent-active-fix"),
		Name:      "Agent Active Fix",
		Slug:      "agent-active-fix",
		ProjectID: p.ID,
		Phase:     "running",
		Template:  "default",
		Labels: map[string]string{
			"github-pr":   "101",
			"github-repo": "acme/widgets",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Start Message Broker on the Server
	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	b := eventbus.NewInProcessEventBus(slog.Default())
	srv.StartMessageBroker(b)

	// Subscribe to the agent's message topic
	msgCh := make(chan *messages.StructuredMessage, 10)
	topic := eventbus.TopicAgentMessages(p.ID, agent.Slug)
	_, err := b.Subscribe(topic, func(ctx context.Context, top string, msg *messages.StructuredMessage) {
		msgCh <- msg
	})
	if err != nil {
		t.Fatalf("failed to subscribe to broker: %v", err)
	}

	// Test 1: Send /fix command, should be routed to active agent
	payload := mustJSON(t, map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   555,
			"body": "@scion /fix resolve the crash in main.go",
		},
		"repository": map[string]interface{}{
			"id":        1,
			"full_name": "acme/widgets",
			"name":      "widgets",
		},
		"sender": map[string]interface{}{
			"login": "dev1",
		},
		"installation": map[string]interface{}{
			"id": 12345,
		},
	})

	sig := signWebhookPayload(payload, "test-webhook-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the active agent received the exact fix instructions
	select {
	case msg := <-msgCh:
		expectedMsg := "resolve the crash in main.go Please also comment in the GitHub PR explaining the changes made for a human reviewer."
		if msg.Msg != expectedMsg {
			t.Errorf("expected msg to be %q, got %q", expectedMsg, msg.Msg)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for message routing on active agent /fix")
	}
}

func TestHandleGitHubWebhook_FixAndSpawnBypass_Mismatch(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// Create installation
	inst := &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Status:         store.GitHubInstallationStatusActive,
	}
	if err := s.CreateGitHubInstallation(ctx, inst); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	// Create project
	p := &store.Project{
		ID:                   tid("project-fix-2"),
		Name:                 "Project Fix 2",
		Slug:                 "project-fix-2",
		GitRemote:            "https://github.com/acme/widgets.git",
		GitHubInstallationID: &inst.InstallationID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create an active running agent with a MISMATCHED template
	agent := &store.Agent{
		ID:        tid("agent-active-fix-mismatch"),
		Name:      "Agent Active Fix Mismatch",
		Slug:      "agent-active-fix-mismatch",
		ProjectID: p.ID,
		Phase:     "running",
		Template:  "some-other-template", // This doesn't match resolved default template "default"
		Labels: map[string]string{
			"github-pr":   "101",
			"github-repo": "acme/widgets",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Start Message Broker on the Server
	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	b := eventbus.NewInProcessEventBus(slog.Default())
	srv.StartMessageBroker(b)

	// Subscribe to the agent's message topic
	msgCh := make(chan *messages.StructuredMessage, 10)
	topic := eventbus.TopicAgentMessages(p.ID, agent.Slug)
	_, err := b.Subscribe(topic, func(ctx context.Context, top string, msg *messages.StructuredMessage) {
		msgCh <- msg
	})
	if err != nil {
		t.Fatalf("failed to subscribe to broker: %v", err)
	}

	// Send /fix command, target is "default" template, so active agent (running "some-other-template") should not match.
	payload := mustJSON(t, map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   555,
			"body": "@scion /fix resolve the crash in main.go",
		},
		"repository": map[string]interface{}{
			"id":        1,
			"full_name": "acme/widgets",
			"name":      "widgets",
		},
		"sender": map[string]interface{}{
			"login": "dev1",
		},
		"installation": map[string]interface{}{
			"id": 12345,
		},
	})

	sig := signWebhookPayload(payload, "test-webhook-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify that the active agent did NOT receive any message because it was bypassed
	select {
	case msg := <-msgCh:
		t.Fatalf("active agent should NOT have received a message because template mismatched, got: %v", msg)
	case <-time.After(500 * time.Millisecond):
		// Success! No message routed to the mismatched agent.
	}
}

func TestHandleGitHubWebhook_NoAppropriateProjectCommentBack(t *testing.T) {
	var commentBody string
	var commentMutex sync.Mutex

	expiresAt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_access_token_abc",
				"expires_at": expiresAt,
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/pulls/101" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 101,
				"head": map[string]interface{}{
					"ref": "my-cool-feature-branch",
				},
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/issues/101/comments" {
			var reqData map[string]string
			if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			commentMutex.Lock()
			commentBody = reqData["body"]
			commentMutex.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 789,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, s := webhookTestServer(t)
	ctx := context.Background()

	pemBytes := generateTestWebhookKey(t)
	instID := int64(123)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	brokerObj := &store.RuntimeBroker{
		ID:     tid("broker-inappropriate-1"),
		Slug:   "broker-inappropriate-1",
		Name:   "Broker Inappropriate 1",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	// Create a shared workspace project with no active agents matching the branch (so it's inappropriate)
	project := &store.Project{
		ID:        tid("proj-inappropriate-1"),
		Name:      "Proj Inappropriate 1",
		Slug:      "proj-inappropriate-1",
		GitRemote: "https://github.com/acme/widgets.git",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
		},
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion /review please!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "coder123",
		},
		"installation": map[string]interface{}{
			"id": 123,
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var finalComment string
	commentMutex.Lock()
	finalComment = commentBody
	commentMutex.Unlock()

	if !strings.Contains(finalComment, "No appropriate Scion project/grove could be found") || !strings.Contains(finalComment, "my-cool-feature-branch") {
		t.Errorf("expected guidance comment containing command info and branch name, got %q", finalComment)
	}
}

func TestHandleGitHubWebhook_FallbackToPRReviewComment(t *testing.T) {
	var commentBody string
	var commentMutex sync.Mutex
	var issuesCalled bool
	var reviewsCalled bool

	expiresAt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/123/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_access_token_abc",
				"expires_at": expiresAt,
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/pulls/101" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 101,
				"head": map[string]interface{}{
					"ref": "my-cool-feature-branch",
				},
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/issues/101/comments" {
			// Fail standard issue comments (representing missing Issues write permission)
			issuesCalled = true
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"message": "Resource not accessible by integration",
			})
			return
		}
		if r.URL.Path == "/repos/acme/widgets/pulls/101/reviews" {
			var reqData map[string]string
			if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			commentMutex.Lock()
			reviewsCalled = true
			commentBody = reqData["body"]
			commentMutex.Unlock()
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 999,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, s := webhookTestServer(t)
	ctx := context.Background()

	pemBytes := generateTestWebhookKey(t)
	instID := int64(123)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	brokerObj := &store.RuntimeBroker{
		ID:     tid("broker-inappropriate-1"),
		Slug:   "broker-inappropriate-1",
		Name:   "Broker Inappropriate 1",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	// Create a shared workspace project with no active agents matching the branch (so it's inappropriate)
	project := &store.Project{
		ID:        tid("proj-inappropriate-1"),
		Name:      "Proj Inappropriate 1",
		Slug:      "proj-inappropriate-1",
		GitRemote: "https://github.com/acme/widgets.git",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
		},
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number": 101,
			"pull_request": map[string]interface{}{
				"url": "https://api.github.com/repos/acme/widgets/pulls/101",
			},
		},
		"comment": map[string]interface{}{
			"id":   111,
			"body": "Hey @scion /review please!",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/widgets",
		},
		"sender": map[string]interface{}{
			"login": "coder123",
		},
		"installation": map[string]interface{}{
			"id": 123,
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if !issuesCalled {
		t.Errorf("expected standard issues comment endpoint to be attempted")
	}

	if !reviewsCalled {
		t.Errorf("expected fallback pulls reviews endpoint to be called when issues comment fails")
	}

	var finalComment string
	commentMutex.Lock()
	finalComment = commentBody
	commentMutex.Unlock()

	if !strings.Contains(finalComment, "No appropriate Scion project/grove could be found") || !strings.Contains(finalComment, "my-cool-feature-branch") {
		t.Errorf("expected fallback review comment containing command info and branch name, got %q", finalComment)
	}
}

func TestHandleGitHubWebhook_PlanCommand(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, s := webhookTestServer(t)
	ctx := context.Background()

	pemBytes := generateTestWebhookKey(t)
	instID := int64(124)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	brokerObj := &store.RuntimeBroker{
		ID:          tid("broker-plan-1"),
		Slug:        "broker-plan-1",
		Name:        "Broker Plan 1",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/plan-widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                     tid("proj-plan-1"),
		Name:                   "Proj Plan 1",
		Slug:                   "proj-plan-1",
		GitRemote:              "https://github.com/acme/plan-widgets.git",
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	if err := s.CreateTemplate(ctx, &store.Template{
		ID: tid("tmpl_default_plan"), Slug: "default", Name: "Default Template",
		Harness: "gemini", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		ContentHash: "abc123",
		Created:     time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("failed to create default template: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerObj.ID,
		BrokerName: brokerObj.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// Payload 1: Issues event ("opened")
	payload := map[string]interface{}{
		"action": "opened",
		"issue": map[string]interface{}{
			"number":   201,
			"html_url": "https://github.com/acme/plan-widgets/issues/201",
			"body":     "Hello @scion /plan refactoring database layers",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/plan-widgets",
		},
		"sender": map[string]interface{}{
			"login": "planner1",
		},
		"installation": map[string]interface{}{
			"id": 124,
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Wait up to 2 seconds for background agent spawning goroutine
	var spawnedAgent *store.Agent
	for i := 0; i < 20; i++ {
		agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
		if err == nil && len(agents.Items) > 0 {
			spawnedAgent = &agents.Items[0]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if spawnedAgent == nil {
		t.Fatalf("expected background goroutine to spawn agent, but none was found")
	}

	if spawnedAgent.Name != "issue-201-agent-planner" {
		t.Errorf("expected agent name 'issue-201-agent-planner', got %q", spawnedAgent.Name)
	}

	if spawnedAgent.Labels["github-action"] != "plan" {
		t.Errorf("expected label github-action='plan', got %q", spawnedAgent.Labels["github-action"])
	}

	if !strings.Contains(spawnedAgent.AppliedConfig.Task, "ONLY plan the changes") || !strings.Contains(spawnedAgent.AppliedConfig.Task, "refactoring database layers") {
		t.Errorf("expected planning specific task description, got %q", spawnedAgent.AppliedConfig.Task)
	}
}

func TestHandleGitHubWebhook_ImplementCommand(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghServer.Close()

	srv, s := webhookTestServer(t)
	ctx := context.Background()

	pemBytes := generateTestWebhookKey(t)
	instID := int64(125)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.config.GitHubAppConfig.APIBaseURL = ghServer.URL
	srv.mu.Unlock()

	brokerObj := &store.RuntimeBroker{
		ID:          tid("broker-implement-1"),
		Slug:        "broker-implement-1",
		Name:        "Broker Implement 1",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/implement-widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                     tid("proj-implement-1"),
		Name:                   "Proj Implement 1",
		Slug:                   "proj-implement-1",
		GitRemote:              "https://github.com/acme/implement-widgets.git",
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	if err := s.CreateTemplate(ctx, &store.Template{
		ID: tid("tmpl_default_implement"), Slug: "default", Name: "Default Template",
		Harness: "gemini", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		ContentHash: "abc123",
		Created:     time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("failed to create default template: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerObj.ID,
		BrokerName: brokerObj.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// Payload 2: Issue comment on a non-PR issue containing @scion and /implement
	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number":       301,
			"html_url":     "https://github.com/acme/implement-widgets/issues/301",
			"pull_request": nil, // Non-PR issue comment!
		},
		"comment": map[string]interface{}{
			"id":   555,
			"body": "Hey @scion /implement the design doc from #201",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/implement-widgets",
		},
		"sender": map[string]interface{}{
			"login": "coder456",
		},
		"installation": map[string]interface{}{
			"id": 125,
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Wait up to 2 seconds for background agent spawning goroutine
	var spawnedAgent *store.Agent
	for i := 0; i < 20; i++ {
		agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
		if err == nil && len(agents.Items) > 0 {
			spawnedAgent = &agents.Items[0]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if spawnedAgent == nil {
		t.Fatalf("expected background goroutine to spawn agent, but none was found")
	}

	if spawnedAgent.Name != "issue-301-agent-implementer" {
		t.Errorf("expected agent name 'issue-301-agent-implementer', got %q", spawnedAgent.Name)
	}

	if spawnedAgent.Labels["github-action"] != "implement" {
		t.Errorf("expected label github-action='implement', got %q", spawnedAgent.Labels["github-action"])
	}

	if !strings.Contains(spawnedAgent.AppliedConfig.Task, "Implement the plan referenced in") || !strings.Contains(spawnedAgent.AppliedConfig.Task, "the design doc from #201") {
		t.Errorf("expected implementation specific task description, got %q", spawnedAgent.AppliedConfig.Task)
	}
}

func TestHandleGitHubWebhook_PlanCommand_ActiveAgent(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	pemBytes := generateTestWebhookKey(t)
	instID := int64(124)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
	srv.mu.Unlock()

	brokerObj := &store.RuntimeBroker{
		ID:          tid("broker-plan-active-1"),
		Slug:        "broker-plan-active-1",
		Name:        "Broker Plan Active 1",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	installation := &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/plan-widgets"},
		Status:         store.GitHubInstallationStatusActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.CreateGitHubInstallation(ctx, installation); err != nil {
		t.Fatalf("failed to create installation: %v", err)
	}

	project := &store.Project{
		ID:                     tid("proj-plan-active-1"),
		Name:                   "Proj Plan Active 1",
		Slug:                   "proj-plan-active-1",
		GitRemote:              "https://github.com/acme/plan-widgets.git",
		GitHubInstallationID:   &instID,
		DefaultRuntimeBrokerID: brokerObj.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	if err := s.CreateTemplate(ctx, &store.Template{
		ID: tid("tmpl_default_plan_active"), Slug: "default", Name: "Default Template",
		Harness: "gemini", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		ContentHash: "abc123",
		Created:     time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("failed to create default template: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerObj.ID,
		BrokerName: brokerObj.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// Pre-create an active running agent labeled with the issue
	activeAgent := &store.Agent{
		ID:        tid("agent-plan-active-1"),
		Slug:      "agent-plan-active-1",
		Name:      "agent-plan-active-1",
		ProjectID: project.ID,
		Template:  "default",
		Phase:     "running",
		Labels: map[string]string{
			"github-pr":    "201",
			"github-issue": "201",
			"github-repo":  "acme/plan-widgets",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, activeAgent); err != nil {
		t.Fatalf("failed to create active agent: %v", err)
	}

	// Payload: Issue comment on issue 201 containing @scion and /plan
	payload := map[string]interface{}{
		"action": "created",
		"issue": map[string]interface{}{
			"number":       201,
			"html_url":     "https://github.com/acme/plan-widgets/issues/201",
			"pull_request": nil,
		},
		"comment": map[string]interface{}{
			"id":   666,
			"body": "Hey @scion /plan please refine to use Postgres",
		},
		"repository": map[string]interface{}{
			"full_name": "acme/plan-widgets",
		},
		"sender": map[string]interface{}{
			"login": "planner1",
		},
		"installation": map[string]interface{}{
			"id": 124,
		},
	}

	payloadBytes := mustJSON(t, payload)
	sig := signWebhookPayload(payloadBytes, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payloadBytes))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify that a message was created and sent to the active agent instead of spawning a new agent
	messages, err := s.ListMessages(ctx, store.MessageFilter{AgentID: activeAgent.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list messages: %v", err)
	}

	if len(messages.Items) == 0 {
		t.Fatalf("expected message to be routed to active agent, but none was found")
	}

	msg := messages.Items[0]
	if !strings.Contains(msg.Msg, "Please refine/revise the plan based on the following feedback: please refine to use Postgres") {
		t.Errorf("expected plan revision instruction in message, got %q", msg.Msg)
	}

	// Ensure no other agent was spawned
	agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list agents: %v", err)
	}
	if len(agents.Items) != 1 {
		t.Errorf("expected exactly 1 agent to exist, but found %d", len(agents.Items))
	}
}
