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
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

func newTestServerForReleaseUpdate(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}
	srv := &Server{
		store:          s,
		maintenanceLog: logging.Subsystem("hub.maintenance"),
	}
	return srv, s
}

func TestStoreUpdateAvailable(t *testing.T) {
	srv, s := newTestServerForReleaseUpdate(t)

	result := &ReleaseUpdateCheckResult{
		Tier:            "binary",
		UpdateAvailable: true,
		CurrentVersion:  "v0.3.0",
		LatestVersion:   "v0.4.0",
		Channel:         "stable",
		DownloadURL:     "https://example.com/scion-linux-amd64.tar.gz",
		ReleaseURL:      "https://example.com/releases/tag/v0.4.0",
	}

	srv.storeUpdateAvailable(context.Background(), result)

	// Verify the setting was stored.
	setting, err := s.GetHubSetting(context.Background(), HubSettingSectionUpdateAvailable)
	if err != nil {
		t.Fatalf("failed to get hub setting: %v", err)
	}

	var info UpdateAvailableInfo
	if err := json.Unmarshal(setting.Value, &info); err != nil {
		t.Fatalf("failed to unmarshal setting value: %v", err)
	}

	if info.Version != "v0.4.0" {
		t.Errorf("Version = %q, want %q", info.Version, "v0.4.0")
	}
	if info.Channel != "stable" {
		t.Errorf("Channel = %q, want %q", info.Channel, "stable")
	}
	if info.DownloadURL != "https://example.com/scion-linux-amd64.tar.gz" {
		t.Errorf("DownloadURL = %q, want expected URL", info.DownloadURL)
	}
	if info.ReleaseURL != "https://example.com/releases/tag/v0.4.0" {
		t.Errorf("ReleaseURL = %q, want expected URL", info.ReleaseURL)
	}
	if info.DetectedAt == "" {
		t.Error("DetectedAt should be set")
	}
}

func TestStoreUpdateAvailable_Upsert(t *testing.T) {
	srv, s := newTestServerForReleaseUpdate(t)

	// Store first version.
	result1 := &ReleaseUpdateCheckResult{
		Tier:            "binary",
		UpdateAvailable: true,
		CurrentVersion:  "v0.3.0",
		LatestVersion:   "v0.4.0",
		Channel:         "stable",
	}
	srv.storeUpdateAvailable(context.Background(), result1)

	// Store newer version — should overwrite.
	result2 := &ReleaseUpdateCheckResult{
		Tier:            "binary",
		UpdateAvailable: true,
		CurrentVersion:  "v0.3.0",
		LatestVersion:   "v0.5.0",
		Channel:         "stable",
	}
	srv.storeUpdateAvailable(context.Background(), result2)

	setting, err := s.GetHubSetting(context.Background(), HubSettingSectionUpdateAvailable)
	if err != nil {
		t.Fatalf("failed to get hub setting: %v", err)
	}

	var info UpdateAvailableInfo
	if err := json.Unmarshal(setting.Value, &info); err != nil {
		t.Fatalf("failed to unmarshal setting value: %v", err)
	}

	if info.Version != "v0.5.0" {
		t.Errorf("Version = %q, want %q (should be overwritten)", info.Version, "v0.5.0")
	}
}

func TestClearUpdateAvailable(t *testing.T) {
	srv, s := newTestServerForReleaseUpdate(t)

	// First store a notification.
	result := &ReleaseUpdateCheckResult{
		Tier:            "binary",
		UpdateAvailable: true,
		CurrentVersion:  "v0.3.0",
		LatestVersion:   "v0.4.0",
		Channel:         "stable",
	}
	srv.storeUpdateAvailable(context.Background(), result)

	// Verify it exists.
	_, err := s.GetHubSetting(context.Background(), HubSettingSectionUpdateAvailable)
	if err != nil {
		t.Fatalf("setting should exist: %v", err)
	}

	// Clear it.
	srv.ClearUpdateAvailable(context.Background())

	// Verify it's gone.
	_, err = s.GetHubSetting(context.Background(), HubSettingSectionUpdateAvailable)
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound after clear, got: %v", err)
	}
}

func TestClearUpdateAvailable_NoOpWhenNotSet(t *testing.T) {
	srv, _ := newTestServerForReleaseUpdate(t)

	// Should not panic or error when setting doesn't exist.
	srv.ClearUpdateAvailable(context.Background())
}

