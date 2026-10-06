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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// hookScriptedEntry is what a fake manager reports as the harness-config
// launch resolved, as pkg/agent does at its resolution points.
var hookScriptedEntry = config.HarnessConfigEntry{
	Harness:     "generic",
	Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
}

// hookManager is a manager whose launch steps evaluate the policy on their
// context exactly as pkg/agent does (agent.CheckHarnessConfigPolicy) with a
// container-script entry named "hook-hc", at the step named by at.
type hookManager struct {
	provisionCapturingManager
	at string // "preflight", "provision" or "start"
	// notEvaluated makes the step report a policy refusal for a
	// harness-config that could not be evaluated, as Start does after a
	// resolve error with a provisioner staged.
	notEvaluated bool
}

func (m *hookManager) check(ctx context.Context, step string) error {
	if m.at != step {
		return nil
	}
	if m.notEvaluated {
		if config.HarnessConfigPolicyFromContext(ctx) == nil {
			return nil
		}
		return fmt.Errorf("%w: %w", agent.ErrHarnessConfigPolicy, &agent.HarnessConfigNotEvaluatedError{Name: "hook-hc", Cause: errors.New("resolve failed at /some/broker/path")})
	}
	return agent.CheckHarnessConfigPolicy(ctx, "hook-hc", hookScriptedEntry)
}

func (m *hookManager) Preflight(ctx context.Context, opts api.StartOptions) error {
	return m.check(ctx, "preflight")
}

func (m *hookManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	if err := m.check(ctx, "provision"); err != nil {
		return nil, err
	}
	return m.provisionCapturingManager.Provision(ctx, opts)
}

func (m *hookManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	if err := m.check(ctx, "start"); err != nil {
		return nil, err
	}
	return m.provisionCapturingManager.Start(ctx, opts)
}

func hookTestServer(t *testing.T, allow bool, at string) (*Server, *hookManager) {
	t.Helper()
	srv, _, _ := dispatchTestEnv(t, allow)
	mgr := &hookManager{at: at}
	srv.manager = mgr
	return srv, mgr
}

func postJSON(t *testing.T, srv *Server, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// Each dispatch path attaches the broker's policy to the launch context and
// answers a refusal from any resolution point with the same 403.
func TestHarnessPolicyHook_SameRefusalAtEveryHookPoint(t *testing.T) {
	cases := []struct {
		name, at, path, body string
	}{
		{"create sync start", "start", "/api/v1/agents",
			`{"name": "h1", "id": "id-h1", "slug": "h1", "config": {"template": "none"}}`},
		{"create provision-only", "provision", "/api/v1/agents",
			`{"name": "h2", "id": "id-h2", "slug": "h2", "provisionOnly": true, "config": {"template": "none"}}`},
		{"create async admission", "preflight", "/api/v1/agents",
			`{"name": "h3", "id": "id-h3", "slug": "h3", "asyncLaunch": true, "launchId": "L-h3", "launchTimeoutSeconds": 300, "config": {"template": "none"}}`},
		{"start", "start", "/api/v1/agents/h4/start", `{}`},
		{"restart", "start", "/api/v1/agents/h5/restart", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := hookTestServer(t, false, tc.at)
			code, body := postJSON(t, srv, tc.path, tc.body)
			assertPolicyRefusal(t, code, body, "hook-hc")
		})
	}
	t.Run("allow=true passes", func(t *testing.T) {
		srv, mgr := hookTestServer(t, true, "start")
		code, body := postJSON(t, srv, "/api/v1/agents/h6/start", `{}`)
		if code != http.StatusAccepted || mgr.StartCalls() != 1 {
			t.Fatalf("expected 202 with allow=true, got %d: %s", code, body)
		}
	})
}

