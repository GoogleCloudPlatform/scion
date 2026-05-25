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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/broker"
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
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["status"] != "pong" {
		t.Errorf("expected pong, got %s", resp["status"])
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
		ID:        "project-1",
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
	updatedProject, err := s.GetProject(ctx, "project-1")
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
		ID:                   "project-1",
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
	updatedProject, _ := s.GetProject(ctx, "project-1")
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
		ID:                   "project-1",
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
	updatedProject, _ := s.GetProject(ctx, "project-1")
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
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["status"] != "ignored" {
		t.Errorf("expected ignored status, got %s", resp["status"])
	}
}

func TestMatchProjectsToInstallation(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	// Create projects with different git remotes
	projects := []*store.Project{
		{ID: "g1", Name: "G1", Slug: "g1", GitRemote: "https://github.com/acme/widgets.git", Created: time.Now(), Updated: time.Now()},
		{ID: "g2", Name: "G2", Slug: "g2", GitRemote: "https://github.com/acme/api.git", Created: time.Now(), Updated: time.Now()},
		{ID: "g3", Name: "G3", Slug: "g3", GitRemote: "https://github.com/other/repo.git", Created: time.Now(), Updated: time.Now()},
		{ID: "g4", Name: "G4", Slug: "g4", Created: time.Now(), Updated: time.Now()}, // No git remote
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
	for _, gID := range []string{"g1", "g2"} {
		project, _ := s.GetProject(ctx, gID)
		if project.GitHubInstallationID == nil {
			t.Errorf("project %s should be associated with installation", gID)
		} else if *project.GitHubInstallationID != 12345 {
			t.Errorf("project %s has wrong installation ID: %d", gID, *project.GitHubInstallationID)
		}
	}

	// Verify non-matching project was NOT associated
	g3, _ := s.GetProject(ctx, "g3")
	if g3.GitHubInstallationID != nil {
		t.Error("project g3 should not be associated")
	}

	// Verify no-remote project was NOT associated
	g4, _ := s.GetProject(ctx, "g4")
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
		ID:                   "g1",
		Name:                 "G1",
		Slug:                 "g1",
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
	updatedProject, _ := s.GetProject(ctx, "g1")
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
		ID:                   "project-event-1",
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
	if updates[0].ID != "project-event-1" {
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
		ID:                   "project-event-2",
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
	if updates[0].ID != "project-event-2" {
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
		ID:        "project-event-3",
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
	if updates[0].ID != "project-event-3" {
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
		ID:        "proj-mention-1",
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
		ID:        "proj-active-1",
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
		ID:        "agent-active-1",
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
	b := broker.NewInProcessBroker(slog.Default())
	srv.StartMessageBroker(b)

	// 4. Subscribe to the agent's message topic on the broker
	msgCh := make(chan *messages.StructuredMessage, 10)
	topic := broker.TopicAgentMessages(project.ID, agent.Slug)
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
		if msg.RecipientID != "agent-active-1" {
			t.Errorf("expected recipient ID 'agent-active-1', got %q", msg.RecipientID)
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
		ID:     "broker-fallback-1",
		Slug:   "broker-fallback-1",
		Name:   "Broker Fallback 1",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, brokerObj); err != nil {
		t.Fatalf("failed to create broker: %v", err)
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
		ID:                     "proj-fallback-1",
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
