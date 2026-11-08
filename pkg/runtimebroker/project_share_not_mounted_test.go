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

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A start refused because the project storage share is not mounted on the
// broker (agent.ErrProjectShareNotMounted, whose text names the mount
// point) is answered 503 project_storage_unavailable with the field-name
// text on the synchronous create, start and restart, and reported with the
// same code and text by the async launch. None carries the path.
const projectShareTestPath = "/mnt/nfs/share-1"

func projectShareNotMountedErr() error {
	return fmt.Errorf("start agent: %w (%s)", agent.ErrProjectShareNotMounted, projectShareTestPath)
}

// Every synchronous path that runs Manager.Start: create, start and
// restart (the restart's stop still runs first; ptone/scion#6371).
func TestProjectShareNotMounted_SyncPaths(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"create", "/api/v1/agents", `{"name": "share-agent", "config": {"template": "claude"}}`},
		{"start", "/api/v1/agents/test-agent-1/start", `{}`},
		{"restart", "/api/v1/agents/test-agent-1/restart", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			srv.manager.(*mockManager).startErr = projectShareNotMountedErr()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s = %d %s, want 503", tc.name, w.Code, w.Body.String())
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", w.Body.String(), err)
			}
			if body.Error.Code != ErrCodeProjectStorageUnavailable {
				t.Errorf("code = %q, want %q", body.Error.Code, ErrCodeProjectStorageUnavailable)
			}
			if want := scionrt.ProjectShareUnavailableText(scionrt.ProjectShareMountPathField, true); body.Error.Message != want {
				t.Errorf("message = %q, want %q", body.Error.Message, want)
			}
			if strings.Contains(w.Body.String(), projectShareTestPath) {
				t.Errorf("body names the mount point: %s", w.Body.String())
			}
		})
	}
}

func TestClassifyStartError_ProjectShareNotMounted(t *testing.T) {
	code, _, message := classifyStartError(context.Background(), projectShareNotMountedErr(), "", "", store.LaunchKindCreate)
	if code != ErrCodeProjectStorageUnavailable {
		t.Errorf("code = %q, want %q", code, ErrCodeProjectStorageUnavailable)
	}
	if want := scionrt.ProjectShareUnavailableText(scionrt.ProjectShareMountPathField, true); message != want {
		t.Errorf("message = %q, want %q", message, want)
	}
	if strings.Contains(message, projectShareTestPath) {
		t.Errorf("message names the mount point: %q", message)
	}
}