// Create admission evaluates the policy on the harness-config launch
// resolves, here named by the template rather than the request, and refuses
// before any side effect: no Provision or Start, no agent directory, no
// staged bundle.
func TestHarnessPolicyHook_CreateRefusesBeforeSideEffects(t *testing.T) {
	srv, mgr, _ := dispatchTestEnv(t, false)
	root := t.TempDir()
	scion := filepath.Join(root, ".scion")
	writeHarnessConfigDirAt(t, filepath.Join(scion, "harness-configs", "tpl-hc"), scriptedHarnessYAML)
	// Templates resolve against the project path as given, as launch
	// resolves them.
	tplDir := filepath.Join(root, "templates", "tplx")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("harness_config: tpl-hc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body := postJSON(t, srv, "/api/v1/agents",
		`{"name": "se-agent", "id": "id-se", "slug": "se-agent", "projectPath": "`+root+`", "config": {"template": "tplx"}}`)
	assertPolicyRefusal(t, code, body, "tpl-hc")
	if mgr.provisionCalled || mgr.StartCalls() != 0 {
		t.Error("Provision/Start must not run when create admission refuses")
	}
	if _, err := os.Stat(filepath.Join(scion, "agents", "se-agent")); !os.IsNotExist(err) {
		t.Errorf("agent directory exists after refusal (stat err=%v)", err)
	}
}

// The early check uses the dispatch's explicit name, else the profile or
// settings default; it does not treat the template name as a harness-config
// name.
func TestPolicyHarnessConfigName_NoTemplateNameInference(t *testing.T) {
	srv, _, dotScion := dispatchTestEnv(t, false)
	writeHarnessConfig(t, dotScion, "tplname", scriptedHarnessYAML)
	req := CreateAgentRequest{ProjectPath: dotScion, Config: &CreateAgentConfig{Template: "tplname"}}
	name, _, _, _, err := srv.lookupHarnessConfigDirForPolicy(req, "", "")
	if err != nil || name == "tplname" {
		t.Errorf("the template name was used as the harness-config name: name=%q err=%v", name, err)
	}
}

// A stamped create carrying a harness-config ID but no name: the hydrated
// bundle is evaluated.
func TestCreateAgentGate_HydratedBundleWithoutName(t *testing.T) {
	srv, mgr, _ := dispatchTestEnv(t, false)
	attachHubHCStub(t, srv).setBundle(scriptedHarnessYAML)
	code, body := postJSON(t, srv, "/api/v1/agents",
		`{"name": "nn-agent", "id": "id-nn", "slug": "nn-agent", "provisionOnly": true, "config": {"harnessConfigId": "hc-id", "harnessConfigHash": "sha256:dispatch"}}`)
	if code != http.StatusForbidden || !strings.Contains(body, ErrCodeForbidden) {
		t.Fatalf("expected 403 for a hydrated scripted bundle without a name, got %d: %s", code, body)
	}
	if mgr.provisionCalled {
		t.Error("Provision must not run when the gate refuses")
	}
}

func refusalDetails(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var resp ErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("not an ErrorResponse: %v: %s", err, body)
	}
	return resp.Error.Details
}

// A refusal raised from inside Manager.Start carries the start-attempted
// details (Start may already have acted); a refusal from the early check,
// before any side effect, does not.
func TestHarnessPolicyHook_StartRefusalCarriesStartAttempted(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action+" from inside Start", func(t *testing.T) {
			srv, _ := hookTestServer(t, false, "start")
			code, body := postJSON(t, srv, "/api/v1/agents/sa-agent/"+action, `{"runId": "run-sa"}`)
			assertPolicyRefusal(t, code, body, "hook-hc")
			if d := refusalDetails(t, body); d[api.BrokerErrorDetailStartAttempted] != true {
				t.Errorf("expected %s=true in details, got %v", api.BrokerErrorDetailStartAttempted, d)
			}
		})
	}
	t.Run("start early check", func(t *testing.T) {
		srv, mgr, _ := dispatchTestEnv(t, false)
		attachHubHCStub(t, srv).setBundle(scriptedHarnessYAML)
		code, body := postAgentAction(t, srv, "gate-agent", "start", stampedStartBody)
		assertPolicyRefusal(t, code, body, "hub-only")
		if mgr.StartCalls() != 0 {
			t.Fatal("expected the early check to refuse before Start")
		}
		if d := refusalDetails(t, body); d[api.BrokerErrorDetailStartAttempted] != nil {
			t.Errorf("early-check refusal must not carry %s, got %v", api.BrokerErrorDetailStartAttempted, d)
		}
	})
}

// With allow=true no policy is attached, so launch behaves as with none.
func TestHarnessPolicyHook_NotAttachedWhenAllowed(t *testing.T) {
	srv, _, _ := dispatchTestEnv(t, true)
	if config.HarnessConfigPolicyFromContext(srv.withHarnessConfigPolicy(context.Background())) != nil {
		t.Error("allow=true must not attach a harness-config policy")
	}
	srv2, _, _ := dispatchTestEnv(t, false)
	if config.HarnessConfigPolicyFromContext(srv2.withHarnessConfigPolicy(context.Background())) == nil {
		t.Error("allow=false must attach the harness-config policy")
	}
}

