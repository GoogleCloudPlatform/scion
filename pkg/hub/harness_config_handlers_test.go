//go:build !no_sqlite

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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestHarnessConfigList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:      tid("hc_test1"),
		Slug:    "test-hc",
		Name:    "Test HC",
		Harness: "claude",
		Scope:   "global",
		Status:  store.HarnessConfigStatusActive,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/harness-configs", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListHarnessConfigsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.HarnessConfigs) != 1 {
		t.Errorf("expected 1 harness config, got %d", len(resp.HarnessConfigs))
	}
}

func TestHarnessConfigListByProjectID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create a global harness config
	if err := s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("hc_global1"), Slug: "global-hc", Name: "Global HC",
		Harness: "claude", Scope: "global",
		Status:  store.HarnessConfigStatusActive,
		Created: now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create global harness config: %v", err)
	}

	// Create a project-scoped harness config for project "project_abc"
	if err := s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("hc_project1"), Slug: "project-hc", Name: "Project HC",
		Harness: "gemini", Scope: "project", ScopeID: tid("project_abc"),
		Status:  store.HarnessConfigStatusActive,
		Created: now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create project harness config: %v", err)
	}

	// Create a project-scoped harness config for a different project
	if err := s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("hc_project2"), Slug: "other-project-hc", Name: "Other Project HC",
		Harness: "claude", Scope: "project", ScopeID: tid("project_xyz"),
		Status:  store.HarnessConfigStatusActive,
		Created: now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create other project harness config: %v", err)
	}

	// Create a user-scoped harness config
	if err := s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("hc_user1"), Slug: "user-hc", Name: "User HC",
		Harness: "claude", Scope: "user", ScopeID: tid("user_123"),
		Status:  store.HarnessConfigStatusActive,
		Created: now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create user harness config: %v", err)
	}

	// Query with projectId=project_abc should return global + project_abc configs only
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/harness-configs?projectId=%s", tid("project_abc")), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListHarnessConfigsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.TotalCount != 2 {
		t.Errorf("expected 2 harness configs (global + project_abc), got %d", resp.TotalCount)
	}

	// Verify we got the right configs
	ids := map[string]bool{}
	for _, hc := range resp.HarnessConfigs {
		ids[hc.ID] = true
	}
	if !ids[tid("hc_global1")] {
		t.Error("expected global harness config in results")
	}
	if !ids[tid("hc_project1")] {
		t.Error("expected project_abc harness config in results")
	}
	if ids[tid("hc_project2")] {
		t.Error("did not expect project_xyz harness config in results")
	}
	if ids[tid("hc_user1")] {
		t.Error("did not expect user harness config in results")
	}
}

// TestHarnessConfigListByScopeAndProject verifies that combining scope=project
// with a projectId narrows results to that single project (the case used by the
// web resource list). Without the dedicated filter branch this would return
// every project's configs.
func TestHarnessConfigListByScopeAndProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	now := time.Now()

	for _, hc := range []*store.HarnessConfig{
		{ID: tid("hc_g"), Slug: "g", Name: "G", Harness: "claude", Scope: "global",
			Status: store.HarnessConfigStatusActive, Created: now, Updated: now},
		{ID: tid("hc_a"), Slug: "a", Name: "A", Harness: "claude", Scope: "project", ScopeID: tid("project_abc"),
			Status: store.HarnessConfigStatusActive, Created: now, Updated: now},
		{ID: tid("hc_b"), Slug: "b", Name: "B", Harness: "claude", Scope: "project", ScopeID: tid("project_xyz"),
			Status: store.HarnessConfigStatusActive, Created: now, Updated: now},
	} {
		if err := s.CreateHarnessConfig(ctx, hc); err != nil {
			t.Fatalf("failed to create harness config %s: %v", hc.ID, err)
		}
	}

	// scope=project + projectId should return only that project's configs.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/harness-configs?scope=project&projectId="+tid("project_abc"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp ListHarnessConfigsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.HarnessConfigs) != 1 || resp.HarnessConfigs[0].ID != tid("hc_a") {
		ids := make([]string, len(resp.HarnessConfigs))
		for i, hc := range resp.HarnessConfigs {
			ids[i] = hc.ID
		}
		t.Errorf("expected only [%s], got %v", tid("hc_a"), ids)
	}
}

