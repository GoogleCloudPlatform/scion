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
