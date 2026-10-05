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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// unusableResolveErr returns the error harness.Resolve gives for a
// hub-hydrated harness-config "legacy-hc" with provisioner.type builtin,
// and the broker directory it names.
func unusableResolveErr(t *testing.T) (dir string, err error) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "broker-private-cache", "legacy-hc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("harness: claude\nimage: scion-claude:test\nprovisioner:\n  type: builtin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = harness.Resolve(context.Background(), harness.ResolveOptions{Name: "legacy-hc", ConfigDirPath: dir})
	if _, ok := unusableProvisionerFrom(err); !ok {
		t.Fatalf("fixture: expected an UnusableProvisionerError, got %v", err)
	}
	// As pkg/agent wraps it.
	return dir, fmt.Errorf("failed to resolve harness for %q: %w", "legacy-hc", err)
}

// unusableManager fails the launch step named by at with err.
type unusableManager struct {
	provisionCapturingManager
	at  string // "provision" or "start"
	err error
}

func (m *unusableManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	if m.at == "provision" {
		return nil, m.err
	}
	return m.provisionCapturingManager.Provision(ctx, opts)
}

func (m *unusableManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	if m.at == "start" {
		return nil, m.err
	}
	return m.provisionCapturingManager.Start(ctx, opts)
}

func assertUnusableResponse(t *testing.T, code int, body, brokerDir string) ErrorResponse {
	t.Helper()
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", code, body)
	}
	var resp ErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("not an ErrorResponse: %v: %s", err, body)
	}
	if resp.Error.Code != ErrCodeHarnessConfigUnusable {
		t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeHarnessConfigUnusable)
	}
	for _, want := range []string{`"legacy-hc"`, `provisioner.type "builtin"`, "scion harness-config sync legacy-hc"} {
		if !strings.Contains(resp.Error.Message, want) {
			t.Errorf("message %q does not contain %q", resp.Error.Message, want)
		}
	}
	if strings.Contains(body, brokerDir) || strings.Contains(body, "broker-private-cache") {
		t.Errorf("response leaks the broker path: %s", body)
	}
	return resp
}

// Every synchronous dispatch path answers an unusable provisioner with 422
// harness_config_unusable and the actionable public message, without the
// broker's filesystem path (ptone/scion#611).
func TestUnusableProvisioner_DispatchPathsAnswer422(t *testing.T) {
	cases := []struct {
		name, at, path, body string
		startAttempted       bool
	}{
		{"create sync start", "start", "/api/v1/agents",
			`{"name": "u1", "id": "id-u1", "slug": "u1", "config": {"template": "none"}}`, false},
		{"create provision-only", "provision", "/api/v1/agents",
			`{"name": "u2", "id": "id-u2", "slug": "u2", "provisionOnly": true, "config": {"template": "none"}}`, false},
		{"start", "start", "/api/v1/agents/u3/start", `{"runId": "run-u3"}`, true},
		{"restart", "start", "/api/v1/agents/u4/restart", `{"runId": "run-u4"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := dispatchTestEnv(t, true)
			dir, err := unusableResolveErr(t)
			srv.manager = &unusableManager{at: tc.at, err: err}
			code, body := postJSON(t, srv, tc.path, tc.body)
			resp := assertUnusableResponse(t, code, body, dir)
			if tc.startAttempted && resp.Error.Details[api.BrokerErrorDetailStartAttempted] != true {
				t.Errorf("expected %s=true in details, got %v", api.BrokerErrorDetailStartAttempted, resp.Error.Details)
			}
		})
	}
}

// An async launch reports the harness_config_unusable code with the public
// message.
func TestClassifyStartError_UnusableProvisioner(t *testing.T) {
	dir, err := unusableResolveErr(t)
	code, msg := classifyStartError(context.Background(), err)
	if code != ErrCodeHarnessConfigUnusable {
		t.Errorf("code = %q, want %q", code, ErrCodeHarnessConfigUnusable)
	}
	if !strings.Contains(msg, "scion harness-config sync legacy-hc") || strings.Contains(msg, dir) {
		t.Errorf("message should carry the fix without the broker path, got %q", msg)
	}
}

// With allow_container_script_harnesses=false, the early check reports an
// unusable provisioner (the more specific error) before the policy refusal.
func TestUnusableProvisioner_ReportedBeforePolicyRefusal(t *testing.T) {
	srv, mgr, dotScion := dispatchTestEnv(t, false)
	writeHarnessConfig(t, dotScion, "legacy-global", "harness: claude\nimage: scion-claude:test\nprovisioner:\n  type: builtin\n")

	code, body := dispatchAgent(t, srv, "legacy-global")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", code, body)
	}
	var resp ErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("not an ErrorResponse: %v: %s", err, body)
	}
	if resp.Error.Code != ErrCodeHarnessConfigUnusable || !strings.Contains(resp.Error.Message, "scion harness-config upgrade legacy-global --activate-script") ||
		!strings.Contains(resp.Error.Message, "the broker's global copy") || !strings.Contains(resp.Error.Message, "Repair it on the broker host") {
		t.Errorf("unexpected response: %s", body)
	}
	if strings.Contains(body, dotScion) {
		t.Errorf("response leaks the broker path: %s", body)
	}
	if mgr.provisionCalled {
		t.Error("Provision must not run")
	}

	// A usable container-script config is still refused by the policy.
	writeHarnessConfig(t, dotScion, "scripted", scriptedHarnessYAML)
	code, body = dispatchAgent(t, srv, "scripted")
	assertPolicyRefusal(t, code, body, "scripted")
}