func TestHarnessConfigCreate(t *testing.T) {
	srv, _ := testServer(t)

	body := map[string]interface{}{
		"slug":    "new-hc",
		"name":    "New HC",
		"harness": "claude",
		"scope":   "global",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateHarnessConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.HarnessConfig == nil {
		t.Fatalf("expected harness config in response, got nil")
	}

	if resp.HarnessConfig.Slug != "new-hc" {
		t.Errorf("expected slug 'new-hc', got %q", resp.HarnessConfig.Slug)
	}

	if resp.HarnessConfig.Status != store.HarnessConfigStatusActive {
		t.Errorf("expected status 'active' (no files), got %q", resp.HarnessConfig.Status)
	}
}

func TestHarnessConfigGet(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:      tid("hc_get1"),
		Slug:    "get-test",
		Name:    "Get Test",
		Harness: "gemini",
		Scope:   "global",
		Status:  store.HarnessConfigStatusActive,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/harness-configs/%s", tid("hc_get1")), nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result store.HarnessConfig
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result.Name != "Get Test" {
		t.Errorf("expected name 'Get Test', got %q", result.Name)
	}
	if result.Harness != "gemini" {
		t.Errorf("expected harness 'gemini', got %q", result.Harness)
	}
}

func TestHarnessConfigDelete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:      tid("hc_del1"),
		Slug:    "del-test",
		Name:    "Del Test",
		Harness: "claude",
		Scope:   "global",
		Status:  store.HarnessConfigStatusActive,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/harness-configs/%s", tid("hc_del1")), nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify deleted
	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/harness-configs/%s", tid("hc_del1")), nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status 404 after delete, got %d", rec.Code)
	}
}

func TestHarnessConfigPatch(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:      tid("hc_patch1"),
		Slug:    "patch-test",
		Name:    "Patch Test",
		Harness: "claude",
		Scope:   "global",
		Status:  store.HarnessConfigStatusActive,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}

	body := map[string]interface{}{
		"displayName": "Updated Display Name",
		"description": "Updated description",
	}

	rec := doRequest(t, srv, http.MethodPatch, fmt.Sprintf("/api/v1/harness-configs/%s", tid("hc_patch1")), body)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result store.HarnessConfig
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result.DisplayName != "Updated Display Name" {
		t.Errorf("expected display name 'Updated Display Name', got %q", result.DisplayName)
	}
	if result.Description != "Updated description" {
		t.Errorf("expected description 'Updated description', got %q", result.Description)
	}
}