// asyncHookManager evaluates the policy on its Start context, as pkg/agent
// does, after async admission has accepted the launch.
type asyncHookManager struct {
	*asyncManager
}

func (m *asyncHookManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	if err := agent.CheckHarnessConfigPolicy(ctx, "hook-hc", hookScriptedEntry); err != nil {
		return nil, err
	}
	return m.asyncManager.Start(ctx, opts)
}

// A refusal raised inside an async launch is reported with the
// harness_config_policy code and the policy's actionable message.
func TestHarnessPolicyHook_AsyncLaunchRefusalReported(t *testing.T) {
	mgr := &asyncHookManager{asyncManager: newAsyncManager()}
	srv, rtb := newAsyncTestServer(t, mgr)
	srv.config.AllowContainerScriptHarnesses = false

	var mu sync.Mutex
	var code, message string
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.State == hubclient.AgentLaunchReportStateFailed {
			mu.Lock()
			code, message = req.ErrorCode, req.Message
			mu.Unlock()
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-policy", "asyncLaunch": true, "launchId": "L-policy",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return code != ""
	}) {
		t.Fatal("expected a failed launch report")
	}
	mu.Lock()
	defer mu.Unlock()
	if code != "harness_config_policy" {
		t.Errorf("ErrorCode = %q, want harness_config_policy", code)
	}
	if !strings.Contains(message, "allow_container_script_harnesses=true") || !strings.Contains(message, "hook-hc") {
		t.Errorf("message should be the policy's actionable message, got %q", message)
	}
}

// A refusal for a harness-config that could not be evaluated is answered on
// start and restart with a 403, a neutral message naming the harness-config
// and the setting (no paths), and the start-attempted details.
func TestHarnessPolicyHook_NotEvaluatedRefusal(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			srv, _, _ := dispatchTestEnv(t, false)
			srv.manager = &hookManager{at: "start", notEvaluated: true}
			code, body := postJSON(t, srv, "/api/v1/agents/ne-agent/"+action, `{}`)
			if code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", code, body)
			}
			var resp ErrorResponse
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Error.Code != ErrCodeForbidden || resp.Error.Message != `harness-config "hook-hc" not permitted by policy (allow_container_script_harnesses)` {
				t.Errorf("unexpected refusal: %+v", resp.Error)
			}
			if resp.Error.Details[api.BrokerErrorDetailStartAttempted] != true {
				t.Errorf("expected start-attempted details, got %v", resp.Error.Details)
			}
		})
	}
}

// TestHarnessPolicyHook_RestartEarlyCheckBeforeStop (final-broker #3): restart
// evaluates the harness-config policy against the agent's saved harness-config
// before the stop. A scripted harness-config with the policy attached is
// refused with 403, the agent is not stopped, and the refusal carries no
// start-attempted details.
func TestHarnessPolicyHook_RestartEarlyCheckBeforeStop(t *testing.T) {
	srv, mgr, globalScion := dispatchTestEnv(t, false)
	writeHarnessConfig(t, globalScion, "scripted-hc", scriptedHarnessYAML)
	agentHome := config.GetAgentHomePath(globalScion, "restart-gate-agent")
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), []byte(`{"name": "restart-gate-agent", "harnessConfig": "scripted-hc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.agents = []api.AgentInfo{{
		ID: "restart-gate-agent", Name: "restart-gate-agent", ContainerID: "restart-gate-agent",
		ProjectPath: globalScion, Phase: "running",
		Labels: map[string]string{"scion.name": "restart-gate-agent"},
	}}
	code, body := postAgentAction(t, srv, "restart-gate-agent", "restart", `{}`)
	assertPolicyRefusal(t, code, body, "scripted-hc")
	if mgr.stopCalls != 0 || mgr.StartCalls() != 0 {
		t.Fatalf("expected the early check to refuse before Stop/Start, got stop=%d start=%d", mgr.stopCalls, mgr.StartCalls())
	}
	if d := refusalDetails(t, body); d[api.BrokerErrorDetailStartAttempted] != nil {
		t.Errorf("early-check refusal must not carry %s, got %v", api.BrokerErrorDetailStartAttempted, d)
	}
}
