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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type asyncMockRuntime struct{ *runtime.MockRuntime }

func (asyncMockRuntime) SupportsAsyncLaunch() bool { return true }

// TestGenericHarnessEmptyCommand_BrokerRefusesWith400 (ptone/scion#5082):
// create (both synchronous and async admission), start, and restart with the
// generic harness and no task, harness args, or command_args answer 400
// validation_error with the clear message (relayed unchanged by the Hub) and
// never launch a container.
func TestGenericHarnessEmptyCommand_BrokerRefusesWith400(t *testing.T) {
	const wantMsg = "generic harness needs a task, harness args or command_args"

	newEnv := func(t *testing.T, runs *int) (*Server, string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Chdir(t.TempDir())

		global := filepath.Join(home, ".scion")
		tplDir := filepath.Join(global, "templates", "default")
		if err := os.MkdirAll(tplDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(global, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		projectScion := filepath.Join(t.TempDir(), "project", ".scion")
		if err := os.MkdirAll(projectScion, 0o755); err != nil {
			t.Fatal(err)
		}

		rt := asyncMockRuntime{&runtime.MockRuntime{
			NameFunc:        func() string { return "docker" },
			ListFunc:        func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
				*runs++
				return "cid-1", nil
			},
		}}
		mgr := agent.NewManager(rt)
		cfg := testServerConfig()
		cfg.StateDir = t.TempDir()
		srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		return srv, projectScion
	}

	assert400 := func(t *testing.T, code int, body, wantOp string) {
		t.Helper()
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", code, body)
		}
		var resp ErrorResponse
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal ErrorResponse: %v: %s", err, body)
		}
		if resp.Error.Code != ErrCodeValidationError {
			t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeValidationError)
		}
		want := "Failed to " + wantOp + ": " + wantMsg
		if resp.Error.Message != want {
			t.Errorf("message = %q, want %q", resp.Error.Message, want)
		}
	}

	t.Run("sync create", func(t *testing.T) {
		var runs int
		srv, projectScion := newEnv(t, &runs)
		payload, _ := json.Marshal(map[string]any{
			"id":          "id-g1",
			"name":        "g1",
			"slug":        "g1",
			"projectId":   "proj-1",
			"projectPath": projectScion,
			"config": map[string]any{
				"harnessConfig": "generic",
				"image":         "test-image:latest",
			},
		})
		code, body := postJSON(t, srv, "/api/v1/agents", string(payload))
		assert400(t, code, body, "create agent")
		if runs != 0 {
			t.Errorf("runs = %d, want 0", runs)
		}
	})

	t.Run("async create admission", func(t *testing.T) {
		var runs int
		srv, projectScion := newEnv(t, &runs)
		payload, _ := json.Marshal(map[string]any{
			"id":                     "id-g2",
			"name":                   "g2",
			"slug":                   "g2",
			"projectId":              "proj-1",
			"projectPath":            projectScion,
			"asyncLaunch":            true,
			"launchId":               "launch-g2",
			"launchTimeoutSeconds":   120,
			"launchKeepaliveSeconds": 15,
			"config": map[string]any{
				"harnessConfig": "generic",
				"image":         "test-image:latest",
			},
		})
		code, body := postJSON(t, srv, "/api/v1/agents", string(payload))
		assert400(t, code, body, "create agent")
		if runs != 0 {
			t.Errorf("runs = %d, want 0", runs)
		}
	})

	t.Run("provision-only succeeds then sync start and restart fail with 400", func(t *testing.T) {
		var runs int
		srv, projectScion := newEnv(t, &runs)
		provPayload, _ := json.Marshal(map[string]any{
			"id":            "id-g3",
			"name":          "g3",
			"slug":          "g3",
			"projectId":     "proj-1",
			"projectPath":   projectScion,
			"provisionOnly": true,
			"config": map[string]any{
				"harnessConfig": "generic",
				"image":         "test-image:latest",
			},
		})
		code, body := postJSON(t, srv, "/api/v1/agents", string(provPayload))
		if code != http.StatusCreated {
			t.Fatalf("provision-only status = %d, want 201: %s", code, body)
		}

		startPayload, _ := json.Marshal(map[string]any{
			"runId":              "run-g3",
			"projectId":          "proj-1",
			"projectPath":        projectScion,
			"hubEndpoint":        "https://hub.example.com",
			"resolvedEnv":        map[string]string{"SCION_AUTH_TOKEN": "tok-g3"},
			"envClassifications": map[string]api.EnvKind{"SCION_AUTH_TOKEN": api.EnvKindSecretBootstrap},
		})
		code, body = postJSON(t, srv, "/api/v1/agents/g3/start", string(startPayload))
		assert400(t, code, body, "start agent")

		code, body = postJSON(t, srv, "/api/v1/agents/g3/restart", string(startPayload))
		assert400(t, code, body, "restart agent")
		if runs != 0 {
			t.Errorf("runs = %d, want 0", runs)
		}

		// Supplying a task allows start to succeed.
		withTaskPayload, _ := json.Marshal(map[string]any{
			"runId":              "run-g3-ok",
			"projectId":          "proj-1",
			"projectPath":        projectScion,
			"hubEndpoint":        "https://hub.example.com",
			"resolvedEnv":        map[string]string{"SCION_AUTH_TOKEN": "tok-g3"},
			"envClassifications": map[string]api.EnvKind{"SCION_AUTH_TOKEN": api.EnvKindSecretBootstrap},
			"task":               "echo hello",
		})
		code, body = postJSON(t, srv, "/api/v1/agents/g3/start", string(withTaskPayload))
		if code != http.StatusAccepted {
			t.Fatalf("start with task status = %d, want 202: %s", code, body)
		}
		if runs != 1 {
			t.Errorf("runs = %d, want 1", runs)
		}
		if strings.Contains(body, wantMsg) {
			t.Errorf("unexpected error in start response: %s", body)
		}
	})

	t.Run("async classifyStartError", func(t *testing.T) {
		code, _, msg := classifyStartError(context.Background(), harness.ErrEmptyGenericCommand, "", "", store.LaunchKindStart)
		if code != ErrCodeHarnessConfigUnusable {
			t.Errorf("code = %q, want %q", code, ErrCodeHarnessConfigUnusable)
		}
		if msg != wantMsg {
			t.Errorf("msg = %q, want %q", msg, wantMsg)
		}
	})
}
