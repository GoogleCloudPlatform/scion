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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestBuildStartContext_LaunchIDEnv pins SCION_LAUNCH_ID as broker-owned:
// a resolved-env value and its classification are always dropped, because
// Manager.Start sets the variable from the run ID it labels the container
// with.
func TestBuildStartContext_LaunchIDEnv(t *testing.T) {
	tests := []struct {
		name        string
		resolvedEnv map[string]string
		envCls      map[string]api.EnvKind
	}{
		{name: "absent"},
		{name: "resolved env value dropped", resolvedEnv: map[string]string{"SCION_LAUNCH_ID": "forged"}},
		{
			name:        "resolved env value and classification dropped",
			resolvedEnv: map[string]string{"SCION_LAUNCH_ID": "forged"},
			envCls:      map[string]api.EnvKind{"SCION_LAUNCH_ID": api.EnvKindPlain},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:               "my-agent",
				AgentID:            "uuid-1",
				RunID:              "run-1",
				ProjectPath:        filepath.Join(t.TempDir(), "my-project"),
				ResolvedEnv:        tt.resolvedEnv,
				EnvClassifications: tt.envCls,
				HTTPRequest:        httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:          opCreate,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := sc.Opts.Env["SCION_LAUNCH_ID"]; ok {
				t.Errorf("SCION_LAUNCH_ID = %q, want it unset in the start options", got)
			}
			if _, ok := sc.EnvClassifications["SCION_LAUNCH_ID"]; ok {
				t.Errorf("SCION_LAUNCH_ID classification kept")
			}
			if sc.Opts.RunID != "run-1" {
				t.Errorf("RunID = %q, want %q", sc.Opts.RunID, "run-1")
			}
		})
	}
}

func TestBuildStartContext_BasicFields(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-broker"
	cfg.Debug = true
	cfg.StateDir = t.TempDir()

	srv := newTestServerForStartContext(t, cfg)

	projectPath := filepath.Join(t.TempDir(), "my-project")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "my-agent",
		AgentID:     "uuid-1",
		Slug:        "my-agent-slug",
		ProjectID:   "project-1",
		ProjectPath: projectPath,
		Attach:      false,
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Name != "my-agent" {
		t.Errorf("expected name 'my-agent', got %q", sc.Opts.Name)
	}
	if !sc.Opts.BrokerMode {
		t.Error("expected BrokerMode to be true")
	}
	if sc.Opts.Detached == nil || !*sc.Opts.Detached {
		t.Error("expected Detached to be true when Attach=false")
	}

	// Verify broker identity env
	if sc.Opts.Env["SCION_BROKER_NAME"] != "test-broker" {
		t.Errorf("expected SCION_BROKER_NAME='test-broker', got %q", sc.Opts.Env["SCION_BROKER_NAME"])
	}
	if sc.Opts.Env["SCION_BROKER_ID"] != "broker-1" {
		t.Errorf("expected SCION_BROKER_ID='broker-1', got %q", sc.Opts.Env["SCION_BROKER_ID"])
	}
	if sc.Opts.Env["SCION_AGENT_ID"] != "uuid-1" {
		t.Errorf("expected SCION_AGENT_ID='uuid-1', got %q", sc.Opts.Env["SCION_AGENT_ID"])
	}
	if sc.Opts.Env["SCION_AGENT_SLUG"] != "my-agent-slug" {
		t.Errorf("expected SCION_AGENT_SLUG='my-agent-slug', got %q", sc.Opts.Env["SCION_AGENT_SLUG"])
	}
	if sc.Opts.Env["SCION_PROJECT_ID"] != "project-1" {
		t.Errorf("expected SCION_PROJECT_ID='project-1', got %q", sc.Opts.Env["SCION_PROJECT_ID"])
	}
	if _, ok := sc.Opts.Env["SCION_GROVE_ID"]; ok {
		t.Errorf("expected SCION_GROVE_ID to be absent, got %q", sc.Opts.Env["SCION_GROVE_ID"])
	}
	if sc.Opts.Env["SCION_PROJECT_PATH"] != projectPath {
		t.Errorf("expected SCION_PROJECT_PATH=%q, got %q", projectPath, sc.Opts.Env["SCION_PROJECT_PATH"])
	}
	if _, ok := sc.Opts.Env["SCION_GROVE_PATH"]; ok {
		t.Errorf("expected SCION_GROVE_PATH to be absent, got %q", sc.Opts.Env["SCION_GROVE_PATH"])
	}
	// cfg.Debug is set above: the broker's debug setting must not reach the
	// agent environment (ptone/scion#4098).
	for _, key := range []string{"SCION_DEBUG", "SCION_LOG_LEVEL"} {
		if v, ok := sc.Opts.Env[key]; ok {
			t.Errorf("expected %s to be absent when the broker runs with debug, got %q", key, v)
		}
		if _, ok := sc.EnvClassifications[key]; ok {
			t.Errorf("expected no %s classification when the broker runs with debug", key)
		}
	}
}

