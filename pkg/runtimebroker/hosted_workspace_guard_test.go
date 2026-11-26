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

// TestReprovision_HostedBrokerRefusesExternalAbsoluteWorkspace verifies that
// a hosted broker surfaces a Reprovision explicit-workspace refusal as HTTP
// 400 with typed code workspace_not_permitted and fixed public text (never
// echoing the host path).
func TestReprovision_HostedBrokerRefusesExternalAbsoluteWorkspace(t *testing.T) {
	setupTestScionEnv(t)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	projectDir := filepath.Join(homeDir, ".scion", "projects", "demo", ".scion")
	agentDir := filepath.Join(projectDir, "agents", "hosted-reprov-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}

	outsideDir := filepath.Join(t.TempDir(), "external-reprov-dir")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outsideDir: %v", err)
	}

	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
	}
	mgr := agent.NewManager(rt)

	cfg := testServerConfig()
	cfg.BrokerID = "hosted-reprov-broker"
	cfg.WorkstationMode = false
	cfg.ForceRuntime = "docker"
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)

	bodyBytes, err := json.Marshal(CreateAgentRequest{
		Name:          "hosted-reprov-agent",
		ProjectPath:   projectDir,
		ProvisionOnly: true,
		Reprovision:   true,
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
	if strings.Contains(msg, outsideDir) || strings.Contains(msg, "external-reprov-dir") {
		t.Fatalf("message %q must not leak host path", msg)
	}
}

// TestRestartAgent_HostedBrokerRefusesExternalAbsoluteWorkspace verifies that
// a hosted broker restart of an agent whose persisted scion-agent.json holds
// ExplicitWorkspace=true and a /workspace volume outside permitted roots
// returns HTTP 400 with typed code workspace_not_permitted (not 500) and
// fixed public text without starting a container.
func TestRestartAgent_HostedBrokerRefusesExternalAbsoluteWorkspace(t *testing.T) {
	setupTestScionEnv(t)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	projectDir := filepath.Join(homeDir, ".scion", "projects", "demo", ".scion")
	hcDir := filepath.Join(projectDir, "harness-configs", "claude")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatalf("mkdir hcDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nimage: dummy:latest\n"), 0o644); err != nil {
		t.Fatalf("write harness config: %v", err)
	}

	outsideDir := filepath.Join(t.TempDir(), "external-restart-dir")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outsideDir: %v", err)
	}

	agentDir := filepath.Join(projectDir, "agents", "hosted-restart-agent")
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0o755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}
	cfgBytes, err := json.Marshal(&api.ScionConfig{
		Harness:           "claude",
		HarnessConfig:     "claude",
		Image:             "dummy:latest",
		ExplicitWorkspace: true,
		Volumes: []api.VolumeMount{
			{Source: outsideDir, Target: "/workspace"},
		},
	})
	if err != nil {
		t.Fatalf("marshal scion-agent.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), cfgBytes, 0o644); err != nil {
		t.Fatalf("write scion-agent.json: %v", err)
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
	cfg.BrokerID = "hosted-restart-broker"
	cfg.WorkstationMode = false
	cfg.ForceRuntime = "docker"
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)

	bodyBytes, err := json.Marshal(map[string]string{
		"projectPath": projectDir,
		"profile":     "local",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/hosted-restart-agent/restart", strings.NewReader(string(bodyBytes)))
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
	if strings.Contains(msg, outsideDir) || strings.Contains(msg, "external-restart-dir") {
		t.Fatalf("message %q must not leak host path", msg)
	}
	if runCalled {
		t.Fatalf("runtime.Run must not be called when workspace is refused on restart")
	}
}
