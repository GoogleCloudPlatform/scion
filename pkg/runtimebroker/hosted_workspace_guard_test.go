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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestCreateAgent_HostedBrokerRefusesExternalAbsoluteWorkspace verifies L3: a
// hosted broker given an absolute workspace outside its permitted roots via a
// direct broker API call refuses the request with workspace_not_permitted,
// fixed public text (no leaked host path), and creates no container.
func TestCreateAgent_HostedBrokerRefusesExternalAbsoluteWorkspace(t *testing.T) {
	setupTestScionEnv(t)

	outsideDir := filepath.Join(t.TempDir(), "external-host-dir")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outsideDir: %v", err)
	}

	runCalled := false
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		RunFunc: func(_ context.Context, _ runtime.RunConfig) (string, error) {
			runCalled = true
			return "unexpected-container", nil
		},
	}
	mgr := agent.NewManager(rt)

	cfg := testServerConfig()
	cfg.BrokerID = "hosted-ws-broker"
	cfg.WorkstationMode = false
	cfg.ForceRuntime = "docker"
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)

	bodyBytes, err := json.Marshal(CreateAgentRequest{
		Name: "hosted-abs-ws-agent",
		Config: &CreateAgentConfig{
			Workspace: outsideDir,
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	code, msg := decodeError(t, w)
	if code != api.LaunchCodeWorkspaceNotPermitted {
		t.Fatalf("code = %q, want %q", code, api.LaunchCodeWorkspaceNotPermitted)
	}
	if msg != launchFailureText[api.LaunchCodeWorkspaceNotPermitted] {
		t.Fatalf("message = %q, want %q", msg, launchFailureText[api.LaunchCodeWorkspaceNotPermitted])
	}
	if strings.Contains(msg, outsideDir) || strings.Contains(msg, "external-host-dir") {
		t.Fatalf("message %q must not leak host path", msg)
	}
	if runCalled {
		t.Fatalf("runtime.Run must not be called when workspace is refused")
	}
}