func TestBuildStartContext_EnvMerging(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-1",
		ResolvedEnv: map[string]string{
			"KEY_A": "from-hub",
			"KEY_B": "from-hub",
		},
		Config: &CreateAgentConfig{
			Env: []string{"KEY_B=from-config", "KEY_C=from-config"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	// ResolvedEnv is applied first, Config.Env overrides
	if sc.Opts.Env["KEY_A"] != "from-hub" {
		t.Errorf("expected KEY_A='from-hub', got %q", sc.Opts.Env["KEY_A"])
	}
	if sc.Opts.Env["KEY_B"] != "from-config" {
		t.Errorf("expected KEY_B='from-config' (config overrides hub), got %q", sc.Opts.Env["KEY_B"])
	}
	if sc.Opts.Env["KEY_C"] != "from-config" {
		t.Errorf("expected KEY_C='from-config', got %q", sc.Opts.Env["KEY_C"])
	}
}

// TestBuildStartContext_DebugNotPropagatedExplicitLogLevelKept covers
// ptone/scion#4098: a broker running with debug no longer adds SCION_DEBUG to
// agent environments, and an explicit SCION_LOG_LEVEL in the request env (hub
// env or the CLI's --agent-log-level) reaches the agent unchanged, with its
// classification intact.
func TestBuildStartContext_DebugNotPropagatedExplicitLogLevelKept(t *testing.T) {
	const spec = "warn,hub.auth=debug"

	for _, tc := range []struct {
		name        string
		resolvedEnv map[string]string
		config      *CreateAgentConfig
	}{
		{name: "resolved env", resolvedEnv: map[string]string{"SCION_LOG_LEVEL": spec}},
		{name: "config env", config: &CreateAgentConfig{Env: []string{"SCION_LOG_LEVEL=" + spec}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.Debug = true
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:               "agent-log-level",
				ResolvedEnv:        tc.resolvedEnv,
				EnvClassifications: map[string]api.EnvKind{"SCION_LOG_LEVEL": api.EnvKindPlain},
				Config:             tc.config,
				HTTPRequest:        r,
				Operation:          opCreate,
			})
			if err != nil {
				t.Fatal(err)
			}

			if got := sc.Opts.Env["SCION_LOG_LEVEL"]; got != spec {
				t.Errorf("SCION_LOG_LEVEL = %q, want %q unchanged", got, spec)
			}
			if got := sc.EnvClassifications["SCION_LOG_LEVEL"]; got != api.EnvKindPlain {
				t.Errorf("SCION_LOG_LEVEL classification = %q, want %q", got, api.EnvKindPlain)
			}
			if v, ok := sc.Opts.Env["SCION_DEBUG"]; ok {
				t.Errorf("SCION_DEBUG = %q, want it unset when the broker runs with debug", v)
			}
			if _, ok := sc.EnvClassifications["SCION_DEBUG"]; ok {
				t.Error("SCION_DEBUG classification added when the broker runs with debug")
			}
		})
	}
}

