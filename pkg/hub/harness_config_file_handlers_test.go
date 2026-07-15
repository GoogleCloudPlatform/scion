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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func testHarnessConfigFileServer(t *testing.T) (*Server, store.Store, *contentMockStorage) {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping: sqlite driver not registered")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)

	return srv, s, stor
}

func createTestHarnessConfigWithFiles(t *testing.T, s store.Store, stor *contentMockStorage, files map[string]string) *store.HarnessConfig {
	t.Helper()
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:            tid("hc-file-test-1"),
		Name:          "test-hc",
		Slug:          "test-hc",
		Harness:       "claude",
		Scope:         store.HarnessConfigScopeGlobal,
		Status:        store.HarnessConfigStatusActive,
		StoragePath:   "harness-configs/global/test-hc",
		StorageBucket: "test-bucket",
		Updated:       time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
	}

	hcFiles := make([]store.TemplateFile, 0, len(files))
	for path, content := range files {
		objectPath := hc.StoragePath + "/" + path
		stor.content[objectPath] = []byte(content)
		stor.objects[objectPath] = &storage.Object{
			Name: objectPath,
			Size: int64(len(content)),
		}
		hcFiles = append(hcFiles, store.TemplateFile{
			Path: path,
			Size: int64(len(content)),
			Hash: "sha256:placeholder",
		})
	}
	hc.Files = hcFiles
	hc.ContentHash = computeContentHash(hcFiles)

	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create test harness config: %v", err)
	}
	return hc
}

func TestHandleHarnessConfigFileWrite_UpdatesImage(t *testing.T) {
	srv, s, _ := testHarnessConfigFileServer(t)
	ctx := context.Background()

	hc := createTestHarnessConfigWithFiles(t, s, nil, nil)

	// Provide storage via the server (createTestHarnessConfigWithFiles used nil
	// storage because we want the PUT handler to write to the server's storage).
	stor := srv.GetStorage().(*contentMockStorage)

	// Pre-populate storage with the existing placeholder so the harness config
	// has a valid storage path.
	newImage := "us-docker.pkg.dev/my-project/repo/new-image:v2"
	configYAML := "harness: claude\nimage: " + newImage + "\n"

	body := `{"content": "` + strings.ReplaceAll(configYAML, "\n", `\n`) + `"}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/harness-configs/"+hc.ID+"/files/config.yaml",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp HarnessConfigFileWriteResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Path != "config.yaml" {
		t.Errorf("expected path config.yaml, got %s", resp.Path)
	}

	// Verify storage content
	storedContent := stor.content[hc.StoragePath+"/config.yaml"]
	if string(storedContent) != configYAML {
		t.Errorf("unexpected stored content: %s", string(storedContent))
	}

	// Verify hc.Config.Image was updated
	updated, err := s.GetHarnessConfig(ctx, hc.ID)
	if err != nil {
		t.Fatalf("failed to get updated harness config: %v", err)
	}
	if updated.Config == nil {
		t.Fatal("expected Config to be non-nil after writing config.yaml")
	}
	if updated.Config.Image != newImage {
		t.Errorf("expected Config.Image = %q, got %q", newImage, updated.Config.Image)
	}
}