// createHarnessConfigWithContent stores a harness config whose content hash
// and file manifest were set by the server.
func createHarnessConfigWithContent(t *testing.T, s store.Store, id string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{
		ID:          tid(id),
		Slug:        id,
		Name:        "Content Test",
		Description: "original description",
		Harness:     "claude",
		Config:      &store.HarnessConfigData{Harness: "claude", Image: "original-image:1"},
		Scope:       "global",
		Status:      store.HarnessConfigStatusActive,
		ContentHash: "sha256:server-computed",
		Files: []store.TemplateFile{
			{Path: "config.yaml", Size: 42, Hash: "sha256:config", Mode: "0644"},
			{Path: "home/.bashrc", Size: 7, Hash: "sha256:bashrc", Mode: "0644"},
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateHarnessConfig(context.Background(), hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}
	return hc
}

func assertHarnessConfigContentEqual(t *testing.T, label string, want, got *store.HarnessConfig) {
	t.Helper()
	if got.ContentHash != want.ContentHash {
		t.Errorf("%s content hash = %q, want %q", label, got.ContentHash, want.ContentHash)
	}
	if len(got.Files) != len(want.Files) {
		t.Fatalf("%s files = %+v, want %+v", label, got.Files, want.Files)
	}
	for i := range want.Files {
		if got.Files[i] != want.Files[i] {
			t.Errorf("%s files[%d] = %+v, want %+v", label, i, got.Files[i], want.Files[i])
		}
	}
}

// TestHarnessConfigUpdate_PreservesContentHashAndFiles verifies that the
// update handler keeps the server-computed content hash and file list from
// the existing record when the request body carries different values.
func TestHarnessConfigUpdate_PreservesContentHashAndFiles(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	hc := createHarnessConfigWithContent(t, s, "hc-update-content")

	body := store.HarnessConfig{
		Name:        hc.Name,
		Slug:        hc.Slug,
		Harness:     hc.Harness,
		Status:      hc.Status,
		ContentHash: "sha256:request-body",
		Files: []store.TemplateFile{
			{Path: "other.yaml", Size: 1, Hash: "sha256:other", Mode: "0600"},
		},
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/harness-configs/"+hc.ID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var returned store.HarnessConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &returned); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	assertHarnessConfigContentEqual(t, "returned", hc, &returned)

	stored, err := s.GetHarnessConfig(ctx, hc.ID)
	if err != nil {
		t.Fatalf("failed to get harness config: %v", err)
	}
	assertHarnessConfigContentEqual(t, "stored", hc, stored)
}

// TestHarnessConfigUpdate_AppliesUpdatableFields verifies that descriptive
// fields and config still update through the update handler.
func TestHarnessConfigUpdate_AppliesUpdatableFields(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	hc := createHarnessConfigWithContent(t, s, "hc-update-fields")

	body := store.HarnessConfig{
		Name:        "Renamed Config",
		Slug:        hc.Slug,
		DisplayName: "Renamed Display",
		Description: "updated description",
		Harness:     hc.Harness,
		Config:      &store.HarnessConfigData{Harness: "claude", Image: "updated-image:2"},
		Status:      hc.Status,
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/harness-configs/"+hc.ID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	stored, err := s.GetHarnessConfig(ctx, hc.ID)
	if err != nil {
		t.Fatalf("failed to get harness config: %v", err)
	}
	if stored.Name != "Renamed Config" {
		t.Errorf("name = %q, want %q", stored.Name, "Renamed Config")
	}
	if stored.DisplayName != "Renamed Display" {
		t.Errorf("display name = %q, want %q", stored.DisplayName, "Renamed Display")
	}
	if stored.Description != "updated description" {
		t.Errorf("description = %q, want %q", stored.Description, "updated description")
	}
	if stored.Config == nil || stored.Config.Image != "updated-image:2" {
		t.Errorf("config = %+v, want image %q", stored.Config, "updated-image:2")
	}
	assertHarnessConfigContentEqual(t, "stored", hc, stored)
}

// putHarnessConfig sends an update body and returns the stored record.
func putHarnessConfig(t *testing.T, srv *Server, s store.Store, id string, body store.HarnessConfig) *store.HarnessConfig {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/harness-configs/"+id, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := s.GetHarnessConfig(context.Background(), id)
	if err != nil {
		t.Fatalf("failed to get harness config: %v", err)
	}
	return stored
}

// TestHarnessConfigUpdate_PreservesStatus verifies that the update handler
// keeps the stored lifecycle status of a pending harness config.
func TestHarnessConfigUpdate_PreservesStatus(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:      tid("hc-update-status"),
		Slug:    "hc-update-status",
		Name:    "Status Test",
		Harness: "claude",
		Scope:   "global",
		Status:  store.HarnessConfigStatusPending,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}

	stored := putHarnessConfig(t, srv, s, hc.ID, store.HarnessConfig{
		Name:        hc.Name,
		Slug:        hc.Slug,
		Harness:     hc.Harness,
		Description: "updated description",
		Status:      store.HarnessConfigStatusActive,
	})
	if stored.Status != store.HarnessConfigStatusPending {
		t.Errorf("status = %q, want %q", stored.Status, store.HarnessConfigStatusPending)
	}
	if stored.Description != "updated description" {
		t.Errorf("description = %q, want %q", stored.Description, "updated description")
	}
}

// TestHarnessConfigUpdate_PreservesImageStatus verifies that the update
// handler keeps the stored image status and its check timestamp.
func TestHarnessConfigUpdate_PreservesImageStatus(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	checkedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	hc := &store.HarnessConfig{
		ID:      tid("hc-update-image-status"),
		Slug:    "hc-update-image-status",
		Name:    "Image Status Test",
		Harness: "claude",
		Scope:   "global",
		Status:  store.HarnessConfigStatusActive,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}
	if err := s.UpdateHarnessConfigImageStatus(ctx, hc.ID, store.HarnessConfigImageStatusInvalid, checkedAt); err != nil {
		t.Fatalf("failed to set image status: %v", err)
	}

	requestCheckedAt := time.Date(2030, 6, 7, 8, 9, 10, 0, time.UTC)
	stored := putHarnessConfig(t, srv, s, hc.ID, store.HarnessConfig{
		Name:                 hc.Name,
		Slug:                 hc.Slug,
		Harness:              hc.Harness,
		Status:               hc.Status,
		ImageStatus:          store.HarnessConfigImageStatusValid,
		ImageStatusCheckedAt: &requestCheckedAt,
	})
	if stored.ImageStatus != store.HarnessConfigImageStatusInvalid {
		t.Errorf("image status = %q, want %q", stored.ImageStatus, store.HarnessConfigImageStatusInvalid)
	}
	if stored.ImageStatusCheckedAt == nil || !stored.ImageStatusCheckedAt.Equal(checkedAt) {
		t.Errorf("image status checked at = %v, want %v", stored.ImageStatusCheckedAt, checkedAt)
	}
}

// TestHarnessConfigUpdate_RequiresIdentity verifies that the update handler
// responds 401 and leaves the stored record unchanged when the request
// context carries no identity.
func TestHarnessConfigUpdate_RequiresIdentity(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	hc := createHarnessConfigWithContent(t, s, "hc-update-identity")

	body, err := json.Marshal(store.HarnessConfig{
		Name:        "Renamed Without Identity",
		Slug:        hc.Slug,
		Harness:     hc.Harness,
		Description: "updated without identity",
		Status:      hc.Status,
	})
	if err != nil {
		t.Fatalf("failed to marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/harness-configs/"+hc.ID, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.updateHarnessConfig(rec, req, hc)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := s.GetHarnessConfig(ctx, hc.ID)
	if err != nil {
		t.Fatalf("failed to get harness config: %v", err)
	}
	if stored.Name != hc.Name {
		t.Errorf("name = %q, want %q", stored.Name, hc.Name)
	}
	if stored.Description != hc.Description {
		t.Errorf("description = %q, want %q", stored.Description, hc.Description)
	}
}

// TestHarnessConfigUpdate_RecordsAuthenticatedUpdater verifies that the
// update handler records the authenticated caller as the updater.
func TestHarnessConfigUpdate_RecordsAuthenticatedUpdater(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:        tid("hc-update-updater"),
		Slug:      "hc-update-updater",
		Name:      "Updater Test",
		Harness:   "claude",
		Scope:     "global",
		Status:    store.HarnessConfigStatusActive,
		UpdatedBy: "previous-updater",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}

	stored := putHarnessConfig(t, srv, s, hc.ID, store.HarnessConfig{
		Name:      hc.Name,
		Slug:      hc.Slug,
		Harness:   hc.Harness,
		Status:    hc.Status,
		UpdatedBy: "request-body-updater",
	})
	if stored.UpdatedBy != DevUserID {
		t.Errorf("updated by = %q, want authenticated caller %q", stored.UpdatedBy, DevUserID)
	}
}

// TestHandleHarnessConfigFinalize_PersistsModelAliases is a regression test
// for ptone/scion#2365 review round 1 (R2): the production record that
// triggered the bug was written through the push/finalize path
// (handleHarnessConfigFinalize), not the directory-bootstrap sync covered
// by TestBootstrapHarnessConfigsFromDir_PersistsModelAliases. Without this
// test, a regression in the finalize handler specifically would go
// unnoticed because the read-time backfill would silently mask it (aliases
// would still resolve correctly, just via an extra storage download on
// every create instead of the already-stamped record).
func TestHandleHarnessConfigFinalize_PersistsModelAliases(t *testing.T) {
	srv, s, _ := testHarnessConfigFileServer(t)
	ctx := context.Background()

	hc := createTestHarnessConfigWithFiles(t, s, nil, nil)
	stor := srv.GetStorage().(*contentMockStorage)

	configYAML := "harness: codex\nmodel_aliases:\n  small: tiny-model\n  large: finalize-large-model\n"
	objectPath := hc.StoragePath + "/config.yaml"
	stor.content[objectPath] = []byte(configYAML)
	stor.objects[objectPath] = &storage.Object{Name: objectPath, Size: int64(len(configYAML))}

	body := map[string]interface{}{
		"manifest": map[string]interface{}{
			"files": []map[string]interface{}{
				{"path": "config.yaml", "size": len(configYAML), "hash": "sha256:placeholder"},
			},
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/finalize", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	updated, err := s.GetHarnessConfig(ctx, hc.ID)
	if err != nil {
		t.Fatalf("failed to get updated harness config: %v", err)
	}
	if updated.Config == nil || updated.Config.ModelAliases["large"] != "finalize-large-model" {
		t.Errorf("expected Config.ModelAliases[large] = %q after finalize, got %+v", "finalize-large-model", updated.Config)
	}
}

// finalizeHarnessConfigWith posts a finalize request whose manifest lists
// paths, and fails the test unless it succeeds.
func finalizeHarnessConfigWith(t *testing.T, srv *Server, hcID string, paths ...string) {
	t.Helper()
	files := make([]map[string]interface{}, 0, len(paths))
	for _, p := range paths {
		files = append(files, map[string]interface{}{"path": p, "size": 1, "hash": "sha256:placeholder"})
	}
	body := map[string]interface{}{"manifest": map[string]interface{}{"files": files}}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hcID+"/finalize", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleHarnessConfigFinalize_DeletesFilesMissingFromManifest verifies
// that finalize removes the storage objects of files the previous record
// listed but the manifest does not (files deleted locally before a sync,
// ptone/scion#3161). Brokers with local storage hydrate the whole storage
// directory, so a stale object would still reach agents if only the record's
// file list were updated.
func TestHandleHarnessConfigFinalize_DeletesFilesMissingFromManifest(t *testing.T) {
	srv, s, stor := testHarnessConfigFileServer(t)

	hc := createTestHarnessConfigWithFiles(t, s, stor, map[string]string{
		"config.yaml":          "harness: claude\n",
		"removed.yaml":         "gone: true\n",
		"scripts/provision.py": "print()\n",
	})

	finalizeHarnessConfigWith(t, srv, hc.ID, "config.yaml")

	for _, gone := range []string{"removed.yaml", "scripts/provision.py"} {
		if _, ok := stor.objects[hc.StoragePath+"/"+gone]; ok {
			t.Errorf("expected %s to be deleted from storage after finalize", gone)
		}
	}
	if _, ok := stor.objects[hc.StoragePath+"/config.yaml"]; !ok {
		t.Error("expected config.yaml to be kept in storage after finalize")
	}
}

// TestHandleHarnessConfigFinalize_KeepsObjectsNotInPreviousRecord verifies
// that finalize deletes only files the previous record listed, never other
// objects under the storage path: clones live at <scope>/<slug>/<cloneID>,
// and a slug rename leaves StoragePath unchanged, so another config's objects
// can sit below this one's storage path.
func TestHandleHarnessConfigFinalize_KeepsObjectsNotInPreviousRecord(t *testing.T) {
	srv, s, stor := testHarnessConfigFileServer(t)

	hc := createTestHarnessConfigWithFiles(t, s, stor, map[string]string{
		"config.yaml": "harness: claude\n",
	})
	nested := hc.StoragePath + "/hc-other-id/config.yaml"
	stor.content[nested] = []byte("harness: codex\n")
	stor.objects[nested] = &storage.Object{Name: nested, Size: 15}

	finalizeHarnessConfigWith(t, srv, hc.ID, "config.yaml")

	if _, ok := stor.objects[nested]; !ok {
		t.Errorf("expected %s (another config's object) to survive finalize", nested)
	}
	if _, ok := stor.objects[hc.StoragePath+"/config.yaml"]; !ok {
		t.Error("expected config.yaml to be kept in storage after finalize")
	}
}

// localStorageHarnessConfig sets up a server backed by real local storage
// (rooted at a bucket directory below root) and a harness-config record whose
// file list is recordPaths. Every path in storedPaths is written to storage
// below the config's storage path. Local storage resolves object paths with
// filepath.Join, so it shows what a path really points at on disk.
func localStorageHarnessConfig(t *testing.T, recordPaths, storedPaths []string) (srv *Server, s store.Store, hc *store.HarnessConfig, root string) {
	t.Helper()
	srv, s, _ = testHarnessConfigFileServer(t)
	root = t.TempDir()
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: root})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	srv.SetStorage(stor)

	hc = &store.HarnessConfig{
		ID:            tid("hc-local-paths"),
		Name:          "test-hc",
		Slug:          "test-hc",
		Harness:       "claude",
		Scope:         store.HarnessConfigScopeGlobal,
		Status:        store.HarnessConfigStatusActive,
		StoragePath:   "harness-configs/global/test-hc",
		StorageBucket: "b",
	}
	for _, p := range storedPaths {
		if _, err := stor.Upload(context.Background(), hc.StoragePath+"/"+p, strings.NewReader("x\n"), storage.UploadOptions{}); err != nil {
			t.Fatalf("upload %s: %v", p, err)
		}
	}
	for _, p := range recordPaths {
		hc.Files = append(hc.Files, store.TemplateFile{Path: p, Size: 2, Hash: "sha256:placeholder"})
	}
	hc.ContentHash = computeContentHash(hc.Files)
	if err := s.CreateHarnessConfig(context.Background(), hc); err != nil {
		t.Fatalf("CreateHarnessConfig: %v", err)
	}
	return srv, s, hc, root
}

func finalizeHarnessConfigRequest(t *testing.T, srv *Server, hcID string, paths ...string) int {
	t.Helper()
	files := make([]map[string]interface{}, 0, len(paths))
	for _, p := range paths {
		files = append(files, map[string]interface{}{"path": p, "size": 2, "hash": "sha256:placeholder"})
	}
	body := map[string]interface{}{"manifest": map[string]interface{}{"files": files}}
	return doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hcID+"/finalize", body).Code
}

// outsidePath is four levels up from harness-configs/global/test-hc in bucket
// "b", i.e. the storage root's parent directory, outside the bucket.
const outsidePath = "../../../../outside.txt"

func writeOutsideFile(t *testing.T, root string) string {
	t.Helper()
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("keep\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return outside
}

// TestHandleHarnessConfigFinalize_RejectsNonCanonicalPaths verifies that
// finalize rejects manifest paths that do not name exactly one object below
// the config's storage path: parent-directory references, aliases of a
// canonical path, absolute paths and backslashes.
func TestHandleHarnessConfigFinalize_RejectsNonCanonicalPaths(t *testing.T) {
	for _, bad := range []string{
		outsidePath,
		"./config.yaml",
		"scripts//provision.py",
		"scripts/../config.yaml",
		"..",
		"../x",
		"scripts/",
		"/config.yaml",
		`scripts\provision.py`,
		".",
	} {
		t.Run(bad, func(t *testing.T) {
			// Every rejected path names an existing object or directory, so
			// only the path check (not the storage existence check) can
			// reject it.
			srv, s, hc, root := localStorageHarnessConfig(t, []string{"config.yaml"},
				[]string{"config.yaml", "../x", "scripts/provision.py", `scripts\provision.py`})
			writeOutsideFile(t, root)

			if code := finalizeHarnessConfigRequest(t, srv, hc.ID, "config.yaml", bad); code != http.StatusBadRequest {
				t.Fatalf("expected 400 for manifest path %q, got %d", bad, code)
			}
			got, err := s.GetHarnessConfig(context.Background(), hc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Files) != 1 || got.Files[0].Path != "config.yaml" {
				t.Errorf("record must be unchanged after a rejected finalize, got %+v", got.Files)
			}
		})
	}

	// Names that merely start with ".." are ordinary file names.
	for _, ok := range []string{"..x", "scripts/..x"} {
		t.Run("allowed "+ok, func(t *testing.T) {
			srv, s, hc, _ := localStorageHarnessConfig(t, []string{"config.yaml"},
				[]string{"config.yaml", ok})

			if code := finalizeHarnessConfigRequest(t, srv, hc.ID, "config.yaml", ok); code != http.StatusOK {
				t.Fatalf("expected 200 for manifest path %q, got %d", ok, code)
			}
			got, err := s.GetHarnessConfig(context.Background(), hc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Files) != 2 || got.Files[1].Path != ok {
				t.Errorf("expected record to list config.yaml and %q, got %+v", ok, got.Files)
			}
		})
	}
}

// TestHandleHarnessConfigFinalize_KeepsFilesOutsideStoragePath is the
// end-to-end case: a manifest listing a parent-directory path, followed by
// one that drops it, must not delete a file outside the storage root.
func TestHandleHarnessConfigFinalize_KeepsFilesOutsideStoragePath(t *testing.T) {
	srv, _, hc, root := localStorageHarnessConfig(t, []string{"config.yaml"}, []string{"config.yaml"})
	outside := writeOutsideFile(t, root)

	_ = finalizeHarnessConfigRequest(t, srv, hc.ID, "config.yaml", outsidePath)
	if code := finalizeHarnessConfigRequest(t, srv, hc.ID, "config.yaml"); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("file outside storage must survive finalize: %v", err)
	}
}

// TestHandleHarnessConfigFinalize_SkipsUnsafePathsInOldRecord covers records
// written before manifest paths were validated: dropping a parent-directory
// path or an alias of a kept file must not delete anything.
func TestHandleHarnessConfigFinalize_SkipsUnsafePathsInOldRecord(t *testing.T) {
	srv, _, hc, root := localStorageHarnessConfig(t,
		[]string{"config.yaml", outsidePath, "./config.yaml"}, []string{"config.yaml"})
	outside := writeOutsideFile(t, root)

	if code := finalizeHarnessConfigRequest(t, srv, hc.ID, "config.yaml"); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("file outside storage must survive finalize: %v", err)
	}
	kept := filepath.Join(root, "b", filepath.FromSlash(hc.StoragePath), "config.yaml")
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("config.yaml is still listed and must survive dropping its alias ./config.yaml: %v", err)
	}
}

func TestHarnessConfigExposesCapabilities(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:      tid("hc_caps1"),
		Slug:    "caps-hc",
		Name:    "Caps HC",
		Harness: "claude",
		Scope:   "global",
		Status:  store.HarnessConfigStatusActive,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}

	// GET exposes per-item capabilities (dev token is admin → all actions).
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/harness-configs/"+tid("hc_caps1"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got HarnessConfigWithCapabilities
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if got.Cap == nil {
		t.Fatalf("expected _capabilities on GET response, got nil")
	}
	if !hasAction(got.Cap, ActionUpdate) {
		t.Errorf("expected admin to have update capability, got %v", got.Cap.Actions)
	}

	// List exposes per-item and scope capabilities.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/harness-configs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var listResp ListHarnessConfigsResponse
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("failed to decode list response: %v", err)
	}
	if listResp.Capabilities == nil || !hasAction(listResp.Capabilities, ActionList) {
		t.Errorf("expected scope-level list capability, got %v", listResp.Capabilities)
	}
	if len(listResp.HarnessConfigs) != 1 || listResp.HarnessConfigs[0].Cap == nil {
		t.Fatalf("expected one harness config with capabilities")
	}
	if !hasAction(listResp.HarnessConfigs[0].Cap, ActionUpdate) {
		t.Errorf("expected per-item update capability, got %v", listResp.HarnessConfigs[0].Cap.Actions)
	}
}

func hasAction(c *Capabilities, action Action) bool {
	if c == nil {
		return false
	}
	for _, a := range c.Actions {
		if a == string(action) {
			return true
		}
	}
	return false
}
