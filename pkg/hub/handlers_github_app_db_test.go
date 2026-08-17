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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// tempSettingsHome points config.GetGlobalDir() at a temp directory holding a
// minimal settings.yaml, and returns the path to that file.
func tempSettingsHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	if err := os.WriteFile(settingsPath, []byte("schema_version: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return settingsPath
}

const githubAppUpdateBody = `{"app_id":999,"api_base_url":"https://ghe.example.com/api/v3",` +
	`"webhooks_enabled":true,"installation_url":"https://github.com/apps/x"}`

// In postgres mode the GitHub App admin API must write the DB-owned
// `github_app` section, not settings.yaml. Writing only to the pod-local file
// loses the change on restart and lets any later ApplySnapshot revert it to the
// stale DB value (#1103).
func TestUpdateGitHubApp_PersistsToDBInPostgresMode(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	srv, fakeStore, ops := newTestDBServer(t)

	// The DB already holds a github_app section, as seeded from the image's
	// settings.yaml at boot. private_key_path has no field in the update
	// request, so it exercises the carry-over of an untouched field.
	fakeStore.seed("github_app", json.RawMessage(
		`{"app_id":111,"webhooks_enabled":false,"private_key_path":"/etc/ghapp/key.pem"}`))
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ApplySnapshot(srv, ops.Snapshot())
	if got := srv.config.GitHubAppConfig.AppID; got != 111 {
		t.Fatalf("precondition: want in-memory app_id 111 from DB, got %d", got)
	}

	rr := httptest.NewRecorder()
	srv.handleUpdateGitHubApp(rr, adminRequest(http.MethodPut, "/api/v1/github-app", githubAppUpdateBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("update failed: %d %s", rr.Code, rr.Body.String())
	}

	// The DB section now carries the operator's values.
	rec, err := fakeStore.GetHubSetting(context.Background(), "github_app")
	if err != nil {
		t.Fatalf("github_app row missing: %v", err)
	}
	var got opsettings.GitHubAppSettings
	if err := json.Unmarshal(rec.Value, &got); err != nil {
		t.Fatalf("unmarshal github_app section: %v", err)
	}
	if got.AppID != 999 {
		t.Errorf("DB app_id = %d, want 999", got.AppID)
	}
	if got.APIBaseURL != "https://ghe.example.com/api/v3" {
		t.Errorf("DB api_base_url = %q, want the updated value", got.APIBaseURL)
	}
	if got.InstallationURL != "https://github.com/apps/x" {
		t.Errorf("DB installation_url = %q, want the updated value", got.InstallationURL)
	}
	if got.WebhooksEnabled == nil || !*got.WebhooksEnabled {
		t.Errorf("DB webhooks_enabled = %v, want true", got.WebhooksEnabled)
	}
	// The request carries no private_key_path; the existing value must survive.
	if got.PrivateKeyPath != "/etc/ghapp/key.pem" {
		t.Errorf("DB private_key_path = %q, want the pre-existing value to be preserved", got.PrivateKeyPath)
	}
	if rec.Origin != "managed" {
		t.Errorf("DB origin = %q, want %q", rec.Origin, "managed")
	}
	if rec.UpdatedBy != "admin@example.com" {
		t.Errorf("DB updated_by = %q, want the caller's email", rec.UpdatedBy)
	}

	// An unrelated section change made by another admin or replica triggers a
	// full ApplySnapshot. The operator's change must survive it.
	fakeStore.seed("lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`))
	ops.refreshAndApply(context.Background(), srv)
	if got := srv.config.GitHubAppConfig.AppID; got != 999 {
		t.Errorf("in-memory app_id reverted to %d after unrelated snapshot apply, want 999", got)
	}

	// settings.yaml is not the durable home in postgres mode.
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "app_id") {
		t.Errorf("settings.yaml was written in postgres mode:\n%s", data)
	}
}

// An operator can pre-stage private_key_path in settings.yaml before the App
// itself exists, leaving app_id at 0. ApplySnapshot skips the whole github_app
// block while app_id is 0, so the in-memory PrivateKeyPath is still empty when
// the first PUT arrives. The write must fall back to the snapshot rather than
// persist the empty in-memory value over the pre-staged path (#1103).
func TestUpdateGitHubApp_PreservesPreStagedKeyPathWhenAppIDZero(t *testing.T) {
	tempSettingsHome(t)

	// settings.yaml carries the key path; app_id is not set yet.
	fakeStore := newFakeHubSettingStore()
	fileK := newFileKoanf(t, map[string]interface{}{
		"server.github_app.private_key_path": "/etc/ghapp/prestaged.pem",
	})
	ops := NewOperationalSettings(fakeStore, fileK, emptyKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ApplySnapshot(srv, ops.Snapshot())
	if got := srv.config.GitHubAppConfig.PrivateKeyPath; got != "" {
		t.Fatalf("precondition: ApplySnapshot should skip github_app while app_id is 0, got path %q", got)
	}

	rr := httptest.NewRecorder()
	srv.handleUpdateGitHubApp(rr, adminRequest(http.MethodPut, "/api/v1/github-app", githubAppUpdateBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("update failed: %d %s", rr.Code, rr.Body.String())
	}

	rec, err := fakeStore.GetHubSetting(context.Background(), "github_app")
	if err != nil {
		t.Fatalf("github_app row missing: %v", err)
	}
	var got opsettings.GitHubAppSettings
	if err := json.Unmarshal(rec.Value, &got); err != nil {
		t.Fatalf("unmarshal github_app section: %v", err)
	}
	if got.AppID != 999 {
		t.Errorf("DB app_id = %d, want 999", got.AppID)
	}
	if got.PrivateKeyPath != "/etc/ghapp/prestaged.pem" {
		t.Errorf("DB private_key_path = %q, want the pre-staged path to survive the first write", got.PrivateKeyPath)
	}
}

// failingHubSettingStore stands in for a DB outage: reads work, writes do not.
type failingHubSettingStore struct {
	*fakeHubSettingStore
}

func (f *failingHubSettingStore) UpsertHubSetting(context.Context, string, json.RawMessage, string, int64, string) (*store.HubSetting, error) {
	return nil, errors.New("db unavailable")
}

// A failed DB write must surface as a 500. In postgres mode the DB is the sole
// durable store, so a swallowed failure means the value is reverted at the next
// refresh with the client believing it was saved.
func TestUpdateGitHubApp_DBWriteFailureReturns500(t *testing.T) {
	tempSettingsHome(t)

	failing := &failingHubSettingStore{newFakeHubSettingStore()}
	ops := NewOperationalSettings(failing, emptyKoanf(), emptyKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	rr := httptest.NewRecorder()
	srv.handleUpdateGitHubApp(rr, adminRequest(http.MethodPut, "/api/v1/github-app", githubAppUpdateBody))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the DB write fails, got %d: %s", rr.Code, rr.Body.String())
	}
}

// File/SQLite mode has no OperationalSettings service, so settings.yaml remains
// the durable home for the non-sensitive fields.
func TestUpdateGitHubApp_PersistsToFileWithoutOperationalSettings(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	srv := &Server{}

	rr := httptest.NewRecorder()
	srv.handleUpdateGitHubApp(rr, adminRequest(http.MethodPut, "/api/v1/github-app", githubAppUpdateBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("update failed: %d %s", rr.Code, rr.Body.String())
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "app_id: 999") {
		t.Errorf("expected settings.yaml to contain app_id: 999, got:\n%s", data)
	}
}
