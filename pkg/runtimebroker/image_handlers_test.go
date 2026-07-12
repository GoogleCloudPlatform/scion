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

package runtimebroker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func newImageTestServer(t *testing.T, rt *runtime.MockRuntime) *Server {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = false
	mgr := &mockManager{}
	srv := &Server{
		config:  cfg,
		manager: mgr,
		runtime: rt,
		mux:     http.NewServeMux(),
	}
	srv.registerRoutes()
	return srv
}

// --- GET /api/v1/images/status ---

func TestImageStatus_BothExist(t *testing.T) {
	rt := &runtime.MockRuntime{
		ImageExistsFunc: func(_ context.Context, image string) (bool, error) {
			return true, nil
		},
		ImageIDFunc: func(_ context.Context, image string) (string, error) {
			if image == "myimage" {
				return "sha256:aaa", nil
			}
			return "sha256:bbb", nil
		},
	}
	srv := newImageTestServer(t, rt)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/status?short=myimage&long=registry.io/myimage:latest", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp ImageStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LocalShort == nil || !resp.LocalShort.Exists || resp.LocalShort.Hash != "sha256:aaa" {
		t.Errorf("unexpected LocalShort: %+v", resp.LocalShort)
	}
	if resp.LocalLong == nil || !resp.LocalLong.Exists || resp.LocalLong.Hash != "sha256:bbb" {
		t.Errorf("unexpected LocalLong: %+v", resp.LocalLong)
	}
}

func TestImageStatus_ShortExistsLongDoesNot(t *testing.T) {
	rt := &runtime.MockRuntime{
		ImageExistsFunc: func(_ context.Context, image string) (bool, error) {
			return image == "myimage", nil
		},
		ImageIDFunc: func(_ context.Context, image string) (string, error) {
			return "sha256:aaa", nil
		},
	}
	srv := newImageTestServer(t, rt)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/status?short=myimage&long=registry.io/myimage:latest", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp ImageStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LocalShort == nil || !resp.LocalShort.Exists {
		t.Errorf("expected LocalShort.Exists=true, got %+v", resp.LocalShort)
	}
	if resp.LocalLong == nil || resp.LocalLong.Exists {
		t.Errorf("expected LocalLong.Exists=false, got %+v", resp.LocalLong)
	}
}

func TestImageStatus_NeitherExists(t *testing.T) {
	rt := &runtime.MockRuntime{
		ImageExistsFunc: func(_ context.Context, image string) (bool, error) {
			return false, nil
		},
	}
	srv := newImageTestServer(t, rt)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/status?short=a&long=b", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp ImageStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LocalShort == nil || resp.LocalShort.Exists {
		t.Errorf("expected LocalShort.Exists=false, got %+v", resp.LocalShort)
	}
	if resp.LocalLong == nil || resp.LocalLong.Exists {
		t.Errorf("expected LocalLong.Exists=false, got %+v", resp.LocalLong)
	}
}

func TestImageStatus_MissingParams(t *testing.T) {
	srv := newImageTestServer(t, &runtime.MockRuntime{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestImageStatus_RuntimeError(t *testing.T) {
	rt := &runtime.MockRuntime{
		ImageExistsFunc: func(_ context.Context, image string) (bool, error) {
			return false, fmt.Errorf("daemon unavailable")
		},
	}
	srv := newImageTestServer(t, rt)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/status?short=myimage", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestImageStatus_WrongMethod(t *testing.T) {
	srv := newImageTestServer(t, &runtime.MockRuntime{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/status?short=x", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

// --- POST /api/v1/images/pull ---

func TestImagePull_Success(t *testing.T) {
	var pulledImage string
	rt := &runtime.MockRuntime{
		PullImageFunc: func(_ context.Context, image string) error {
			pulledImage = image
			return nil
		},
	}
	srv := newImageTestServer(t, rt)

	body := strings.NewReader(`{"image": "registry.io/myimage:latest"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/pull", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if pulledImage != "registry.io/myimage:latest" {
		t.Errorf("expected pull of %q, got %q", "registry.io/myimage:latest", pulledImage)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "pulled" {
		t.Errorf("expected status=pulled, got %q", resp["status"])
	}
}

func TestImagePull_EmptyImage(t *testing.T) {
	srv := newImageTestServer(t, &runtime.MockRuntime{})

	body := strings.NewReader(`{"image": ""}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/pull", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestImagePull_Failure(t *testing.T) {
	rt := &runtime.MockRuntime{
		PullImageFunc: func(_ context.Context, image string) error {
			return fmt.Errorf("network timeout")
		},
	}
	srv := newImageTestServer(t, rt)

	body := strings.NewReader(`{"image": "registry.io/myimage:latest"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/pull", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestImagePull_WrongMethod(t *testing.T) {
	srv := newImageTestServer(t, &runtime.MockRuntime{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/pull", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

// --- DELETE /api/v1/images/local ---

func TestImageDeleteLocal_ExistsAndRemoved(t *testing.T) {
	var removedImage string
	rt := &runtime.MockRuntime{
		ImageExistsFunc: func(_ context.Context, image string) (bool, error) {
			return true, nil
		},
		RemoveImageFunc: func(_ context.Context, image string) error {
			removedImage = image
			return nil
		},
	}
	srv := newImageTestServer(t, rt)

	body := strings.NewReader(`{"image": "myimage"}`)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/images/local", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if removedImage != "myimage" {
		t.Errorf("expected removal of %q, got %q", "myimage", removedImage)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "removed" {
		t.Errorf("expected status=removed, got %q", resp["status"])
	}
}

func TestImageDeleteLocal_DoesNotExist(t *testing.T) {
	rt := &runtime.MockRuntime{
		ImageExistsFunc: func(_ context.Context, image string) (bool, error) {
			return false, nil
		},
	}
	srv := newImageTestServer(t, rt)

	body := strings.NewReader(`{"image": "myimage"}`)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/images/local", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "not_found" {
		t.Errorf("expected status=not_found, got %q", resp["status"])
	}
}

func TestImageDeleteLocal_RemoveFailure(t *testing.T) {
	rt := &runtime.MockRuntime{
		ImageExistsFunc: func(_ context.Context, image string) (bool, error) {
			return true, nil
		},
		RemoveImageFunc: func(_ context.Context, image string) error {
			return fmt.Errorf("image in use")
		},
	}
	srv := newImageTestServer(t, rt)

	body := strings.NewReader(`{"image": "myimage"}`)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/images/local", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestImageDeleteLocal_EmptyImage(t *testing.T) {
	srv := newImageTestServer(t, &runtime.MockRuntime{})

	body := strings.NewReader(`{"image": ""}`)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/images/local", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestImageDeleteLocal_WrongMethod(t *testing.T) {
	srv := newImageTestServer(t, &runtime.MockRuntime{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/local", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}