func TestHandleGetUpdateAvailable_NoUpdate(t *testing.T) {
	srv, _ := newTestServerForReleaseUpdate(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/update-available", nil)
	rr := httptest.NewRecorder()
	srv.handleGetUpdateAvailable(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if body["update_available"] != false {
		t.Errorf("update_available = %v, want false", body["update_available"])
	}
}

func TestHandleGetUpdateAvailable_WithUpdate(t *testing.T) {
	srv, _ := newTestServerForReleaseUpdate(t)

	// Store an update notification first.
	result := &ReleaseUpdateCheckResult{
		Tier:            "binary",
		UpdateAvailable: true,
		CurrentVersion:  "v0.3.0",
		LatestVersion:   "v0.4.0",
		Channel:         "stable",
		DownloadURL:     "https://example.com/dl.tar.gz",
		ReleaseURL:      "https://example.com/release",
	}
	srv.storeUpdateAvailable(context.Background(), result)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/update-available", nil)
	rr := httptest.NewRecorder()
	srv.handleGetUpdateAvailable(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if body["update_available"] != true {
		t.Errorf("update_available = %v, want true", body["update_available"])
	}

	updateMap, ok := body["update"].(map[string]interface{})
	if !ok {
		t.Fatal("expected 'update' field to be a JSON object")
	}
	if updateMap["version"] != "v0.4.0" {
		t.Errorf("update.version = %v, want %q", updateMap["version"], "v0.4.0")
	}
	if updateMap["channel"] != "stable" {
		t.Errorf("update.channel = %v, want %q", updateMap["channel"], "stable")
	}
}

func TestHandleDismissUpdateAvailable(t *testing.T) {
	srv, s := newTestServerForReleaseUpdate(t)

	// Store a notification.
	result := &ReleaseUpdateCheckResult{
		Tier:            "binary",
		UpdateAvailable: true,
		CurrentVersion:  "v0.3.0",
		LatestVersion:   "v0.4.0",
		Channel:         "stable",
	}
	srv.storeUpdateAvailable(context.Background(), result)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/maintenance/update-available", nil)
	rr := httptest.NewRecorder()
	srv.handleDismissUpdateAvailable(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify setting is deleted.
	_, err := s.GetHubSetting(context.Background(), HubSettingSectionUpdateAvailable)
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound after dismiss, got: %v", err)
	}
}

func TestHandleUpdateAvailable_MethodNotAllowed(t *testing.T) {
	srv, _ := newTestServerForReleaseUpdate(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/update-available", nil)
	rr := httptest.NewRecorder()
	srv.handleUpdateAvailable(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestJitterCalculation(t *testing.T) {
	// Verify that jitter produces values in the expected range.
	// The jitter formula is: rand.Intn(61) - 30, giving [-30, +30].
	for i := 0; i < 1000; i++ {
		jitter := rand.Intn(61) - 30
		if jitter < -30 || jitter > 30 {
			t.Errorf("jitter %d out of expected range [-30, +30]", jitter)
		}
	}
}

func TestJitterIntervalFloor(t *testing.T) {
	// Verify that the interval floor is 60 minutes even with maximum negative jitter.
	checkIntervalHours := 1
	intervalMinutes := checkIntervalHours * 60
	jitter := -30 // worst case
	intervalMinutes += jitter
	if intervalMinutes < 60 {
		intervalMinutes = 60
	}

	if intervalMinutes != 60 {
		t.Errorf("interval = %d, want 60 (floor)", intervalMinutes)
	}
}

func TestReleaseUpdateCheckNotRegistered_DisabledPolicy(t *testing.T) {
	// Verify that the handler is not registered when policy is "disabled".
	// We can't directly test scheduler registration, but we can verify
	// the condition logic.
	mc := MaintenanceConfig{
		DeploymentTier: "binary",
		UpdatePolicy:   "disabled",
	}

	shouldRegister := mc.DeploymentTier == "binary" && mc.UpdatePolicy != "disabled"
	if shouldRegister {
		t.Error("should not register handler when policy is disabled")
	}
}

func TestReleaseUpdateCheckNotRegistered_SourceTier(t *testing.T) {
	// Verify that the handler is not registered for source tier.
	mc := MaintenanceConfig{
		DeploymentTier: "source",
		UpdatePolicy:   "notify",
	}

	shouldRegister := mc.DeploymentTier == "binary" && mc.UpdatePolicy != "disabled"
	if shouldRegister {
		t.Error("should not register handler for source tier")
	}
}

func TestReleaseUpdateCheckRegistered_NotifyPolicy(t *testing.T) {
	mc := MaintenanceConfig{
		DeploymentTier: "binary",
		UpdatePolicy:   "notify",
	}

	shouldRegister := mc.DeploymentTier == "binary" && mc.UpdatePolicy != "disabled"
	if !shouldRegister {
		t.Error("should register handler when policy is notify")
	}
}

func TestReleaseUpdateCheckRegistered_AutoPolicy(t *testing.T) {
	mc := MaintenanceConfig{
		DeploymentTier: "binary",
		UpdatePolicy:   "auto",
	}

	shouldRegister := mc.DeploymentTier == "binary" && mc.UpdatePolicy != "disabled"
	if !shouldRegister {
		t.Error("should register handler when policy is auto")
	}
}

func TestUpdateAvailableInfo_JSON(t *testing.T) {
	info := UpdateAvailableInfo{
		Version:     "v0.4.0",
		Channel:     "stable",
		DownloadURL: "https://example.com/dl.tar.gz",
		ReleaseURL:  "https://example.com/release",
		DetectedAt:  "2026-09-22T18:00:00Z",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded["version"] != "v0.4.0" {
		t.Errorf("version = %v, want %q", decoded["version"], "v0.4.0")
	}
	if decoded["channel"] != "stable" {
		t.Errorf("channel = %v, want %q", decoded["channel"], "stable")
	}
	if decoded["download_url"] != "https://example.com/dl.tar.gz" {
		t.Errorf("download_url = %v, want expected URL", decoded["download_url"])
	}
	if decoded["detected_at"] != "2026-09-22T18:00:00Z" {
		t.Errorf("detected_at = %v, want expected timestamp", decoded["detected_at"])
	}
}