// TestBuildStartContext_AuthTokenPrecedence verifies the SCION_AUTH_TOKEN
// resolution precedence in buildStartContext step 3:
//  1. in.AgentToken (explicit hub-provided field) wins.
//  2. an existing token from ResolvedEnv (start/resume path) is kept and must
//     NOT be clobbered by the broker's own dev SCION_AUTH_TOKEN. This is the
//     regression case for the resume 401 ("compact JWS format must have three
//     parts").
//  3. the broker dev token is used only when neither of the above is present.
func TestBuildStartContext_AuthTokenPrecedence(t *testing.T) {
	// Valid-looking JWT (three dot-separated parts) minted by the hub.
	const hubToken = "header.payload.signature"
	const devToken = "broker-dev-token"
	const explicitToken = "explicit.agent.token"

	t.Run("resolvedEnv token kept over broker dev token", func(t *testing.T) {
		// Broker has its own (invalid-as-JWT) dev token in the environment.
		t.Setenv("SCION_AUTH_TOKEN", devToken)

		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-resume",
			// Hub minted the agent JWT into resolvedEnv on the start/resume path.
			ResolvedEnv: map[string]string{
				"SCION_AUTH_TOKEN": hubToken,
			},
			// AgentToken intentionally empty (start/resume path).
			HTTPRequest: r,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_AUTH_TOKEN"]; got != hubToken {
			t.Errorf("expected SCION_AUTH_TOKEN to keep hub-minted token %q, got %q (broker dev token must not clobber)", hubToken, got)
		}
	})

	t.Run("explicit AgentToken wins", func(t *testing.T) {
		t.Setenv("SCION_AUTH_TOKEN", devToken)

		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:       "agent-create",
			AgentToken: explicitToken,
			ResolvedEnv: map[string]string{
				"SCION_AUTH_TOKEN": hubToken,
			},
			HTTPRequest: r,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_AUTH_TOKEN"]; got != explicitToken {
			t.Errorf("expected SCION_AUTH_TOKEN=%q (explicit AgentToken wins), got %q", explicitToken, got)
		}
	})

	t.Run("broker dev token used as last resort", func(t *testing.T) {
		t.Setenv("SCION_AUTH_TOKEN", devToken)

		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-plain-broker",
			// No AgentToken and no SCION_AUTH_TOKEN in resolvedEnv.
			HTTPRequest: r,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_AUTH_TOKEN"]; got != devToken {
			t.Errorf("expected SCION_AUTH_TOKEN=%q (broker dev fallback), got %q", devToken, got)
		}
	})
}

func TestBuildStartContext_TelemetryOverride(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-1",
		ResolvedEnv: map[string]string{
			"SCION_TELEMETRY_ENABLED": "true",
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.TelemetryOverride == nil || !*sc.Opts.TelemetryOverride {
		t.Error("expected TelemetryOverride to be true when SCION_TELEMETRY_ENABLED=true")
	}
}

func TestBuildStartContext_ResolvedSecrets(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	secrets := []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Value: "secret-value"},
	}
	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:            "agent-1",
		ResolvedSecrets: secrets,
		HTTPRequest:     r,
		Operation:       opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Opts.ResolvedSecrets) != 1 || sc.Opts.ResolvedSecrets[0].Name != "API_KEY" {
		t.Errorf("expected resolved secrets to be passed through, got %v", sc.Opts.ResolvedSecrets)
	}
}

