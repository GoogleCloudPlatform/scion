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
)

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

// TestHandleHarnessConfigFileWrite_PersistsModelAliases is a regression test
// for ptone/scion#2365 review round 1 (R2): the single-file config.yaml
// write handler is one of the four write paths this fix touches, and needs
// its own coverage rather than relying on the directory-bootstrap sync test.
func TestHandleHarnessConfigFileWrite_PersistsModelAliases(t *testing.T) {
	srv, s, _ := testHarnessConfigFileServer(t)
	ctx := context.Background()

	hc := createTestHarnessConfigWithFiles(t, s, nil, nil)

	configYAML := "harness: codex\nmodel_aliases:\n  small: tiny-model\n  large: write-large-model\n"
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

	updated, err := s.GetHarnessConfig(ctx, hc.ID)
	if err != nil {
		t.Fatalf("failed to get updated harness config: %v", err)
	}
	if updated.Config == nil || updated.Config.ModelAliases["large"] != "write-large-model" {
		t.Errorf("expected Config.ModelAliases[large] = %q after file write, got %+v", "write-large-model", updated.Config)
	}
}

// TestHandleHarnessConfigFileUpload_PersistsModelAliases is a regression
// test for ptone/scion#2365 review round 1 (R2): the multipart upload
// handler is a distinct write path from the single-file write and finalize
// handlers, and needs its own coverage.
func TestHandleHarnessConfigFileUpload_PersistsModelAliases(t *testing.T) {
	srv, s, _ := testHarnessConfigFileServer(t)
	ctx := context.Background()

	hc := createTestHarnessConfigWithFiles(t, s, nil, nil)

	configYAML := "harness: codex\nmodel_aliases:\n  small: tiny-model\n  large: upload-large-model\n"
	req := harnessConfigMultipartRequest(t, hc.ID, map[string][]byte{
		"config.yaml": []byte(configYAML),
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	updated, err := s.GetHarnessConfig(ctx, hc.ID)
	if err != nil {
		t.Fatalf("failed to get updated harness config: %v", err)
	}
	if updated.Config == nil || updated.Config.ModelAliases["large"] != "upload-large-model" {
		t.Errorf("expected Config.ModelAliases[large] = %q after multipart upload, got %+v", "upload-large-model", updated.Config)
	}
}
