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
	"sync"
	"testing"
)

// recordingImageManager is a fakeImageManager that records the images it
// was asked to pull or remove.
type recordingImageManager struct {
	fakeImageManager
	mu      sync.Mutex
	pulled  []string
	removed []string
}

func (m *recordingImageManager) PullImage(_ context.Context, image string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pulled = append(m.pulled, image)
	return nil
}

func (m *recordingImageManager) RemoveImage(_ context.Context, image string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, image)
	return nil
}

func decodeStatusBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	return body
}

func decodeErrorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rr.Body.String(), err)
	}
	return body.Error.Code
}

// The local-image delete and pull handlers read the co-located runtime's
// image manager through one getImageManager snapshot (ptone/scion#1374).

func TestHarnessConfigDeleteLocalImage_NoImageManager(t *testing.T) {
	srv, db := setupImageStatusTest(t)
	hc := createTestHarnessConfig(t, db, "hc-del-none", "my-image:latest")

	rr := httptest.NewRecorder()
	srv.handleHarnessConfigDeleteLocalImage(rr, imageStatusRequest(http.MethodDelete, "/api/v1/harness-configs/"+hc.ID+"/local-image"), hc)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := decodeErrorCode(t, rr); code != "no_runtime" {
		t.Fatalf("expected error code no_runtime, got %q", code)
	}
}

func TestHarnessConfigDeleteLocalImage_NotFound(t *testing.T) {
	srv, db := setupImageStatusTest(t)
	hc := createTestHarnessConfig(t, db, "hc-del-missing", "my-image:latest")
	mgr := &recordingImageManager{fakeImageManager: fakeImageManager{exists: map[string]bool{}}}
	srv.SetLocalImageChecker(mgr)

	rr := httptest.NewRecorder()
	srv.handleHarnessConfigDeleteLocalImage(rr, imageStatusRequest(http.MethodDelete, "/api/v1/harness-configs/"+hc.ID+"/local-image"), hc)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := decodeStatusBody(t, rr)["status"]; got != "not_found" {
		t.Fatalf("expected status not_found, got %q", got)
	}
	if len(mgr.removed) != 0 {
		t.Fatalf("RemoveImage must not be called for a missing image, got %v", mgr.removed)
	}
}

func TestHarnessConfigDeleteLocalImage_Removed(t *testing.T) {
	srv, db := setupImageStatusTest(t)
	hc := createTestHarnessConfig(t, db, "hc-del-present", "my-image:latest")
	mgr := &recordingImageManager{fakeImageManager: fakeImageManager{exists: map[string]bool{"my-image:latest": true}}}
	srv.SetLocalImageChecker(mgr)

	rr := httptest.NewRecorder()
	srv.handleHarnessConfigDeleteLocalImage(rr, imageStatusRequest(http.MethodDelete, "/api/v1/harness-configs/"+hc.ID+"/local-image"), hc)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := decodeStatusBody(t, rr)["status"]; got != "removed" {
		t.Fatalf("expected status removed, got %q", got)
	}
	if len(mgr.removed) != 1 || mgr.removed[0] != "my-image:latest" {
		t.Fatalf("expected RemoveImage(my-image:latest) once, got %v", mgr.removed)
	}
}

func TestHarnessConfigPullImage_NoImageManager(t *testing.T) {
	srv, db := setupImageStatusTest(t)
	hc := createTestHarnessConfig(t, db, "hc-pull-none", "my-image:latest")

	rr := httptest.NewRecorder()
	srv.handleHarnessConfigPullImage(rr, imageStatusRequest(http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/pull-image"), hc)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := decodeErrorCode(t, rr); code != "no_runtime" {
		t.Fatalf("expected error code no_runtime, got %q", code)
	}
}

func TestHarnessConfigPullImage_Pulled(t *testing.T) {
	srv, db := setupImageStatusTest(t)
	hc := createTestHarnessConfig(t, db, "hc-pull-present", "my-image:latest")
	mgr := &recordingImageManager{fakeImageManager: fakeImageManager{exists: map[string]bool{}}}
	srv.SetLocalImageChecker(mgr)

	rr := httptest.NewRecorder()
	srv.handleHarnessConfigPullImage(rr, imageStatusRequest(http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/pull-image"), hc)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeStatusBody(t, rr)
	if body["status"] != "pulled" {
		t.Fatalf("expected status pulled, got %q", body["status"])
	}
	if len(mgr.pulled) != 1 || mgr.pulled[0] != body["image"] {
		t.Fatalf("expected PullImage(%q) once, got %v", body["image"], mgr.pulled)
	}
}