// TestBuildStartContext_DropsReservedTargetResolvedSecret verifies that an
// environment-type resolved secret whose target is reserved for scion's own
// control-plane env vars is dropped before it reaches opts.ResolvedSecrets,
// as defense in depth for a row that reached this call without going through
// the hub's create/patch check or dispatch-time drop. A non-reserved secret
// in the same request must still pass through.
func TestBuildStartContext_DropsReservedTargetResolvedSecret(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	secrets := []api.ResolvedSecret{
		{Name: "RESERVED_NAME_SECRET", Type: "environment", Target: "SCION_METADATA_MODE", Value: "passthrough"},
		{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "secret-value"},
	}
	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:            "agent-1",
		ResolvedSecrets: secrets,
		HTTPRequest:     r,
		Operation:       opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, rs := range sc.Opts.ResolvedSecrets {
		if rs.Target == "SCION_METADATA_MODE" {
			t.Errorf("expected reserved-target secret to be dropped, found it in opts.ResolvedSecrets: %+v", rs)
		}
	}
	if len(sc.Opts.ResolvedSecrets) != 1 || sc.Opts.ResolvedSecrets[0].Name != "API_KEY" {
		t.Errorf("expected only the non-reserved secret to pass through, got %v", sc.Opts.ResolvedSecrets)
	}
}

func TestBuildStartContext_ConfigFields(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-1",
		Config: &CreateAgentConfig{
			Template:      "my-template",
			Image:         "my-image:latest",
			HarnessConfig: "claude",
			HarnessAuth:   "api-key",
			Task:          "write tests",
			Workspace:     "/workspace",
			Profile:       "default",
			Branch:        "feature-1",
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Template != "my-template" {
		t.Errorf("expected Template='my-template', got %q", sc.Opts.Template)
	}
	if sc.Opts.Image != "my-image:latest" {
		t.Errorf("expected Image='my-image:latest', got %q", sc.Opts.Image)
	}
	if sc.Opts.HarnessConfig != "claude" {
		t.Errorf("expected HarnessConfig='claude', got %q", sc.Opts.HarnessConfig)
	}
	if sc.Opts.Task != "write tests" {
		t.Errorf("expected Task='write tests', got %q", sc.Opts.Task)
	}
	if sc.TemplateSlug != "my-template" {
		t.Errorf("expected TemplateSlug='my-template', got %q", sc.TemplateSlug)
	}
}

func TestBuildStartContext_GitClone(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectPath: "/some/path",
		Config: &CreateAgentConfig{
			Branch: "feature-1",
			GitClone: &api.GitCloneConfig{
				URL:    "https://github.com/org/repo.git",
				Branch: "main",
				Depth:  intPtr(1),
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_GIT_CLONE_URL"] != "https://github.com/org/repo.git" {
		t.Errorf("expected SCION_GIT_CLONE_URL set, got %q", sc.Opts.Env["SCION_GIT_CLONE_URL"])
	}
	if sc.Opts.Env["SCION_GIT_BRANCH"] != "main" {
		t.Errorf("expected SCION_GIT_BRANCH='main', got %q", sc.Opts.Env["SCION_GIT_BRANCH"])
	}
	if sc.Opts.Env["SCION_GIT_DEPTH"] != "1" {
		t.Errorf("expected SCION_GIT_DEPTH='1', got %q", sc.Opts.Env["SCION_GIT_DEPTH"])
	}
	if sc.Opts.Env["SCION_AGENT_BRANCH"] != "feature-1" {
		t.Errorf("expected SCION_AGENT_BRANCH='feature-1', got %q", sc.Opts.Env["SCION_AGENT_BRANCH"])
	}
	// Git clone mode should clear workspace but preserve project path
	// so ProvisionAgent can resolve the correct agent directory.
	if sc.Opts.Workspace != "" {
		t.Errorf("expected Workspace to be empty in git clone mode, got %q", sc.Opts.Workspace)
	}
	if sc.Opts.ProjectPath != "/some/path" {
		t.Errorf("expected ProjectPath to be preserved in git clone mode, got %q", sc.Opts.ProjectPath)
	}
}

func TestBuildStartContext_NilHTTPRequest(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Should not panic with nil HTTPRequest
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:      "agent-1",
		Operation: opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Opts.Name != "agent-1" {
		t.Errorf("expected name 'agent-1', got %q", sc.Opts.Name)
	}
}

// TestBuildStartContext_RequiresOperation proves buildStartContext rejects a
// missing Operation with a clear error rather than inferring one from
// whether HTTPRequest happens to be set (GoogleCloudPlatform/scion#1931).
func TestBuildStartContext_RequiresOperation(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-1",
		// Operation intentionally omitted.
	})
	if err == nil {
		t.Fatal("expected an error when Operation is not set")
	}
	if !strings.Contains(err.Error(), "Operation not set") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "Operation not set")
	}
}

// TestBuildStartContext_FreshProvisionSetOnlyForCreate proves
// sc.Opts.FreshProvision is true only for opCreate and false for
// opHTTPStart and opHTTPRestart (GoogleCloudPlatform/scion#1931).
func TestBuildStartContext_FreshProvisionSetOnlyForCreate(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	for _, tt := range []struct {
		op   startOperation
		want bool
	}{
		{op: opCreate, want: true},
		{op: opHTTPStart, want: false},
		{op: opHTTPRestart, want: false},
	} {
		t.Run(string(tt.op), func(t *testing.T) {
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-1",
				ProjectPath: "/some/path",
				Operation:   tt.op,
			})
			if err != nil {
				t.Fatalf("buildStartContext failed: %v", err)
			}
			if sc.Opts.FreshProvision != tt.want {
				t.Errorf("Opts.FreshProvision = %v, want %v for %s", sc.Opts.FreshProvision, tt.want, tt.op)
			}
		})
	}
}

// TestBuildStartContext_InvalidOperationCreatesNothing proves the Operation
// precondition runs before any directory or file side effect: given a
// ProjectPath/ProjectID combination that would otherwise create a project
// directory and write a marker file, an invalid Operation still leaves the
// filesystem untouched.
func TestBuildStartContext_InvalidOperationCreatesNothing(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	projectPath := filepath.Join(t.TempDir(), "not-yet-created")

	for _, tt := range []struct {
		name string
		op   startOperation
	}{
		{name: "empty", op: ""},
		{name: "unrecognized", op: startOperation("bogus")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-1",
				ProjectPath: projectPath,
				ProjectSlug: "some-project",
				ProjectID:   "project-uuid-1",
				Operation:   tt.op,
			})
			if err == nil {
				t.Fatal("expected an error for an invalid Operation")
			}
			if _, statErr := os.Stat(projectPath); !os.IsNotExist(statErr) {
				t.Errorf("expected %q not to be created, but os.Stat returned: %v", projectPath, statErr)
			}
		})
	}
}

func TestBuildStartContext_AttachMode(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:      "agent-1",
		Attach:    true,
		Operation: opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Opts.Detached == nil || *sc.Opts.Detached {
		t.Error("expected Detached to be false when Attach=true")
	}
}

func TestBuildStartContext_NoAuth(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	secrets := []api.ResolvedSecret{
		{Name: "CLAUDE_AUTH", Type: "file", Value: "secret-data", Target: "~/.claude/.credentials.json"},
		{Name: "API_KEY", Type: "environment", Value: "key-value", Target: "API_KEY"},
	}

	t.Run("NoAuth=true nils out secrets and sets opts.NoAuth", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:            "noauth-agent",
			ResolvedSecrets: secrets,
			NoAuth:          true,
			HTTPRequest:     r,
			Operation:       opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		if !sc.Opts.NoAuth {
			t.Error("expected opts.NoAuth to be true")
		}
		if sc.Opts.ResolvedSecrets != nil {
			t.Errorf("expected nil ResolvedSecrets with NoAuth, got %d", len(sc.Opts.ResolvedSecrets))
		}
	})

	t.Run("NoAuth=false passes secrets through", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:            "auth-agent",
			ResolvedSecrets: secrets,
			NoAuth:          false,
			HTTPRequest:     r,
			Operation:       opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		if sc.Opts.NoAuth {
			t.Error("expected opts.NoAuth to be false")
		}
		if len(sc.Opts.ResolvedSecrets) != 2 {
			t.Errorf("expected 2 resolved secrets, got %d", len(sc.Opts.ResolvedSecrets))
		}
	})
}

// TestBuildStartContext_WorkspaceMode verifies that SCION_WORKSPACE_MODE is emitted
// with the correct canonical value for each wire label, and defaults to shared-plain.
func TestBuildStartContext_WorkspaceMode(t *testing.T) {
	cases := []struct {
		name      string
		wireLabel string
		wantMode  string
	}{
		{
			name:      "shared wire label",
			wireLabel: store.WorkspaceModeShared,
			wantMode:  "shared-plain",
		},
		{
			name:      "per-agent wire label",
			wireLabel: store.WorkspaceModePerAgent,
			wantMode:  "clone-per-agent",
		},
		{
			name:      "worktree-per-agent wire label",
			wireLabel: store.WorkspaceModeWorktreePerAgent,
			wantMode:  "worktree-per-agent",
		},
		{
			name:      "empty wire label defaults to shared-plain",
			wireLabel: "",
			wantMode:  "shared-plain",
		},
		{
			name:      "unrecognized wire label defaults to shared-plain",
			wireLabel: "unknown-mode",
			wantMode:  "shared-plain",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:          "agent-1",
				WorkspaceMode: tc.wireLabel,
				HTTPRequest:   r,
				Operation:     opCreate,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := sc.Opts.Env["SCION_WORKSPACE_MODE"]; got != tc.wantMode {
				t.Errorf("SCION_WORKSPACE_MODE: got %q, want %q", got, tc.wantMode)
			}
		})
	}
}

// TestBuildStartContext_WorkspaceMode_StartPathFallback verifies that on the
// start/restart path (WorkspaceMode==""), a hub-injected SCION_WORKSPACE_MODE in
// resolvedEnv is propagated to the container env, and the broker-side code does
// not overwrite it with the shared-plain default.
func TestBuildStartContext_WorkspaceMode_StartPathFallback(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-resume",
		// Hub injects canonical value via resolvedEnv on start path.
		ResolvedEnv: map[string]string{
			"SCION_WORKSPACE_MODE": "clone-per-agent",
			"SCION_WORKSPACE_GIT":  "true",
		},
		// WorkspaceMode intentionally empty (start/restart path).
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := sc.Opts.Env["SCION_WORKSPACE_MODE"]; got != "clone-per-agent" {
		t.Errorf("SCION_WORKSPACE_MODE: got %q, want %q", got, "clone-per-agent")
	}
	if got := sc.Opts.Env["SCION_WORKSPACE_GIT"]; got != "true" {
		t.Errorf("SCION_WORKSPACE_GIT: got %q, want %q", got, "true")
	}
}

// TestBuildStartContext_WorkspaceGit verifies that SCION_WORKSPACE_GIT is emitted
// correctly based on the provisioning path.
//
// Coverage note: the highest-priority path — worktreeProvisioned=true (from
// tryProvisionWorktree in start_context.go) — is not covered here because it
// requires a real host-side git repository setup that is outside unit test scope.
// The code path is simple (isGitWorkspace := worktreeProvisioned || ...) and
// covered by the worktree integration tests. The remaining three priority levels
// (GitClone config, on-disk git dir, hub-injected resolvedEnv) are tested below.
func TestBuildStartContext_WorkspaceGit(t *testing.T) {
	t.Run("git clone config implies git workspace", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-clone",
			Config: &CreateAgentConfig{
				GitClone: &api.GitCloneConfig{
					URL:    "https://github.com/example/repo.git",
					Branch: "main",
				},
			},
			HTTPRequest: r,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_WORKSPACE_GIT"]; got != "true" {
			t.Errorf("SCION_WORKSPACE_GIT: got %q, want %q", got, "true")
		}
	})

	t.Run("no git clone and no workspace implies absent git var", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-plain",
			Config:      &CreateAgentConfig{},
			HTTPRequest: r,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if v, present := sc.Opts.Env["SCION_WORKSPACE_GIT"]; present {
			t.Errorf("SCION_WORKSPACE_GIT should be absent for non-git workspace, got %q", v)
		}
	})

	t.Run("start path hub-injected SCION_WORKSPACE_GIT propagates", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-start",
			ResolvedEnv: map[string]string{
				"SCION_WORKSPACE_GIT": "true",
			},
			HTTPRequest: r,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_WORKSPACE_GIT"]; got != "true" {
			t.Errorf("SCION_WORKSPACE_GIT: got %q, want %q", got, "true")
		}
	})

	t.Run("shared-plain git workspace on disk", func(t *testing.T) {
		// Create a temporary git repo to simulate a shared-plain git workspace
		gitDir := t.TempDir()
		initCmd := exec.Command("git", "init", gitDir)
		if out, err := initCmd.CombinedOutput(); err != nil {
			t.Skipf("git init failed: %s", string(out))
		}

		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-shared-git",
			Config: &CreateAgentConfig{
				Workspace: gitDir,
			},
			HTTPRequest: r,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := sc.Opts.Env["SCION_WORKSPACE_GIT"]; got != "true" {
			t.Errorf("SCION_WORKSPACE_GIT: got %q, want %q (shared git workspace on disk should set it)", got, "true")
		}
	})
}

// TestBuildStartContext_EnvClassificationsCarried verifies that the merged
// env classification map (hub-sent + broker-written keys) survives
// buildStartContext on the returned startContext. Two cases:
//
//  1. Hub sends a non-nil EnvClassifications → the returned field is non-nil,
//     contains hub keys, AND additionally contains broker-written keys.
//  2. Hub sends nil (old hub / version skew) → the returned field is nil,
//     NOT an empty map. This preserves the three-state contract described on
//     api.EnvKind: nil means "classification unavailable", distinct from
//     "all classified" (non-nil empty map).
//
// Pinning these invariants prevents a regression where buildStartContext
// computes the classification map but drops it on return, making the
// broker's ~35 classification sites unrecoverable downstream.
func TestBuildStartContext_EnvClassificationsCarried(t *testing.T) {
	t.Run("non-nil hub classifications merged with broker keys", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		hubCls := map[string]api.EnvKind{
			"HUB_VAR": api.EnvKindPlain,
		}

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:               "agent-cls",
			EnvClassifications: hubCls,
			HTTPRequest:        r,
			Operation:          opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		if sc.EnvClassifications == nil {
			t.Fatal("EnvClassifications is nil; expected non-nil map carrying hub + broker keys")
		}

		// Hub key must survive.
		if kind, ok := sc.EnvClassifications["HUB_VAR"]; !ok {
			t.Error("hub key HUB_VAR missing from EnvClassifications")
		} else if kind != api.EnvKindPlain {
			t.Errorf("HUB_VAR: got kind %q, want %q", kind, api.EnvKindPlain)
		}

		// Broker-written key: SCION_WORKSPACE_MODE is always classified by the
		// broker (it falls through to the shared-plain default when no
		// WorkspaceMode is provided). Assert the specific key and kind, not
		// just len() > 0, so this test cannot pass on the wrong map.
		if kind, ok := sc.EnvClassifications["SCION_WORKSPACE_MODE"]; !ok {
			t.Error("broker key SCION_WORKSPACE_MODE missing from EnvClassifications")
		} else if kind != api.EnvKindPlain {
			t.Errorf("SCION_WORKSPACE_MODE: got kind %q, want %q", kind, api.EnvKindPlain)
		}
	})

	t.Run("nil hub classifications stays nil", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		r := httptest.NewRequest("POST", "/api/v1/agents", nil)
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name: "agent-nil-cls",
			// EnvClassifications intentionally nil — simulates an old hub
			// that does not send classification data.
			HTTPRequest: r,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Must be nil, NOT an empty non-nil map. assert.Empty would pass
		// for both; this explicit nil check is the point of this test case.
		if sc.EnvClassifications != nil {
			t.Errorf("EnvClassifications: got %v (len %d), want nil — "+
				"nil hub classifications must propagate as nil to preserve "+
				"the three-state contract", sc.EnvClassifications, len(sc.EnvClassifications))
		}
	})
}
