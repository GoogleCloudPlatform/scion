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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func newTestServerForStartContext(t *testing.T, cfg ServerConfig) *Server {
	t.Helper()
	return newTestServerForStartContextRuntime(t, cfg, "mock")
}

// newTestServerForStartContextRuntime is newTestServerForStartContext with a
// caller-chosen runtime name, so tests can exercise runtime-conditional
// behavior in buildStartContext (which reads s.runtime.Name()) without a real
// Kubernetes or Docker runtime.
func newTestServerForStartContextRuntime(t *testing.T, cfg ServerConfig, runtimeName string) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWd)
	})

	dotScion := filepath.Join(tmpDir, ".scion")
	if err := os.Mkdir(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	// The active profile's runtime type matches runtimeName, and
	// cfg.ForceRuntime below is set to the same value: resolveManagerForOpts
	// (handlers.go) then short-circuits on its ForceRuntime-equals-default
	// check for the no-explicit-profile case, exactly as it does outside
	// tests, rather than falling through to settings resolution and finding
	// a mismatched type.
	settingsYAML := fmt.Sprintf(`schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: %s
runtimes:
    %s:
        type: %s
`, runtimeName, runtimeName, runtimeName)
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create dummy templates to satisfy FindTemplate
	templatesDir := filepath.Join(dotScion, "templates")
	if err := os.MkdirAll(templatesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templatesDir, "default"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templatesDir, "claude"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg.ForceRuntime = runtimeName
	mgr := &envCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return runtimeName }}
	return New(cfg, mgr, rt)
}

// newTestServerForStartContextMultiProfile is newTestServerForStartContextRuntime
// with a second, differently-typed profile registered alongside the active
// one, so tests can exercise a dispatch whose Config.Profile (or saved
// profile) names a runtime other than the broker's default — the scenario
// resolveManagerForOpts exists to get right (ptone/scion#2328). The
// broker's default runtime is defaultRuntimeName (used when no profile is
// named, matching newTestServerForStartContextRuntime's setup); the second
// profile is named otherProfile and resolves to otherRuntimeName.
func newTestServerForStartContextMultiProfile(t *testing.T, cfg ServerConfig, defaultRuntimeName, otherProfile, otherRuntimeName string) (*Server, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWd)
	})

	dotScion := filepath.Join(tmpDir, ".scion")
	if err := os.Mkdir(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := fmt.Sprintf(`schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: %s
    %s:
        runtime: %s
runtimes:
    %s:
        type: %s
    %s:
        type: %s
`, defaultRuntimeName, otherProfile, otherRuntimeName, defaultRuntimeName, defaultRuntimeName, otherRuntimeName, otherRuntimeName)
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	templatesDir := filepath.Join(dotScion, "templates")
	if err := os.MkdirAll(templatesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templatesDir, "default"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templatesDir, "claude"), 0755); err != nil {
		t.Fatal(err)
	}

	// Deliberately no cfg.ForceRuntime here (unlike
	// newTestServerForStartContextRuntime): ForceRuntime short-circuits
	// resolveManagerForOpts before it ever consults settings.yaml, which
	// would defeat the entire point of this helper — exercising a dispatch
	// whose profile names a different runtime than the broker's default.
	mgr := &envCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return defaultRuntimeName }}
	srv := New(cfg, mgr, rt)
	// Once settings resolve the dispatch's profile to otherRuntimeName
	// (differing from defaultRuntimeName), resolveManagerForOpts falls
	// through to this resolver to construct it. A real "kubernetes" runtime
	// fails to construct in a test sandbox with no cluster available — unlike
	// "docker", whose construction defers failure to actual use — so this
	// returns a mock instead, the same pattern
	// newTestServerForSavedProfileRemap (hub_default_passthrough_downgrade_test.go)
	// uses for the identical problem. This helper only ever registers one
	// non-default profile (otherProfile -> otherRuntimeName), so the
	// resolver can report otherRuntimeName unconditionally rather than
	// inspecting profileFlag.
	srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profileFlag string) runtime.Runtime {
		return &runtime.MockRuntime{NameFunc: func() string { return otherRuntimeName }}
	}
	return srv, dotScion
}

// writeSavedAgentProfile writes an agent-info.json recording profile as the
// agent's saved profile, the on-disk source agent.GetSavedProfile reads from
// on start/restart dispatch (see pkg/agent/provision.go). dotScionDir is the
// project's .scion directory (as returned alongside projectPath from the
// test's own project setup), matching what config.GetAgentHomePath expects.
func writeSavedAgentProfile(t *testing.T, dotScionDir, agentName, profile string) {
	t.Helper()
	agentHome := config.GetAgentHomePath(dotScionDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	info := api.AgentInfo{Name: agentName, Profile: profile}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), data, 0o644); err != nil {
		t.Fatal(err)
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
	if sc.Opts.Env["SCION_DEBUG"] != "1" {
		t.Errorf("expected SCION_DEBUG='1', got %q", sc.Opts.Env["SCION_DEBUG"])
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

// TestRedactCloneURL covers the userinfo shapes a clone URL can carry: a
// user:pass pair, a username-only token (the "https://TOKEN@host" form,
// which net/url's Redacted() alone would leave visible since there is no
// password to mask), a token carried in the query string or fragment
// instead of userinfo, and an unparseable URL — none of which may ever be
// logged or returned to a client raw.
func TestRedactCloneURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "user and password",
			in:   "https://user:supersecret@github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "username-only token",
			in:   "https://ghp_supersecrettoken@github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "token in query string",
			in:   "https://github.com/org/repo.git?access_token=supersecret",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "token in fragment",
			in:   "https://github.com/org/repo.git#access_token=supersecret",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "no credentials",
			in:   "https://github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
		},
		{
			name: "unparseable",
			in:   "://not a url",
			want: "<unparseable>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactCloneURL(tt.in)
			if got != tt.want {
				t.Errorf("redactCloneURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.Contains(got, "supersecret") {
				t.Errorf("redactCloneURL(%q) still contains the credential: %q", tt.in, got)
			}
		})
	}
}

// TestSanitizeCloneErrorText covers the two forms a clone URL can take
// inside a provisioning error: the exact raw URL this package embeds into
// its own error text, and git's own reformatted echo of the same URL in its
// stderr output (which already strips userinfo on its own, but can still
// carry the query string or fragment).
func TestSanitizeCloneErrorText(t *testing.T) {
	t.Run("userinfo and query embedded verbatim", func(t *testing.T) {
		rawURL := "https://SUPERSECRETTOKEN@127.0.0.1:1/x.git?access_token=QSECRET"
		errText := "ProvisionShared: git clone: git clone " + rawURL + ": exit status 128"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "SUPERSECRETTOKEN") {
			t.Errorf("sanitized error text still contains the userinfo token: %q", got)
		}
		if strings.Contains(got, "QSECRET") {
			t.Errorf("sanitized error text still contains the query-string token: %q", got)
		}
		if !strings.Contains(got, "127.0.0.1") {
			t.Errorf("expected the host to remain in the sanitized text, got: %q", got)
		}
	})

	t.Run("a fragment embedded verbatim", func(t *testing.T) {
		rawURL := "https://127.0.0.1:1/x.git#FRAGMENTTOKEN"
		errText := "git clone " + rawURL + ": exit status 128"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "FRAGMENTTOKEN") {
			t.Errorf("sanitized error text still contains the fragment: %q", got)
		}
	})

	t.Run("a differently formatted echo carrying only the userinfo", func(t *testing.T) {
		// Isolates the userinfo strip on its own: the port differs from the
		// raw URL, so the exact raw URL is not a substring of errText at
		// all, and the wholesale exact-match replace cannot apply here —
		// only the individual userinfo strip can remove the token.
		rawURL := "https://SECRETUSER@127.0.0.1:1/x.git"
		errText := "fatal: Authentication failed for 'https://SECRETUSER@127.0.0.1/x.git'"
		if strings.Contains(errText, rawURL) {
			t.Fatalf("test setup error: errText must not contain the exact raw URL %q", rawURL)
		}
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "SECRETUSER") {
			t.Errorf("sanitized error text still contains the userinfo token from a reformatted echo: %q", got)
		}
	})

	t.Run("a differently formatted echo carrying only the fragment", func(t *testing.T) {
		// Isolates the fragment strip on its own, the same way the query
		// case above isolates the query strip: the port differs, so the
		// exact raw URL is not a substring of errText.
		rawURL := "https://127.0.0.1:1/x.git#FRAGMENTTOKEN"
		errText := "fatal: unable to access 'https://127.0.0.1/x.git#FRAGMENTTOKEN'"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "FRAGMENTTOKEN") {
			t.Errorf("sanitized error text still contains the fragment token from a reformatted echo: %q", got)
		}
	})

	t.Run("a differently formatted echo of the same URL", func(t *testing.T) {
		// git's own diagnostic output strips userinfo on its own when it
		// echoes a URL, but can still carry the query string, in a form
		// that differs from the exact raw URL string (for example a
		// trailing slash) — so a wholesale match against the raw URL alone
		// does not catch it; the query-string strip must apply on its own.
		rawURL := "https://SUPERSECRETTOKEN@127.0.0.1:1/x.git?access_token=QSECRET"
		errText := "fatal: unable to access 'https://127.0.0.1:1/x.git?access_token=QSECRET/'"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "QSECRET") {
			t.Errorf("sanitized error text still contains the query-string token from a reformatted echo: %q", got)
		}
	})

	t.Run("a percent-encoded userinfo is removed only by the exact-match replace", func(t *testing.T) {
		// url.Userinfo.String() re-escapes minimally, so a userinfo that
		// arrived percent-encoded (here "%7E" for "~") comes back out as a
		// literal "~" — a different string than the exact raw URL text.
		// The individual userinfo strip searches for that decoded form and
		// does not find it; only the wholesale exact-match replace, which
		// compares against the raw URL text as given, removes it.
		rawURL := "https://SECRET%7Etok@127.0.0.1:1/x.git"
		errText := "git clone " + rawURL + ": exit status 128"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "SECRET") {
			t.Errorf("sanitized error text still contains the percent-encoded userinfo: %q", got)
		}
	})

	t.Run("a percent-encoded fragment is removed only by the exact-match replace", func(t *testing.T) {
		// u.Fragment is the decoded form ("%41" -> "A"), which differs from
		// the raw URL's own percent-encoded text. Same reasoning as above,
		// for the fragment strip instead of the userinfo strip.
		rawURL := "https://127.0.0.1:1/x.git#FRAG%41TOK"
		errText := "git clone " + rawURL + ": exit status 128"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "FRAG%41TOK") {
			t.Errorf("sanitized error text still contains the percent-encoded fragment: %q", got)
		}
	})

	t.Run("an empty userinfo does not strip unrelated @ signs", func(t *testing.T) {
		// A URL with an empty userinfo section (e.g. "https://@host/...")
		// parses with a non-nil but empty u.User. u.User.String() is then
		// "", so the naive strip pattern would be just "@" and remove every
		// "@" in the text, not only a genuine credential.
		rawURL := "https://@example.com/r.git"
		errText := "fatal: could not read from remote repository, please check access rights: ssh git@other.example and try again"
		got := sanitizeCloneErrorText(errText, rawURL)
		if !strings.Contains(got, "git@other.example") {
			t.Errorf("sanitized error text lost an unrelated @, got: %q", got)
		}
	})

	t.Run("a userinfo of just a colon does not strip unrelated :@ text", func(t *testing.T) {
		// A URL with an empty username and an empty password separated by a
		// colon (e.g. "https://:@host/...") also parses with a non-nil
		// u.User, and u.User.String() is ":" — carrying no credential —
		// but the naive strip pattern would still remove every ":@" in the
		// text.
		rawURL := "https://:@example.com/r.git"
		errText := "unrelated text containing a:@b elsewhere"
		got := sanitizeCloneErrorText(errText, rawURL)
		if !strings.Contains(got, "a:@b") {
			t.Errorf("sanitized error text lost unrelated \":@\" text, got: %q", got)
		}
	})

	t.Run("a differently formatted echo carrying only a password", func(t *testing.T) {
		// An empty username with a non-empty password still has to be
		// stripped: the port differs, so only the individual userinfo strip
		// can remove it.
		rawURL := "https://:PWONLY@127.0.0.1:1/x.git"
		errText := "fatal: Authentication failed for 'https://:PWONLY@127.0.0.1/x.git'"
		if strings.Contains(errText, rawURL) {
			t.Fatalf("test setup error: errText must not contain the exact raw URL %q", rawURL)
		}
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "PWONLY") {
			t.Errorf("sanitized error text still contains the password from a reformatted echo: %q", got)
		}
	})

	t.Run("an unparseable URL still strips the exact raw text", func(t *testing.T) {
		rawURL := "https://user:TOKEN@host/%zz"
		errText := "git clone " + rawURL + ": exit status 128"
		got := sanitizeCloneErrorText(errText, rawURL)
		if strings.Contains(got, "TOKEN") {
			t.Errorf("sanitized error text still contains the credential from an unparseable URL: %q", got)
		}
		if strings.Contains(got, rawURL) {
			t.Errorf("sanitized error text still contains the raw URL verbatim: %q", got)
		}
	})

	t.Run("empty inputs pass through unchanged", func(t *testing.T) {
		if got := sanitizeCloneErrorText("", "https://host/r.git"); got != "" {
			t.Errorf("expected empty errText to stay empty, got %q", got)
		}
		if got := sanitizeCloneErrorText("some error", ""); got != "some error" {
			t.Errorf("expected an empty rawURL to leave errText unchanged, got %q", got)
		}
	})
}

// TestTryProvisionWorktree_FallbackFailureLogNeverContainsCredentials drives
// a fresh agent's first provisioning attempt against an unreachable URL
// carrying both a userinfo token and a query-string token through
// provision.ProvisionShared's real git clone, and captures the resulting
// slog.Warn log line next to the already-redacted clone_url attribute.
// Neither token may appear anywhere in the captured log output.
func TestTryProvisionWorktree_FallbackFailureLogNeverContainsCredentials(t *testing.T) {
	requireWorktreeGit(t)

	const secretToken = "SUPERSECRETSAUCE"
	credentialedURL := "https://" + secretToken + "@127.0.0.1:1/x.git?access_token=" + secretToken

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldLogger)

	opts := &api.StartOptions{}
	_, _ = srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: credentialedURL}},
	}, opts, map[string]string{}, srv.runtime.Name())

	logged := buf.String()
	if strings.Contains(logged, secretToken) {
		t.Errorf("provisioning-failure log must never contain the clone URL's token, got: %s", logged)
	}
}

// TestBuildStartContext_WorktreePerAgentStart_PreExistedFailureLogNeverContainsCredentials
// is the preExisted-path counterpart: a second start for an agent whose
// worktree already exists, injected with a credentialed URL and a
// provisioning failure, must never write the clone URL's userinfo or
// query-string token to the slog.Error log line at that call site either.
//
// The injected failure here (the agent's own worktree target replaced with a
// plain file) reaches provision.ProvisionShared through the same code path
// as every other preExisted failure: since the fail-closed guard just above
// this call already requires the provisioning sentinel to exist, ProvisionShared
// always skips its own git-clone step and goes straight to ensureWorktree —
// so its returned error can never itself contain the raw clone URL here,
// only a worktree-shape or branch message. sanitizeCloneErrorText is still
// applied defensively regardless. What this test actually pins is the
// "clone_url" attribute logged alongside it: it is always the redactCloneURL
// output, never GitClone.URL itself, independent of how ProvisionShared
// fails.
func TestBuildStartContext_WorktreePerAgentStart_PreExistedFailureLogNeverContainsCredentials(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	in := startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	}

	sc1, err := srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("first buildStartContext failed: %v", err)
	}
	worktreePath := sc1.Opts.Workspace
	if worktreePath == "" {
		t.Fatal("expected a worktree Workspace path to be set")
	}

	// Replace the agent's own worktree with a plain file so ensureWorktree's
	// own-path check refuses to reuse or remove it on the next call.
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worktreePath, []byte("not a worktree"), 0o644); err != nil {
		t.Fatal(err)
	}

	const secretToken = "SUPERSECRETSAUCE"
	in.Config.GitClone = &api.GitCloneConfig{URL: "https://" + secretToken + "@127.0.0.1:1/x.git?access_token=" + secretToken}

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldLogger)

	if _, err := srv.buildStartContext(context.Background(), in); err == nil {
		t.Fatal("expected the second buildStartContext to fail")
	}

	logged := buf.String()
	if strings.Contains(logged, secretToken) {
		t.Errorf("the preExisted-failure log must never contain the clone URL's token, got: %s", logged)
	}
	if !strings.Contains(logged, `clone_url=`) {
		t.Fatalf("expected the log line to carry a clone_url attribute, got: %s", logged)
	}
}

// TestIsStrictWorktreeChild guards the only shape of path
// tryProvisionWorktree is ever allowed to pass to `git worktree remove` or
// os.RemoveAll: a real descendant of <base>/worktrees, never that directory
// itself and never something outside it. An empty AgentID makes
// provision.WorktreePath resolve to the shared "worktrees" parent directory
// of every agent (GoogleCloudPlatform/scion#1931) — the shape this check
// exists to reject.
func TestIsStrictWorktreeChild(t *testing.T) {
	base := "/proj/workspace"
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"real descendant", "/proj/workspace/worktrees/agent-1", true},
		{"nested descendant", "/proj/workspace/worktrees/agent-1/sub", true},
		{"the worktrees dir itself", "/proj/workspace/worktrees", false},
		{"the worktrees dir with trailing slash", "/proj/workspace/worktrees/", false},
		{"a sibling directory", "/proj/workspace/other", false},
		{"outside the project entirely", "/etc/passwd", false},
		{"a relative segment resolving back out", "/proj/workspace/worktrees/../other", false},
		{"empty path", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStrictWorktreeChild(base, tt.path); got != tt.want {
				t.Errorf("isStrictWorktreeChild(%q, %q) = %v, want %v", base, tt.path, got, tt.want)
			}
		})
	}
	if isStrictWorktreeChild("", "/proj/workspace/worktrees/agent-1") {
		t.Error("expected false when base is empty")
	}
}

// TestShouldCleanupPartialWorktree_NeverTheSharedWorktreesDir is the direct
// unit-level guard for GoogleCloudPlatform/scion#1931: provision.WorktreePath
// resolves to the shared "worktrees" parent directory itself when AgentID is
// empty, so a resolver bug that ever produces that value as "the worktree
// path" must never be allowed to authorize its removal — nor to authorize
// removing anything that pre-existed.
func TestShouldCleanupPartialWorktree_NeverTheSharedWorktreesDir(t *testing.T) {
	projectRoot := "/proj/workspace"
	sharedWorktreesDir := filepath.Join(projectRoot, "worktrees")
	ownWorktree := filepath.Join(sharedWorktreesDir, "agent-1")

	tests := []struct {
		name         string
		projectRoot  string
		worktreePath string
		preExisted   bool
		want         bool
	}{
		{"normal partial cleanup, not preExisted", projectRoot, ownWorktree, false, true},
		{"never when preExisted, even for a valid own path", projectRoot, ownWorktree, true, false},
		{"never the shared worktrees dir itself, even if not preExisted", projectRoot, sharedWorktreesDir, false, false},
		{"never outside the project", projectRoot, "/etc/passwd", false, false},
		{"empty worktreePath", projectRoot, "", false, false},
		{"empty projectRoot", "", ownWorktree, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldCleanupPartialWorktree(tt.projectRoot, tt.worktreePath, tt.preExisted); got != tt.want {
				t.Errorf("shouldCleanupPartialWorktree(%q, %q, %v) = %v, want %v",
					tt.projectRoot, tt.worktreePath, tt.preExisted, got, tt.want)
			}
		})
	}
}

// TestTryProvisionWorktree_InvalidAgentIDLeavesSharedBaseIntact is an
// end-to-end guard (through the real resolveWorktreeProvision ->
// tryProvisionWorktree path, not a hand-built worktreeProvisionResult) for
// an AgentID of "..". Since provision.WorktreePath(base, agentID) is
// filepath.Join(base, "worktrees", agentID), an AgentID of ".." would
// otherwise resolve to base itself (filepath.Join cleans "worktrees/.."
// away) — the shared clone root holding the common .git and every other
// agent's worktrees. isSingleCleanPathElement rejects this AgentID outright,
// before anything is resolved or created on disk; validateMountedWorktree
// is a second, independent check on whatever path is finally about to be
// mounted.
func TestTryProvisionWorktree_InvalidAgentIDLeavesSharedBaseIntact(t *testing.T) {
	requireWorktreeGit(t)

	for _, agentID := range []string{"..", "a/b"} {
		t.Run("agentID="+agentID, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			projectPath := filepath.Join(t.TempDir(), "proj")
			if err := os.MkdirAll(projectPath, 0o755); err != nil {
				t.Fatal(err)
			}
			invalidGitClone := &api.GitCloneConfig{URL: filepath.Join(t.TempDir(), "does-not-exist.git")}

			opts := &api.StartOptions{}
			ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
				Name: "agent-x", AgentID: agentID,
				ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				Config:        &CreateAgentConfig{GitClone: invalidGitClone},
			}, opts, map[string]string{}, srv.runtime.Name())

			if ok {
				t.Error("expected ok=false for an invalid AgentID")
			}
			if err == nil {
				t.Error("expected an error rejecting the invalid AgentID")
			}

			// The shared base must never be created, let alone removed, as a
			// side effect of this rejected attempt: any stat error other than
			// "does not exist" would indicate something unexpected happened
			// to it.
			base := filepath.Join(projectPath, "workspace")
			if _, statErr := os.Stat(base); statErr != nil && !os.IsNotExist(statErr) {
				t.Fatalf("unexpected error checking the shared base for AgentID=%q: %v", agentID, statErr)
			}
		})
	}
}

// setUpAgent1SharedBase provisions a first agent's real worktree via
// tryProvisionWorktree, establishing the shared base clone that later
// scenarios in this file set up pre-existing state under.
func setUpAgent1SharedBase(t *testing.T, srv *Server, projectPath, bare string) {
	t.Helper()
	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-1", AgentID: "agent-1",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())
	if err != nil || !ok {
		t.Fatalf("setup: tryProvisionWorktree for agent-1: ok=%v err=%v", ok, err)
	}
}

// TestTryProvisionWorktree_SymlinkedOwnWorktreeRejected proves a symlink
// planted at an agent's own worktree target — pointing at a directory
// outside the shared base — is rejected rather than mounted, and that the
// symlink and its target are left untouched.
func TestTryProvisionWorktree_SymlinkedOwnWorktreeRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	outsideDir := t.TempDir()
	sentinelFile := filepath.Join(outsideDir, "sentinel.txt")
	const sentinelContent = "must not be mounted"
	if err := os.WriteFile(sentinelFile, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}

	agent2Path := filepath.Join(base, "worktrees", "agent-2")
	if err := os.Symlink(outsideDir, agent2Path); err != nil {
		t.Fatal(err)
	}

	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-2", AgentID: "agent-2",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false for a symlinked worktree target")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the symlinked worktree target")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
	if target, readErr := os.Readlink(agent2Path); readErr != nil || target != outsideDir {
		t.Errorf("expected the symlink to survive unchanged, got target=%q err=%v", target, readErr)
	}
	if got, readErr := os.ReadFile(sentinelFile); readErr != nil || string(got) != sentinelContent {
		t.Errorf("expected the outside file to survive untouched, got %q err=%v", got, readErr)
	}
}

// TestTryProvisionWorktree_SymlinkedWorktreesDirRejected proves that if the
// shared base's own "worktrees" directory is itself a symlink to somewhere
// outside the base, provisioning is rejected before it creates anything
// through that symlink.
func TestTryProvisionWorktree_SymlinkedWorktreesDirRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	worktreesDir := filepath.Join(base, "worktrees")
	outsideDir := t.TempDir()

	if err := os.RemoveAll(worktreesDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, worktreesDir); err != nil {
		t.Fatal(err)
	}

	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-2", AgentID: "agent-2",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false when the worktrees directory is a symlink")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the symlinked worktrees directory")
	}
	entries, _ := os.ReadDir(outsideDir)
	if len(entries) != 0 {
		t.Errorf("expected nothing created in the outside directory, found: %v", entries)
	}
}

// TestTryProvisionWorktree_SharerRegistryOutsidePathRejected proves that a
// sharer-registry marker naming a worktree path outside the shared base is
// never joined or mounted.
func TestTryProvisionWorktree_SharerRegistryOutsidePathRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	outsideDir := t.TempDir()
	sentinelFile := filepath.Join(outsideDir, "sentinel.txt")
	const sentinelContent = "must not be mounted"
	if err := os.WriteFile(sentinelFile, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := provision.RegisterSharer(base, "agent-3", outsideDir, "some-other-agent"); err != nil {
		t.Fatalf("plant sharer marker: %v", err)
	}

	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-3", AgentID: "agent-3",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false when the sharer registry names a path outside the base")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the sharer-registry entry")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
	if got, readErr := os.ReadFile(sentinelFile); readErr != nil || string(got) != sentinelContent {
		t.Errorf("expected the outside file to survive untouched, got %q err=%v", got, readErr)
	}
}

// TestTryProvisionWorktree_SharerRegistryFakeGitfileStillRejected proves the
// final mount-time gate catches a sharer-registry entry that would pass
// ensureWorktree's own worktree-shape check — a .git gitfile whose target
// textually resolves under the base's own admin directory — but whose
// physical location is still outside the base's "worktrees" directory once
// symlinks are resolved.
func TestTryProvisionWorktree_SharerRegistryFakeGitfileStillRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	outsideDir := t.TempDir()
	sentinelFile := filepath.Join(outsideDir, "sentinel.txt")
	const sentinelContent = "must not be mounted"
	if err := os.WriteFile(sentinelFile, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}
	adminEntry := filepath.Join(base, ".git", "worktrees", "fake-entry")
	if err := os.WriteFile(filepath.Join(outsideDir, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := provision.RegisterSharer(base, "agent-3", outsideDir, "some-other-agent"); err != nil {
		t.Fatalf("plant sharer marker: %v", err)
	}

	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-3", AgentID: "agent-3",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false when the sharer registry names a path outside the base, even with a matching gitfile")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the sharer-registry entry")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
	if got, readErr := os.ReadFile(sentinelFile); readErr != nil || string(got) != sentinelContent {
		t.Errorf("expected the outside file to survive untouched, got %q err=%v", got, readErr)
	}
}

// TestValidateMountedWorktree is a direct, function-level unit test for the
// final mount-time gate. It calls validateMountedWorktree directly against a
// real shared base, independent of ensureWorktree's own equivalent checks —
// which, in every scenario reachable through tryProvisionWorktree, already
// validate a registry or git-list join target before validateMountedWorktree
// ever sees it. Calling the function directly here pins its own behavior
// even when those earlier checks would otherwise already have refused the
// same input.
func TestValidateMountedWorktree(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)
	base := filepath.Join(projectPath, "workspace")
	agent1Worktree := filepath.Join(base, "worktrees", "agent-1")

	t.Run("a direct child real worktree is accepted", func(t *testing.T) {
		if err := validateMountedWorktree(agent1Worktree, base); err != nil {
			t.Errorf("expected the real direct-child worktree to be accepted, got: %v", err)
		}
	})

	t.Run("a nested subdirectory of a real worktree is rejected", func(t *testing.T) {
		nested := filepath.Join(agent1Worktree, "sub")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		adminEntry := filepath.Join(base, ".git", "worktrees", "fake-entry")
		if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := validateMountedWorktree(nested, base); err == nil {
			t.Error("expected a nested subdirectory of a real worktree to be rejected")
		}
	})

	t.Run("an intermediate symlink component is rejected", func(t *testing.T) {
		outsideDir := t.TempDir()
		sub := filepath.Join(outsideDir, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		adminEntry := filepath.Join(base, ".git", "worktrees", "fake-entry-2")
		if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		lnk := filepath.Join(agent1Worktree, "lnk2")
		if err := os.Symlink(outsideDir, lnk); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(lnk, "sub")
		if err := validateMountedWorktree(marker, base); err == nil {
			t.Error("expected a path reached through an intermediate symlink to be rejected")
		}
	})

	t.Run("a path with a symlink-and-dot-dot component is rejected", func(t *testing.T) {
		adminEntry := filepath.Join(base, ".git", "worktrees", "agent-1")
		inner := filepath.Join(agent1Worktree, "agent-1")
		if err := os.MkdirAll(inner, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inner, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(agent1Worktree, filepath.Join(agent1Worktree, "up")); err != nil {
			t.Fatal(err)
		}
		sep := string(filepath.Separator)
		p := filepath.Join(agent1Worktree, "up") + sep + ".." + sep + "agent-1"
		if err := validateMountedWorktree(p, base); err == nil {
			t.Errorf("expected %s to be rejected", p)
		}
	})

	t.Run("a path through a symlink back to the worktrees directory is rejected", func(t *testing.T) {
		adminEntry := filepath.Join(base, ".git", "worktrees", "agent-1")
		// the same gitdir in absolute form, so the shape check alone accepts the path below
		if err := os.WriteFile(filepath.Join(agent1Worktree, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, "worktrees"), filepath.Join(agent1Worktree, "wt")); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(agent1Worktree, "wt", "agent-1")
		if err := validateMountedWorktree(p, base); err == nil {
			t.Errorf("expected %s to be rejected", p)
		}
	})
}

// TestTryProvisionWorktree_SharerRegistryNestedMarkerRejected proves that a
// sharer-registry marker naming a subdirectory of an existing real worktree —
// not a direct child of the shared base's own "worktrees" directory — is
// refused, even with a gitfile that resolves into the base's own admin
// directory. The resolved workspace must be a direct child of the base's own
// "worktrees" directory; entries directly inside "worktrees" are created
// only by the broker.
func TestTryProvisionWorktree_SharerRegistryNestedMarkerRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	agent1Worktree := filepath.Join(base, "worktrees", "agent-1")
	nested := filepath.Join(agent1Worktree, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	adminEntry := filepath.Join(base, ".git", "worktrees", "fake-entry")
	if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := provision.RegisterSharer(base, "agent-4", nested, "agent-1"); err != nil {
		t.Fatalf("plant sharer marker: %v", err)
	}

	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-4", AgentID: "agent-4",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false when the sharer registry names a nested subdirectory of another worktree")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the nested sharer-registry entry")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
	if _, statErr := os.Stat(nested); statErr != nil {
		t.Errorf("expected the nested directory to survive untouched, stat error: %v", statErr)
	}
}

// TestTryProvisionWorktree_SharerRegistryIntermediateSymlinkRejected proves
// that a sharer-registry marker reached through an intermediate symlink
// component is refused once its physical location is resolved, even though
// the final path component itself is a real, non-symlink directory with a
// gitfile that looks valid on its own.
func TestTryProvisionWorktree_SharerRegistryIntermediateSymlinkRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	agent1Worktree := filepath.Join(base, "worktrees", "agent-1")

	outsideDir := t.TempDir()
	sub := filepath.Join(outsideDir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	adminEntry := filepath.Join(base, ".git", "worktrees", "fake-entry")
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lnk := filepath.Join(agent1Worktree, "lnk")
	if err := os.Symlink(outsideDir, lnk); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(lnk, "sub")

	if err := provision.RegisterSharer(base, "agent-5", marker, "agent-1"); err != nil {
		t.Fatalf("plant sharer marker: %v", err)
	}

	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-5", AgentID: "agent-5",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false when the sharer registry names a path reached through an intermediate symlink")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the marker reached through the symlink")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
	if got, readErr := os.Readlink(lnk); readErr != nil || got != outsideDir {
		t.Errorf("expected the symlink to survive unchanged, got target=%q err=%v", got, readErr)
	}
}

// TestTryProvisionWorktree_SharerRegistryNonCanonicalPathRejected proves a
// sharer-registry marker is refused when its own text is not already the
// literal, canonical "worktrees/<name>" form, even when every resolved-path
// check on it would otherwise pass. A marker built as
// "worktrees/agent-1/lnk/../agent-1", where lnk is a symlink back to
// worktrees/agent-1, resolves physically (following the real filesystem,
// symlinks included) to the legitimate worktrees/agent-1 — so both
// EvalSymlinks-based checks accept it. But this exact string is what is
// stored and later consumed downstream by a purely lexical Clean (no
// symlink resolution), which resolves the same text to a different
// location, worktrees/agent-1/agent-1.
func TestTryProvisionWorktree_SharerRegistryNonCanonicalPathRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	agent1Worktree := filepath.Join(base, "worktrees", "agent-1")

	// A second worktree-shaped directory nested inside agent-1's own
	// worktree, reachable only through the lexical (not physical) reading
	// of the marker below.
	inner := filepath.Join(agent1Worktree, "agent-1")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	adminEntry := filepath.Join(base, ".git", "worktrees", "agent-1")
	if err := os.WriteFile(filepath.Join(inner, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// lnk resolves back to agent-1's own real worktree, so the marker below
	// physically resolves to a direct child (worktrees/agent-1) even though
	// its own text is not that direct-child form.
	lnk := filepath.Join(agent1Worktree, "lnk")
	if err := os.Symlink(agent1Worktree, lnk); err != nil {
		t.Fatal(err)
	}
	// filepath.Join would lexically clean "lnk/../agent-1" away before this
	// even reaches RegisterSharer, defeating the point of this test — the
	// exact literal text with "lnk/.." still in it is what gets stored, and
	// it is not resolved by anything until it is used downstream.
	marker := lnk + string(filepath.Separator) + ".." + string(filepath.Separator) + "agent-1"

	if err := provision.RegisterSharer(base, "agent-9", marker, "agent-1"); err != nil {
		t.Fatalf("plant sharer marker: %v", err)
	}

	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-9", AgentID: "agent-9",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false for a marker that is not already in canonical direct-child form")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the non-canonical marker")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
}

// TestTryProvisionWorktree_SymlinkedProjectParentAccepted proves a project
// path reached through a symlinked ancestor directory — a supported,
// legitimate configuration — is still accepted and mounted: the final
// containment check resolves both the "worktrees" directory and the
// worktree path through that same ancestor symlink before comparing them,
// rather than comparing their un-resolved text.
func TestTryProvisionWorktree_SymlinkedProjectParentAccepted(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	realProjectDir := filepath.Join(t.TempDir(), "real-proj")
	if err := os.MkdirAll(realProjectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(t.TempDir(), "proj-link")
	if err := os.Symlink(realProjectDir, projectPath); err != nil {
		t.Fatal(err)
	}

	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-1", AgentID: "agent-1",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if err != nil {
		t.Fatalf("expected provisioning through a symlinked project parent to succeed, got err=%v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for a symlinked project parent")
	}
	if opts.Workspace == "" {
		t.Fatal("expected a workspace to be mounted")
	}
}

// TestBuildStartContext_GitCloneDebugLogRedactsCredentials proves the
// git-clone-mode debug log never contains a clone URL's embedded
// credentials, whether they are a user:pass pair or a username-only token.
func TestBuildStartContext_GitCloneDebugLogRedactsCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{name: "user and password", url: "https://user:supersecret@github.com/org/repo.git"},
		{name: "username-only token", url: "https://supersecret@github.com/org/repo.git"},
		{name: "token in query string", url: "https://github.com/org/repo.git?access_token=supersecret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			cfg.Debug = true
			srv := newTestServerForStartContext(t, cfg)

			var buf bytes.Buffer
			srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)
			_, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-1",
				ProjectPath: "/some/path",
				Config: &CreateAgentConfig{
					GitClone: &api.GitCloneConfig{
						URL:    tc.url,
						Branch: "main",
					},
				},
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if err != nil {
				t.Fatal(err)
			}

			logged := buf.String()
			if strings.Contains(logged, "supersecret") {
				t.Errorf("debug log must not contain the clone URL's credentials, got: %s", logged)
			}
			if !strings.Contains(logged, "github.com") {
				t.Errorf("expected the redacted cloneURL to still be logged, got: %s", logged)
			}
		})
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

func TestBuildStartContext_HubManagedProjectWritesMarker(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Simulate a hub-managed project: ProjectSlug set, ProjectPath pre-resolved
	// (as the createAgent handler does for env-gather), and ProjectID from hub.
	projectsDir := t.TempDir()
	projectPath := filepath.Join(projectsDir, "web-demo")

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: "web-demo",
		ProjectPath: projectPath,
		ProjectID:   "6d868c0f-b862-49e0-a44b-3555a3887ee3",
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify .scion marker file was created (not a directory)
	scionPath := filepath.Join(projectPath, ".scion")
	if !config.IsProjectMarkerFile(scionPath) {
		t.Fatal(".scion marker file was not created")
	}

	// Verify project-id was written via marker
	marker, err := config.ReadProjectMarker(scionPath)
	if err != nil {
		t.Fatalf("failed to read .scion marker: %v", err)
	}
	if marker.ProjectID != "6d868c0f-b862-49e0-a44b-3555a3887ee3" {
		t.Errorf("expected project-id '6d868c0f-b862-49e0-a44b-3555a3887ee3', got %q", marker.ProjectID)
	}
	if marker.ProjectSlug != "web-demo" {
		t.Errorf("expected project-slug 'web-demo', got %q", marker.ProjectSlug)
	}

	// Verify external project-configs directories were created
	extPath, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatalf("failed to get external project path: %v", err)
	}
	if extPath == "" {
		t.Fatal("expected non-empty external project path")
	}
	extAgents := filepath.Join(extPath, "agents")
	if _, err := os.Stat(extAgents); os.IsNotExist(err) {
		t.Fatalf("external agents dir was not created: %s", extAgents)
	}

	// ProjectPath should be passed through to opts
	if sc.Opts.ProjectPath != projectPath {
		t.Errorf("expected ProjectPath %q, got %q", projectPath, sc.Opts.ProjectPath)
	}
}

func TestBuildStartContext_HubManagedProjectSlugResolution(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Simulate: ProjectSlug set, ProjectPath empty (buildStartContext resolves it),
	// ProjectID from hub. This is the path when the handler doesn't pre-resolve.
	home := t.TempDir()
	t.Setenv("HOME", home)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: "my-project",
		ProjectID:   "aabbccdd-1234-5678-9012-abcdef123456",
		Operation:   opCreate,
		// A current hub sends the hub-managed project path as the
		// workspace for a shared non-git project (see
		// ambiguousNonGitWorkspace).
		Config: &CreateAgentConfig{Workspace: filepath.Join(home, ".scion", "projects", "my-project")},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify project-id was written via marker file
	scionPath := filepath.Join(sc.Opts.ProjectPath, ".scion")
	marker, err := config.ReadProjectMarker(scionPath)
	if err != nil {
		t.Fatalf("failed to read .scion marker: %v", err)
	}
	if marker.ProjectID != "aabbccdd-1234-5678-9012-abcdef123456" {
		t.Errorf("expected project-id from hub, got %q", marker.ProjectID)
	}
}

// TestBuildStartContext_NeverFallsBackToLegacyGrovesDir is the negative test
// for the deleted groves/ fallback: even when a legacy
// ~/.scion/groves/<slug> directory holds real content (a migrator-conflict
// leftover, or a project that predates the migrator ever running) and
// ~/.scion/projects/<slug> holds only infrastructure, buildStartContext must
// always resolve to the canonical projects/ path.
func TestBuildStartContext_NeverFallsBackToLegacyGrovesDir(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	home := t.TempDir()
	t.Setenv("HOME", home)

	slug := "no-legacy-fallback"
	projectsDir := filepath.Join(home, ".scion", "projects", slug)
	if err := os.MkdirAll(filepath.Join(projectsDir, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	grovesDir := filepath.Join(home, ".scion", "groves", slug)
	if err := os.MkdirAll(grovesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grovesDir, "README.md"), []byte("# workspace"), 0644); err != nil {
		t.Fatal(err)
	}

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: slug,
		ProjectID:   "aabbccdd-1234-5678-9012-abcdef123456",
		Operation:   opCreate,
		Config:      &CreateAgentConfig{Workspace: projectsDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Opts.ProjectPath != projectsDir {
		t.Errorf("ProjectPath = %q, want %q (must never fall back to the legacy groves dir)", sc.Opts.ProjectPath, projectsDir)
	}
}

// TestBuildStartContext_GlobalDirFailureMessageOmitsRawError proves that a
// config.GetGlobalDir failure (ProjectSlug set, HOME unresolvable) returns a
// startContextError whose Message is a fixed string, not "...: " + the raw
// os.UserHomeDir error text — a 4xx Status writes Message verbatim to the
// HTTP response body (writeStartContextError), so it must never be built by
// string-concatenating a wrapped error even when this particular failure
// isn't itself a 4xx.
func TestBuildStartContext_GlobalDirFailureMessageOmitsRawError(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectSlug: "my-project",
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatal("buildStartContext() with unresolvable HOME: expected an error, got nil")
	}
	sce, ok := err.(*startContextError)
	if !ok {
		t.Fatalf("err = %T, want *startContextError", err)
	}
	if strings.Contains(sce.Message, "$HOME") || strings.Contains(sce.Message, "defined") {
		t.Errorf("Message = %q, embeds the raw os.UserHomeDir error text", sce.Message)
	}
	if sce.Message != "Failed to resolve the global config directory" {
		t.Errorf("Message = %q, want the fixed string", sce.Message)
	}
	if sce.OriginalErr == nil {
		t.Error("OriginalErr = nil, want the underlying error preserved for logging/span")
	}
}

// TestBuildStartContext_HubEndpointResolutionFailureMessageOmitsRawError
// proves the same for resolveEffectiveHubEndpoint's failure path (triggered
// here via cloudrun-sandbox with no configured hub listen port): Message
// must be a fixed string, not the wrapped error's own text.
func TestBuildStartContext_HubEndpointResolutionFailureMessageOmitsRawError(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.HubListenPort = 0
	srv := newTestServerForStartContextRuntime(t, cfg, "cloudrun-sandbox")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-sandbox-no-port",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatal("buildStartContext() with HubListenPort=0 on cloudrun-sandbox: expected an error, got nil")
	}
	sce, ok := err.(*startContextError)
	if !ok {
		t.Fatalf("err = %T, want *startContextError", err)
	}
	if strings.Contains(sce.Message, "listen port") {
		t.Errorf("Message = %q, embeds the raw hub-endpoint-resolution error text", sce.Message)
	}
	if sce.Message != "Failed to resolve the hub endpoint" {
		t.Errorf("Message = %q, want the fixed string", sce.Message)
	}
	if sce.OriginalErr == nil {
		t.Error("OriginalErr = nil, want the underlying error preserved for logging/span")
	}
}

func TestBuildStartContext_HubManagedProjectPreservesExistingProjectID(t *testing.T) {
	t.Run("preserves when external config dir exists", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		// Pre-create .scion as a directory with an existing project-id (git project)
		projectPath := filepath.Join(t.TempDir(), "existing-project")
		scionDir := filepath.Join(projectPath, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatal(err)
		}
		existingID := "existing-id-1234-5678"
		if err := config.WriteProjectID(scionDir, existingID); err != nil {
			t.Fatal(err)
		}

		// Create the external config dir so it looks like a live project (not stale).
		extDir, err := config.GetGitProjectExternalConfigDir(scionDir)
		if err != nil {
			t.Fatalf("failed to get external config dir: %v", err)
		}
		if err := os.MkdirAll(extDir, 0755); err != nil {
			t.Fatalf("failed to create external config dir: %v", err)
		}

		_, err = srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			ProjectSlug: "existing-project",
			ProjectPath: projectPath,
			ProjectID:   "new-id-from-hub",
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Verify existing project-id was NOT overwritten (external dir exists → not stale)
		projectID, err := config.ReadProjectID(scionDir)
		if err != nil {
			t.Fatalf("failed to read project-id: %v", err)
		}
		if projectID != existingID {
			t.Errorf("expected existing project-id %q to be preserved, got %q", existingID, projectID)
		}
	})

	t.Run("overwrites when external config dir missing (stale)", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		// Pre-create .scion as a directory with an existing project-id (git project)
		projectPath := filepath.Join(t.TempDir(), "existing-project")
		scionDir := filepath.Join(projectPath, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatal(err)
		}
		existingID := "existing-id-1234-5678"
		if err := config.WriteProjectID(scionDir, existingID); err != nil {
			t.Fatal(err)
		}
		// Do NOT create the external config dir → simulates project deletion.

		newID := "new-id-from-hub"
		_, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			ProjectSlug: "existing-project",
			ProjectPath: projectPath,
			ProjectID:   newID,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Verify stale project-id was overwritten with the hub's new ID.
		projectID, err := config.ReadProjectID(scionDir)
		if err != nil {
			t.Fatalf("failed to read project-id: %v", err)
		}
		if projectID != newID {
			t.Errorf("expected stale project-id to be overwritten with %q, got %q", newID, projectID)
		}
	})
}

func TestBuildStartContext_HubManagedProjectPreservesExistingMarker(t *testing.T) {
	t.Run("preserves when external config dir exists", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		// Pre-create .scion as a marker file (hub-managed project)
		projectPath := filepath.Join(t.TempDir(), "existing-project")
		if err := os.MkdirAll(projectPath, 0755); err != nil {
			t.Fatal(err)
		}
		existingID := "existing-id-1234-5678"
		scionPath := filepath.Join(projectPath, ".scion")
		existingMarker := &config.ProjectMarker{
			ProjectID:   existingID,
			ProjectName: "existing-project",
			ProjectSlug: "existing-project",
		}
		if err := config.WriteProjectMarker(scionPath, existingMarker); err != nil {
			t.Fatal(err)
		}

		// Create the external config dir so it looks like a live project (not stale).
		extPath, err := existingMarker.ExternalProjectPath()
		if err != nil {
			t.Fatalf("failed to get external project path: %v", err)
		}
		if err := os.MkdirAll(extPath, 0755); err != nil {
			t.Fatalf("failed to create external config dir: %v", err)
		}

		_, err = srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			ProjectSlug: "existing-project",
			ProjectPath: projectPath,
			ProjectID:   "new-id-from-hub",
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Verify existing marker was NOT overwritten (external dir exists → not stale)
		marker, err := config.ReadProjectMarker(scionPath)
		if err != nil {
			t.Fatalf("failed to read marker: %v", err)
		}
		if marker.ProjectID != existingID {
			t.Errorf("expected existing project-id %q to be preserved, got %q", existingID, marker.ProjectID)
		}
	})

	t.Run("overwrites when external config dir missing (stale)", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		// Pre-create .scion as a marker file (hub-managed project)
		projectPath := filepath.Join(t.TempDir(), "existing-project")
		if err := os.MkdirAll(projectPath, 0755); err != nil {
			t.Fatal(err)
		}
		existingID := "existing-id-1234-5678"
		scionPath := filepath.Join(projectPath, ".scion")
		if err := config.WriteProjectMarker(scionPath, &config.ProjectMarker{
			ProjectID:   existingID,
			ProjectName: "existing-project",
			ProjectSlug: "existing-project",
		}); err != nil {
			t.Fatal(err)
		}
		// Do NOT create the external config dir → simulates project deletion.

		newID := "new-id-from-hub"
		_, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			ProjectSlug: "existing-project",
			ProjectPath: projectPath,
			ProjectID:   newID,
			Operation:   opCreate,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Verify stale marker was overwritten with the hub's new ID.
		marker, err := config.ReadProjectMarker(scionPath)
		if err != nil {
			t.Fatalf("failed to read marker: %v", err)
		}
		if marker.ProjectID != newID {
			t.Errorf("expected stale marker project-id to be overwritten with %q, got %q", newID, marker.ProjectID)
		}
	})
}

// TestBuildStartContext_LinkedGitProjectUpdatesStaleProjectID verifies that for
// linked git projects (where ProjectSlug is empty but ProjectID and ProjectPath
// are set), a stale on-disk project-id is overwritten when the external config
// dir was cleaned up. This is the primary regression test for miller79/scion#28.
func TestBuildStartContext_LinkedGitProjectUpdatesStaleProjectID(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Simulate a linked git project workspace with a stale .scion/project-id.
	projectPath := filepath.Join(t.TempDir(), "my-repo")
	scionDir := filepath.Join(projectPath, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	staleID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := config.WriteProjectID(scionDir, staleID); err != nil {
		t.Fatal(err)
	}
	// No external config dir created → the old project was deleted.

	newID := "11111111-2222-3333-4444-555555555555"
	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectPath: projectPath,
		ProjectID:   newID,
		// ProjectSlug intentionally empty — linked git project path
		// (hub dispatcher omits slug when provider has LocalPath).
		Operation: opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify the stale project-id was overwritten with the hub's new ID.
	projectID, err := config.ReadProjectID(scionDir)
	if err != nil {
		t.Fatalf("failed to read project-id: %v", err)
	}
	if projectID != newID {
		t.Errorf("expected stale project-id to be overwritten with %q, got %q", newID, projectID)
	}
}

func TestBuildStartContext_HubEndpoint(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.HubEndpoint = "https://hub.example.com"
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Without HTTPRequest, the connection endpoint is empty, so the broker's
	// own HubEndpoint is used.
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:      "agent-1",
		Operation: opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Opts.Env["SCION_HUB_ENDPOINT"] != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com', got %q", sc.Opts.Env["SCION_HUB_ENDPOINT"])
	}
	if sc.Opts.Env["SCION_HUB_URL"] != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_URL='https://hub.example.com', got %q", sc.Opts.Env["SCION_HUB_URL"])
	}
}

// TestBuildStartContext_TrustedHubEndpointOperatorTiersOnly proves
// StartOptions.TrustedHubEndpoint is populated only from an
// operator-derived tier (the broker's own configured HubEndpoint here; the
// request HubEndpoint and the hub connection endpoint rank above it but are
// exercised at the hubenv_test.go unit level), while the DELIVERED
// SCION_HUB_ENDPOINT env value still follows the full resolution chain
// regardless of which tier supplied it. A tenant-controllable resolved-env
// value and a project-settings-only value both reach the agent's own env,
// but neither ever reaches TrustedHubEndpoint.
func TestBuildStartContext_TrustedHubEndpointOperatorTiersOnly(t *testing.T) {
	const tenantEndpoint = "http://169.254.169.254"

	t.Run("resolved-env-only value is delivered but not trusted", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			Operation:   opCreate,
			ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": tenantEndpoint},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Existence control: the value really does reach the agent's own
		// env, so the TrustedHubEndpoint assertion below is not vacuously
		// passing because nothing was resolved at all.
		if sc.Opts.Env["SCION_HUB_ENDPOINT"] != tenantEndpoint {
			t.Fatalf("existence control failed: SCION_HUB_ENDPOINT = %q, want %q", sc.Opts.Env["SCION_HUB_ENDPOINT"], tenantEndpoint)
		}
		if sc.Opts.TrustedHubEndpoint != "" {
			t.Errorf("TrustedHubEndpoint = %q, want \"\" (a resolved-env-only value must never be trusted for egress)", sc.Opts.TrustedHubEndpoint)
		}
	})

	t.Run("broker config endpoint is trusted even with a resolved-env value present", func(t *testing.T) {
		const operatorEndpoint = "https://hub.operator.example"
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		cfg.HubEndpoint = operatorEndpoint
		srv := newTestServerForStartContext(t, cfg)

		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-2",
			Operation:   opCreate,
			ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": tenantEndpoint},
		})
		if err != nil {
			t.Fatal(err)
		}
		if sc.Opts.TrustedHubEndpoint != operatorEndpoint {
			t.Errorf("TrustedHubEndpoint = %q, want %q (the operator-configured broker endpoint)", sc.Opts.TrustedHubEndpoint, operatorEndpoint)
		}
	})

	// Project settings is itself a hub-resolved file for a hub-managed
	// project — the same tenant-reachable precondition the resolved-env
	// tier is excluded for.
	t.Run("project-settings-only value is delivered but not trusted", func(t *testing.T) {
		const settingsEndpoint = "https://settings.example.com"
		projectDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte("hub:\n  endpoint: "+settingsEndpoint+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-3",
			Operation:   opCreate,
			ProjectPath: projectDir,
		})
		if err != nil {
			t.Fatal(err)
		}
		if sc.Opts.Env["SCION_HUB_ENDPOINT"] != settingsEndpoint {
			t.Fatalf("existence control failed: SCION_HUB_ENDPOINT = %q, want %q", sc.Opts.Env["SCION_HUB_ENDPOINT"], settingsEndpoint)
		}
		if sc.Opts.TrustedHubEndpoint != "" {
			t.Errorf("TrustedHubEndpoint = %q, want \"\" (a project-settings-only value must never be trusted for egress)", sc.Opts.TrustedHubEndpoint)
		}
	})
}

func TestBuildStartContext_GCPMetadataDefaultBlock(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// No GCPIdentity config — should default to block mode
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-no-gcp",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block' by default, got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["SCION_METADATA_PORT"] != "18380" {
		t.Errorf("expected SCION_METADATA_PORT='18380', got %q", sc.Opts.Env["SCION_METADATA_PORT"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
	// No SA env vars should be set in block mode
	if sc.Opts.Env["SCION_METADATA_SA_EMAIL"] != "" {
		t.Errorf("expected empty SCION_METADATA_SA_EMAIL in block mode, got %q", sc.Opts.Env["SCION_METADATA_SA_EMAIL"])
	}
}

func TestBuildStartContext_GCPMetadataPassthrough(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Explicit passthrough — should NOT set metadata env vars
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-passthrough",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "passthrough",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	// SCION_METADATA_MODE is still recorded for passthrough (unlike the
	// redirect vars below) — it is the only channel through which
	// downstream auth-type auto-detection (e.g. antigravity's vertex-ai
	// selection, ptone/scion#1873) can tell a GCP SA is reachable via
	// passthrough on a freshly created agent.
	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected SCION_METADATA_MODE='passthrough', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST for passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "" {
		t.Errorf("expected no GCE_METADATA_ROOT for passthrough, got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
}

func TestBuildStartContext_GCPMetadataExplicitBlock(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-block",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "block",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
}

// TestBuildStartContext_GCPMetadataBlockRejectedOnKubernetes covers the phase
// 1 rule from ptone/scion#2328: "block" is not offered on the Kubernetes
// runtime. The broker is the enforcement point here because it is the one
// place that knows the concrete runtime with certainty at dispatch time. Both
// spellings the codebase accepts for the Kubernetes runtime name ("kubernetes"
// and "k8s") must be covered.
func TestBuildStartContext_GCPMetadataBlockRejectedOnKubernetes(t *testing.T) {
	for _, runtimeName := range []string{"kubernetes", "k8s"} {
		t.Run(runtimeName, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContextRuntime(t, cfg, runtimeName)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-k8s-block",
				Config: &CreateAgentConfig{
					GCPIdentity: &GCPIdentityConfig{
						MetadataMode: "block",
					},
				},
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if err == nil {
				t.Fatalf("expected an error for block mode on runtime %q, got nil (env: %v)", runtimeName, sc.Opts.Env)
			}
			// Nothing must proceed: no partial pod/env context is returned
			// alongside the rejection.
			if sc != nil {
				t.Errorf("expected nil startContext alongside the error, got %+v", sc)
			}
			if !strings.Contains(err.Error(), "Kubernetes") {
				t.Errorf("expected the error to name the Kubernetes runtime, got %q", err.Error())
			}
			if !strings.Contains(err.Error(), "assign") || !strings.Contains(err.Error(), "passthrough") {
				t.Errorf("expected the error to name assign/passthrough as alternatives, got %q", err.Error())
			}
			if !strings.Contains(err.Error(), "project or hub default") {
				t.Errorf("expected the error to mention changing the project or hub default, got %q", err.Error())
			}
		})
	}
}

// TestBuildStartContext_GCPMetadataNoIdentityInputOnKubernetesDefaultsToPassthrough
// covers the no-input fallback case: when the caller supplies no GCP identity
// information at all (no Config.GCPIdentity, no resolvedEnv
// SCION_METADATA_MODE), buildStartContext's own secure default is
// runtime-aware. On Kubernetes it resolves to "passthrough", not "block" —
// Kubernetes does not support "block" (ptone/scion#2328 phase 1), and the
// Hub's own resolution ladder deliberately leaves the mode unset for exactly
// this "nothing configured" case (as opposed to an explicit project or hub
// default of "block", which the Hub still threads through explicitly and
// which is rejected — see
// TestBuildStartContext_GCPMetadataBlockRejectedOnKubernetesFromResolvedEnv)
// so this runtime-appropriate default can apply.
func TestBuildStartContext_GCPMetadataNoIdentityInputOnKubernetesDefaultsToPassthrough(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-no-identity-input",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected no GCP identity input on Kubernetes to be accepted, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected the Kubernetes-specific default 'passthrough', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST for the passthrough default, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_GCPMetadataNoIdentityInputOnDockerDefaultsToBlock is
// the docker-side half of the same guard: the runtime-aware default must not
// change anything for every runtime except Kubernetes. Docker keeps exactly
// its pre-existing "block" default when the caller supplies no GCP identity
// information at all.
func TestBuildStartContext_GCPMetadataNoIdentityInputOnDockerDefaultsToBlock(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "docker")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-docker-no-identity-input",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected no GCP identity input on docker to be accepted, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected the unchanged default 'block', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380' for the block default, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_GCPMetadataBlockRejectedOnKubernetesFromResolvedEnv
// covers block arriving via a project or hub default, which reaches the
// broker as hub-supplied resolvedEnv (the start path) rather than an explicit
// Config.GCPIdentity. Stored block defaults are not migrated; they simply
// fail a Kubernetes dispatch with the same actionable error.
func TestBuildStartContext_GCPMetadataBlockRejectedOnKubernetesFromResolvedEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-default-block",
		ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block"},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for a resolved-env block default on Kubernetes, got nil (env: %v)", sc.Opts.Env)
	}
	if !strings.Contains(err.Error(), "Kubernetes") {
		t.Errorf("expected the error to name the Kubernetes runtime, got %q", err.Error())
	}
}

// TestBuildStartContext_GCPMetadataBlockRejectedAfterProjectDirResolution
// pins the ordering of the Kubernetes/"block" rejection relative to
// buildStartContext's own project-directory resolution (WriteProjectMarker,
// MkdirAll): the rejection runs after that resolution, not before it.
// Settings and the saved profile the rejection's runtime resolution depends
// on must be read from the final, post-update location; a fresh hub-managed
// project (the
// ProjectSlug+ProjectID-with-no-existing-ProjectPath shape used below) or a
// stale-marker rewrite would otherwise resolve against the pre-update
// location. The rejection still runs before any pod or env is built, which
// is covered by every other GCPMetadataBlockRejectedOnKubernetes* test
// rejecting before Opts is ever populated; this test instead pins that the
// project directory and marker now do exist by the time the rejection
// happens.
func TestBuildStartContext_GCPMetadataBlockRejectedAfterProjectDirResolution(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("GetGlobalDir: %v", err)
	}
	const slug = "hub-managed-project-block-test"
	projectDir := filepath.Join(globalDir, "projects", slug)
	t.Cleanup(func() { _ = os.RemoveAll(projectDir) })
	if _, statErr := os.Stat(projectDir); !os.IsNotExist(statErr) {
		t.Fatalf("precondition failed: %s already exists", projectDir)
	}

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-block-no-side-effects",
		ProjectSlug: slug,
		ProjectID:   "project-id-for-side-effect-check",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected the dispatch to be rejected, got nil (env: %v)", sc.Opts.Env)
	}
	if _, statErr := os.Stat(projectDir); os.IsNotExist(statErr) {
		t.Errorf("expected the project directory/marker to already exist by the time the rejection runs, but %s does not exist", projectDir)
	}
}

// TestBuildStartContext_GCPMetadataPassthroughUnchangedOnKubernetes guards the
// inversion against over-reach: only "block" is rejected on Kubernetes;
// "passthrough" must behave exactly as on any other runtime.
//
// This test used to also cover "assign" (as
// TestBuildStartContext_GCPMetadataAssignAndPassthroughUnchangedOnKubernetes),
// asserting the placeholder behavior of emulator env unchanged on Kubernetes
// before this change. ptone/scion#2328 gives Kubernetes "assign" its own,
// different behavior (Workload Identity instead of the emulator), so that
// case moved to TestBuildStartContext_KubernetesAssign* below.
func TestBuildStartContext_GCPMetadataPassthroughUnchangedOnKubernetes(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-k8s-passthrough",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "passthrough"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected passthrough to be accepted on Kubernetes, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected SCION_METADATA_MODE='passthrough', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST for passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_GCPMetadataAssignUnchangedOnDocker covers the other
// required-unchanged case: "assign" on any non-Kubernetes runtime (Docker
// and, by extension, Apple/Podman) still uses the sciontool metadata emulator
// exactly as before — only the Kubernetes runtime's "assign" behavior changes
// here.
func TestBuildStartContext_GCPMetadataAssignUnchangedOnDocker(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "docker")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-docker-assign",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "sa@proj.iam.gserviceaccount.com",
				ProjectID:    "proj",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected assign to be accepted on docker, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "assign" {
		t.Errorf("expected SCION_METADATA_MODE='assign', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
	if sc.Opts.Env["SCION_METADATA_SA_EMAIL"] != "sa@proj.iam.gserviceaccount.com" {
		t.Errorf("expected SCION_METADATA_SA_EMAIL to be set, got %q", sc.Opts.Env["SCION_METADATA_SA_EMAIL"])
	}
	if sc.Opts.Env["SCION_METADATA_PROJECT_ID"] != "proj" {
		t.Errorf("expected SCION_METADATA_PROJECT_ID to be set, got %q", sc.Opts.Env["SCION_METADATA_PROJECT_ID"])
	}
}

// newTestProjectSettings writes a project directory whose .scion/settings.yaml
// is exactly settingsYAML, for tests that need the runtime/profile *type*
// question (e.g. "kubernetes") resolved via
// config.LoadEffectiveSettings(in.ProjectPath) — project settings are
// allowed to answer that question. The Kubernetes ServiceAccount mapping
// itself is a separate, operator-only question — see newTestGlobalSettings.
func newTestProjectSettings(t *testing.T, settingsYAML string) string {
	t.Helper()
	projectDir := t.TempDir()
	dotScion := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	return projectDir
}

// newTestGlobalSettings writes settingsYAML to the broker's own global
// settings file. newTestServerForStartContextRuntime sets HOME to a fresh
// temp dir per test via t.Setenv, so config.GetGlobalDir() (HOME/.scion)
// resolves here — the Kubernetes ServiceAccount mapping is read only from
// this location, never from a project's own settings.yaml.
func newTestGlobalSettings(t *testing.T, settingsYAML string) {
	t.Helper()
	globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
}

// testKubernetesProjectSettingsYAML declares only the runtime *type*
// (kubernetes) for the "local" profile — no mapping. Used as the project
// settings in every test below. The runtime entry's own name must be
// "kubernetes" (matching the runtimeName most tests below pass to
// newTestServerForStartContextRuntime, which sets cfg.ForceRuntime to that
// same value): ForceRuntime short-circuits resolveDispatchProfileSelection
// before profile/project settings are consulted at all, and its
// RuntimeEntryName result is the ForceRuntime value itself, which the
// Kubernetes ServiceAccount mapping lookup below then uses as the global
// settings runtime-entry key.
const testKubernetesProjectSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
`

// testKubernetesMappingGlobalSettingsYAML is the operator-side global
// settings.yaml: same profile/runtime shape as
// testKubernetesProjectSettingsYAML, plus the GSA-to-KSA mapping, keyed
// under the "kubernetes" runtime entry to match the ForceRuntime-resolved
// RuntimeEntryName (see testKubernetesProjectSettingsYAML's comment).
const testKubernetesMappingGlobalSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: agent-worker-ksa
`

// TestBuildStartContext_KubernetesAssignWithMapping covers the happy path: a
// Kubernetes "assign" dispatch whose GSA has a mapped KSA gets the KSA
// resolved (for pkg/agent/run.go to apply to the pod's ServiceAccountName),
// keeps the informational SA email/project ID env, and gets no
// emulator-specific env (mode, redirect, or port).
func TestBuildStartContext_KubernetesAssignWithMapping(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-mapped",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected a mapped GSA to be accepted, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected SCION_METADATA_MODE='passthrough' (no emulator), got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["SCION_METADATA_SA_EMAIL"] != "agent-worker@my-project.iam.gserviceaccount.com" {
		t.Errorf("expected SCION_METADATA_SA_EMAIL to be kept as informational env, got %q", sc.Opts.Env["SCION_METADATA_SA_EMAIL"])
	}
	if sc.Opts.Env["SCION_METADATA_PROJECT_ID"] != "my-project" {
		t.Errorf("expected SCION_METADATA_PROJECT_ID to be kept as informational env, got %q", sc.Opts.Env["SCION_METADATA_PROJECT_ID"])
	}
	for _, key := range []string{"GCE_METADATA_HOST", "GCE_METADATA_ROOT", "SCION_METADATA_PORT"} {
		if v, ok := sc.Opts.Env[key]; ok {
			t.Errorf("expected no %s for Kubernetes assign (no emulator), got %q", key, v)
		}
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
	}
	if sc.Opts.InlineConfig != nil {
		t.Errorf("expected InlineConfig to be left nil (the resolved KSA is not carried there), got %+v", sc.Opts.InlineConfig)
	}
}

// testKubernetesMappingWithNamespaceGlobalSettingsYAML extends
// testKubernetesMappingGlobalSettingsYAML with an operator-pinned namespace
// on the "kubernetes" runtime entry, for the namespace-conflict tests below.
const testKubernetesMappingWithNamespaceGlobalSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        namespace: scion-agents
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: agent-worker-ksa
`

// TestBuildStartContext_KubernetesAssignConflictingNamespaceRejected: the
// Workload Identity principal is (namespace, KSA)
// together, so an explicit request-level namespace that differs from the
// operator's pinned namespace must be rejected the same authoritative way a
// differing explicit KSA already is.
func TestBuildStartContext_KubernetesAssignConflictingNamespaceRejected(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingWithNamespaceGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-namespace-conflict",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
			Kubernetes: &api.KubernetesConfig{Namespace: "some-other-namespace"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for a conflicting explicit namespace, got nil (env: %v)", sc.Opts.Env)
	}
	if sc != nil {
		t.Errorf("expected nil startContext alongside the error, got %+v", sc)
	}
	if !strings.Contains(err.Error(), "some-other-namespace") || !strings.Contains(err.Error(), "scion-agents") {
		t.Errorf("expected the error to name both the explicit and operator-configured namespaces, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignMatchingNamespaceAccepted covers the
// non-conflicting half: an explicit namespace that already equals the
// operator's pinned namespace proceeds normally.
func TestBuildStartContext_KubernetesAssignMatchingNamespaceAccepted(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingWithNamespaceGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-namespace-match",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
			Kubernetes: &api.KubernetesConfig{Namespace: "scion-agents"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected a matching explicit namespace to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
	}
}

// TestBuildStartContext_KubernetesAssignNoOperatorNamespace covers a broker
// whose runtime entry sets no namespace (testKubernetesMappingGlobalSettingsYAML,
// unlike the *WithNamespace variant above). The namespace then falls back to
// the Kubernetes runtime's default (SCION_K8S_NAMESPACE here), so an
// explicit request namespace is accepted only when it equals that default.
func TestBuildStartContext_KubernetesAssignNoOperatorNamespace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace string
		wantErr   bool
	}{
		{name: "explicit namespace other than the runtime default refused", namespace: "whatever-namespace", wantErr: true},
		{name: "explicit namespace equal to the runtime default accepted", namespace: "ns-runtime-default", wantErr: false},
		{name: "no explicit namespace accepted", namespace: "", wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_K8S_NAMESPACE", "ns-runtime-default")
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
			projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
			newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

			createCfg := &CreateAgentConfig{
				GCPIdentity: &GCPIdentityConfig{
					MetadataMode: "assign",
					SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				},
			}
			if tc.namespace != "" {
				createCfg.Kubernetes = &api.KubernetesConfig{Namespace: tc.namespace}
			}
			r := httptest.NewRequest("POST", "/api/v1/agents", nil)
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-k8s-assign-no-operator-namespace",
				ProjectPath: projectDir,
				Config:      createCfg,
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for an explicit namespace other than the runtime default, got nil")
				}
				var sce *startContextError
				if !errors.As(err, &sce) || sce.Status != http.StatusBadRequest {
					t.Fatalf("expected a 400 startContextError, got %v", err)
				}
				if !strings.Contains(err.Error(), tc.namespace) || !strings.Contains(err.Error(), "ns-runtime-default") {
					t.Errorf("expected the error to name the explicit and the resolved namespace, got %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("expected acceptance, got %v", err)
			}
			if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
				t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
			}
		})
	}
}

// TestBuildStartContext_KubernetesAssignMixedCaseSAEmailStillResolves proves
// the case-normalization fix directly: a mixed-case GSA email (as a caller
// might send it, even though canonical GCP service account emails are
// lowercase) must still resolve the mapping and must not then fail the
// resolved-entry validation, which requires a lowercase email — the mapping
// lookup and the validation call must agree on the same lower-cased value.
func TestBuildStartContext_KubernetesAssignMixedCaseSAEmailStillResolves(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-mixed-case",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "Agent-Worker@My-Project.IAM.GServiceAccount.com",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected a mixed-case GSA email to still resolve the mapping, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
	}
}

// TestBuildStartContext_KubernetesAssignWithoutMapping covers the required
// rejection: a Kubernetes "assign" dispatch whose GSA has no mapped KSA
// fails before any pod or env is built, with an actionable error naming the
// setting to add. The message must be the specific "no ... mapped" text, not
// merely any error — ValidateKubernetesServiceAccountMappings's own error
// text also happens to mention kubernetes_service_account_mappings and the
// GSA email, so a looser assertion would not catch the reject check itself
// being disabled.
func TestBuildStartContext_KubernetesAssignWithoutMapping(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-unmapped",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "unmapped@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for an unmapped GSA, got nil (env: %v)", sc.Opts.Env)
	}
	if sc != nil {
		t.Errorf("expected nil startContext alongside the error, got %+v", sc)
	}
	const wantSubstr = `has no Kubernetes ServiceAccount mapped for "unmapped@my-project.iam.gserviceaccount.com"`
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("expected the error to contain %q, got %q", wantSubstr, err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignEmptyGSAEmail covers the distinct
// "no GSA at all" case: it must not be reported as "no mapping found", since
// there is nothing to look up and the fix is different (supply a GSA, not
// add a mapping).
func TestBuildStartContext_KubernetesAssignEmptyGSAEmail(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-no-gsa",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for assign with no GSA email, got nil (env: %v)", sc.Opts.Env)
	}
	if strings.Contains(err.Error(), "no Kubernetes ServiceAccount mapped") {
		t.Errorf("expected a distinct 'requires a service account email' error, not the no-mapping error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "requires a service account email") {
		t.Errorf("expected the error to say a service account email is required, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignGlobalSettingsLoadFailure covers the
// other distinct error: the broker's global settings.yaml itself failing to
// load (malformed YAML) must not be reported as "no mapping found" either —
// the fix is different (repair settings.yaml, not add a mapping).
func TestBuildStartContext_KubernetesAssignGlobalSettingsLoadFailure(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, "schema_version: \"1\"\nprofiles: [this is not valid yaml for a map")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-bad-global-settings",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for a malformed global settings file, got nil (env: %v)", sc.Opts.Env)
	}
	if strings.Contains(err.Error(), "no Kubernetes ServiceAccount mapped") {
		t.Errorf("expected a distinct settings-load error, not the no-mapping error, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignConflictingInlineServiceAccountName
// covers the conflict rule via InlineConfig.Kubernetes: the mapping is
// authoritative, so an explicit inline ServiceAccountName naming a
// *different* KSA than the mapping is refused rather than silently honored
// or silently overridden — either direction would run the pod as an
// identity other than the one the mapping says this GSA should get.
func TestBuildStartContext_KubernetesAssignConflictingInlineServiceAccountName(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-conflict-inline",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		InlineConfig: &api.ScionConfig{
			Kubernetes: &api.KubernetesConfig{ServiceAccountName: "some-other-ksa"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for a conflicting explicit ServiceAccountName, got nil (env: %v)", sc.Opts.Env)
	}
	if sc != nil {
		t.Errorf("expected nil startContext alongside the error, got %+v", sc)
	}
	if !strings.Contains(err.Error(), "some-other-ksa") || !strings.Contains(err.Error(), "agent-worker-ksa") {
		t.Errorf("expected the error to name both the explicit and mapped ServiceAccount names, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignConflictingConfigServiceAccountName
// covers the same conflict rule via the create request's Config.Kubernetes
// field (not InlineConfig) — a separate source the conflict check also
// reads, and one that must independently reject a mismatch.
func TestBuildStartContext_KubernetesAssignConflictingConfigServiceAccountName(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-conflict-config",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
			Kubernetes: &api.KubernetesConfig{ServiceAccountName: "config-level-other-ksa"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for a conflicting Config.Kubernetes ServiceAccountName, got nil (env: %v)", sc.Opts.Env)
	}
	if sc != nil {
		t.Errorf("expected nil startContext alongside the error, got %+v", sc)
	}
	if !strings.Contains(err.Error(), "config-level-other-ksa") || !strings.Contains(err.Error(), "agent-worker-ksa") {
		t.Errorf("expected the error to name both the explicit and mapped ServiceAccount names, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignExplicitServiceAccountNameMatchesMapping
// covers the non-conflicting half of the same rule: an explicit inline
// ServiceAccountName that already equals the mapped KSA proceeds normally.
func TestBuildStartContext_KubernetesAssignExplicitServiceAccountNameMatchesMapping(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-match",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		InlineConfig: &api.ScionConfig{
			Kubernetes: &api.KubernetesConfig{ServiceAccountName: "agent-worker-ksa"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected a matching explicit ServiceAccountName to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
	}
	// The original explicit InlineConfig is untouched: the resolved value is
	// carried on the dedicated opts field, not written back into it.
	if got := sc.Opts.InlineConfig.Kubernetes.ServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected the original InlineConfig ServiceAccountName to remain 'agent-worker-ksa', got %q", got)
	}
}

// TestBuildStartContext_KubernetesAssignResolvedEnvSAEmailSource covers the
// start/restart shape: no Config at all, the GSA email and project ID arrive
// only via a hub-injected resolvedEnv (as httpdispatcher.go actually sends
// them), and the mapping must still resolve from that source.
func TestBuildStartContext_KubernetesAssignResolvedEnvSAEmailSource(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-resolvedenv",
		ProjectPath: projectDir,
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    "agent-worker@my-project.iam.gserviceaccount.com",
			"SCION_METADATA_PROJECT_ID":  "my-project",
		},
		HTTPRequest: r,
		Operation:   opHTTPStart,
	})
	if err != nil {
		t.Fatalf("expected the resolvedEnv-sourced GSA to resolve, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected ResolvedKubernetesServiceAccountName='agent-worker-ksa', got %q", got)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected SCION_METADATA_MODE='passthrough', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
}

// TestBuildStartContext_KubernetesAssignCreateAndStartProduceSameEnv pins the
// requirement directly: a create-shaped dispatch (Config.GCPIdentity) and a
// start-shaped dispatch (ResolvedEnv, as the hub actually sends on
// start/restart) for the same GSA must produce byte-identical
// SCION_METADATA_* env, not differ by dispatch path.
func TestBuildStartContext_KubernetesAssignCreateAndStartProduceSameEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	metadataKeys := []string{"SCION_METADATA_MODE", "SCION_METADATA_SA_EMAIL", "SCION_METADATA_PROJECT_ID", "GCE_METADATA_HOST", "GCE_METADATA_ROOT", "SCION_METADATA_PORT"}

	srvCreate := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDirCreate := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)
	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	scCreate, err := srvCreate.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-create",
		ProjectPath: projectDirCreate,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("create path: expected acceptance, got %v", err)
	}

	cfg2 := DefaultServerConfig()
	cfg2.StateDir = t.TempDir()
	srvStart := newTestServerForStartContextRuntime(t, cfg2, "kubernetes")
	projectDirStart := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)
	scStart, err := srvStart.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-start",
		ProjectPath: projectDirStart,
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    "agent-worker@my-project.iam.gserviceaccount.com",
			"SCION_METADATA_PROJECT_ID":  "my-project",
		},
		HTTPRequest: r,
		Operation:   opHTTPStart,
	})
	if err != nil {
		t.Fatalf("start path: expected acceptance, got %v", err)
	}

	for _, key := range metadataKeys {
		createVal, createOK := scCreate.Opts.Env[key]
		startVal, startOK := scStart.Opts.Env[key]
		if createOK != startOK || createVal != startVal {
			t.Errorf("%s differs between create and start: create=(%q,%v) start=(%q,%v)", key, createVal, createOK, startVal, startOK)
		}
	}
	if scCreate.Opts.ResolvedKubernetesServiceAccountName != scStart.Opts.ResolvedKubernetesServiceAccountName {
		t.Errorf("ResolvedKubernetesServiceAccountName differs between create (%q) and start (%q)",
			scCreate.Opts.ResolvedKubernetesServiceAccountName, scStart.Opts.ResolvedKubernetesServiceAccountName)
	}
}

// TestBuildStartContext_KubernetesAssignSavedProfileFallback covers the
// start/restart profile source: with no Config at all, the profile must come
// from the agent's own saved profile (agent.GetSavedProfile), not silently
// fall back to the project's active profile. Uses
// newTestServerForStartContextMultiProfile (not
// newTestServerForStartContextRuntime): ForceRuntime short-circuits
// resolveDispatchProfileSelection before any profile is resolved at all, which
// would defeat this test's entire point. The broker's default profile
// ("local", pinned to a non-Kubernetes runtime) and the agent's saved
// profile ("other-profile", pinned to Kubernetes with its own KSA mapping)
// are deliberately different, so only reading the correct one passes.
func TestBuildStartContext_KubernetesAssignSavedProfileFallback(t *testing.T) {
	const otherProfile = "other-profile"
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, "docker", otherProfile, "kubernetes")
	newTestGlobalSettings(t, fmt.Sprintf(`schema_version: "1"
profiles:
    %s:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: saved-profile-ksa
runtimes:
    kubernetes:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: runtime-entry-ksa
`, otherProfile))

	// Persist a saved profile for this agent, distinct from the broker's
	// default, the same way agent.GetSavedProfile reads it (agent-info.json
	// under the agent's home directory).
	const agentName = "agent-k8s-assign-saved-profile"
	writeSavedAgentProfile(t, dotScion, agentName, otherProfile)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: agentName,
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    "agent-worker@my-project.iam.gserviceaccount.com",
		},
		HTTPRequest: r,
		Operation:   opHTTPStart,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "saved-profile-ksa" {
		t.Errorf("expected the agent's saved profile mapping 'saved-profile-ksa' to be used, got %q (runtime-entry-ksa would mean the profile-level entry was skipped)", got)
	}
}

// TestBuildStartContext_KubernetesAssignProjectLevelMappingLogsWarning covers
// the operator-experience side of the "project settings cannot override this
// mapping" rule: a project that sets kubernetes_service_account_mappings on
// its own runtime entry does not just get ignored silently — the broker logs
// a warning naming the setting, so the mistake is visible instead of only
// showing up later as a confusing "no mapping" error for some other GSA.
// This dispatch itself still succeeds, using the global mapping.
func TestBuildStartContext_KubernetesAssignProjectLevelMappingLogsWarning(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	var buf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	projectDir := newTestProjectSettings(t, `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: project-level-ksa-ignored
`)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-project-level-warning",
		ProjectPath: projectDir,
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    "agent-worker@my-project.iam.gserviceaccount.com",
		},
		HTTPRequest: r,
		Operation:   opHTTPStart,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted (using the global mapping), got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected the global mapping 'agent-worker-ksa' to be used (not the project-level entry), got %q", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "kubernetes_service_account_mappings") || !strings.Contains(logged, "never consulted") {
		t.Errorf("expected a warning naming kubernetes_service_account_mappings as never consulted from project settings, got log: %s", logged)
	}
}

// TestBuildStartContext_KubernetesAssignNonActiveProfileMapping covers an
// explicit, non-active profile named on the create request: the mapping must
// come from that named profile, not silently from the broker's default
// profile (pinned to a non-Kubernetes runtime here, so the two are also
// distinguishable by isKubernetes, not just by which KSA resolves).
func TestBuildStartContext_KubernetesAssignNonActiveProfileMapping(t *testing.T) {
	const otherProfile = "other-profile"
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv, _ := newTestServerForStartContextMultiProfile(t, cfg, "docker", otherProfile, "kubernetes")
	newTestGlobalSettings(t, fmt.Sprintf(`schema_version: "1"
profiles:
    %s:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: other-profile-ksa
runtimes:
    kubernetes:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: runtime-entry-ksa
`, otherProfile))

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-k8s-assign-non-active-profile",
		Config: &CreateAgentConfig{
			Profile: otherProfile,
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "other-profile-ksa" {
		t.Errorf("expected the named profile's mapping 'other-profile-ksa' to be used, got %q (runtime-entry-ksa would mean the requested profile was ignored)", got)
	}
}

// TestBuildStartContext_KubernetesAssignInvalidResolvedEntryRejected covers
// the broker-side re-validation of the resolved mapping entry: settings.yaml
// can be hand-edited (or DB-overlaid) without going through the schema
// validator, so an invalid KSA name that nonetheless resolves via
// ResolveKubernetesServiceAccountMapping must still be rejected here.
func TestBuildStartContext_KubernetesAssignInvalidResolvedEntryRejected(t *testing.T) {
	const settingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: Invalid_KSA_Name
`
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, settingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-invalid-entry",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for an invalid resolved KSA name, got nil (env: %v)", sc.Opts.Env)
	}
	if !strings.Contains(err.Error(), "not a valid Kubernetes ServiceAccount name") {
		t.Errorf("expected the error to say the resolved KSA name is invalid, got %q", err.Error())
	}
}

// TestBuildStartContext_KubernetesAssignProjectSettingsCannotOverrideMapping
// is the operator-only-source requirement's own test: a project's
// settings.yaml defines a *conflicting* mapping for the same GSA, but the
// broker's global mapping must still be the one that wins — a project
// (potentially a repository any contributor can edit) must not be able to
// redirect a Kubernetes assign dispatch to a different identity than the one
// the operator configured.
func TestBuildStartContext_KubernetesAssignProjectSettingsCannotOverrideMapping(t *testing.T) {
	const projectSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: project-sourced-ksa
`
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, projectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-k8s-assign-project-override-attempt",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected the operator's global mapping 'agent-worker-ksa' to win, got %q (project-sourced-ksa would mean the project settings.yaml overrode it)", got)
	}
}

// TestBuildStartContext_KubernetesAssignProfileMappingOverridesRuntimeMapping
// exercises the settings precedence end-to-end through buildStartContext (the
// unit-level precedence itself is covered directly by
// TestResolveKubernetesServiceAccountMapping in pkg/config): a profile-level
// mapping for the requested GSA wins over the runtime-level mapping for the
// same GSA, both read from the broker's global settings. Uses
// newTestServerForStartContextMultiProfile (not
// newTestServerForStartContextRuntime): ForceRuntime short-circuits
// resolveDispatchProfileSelection before any profile is resolved at all,
// which would always leave ProfileName empty and defeat this test's point —
// the broker's default profile ("local") is the one under test here, pinned
// to Kubernetes; the unused "other-profile"/"docker" values just satisfy the
// helper's signature.
func TestBuildStartContext_KubernetesAssignProfileMappingOverridesRuntimeMapping(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv, _ := newTestServerForStartContextMultiProfile(t, cfg, "kubernetes", "other-profile", "docker")
	newTestGlobalSettings(t, `schema_version: "1"
profiles:
    local:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: profile-ksa
runtimes:
    kubernetes:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: runtime-ksa
`)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-k8s-assign-precedence",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "assign",
				SAEmail:      "agent-worker@my-project.iam.gserviceaccount.com",
				ProjectID:    "my-project",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected the dispatch to be accepted, got %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "profile-ksa" {
		t.Errorf("expected the profile-level mapping 'profile-ksa' to win, got %q", got)
	}
}

// TestStartAgentEndpoint_KubernetesAssignResolvedKSAReachesOpts and
// TestRestartAgentEndpoint_KubernetesAssignResolvedKSAReachesOpts exercise the
// full HTTP handler (not just buildStartContext directly), confirming
// startAgent/restartAgent pass the broker-resolved KSA through to
// mgr.Start's opts unmodified — the "reaches the pod spec" requirement, one
// level up from pkg/agent's own TestStart_ResolvedKubernetesServiceAccountNameOverridesTemplate
// and TestStart_RestartOfExistingAgent_ResolvedKubernetesServiceAccountNameOverridesPersistedValue,
// which cover run.go's application of the field once it reaches Start.

func TestStartAgentEndpoint_KubernetesAssignResolvedKSAReachesOpts(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	mgr, ok := srv.manager.(*envCapturingManager)
	if !ok {
		t.Fatalf("expected *envCapturingManager, got %T", srv.manager)
	}

	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	bodyJSON := `{"projectPath": ` + strconv.Quote(projectDir) + `, "resolvedEnv": {"SCION_METADATA_MODE": "assign", "SCION_METADATA_MODE_SOURCE": "hub", "SCION_METADATA_SA_EMAIL": "agent-worker@my-project.iam.gserviceaccount.com", "SCION_METADATA_PROJECT_ID": "my-project"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-k8s-assign-start/start", strings.NewReader(bodyJSON))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted && w.Code != http.StatusOK {
		t.Fatalf("expected a success status, got %d: %s", w.Code, w.Body.String())
	}
	if got := mgr.lastStartOpts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected opts.ResolvedKubernetesServiceAccountName='agent-worker-ksa' to reach mgr.Start, got %q", got)
	}
}

func TestRestartAgentEndpoint_KubernetesAssignResolvedKSAReachesOpts(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	mgr, ok := srv.manager.(*envCapturingManager)
	if !ok {
		t.Fatalf("expected *envCapturingManager, got %T", srv.manager)
	}

	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	// restartAgent looks the agent up by name via manager.List before calling
	// buildStartContext, to resolve its ProjectPath — register it here the
	// same way an already-running agent would appear.
	mgr.agents = []api.AgentInfo{{Name: "agent-k8s-assign-restart", ProjectPath: projectDir}}

	bodyJSON := `{"resolvedEnv": {"SCION_METADATA_MODE": "assign", "SCION_METADATA_MODE_SOURCE": "hub", "SCION_METADATA_SA_EMAIL": "agent-worker@my-project.iam.gserviceaccount.com", "SCION_METADATA_PROJECT_ID": "my-project"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-k8s-assign-restart/restart", strings.NewReader(bodyJSON))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("expected a success status, got %d: %s", w.Code, w.Body.String())
	}
	if got := mgr.lastStartOpts.ResolvedKubernetesServiceAccountName; got != "agent-worker-ksa" {
		t.Errorf("expected opts.ResolvedKubernetesServiceAccountName='agent-worker-ksa' to reach mgr.Start, got %q", got)
	}
}

// TestStartAgentEndpoint_KubernetesAssignConflictingInlineServiceAccountNameRejected
// confirms the explicit-conflict check fires through the actual start HTTP
// path (a JSON inlineConfig field in the request body), not only when
// buildStartContext is called directly — mgr.Start must never be reached.
func TestStartAgentEndpoint_KubernetesAssignConflictingInlineServiceAccountNameRejected(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	mgr, ok := srv.manager.(*envCapturingManager)
	if !ok {
		t.Fatalf("expected *envCapturingManager, got %T", srv.manager)
	}

	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	bodyJSON := `{"projectPath": ` + strconv.Quote(projectDir) + `, "resolvedEnv": {"SCION_METADATA_MODE": "assign", "SCION_METADATA_MODE_SOURCE": "hub", "SCION_METADATA_SA_EMAIL": "agent-worker@my-project.iam.gserviceaccount.com"}, "inlineConfig": {"kubernetes": {"serviceAccountName": "some-other-ksa"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-k8s-assign-start-conflict/start", strings.NewReader(bodyJSON))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Pins the 400-status-preservation fix: buildStartContext's
	// startContextError carries Status: http.StatusBadRequest for this
	// rejection, and the handler must surface that status, not collapse it
	// to a generic 500.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for a conflicting inline ServiceAccountName (not collapsed to a 500), got %d: %s", w.Code, w.Body.String())
	}
	if mgr.startCalls != 0 {
		t.Errorf("expected mgr.Start to never be called, got %d calls", mgr.startCalls)
	}
	if !strings.Contains(w.Body.String(), "some-other-ksa") {
		t.Errorf("expected the error response to name the conflicting ServiceAccountName, got %s", w.Body.String())
	}
}

// TestCreateAgentEndpoint_BuildStartContext400RecordedAsAttemptStatus guards
// handlers.go's createAgent: when buildStartContext rejects the request with
// a 400 (the same Kubernetes assign conflict as the start-path test above),
// the recorded dispatch-attempt HTTPStatus must be that 400, not a reversion
// to an unconditional 500 (M19) — a Hub polling the attempt status by
// requestId needs the real status to distinguish a client-correctable
// rejection from a broker-side failure.
func TestCreateAgentEndpoint_BuildStartContext400RecordedAsAttemptStatus(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testKubernetesMappingGlobalSettingsYAML)

	const requestID = "req-m19-attempt-status"
	bodyJSON := `{"requestId": "` + requestID + `", "name": "agent-k8s-assign-create-conflict", "projectPath": ` + strconv.Quote(projectDir) + `, "resolvedEnv": {"SCION_METADATA_MODE": "assign", "SCION_METADATA_MODE_SOURCE": "hub", "SCION_METADATA_SA_EMAIL": "agent-worker@my-project.iam.gserviceaccount.com"}, "inlineConfig": {"kubernetes": {"serviceAccountName": "some-other-ksa"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for a conflicting inline ServiceAccountName, got %d: %s", w.Code, w.Body.String())
	}

	attempt, ok := srv.dispatchAttempts[requestID]
	if !ok {
		t.Fatalf("expected a recorded dispatch attempt for requestId %q", requestID)
	}
	if attempt.HTTPStatus != http.StatusBadRequest {
		t.Errorf("expected recorded attempt HTTPStatus=400, got %d", attempt.HTTPStatus)
	}
}

// TestStartAgentEndpoint_NoStrayProjectMarkerFromResolvedProjectPath is the
// regression anchor for the stray-".scion"-marker bug (M17): startAgent's
// fallback resolves opts.ProjectPath from an existing container's recorded
// ProjectPath only when the start request's own body carries neither
// projectPath nor projectSlug. That resolved value must be used only for the
// rest of this handler and the dispatch below, never fed back into
// buildStartContext's own in.ProjectPath — buildStartContext's
// project-marker block (start_context.go, guarded on
// in.ProjectPath != "" && (in.ProjectSlug != "" || in.ProjectID != "")) would
// otherwise create a ".scion" marker nested inside what is typically already
// a split-storage external directory, not a project root. ProjectID is
// supplied here (via the projectId query parameter, independent of the
// request body) so the marker block's guard is satisfied as soon as
// in.ProjectPath is non-empty, isolating exactly the behavior this test
// guards.
func TestStartAgentEndpoint_NoStrayProjectMarkerFromResolvedProjectPath(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "docker")

	// externalPath simulates an existing agent's recorded ProjectPath in
	// split-storage mode: a directory that is not itself a project root and
	// has no ".scion" of its own yet.
	externalPath := filepath.Join(t.TempDir(), "external")
	if err := os.MkdirAll(externalPath, 0755); err != nil {
		t.Fatal(err)
	}

	mgr, ok := srv.manager.(*envCapturingManager)
	if !ok {
		t.Fatalf("expected *envCapturingManager, got %T", srv.manager)
	}
	mgr.agents = []api.AgentInfo{{Name: "existing-agent", ProjectPath: externalPath, ProjectID: "proj-123"}}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/existing-agent/start?projectId=proj-123", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("expected a success status, got %d: %s", w.Code, w.Body.String())
	}

	strayMarker := filepath.Join(externalPath, config.DotScion)
	if _, err := os.Stat(strayMarker); err == nil {
		t.Errorf("expected no stray marker at %s, but one was created", strayMarker)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking for stray marker: %v", err)
	}
}

// TestBuildStartContext_GCPMetadataBlockUnchangedOnDocker guards the
// inversion from the other direction: "block" must remain valid on Docker
// (and, by extension, any non-Kubernetes runtime).
func TestBuildStartContext_GCPMetadataBlockUnchangedOnDocker(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "docker")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-docker-block",
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "block",
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected block mode to be accepted on docker, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_GCPMetadataUnknownModeRejected covers the fail-open case
// the allow-list exists to close. Before the inversion an unrecognised mode was
// treated as a no-op: no SCION_METADATA_MODE, no GCE_METADATA_HOST redirect, and
// therefore a container talking to the real GCE metadata server. The failure was
// silent, which is what made it dangerous — a typo in a mode string downgraded
// the agent to unprotected rather than refusing to start it.
//
// The empty-string case matters most. It is not a typo an operator makes; it is
// what a non-nil GCPIdentity with an unset MetadataMode produces, and it used to
// be indistinguishable from passthrough.
func TestBuildStartContext_GCPMetadataUnknownModeRejected(t *testing.T) {
	modes := map[string]string{
		"typo":            "blocked",
		"unknown value":   "sandbox",
		"empty":           "",
		"wrong case":      "Block",
		"leading space":   " block",
		"prefix of known": "bl",
	}

	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-bad-mode",
				Config: &CreateAgentConfig{
					GCPIdentity: &GCPIdentityConfig{
						MetadataMode: mode,
					},
				},
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if err == nil {
				t.Fatalf("expected an error for metadata mode %q, got nil (env: %v)", mode, sc.Opts.Env)
			}
			// The start must fail rather than proceed with a partial or absent
			// redirect, so there is no context to inspect at all.
			if sc != nil {
				t.Errorf("expected nil startContext alongside the error, got %+v", sc)
			}
			if !strings.Contains(err.Error(), "Invalid GCP metadata mode") {
				t.Errorf("expected an invalid-mode error, got %q", err.Error())
			}
		})
	}
}

// TestBuildStartContext_GCPMetadataUnknownModeFromResolvedEnv checks the second
// input path. The mode can also arrive as hub-injected resolvedEnv on the start
// path, where there is no CreateAgentConfig at all, so the allow-list has to
// cover it too — a broker that validated only the create path would still accept
// an unknown mode from a newer or misbehaving hub.
func TestBuildStartContext_GCPMetadataUnknownModeFromResolvedEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-bad-env-mode",
		ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "blocked"},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err == nil {
		t.Fatalf("expected an error for hub-injected mode 'blocked', got nil (env: %v)", sc.Opts.Env)
	}
	if !strings.Contains(err.Error(), "Invalid GCP metadata mode") {
		t.Errorf("expected an invalid-mode error, got %q", err.Error())
	}
}

// TestBuildStartContext_GCPMetadataModeFromResolvedEnvStillAccepted guards the
// inversion against over-reach: the start path must keep working for the modes
// the hub legitimately injects. SCION_METADATA_MODE_SOURCE=hub is included
// because "assign" is an elevated mode: without the marker the broker treats
// it as untrusted and downgrades it (see
// TestBuildStartContext_GCPMetadataElevatedModeWithoutSourceMarkerDowngraded).
func TestBuildStartContext_GCPMetadataModeFromResolvedEnvStillAccepted(t *testing.T) {
	for _, mode := range []string{"assign", "block"} {
		t.Run(mode, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-env-mode",
				ResolvedEnv: map[string]string{
					"SCION_METADATA_MODE":        mode,
					"SCION_METADATA_MODE_SOURCE": "hub",
				},
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if err != nil {
				t.Fatalf("expected mode %q to be accepted, got %v", mode, err)
			}
			if sc.Opts.Env["SCION_METADATA_MODE"] != mode {
				t.Errorf("expected SCION_METADATA_MODE=%q, got %q", mode, sc.Opts.Env["SCION_METADATA_MODE"])
			}
			if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
				t.Errorf("expected the metadata redirect to be set, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
			}
		})
	}
}

// TestBuildStartContext_GCPMetadataFromResolvedEnv verifies that the start
// path (no Config.GCPIdentity) picks up GCP identity from resolvedEnv
// injected by the hub.  This is the code path hit by "Create & Edit" where
// the agent is provisioned first and then started with updated config.
func TestBuildStartContext_GCPMetadataFromResolvedEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Simulate hub injecting GCP identity via resolvedEnv (start path).
	// SCION_METADATA_MODE_SOURCE marks "assign" as the hub's own
	// authoritative write; without it the broker would downgrade it.
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-resolved-assign",
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "assign",
			"SCION_METADATA_MODE_SOURCE": "hub",
			"SCION_METADATA_SA_EMAIL":    "sa@proj.iam.gserviceaccount.com",
			"SCION_METADATA_PROJECT_ID":  "my-project",
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "assign" {
		t.Errorf("expected SCION_METADATA_MODE='assign', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["SCION_METADATA_SA_EMAIL"] != "sa@proj.iam.gserviceaccount.com" {
		t.Errorf("expected SA email from resolvedEnv, got %q", sc.Opts.Env["SCION_METADATA_SA_EMAIL"])
	}
	if sc.Opts.Env["SCION_METADATA_PROJECT_ID"] != "my-project" {
		t.Errorf("expected project ID from resolvedEnv, got %q", sc.Opts.Env["SCION_METADATA_PROJECT_ID"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

func TestBuildStartContext_GCPMetadataPassthroughFromResolvedEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Simulate hub injecting passthrough mode via resolvedEnv. The source
	// marker is required here too: passthrough is an elevated mode.
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-resolved-passthrough",
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":        "passthrough",
			"SCION_METADATA_MODE_SOURCE": "hub",
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Passthrough should NOT set metadata server env vars
	if sc.Opts.Env["SCION_METADATA_PORT"] != "" {
		t.Errorf("expected no SCION_METADATA_PORT for passthrough, got %q", sc.Opts.Env["SCION_METADATA_PORT"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST for passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_GCPMetadataElevatedModeWithoutSourceMarkerDowngraded
// is the version-skew case SCION_METADATA_MODE_SOURCE exists to close: a hub
// old enough to predate the marker sends SCION_METADATA_MODE without it, on
// the same fallback path a stored env var or secret can also reach (see the
// reserved-target checks elsewhere in this change). Absent the marker, an
// elevated mode must be downgraded to the secure default rather than
// trusted.
func TestBuildStartContext_GCPMetadataElevatedModeWithoutSourceMarkerDowngraded(t *testing.T) {
	for _, mode := range []string{"assign", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			r := httptest.NewRequest("POST", "/api/v1/agents", nil)

			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-unmarked-env-mode",
				ResolvedEnv: map[string]string{"SCION_METADATA_MODE": mode},
				HTTPRequest: r,
				Operation:   opCreate,
			})
			if err != nil {
				t.Fatalf("expected downgrade, not rejection, for mode %q, got error: %v", mode, err)
			}
			if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
				t.Errorf("expected unmarked mode %q to be downgraded to block, got %q", mode, sc.Opts.Env["SCION_METADATA_MODE"])
			}
			if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
				t.Errorf("expected the metadata redirect to be set after downgrade, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
			}
		})
	}
}

// newTestServerWithRuntime creates a test server like newTestServerForStartContext
// but with a custom runtime name.
func newTestServerWithRuntime(t *testing.T, cfg ServerConfig, runtimeName string) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWd)
	})

	dotScion := filepath.Join(tmpDir, ".scion")
	if err := os.Mkdir(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: mock
runtimes:
    mock:
        type: mock
`
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	templatesDir := filepath.Join(dotScion, "templates")
	if err := os.MkdirAll(templatesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templatesDir, "default"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templatesDir, "claude"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg.ForceRuntime = "mock"
	mgr := &envCapturingManager{}
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
	}
	return New(cfg, mgr, rt)
}

// TestBuildStartContext_CloudrunSandboxHubEndpoint verifies hub endpoint
// behaviour for the cloudrun-sandbox runtime. SCION_METADATA_BIND_ADDRESS
// pins an explicit TEST-NET-3 (RFC 5737) documentation address so the result
// is deterministic instead of depending on interface discovery (unavailable
// in CI). The endpoint must be http://<bind-address>:<port> with the port
// read from the broker's own HubListenPort config — not a public
// IAP-fronted URL, which the sandbox cannot authenticate against.
func TestBuildStartContext_CloudrunSandboxHubEndpoint(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.HubListenPort = 8080
	srv := newTestServerForStartContextRuntime(t, cfg, "cloudrun-sandbox")

	t.Setenv("SCION_METADATA_BIND_ADDRESS", "203.0.113.5")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-sandbox",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("buildStartContext() unexpected error: %v", err)
	}

	const want = "http://203.0.113.5:8080"
	if ep := sc.Opts.Env["SCION_HUB_ENDPOINT"]; ep != want {
		t.Fatalf("SCION_HUB_ENDPOINT = %q, want %q (bind address plus HubListenPort)", ep, want)
	}

	// Metadata vars must still be localhost — emulator runs inside sandbox.
	host := sc.Opts.Env["GCE_METADATA_HOST"]
	root := sc.Opts.Env["GCE_METADATA_ROOT"]
	if host != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", host)
	}
	if root != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", root)
	}
}

// TestBuildStartContext_CloudrunSandboxHubNeverRunApp is a durable guard:
// assert the negative so the test survives refactoring and catches the
// shipped defect pattern. If buildStartContext succeeds for cloudrun-sandbox,
// the hub endpoint must NEVER be a public IAP-fronted URL.
func TestBuildStartContext_CloudrunSandboxHubNeverRunApp(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.HubListenPort = 8080
	srv := newTestServerForStartContextRuntime(t, cfg, "cloudrun-sandbox")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Simulate a hub-dispatched agent whose resolvedEnv carries the public URL.
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-sandbox-iap",
		ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": "https://my-instance-xyz.run.app"},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		// In CI this fails (no link-local) — that is correct.
		return
	}
	ep := sc.Opts.Env["SCION_HUB_ENDPOINT"]
	if strings.Contains(ep, "run.app") {
		t.Fatalf("cloudrun-sandbox SCION_HUB_ENDPOINT must never contain run.app, got %q", ep)
	}
}

// TestBuildStartContext_GCPMetadataBothVarsAlwaysMatch guards the invariant
// that GCE_METADATA_HOST and GCE_METADATA_ROOT are always set to the same
// value. A mismatch means gcloud (ROOT) and language SDKs (HOST) would talk
// to different servers. Only tests non-cloudrun-sandbox runtimes since
// cloudrun-sandbox may fail in CI (no link-local).
func TestBuildStartContext_GCPMetadataBothVarsAlwaysMatch(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerWithRuntime(t, cfg, "mock")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-both-vars",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	host := sc.Opts.Env["GCE_METADATA_HOST"]
	root := sc.Opts.Env["GCE_METADATA_ROOT"]
	if host != root {
		t.Errorf("GCE_METADATA_HOST=%q GCE_METADATA_ROOT=%q — must match", host, root)
	}
}

// gcpIdentityDispatchProfileCases is the shared table for
// TestBuildStartContext_GCPIdentityUsesDispatchProfile_{Create,Start,Restart}:
// a broker can register more than one profile (e.g. both a "docker" and a
// "kubernetes" profile), and a dispatch's profile selects which one it runs
// on (ptone/scion#2328). The GCP identity check must follow the runtime the
// profile THIS dispatch names resolves to, not the broker's default runtime
// — covering both directions catches a fix that only handles one.
var gcpIdentityDispatchProfileCases = []struct {
	name                      string
	defaultRuntime            string
	profileRuntime            string
	wantDefaultMode           string // resolved mode when no GCP identity is configured
	wantExplicitBlockRejected bool
}{
	{
		name:                      "docker-default broker, kubernetes profile",
		defaultRuntime:            "docker",
		profileRuntime:            "kubernetes",
		wantDefaultMode:           "passthrough",
		wantExplicitBlockRejected: true,
	},
	{
		name:                      "kubernetes-default broker, docker profile",
		defaultRuntime:            "kubernetes",
		profileRuntime:            "docker",
		wantDefaultMode:           "block",
		wantExplicitBlockRejected: false,
	},
}

const gcpIdentityDispatchOtherProfile = "other-profile"

// assertGCPIdentityDefaultMode calls buildStartContext with makeInputs (which
// must configure no GCP identity at all) and checks the resolved
// SCION_METADATA_MODE matches wantMode.
func assertGCPIdentityDefaultMode(t *testing.T, srv *Server, wantMode string, makeInputs func() startContextInputs) {
	t.Helper()
	sc, err := srv.buildStartContext(context.Background(), makeInputs())
	if err != nil {
		t.Fatalf("expected no GCP identity input to be accepted, got %v", err)
	}
	if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != wantMode {
		t.Errorf("expected SCION_METADATA_MODE=%q, got %q", wantMode, got)
	}
}

// assertGCPIdentityExplicitBlock calls buildStartContext with makeInputs
// (which must configure an explicit "block") and checks it is rejected or
// accepted per wantRejected.
func assertGCPIdentityExplicitBlock(t *testing.T, srv *Server, wantRejected bool, makeInputs func() startContextInputs) {
	t.Helper()
	sc, err := srv.buildStartContext(context.Background(), makeInputs())
	if wantRejected {
		if err == nil {
			t.Fatalf("expected explicit block to be rejected, got nil (env: %v)", sc.Opts.Env)
		}
		if !strings.Contains(err.Error(), "Kubernetes") {
			t.Errorf("expected the error to name the Kubernetes runtime, got %q", err.Error())
		}
		return
	}
	if err != nil {
		t.Fatalf("expected explicit block to be accepted, got %v", err)
	}
	if got := sc.Opts.Env["SCION_METADATA_MODE"]; got != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block', got %q", got)
	}
}

// TestBuildStartContext_GCPIdentityUsesDispatchProfile_Create covers the
// create path, where the profile comes from the request's Config.Profile.
func TestBuildStartContext_GCPIdentityUsesDispatchProfile_Create(t *testing.T) {
	for _, tt := range gcpIdentityDispatchProfileCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("no identity configured", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, _ := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				r := httptest.NewRequest("POST", "/api/v1/agents", nil)
				assertGCPIdentityDefaultMode(t, srv, tt.wantDefaultMode, func() startContextInputs {
					return startContextInputs{
						Name:        "agent-profile-default",
						Config:      &CreateAgentConfig{Profile: gcpIdentityDispatchOtherProfile},
						HTTPRequest: r,
						Operation:   opCreate,
					}
				})
			})

			t.Run("explicit block", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, _ := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				r := httptest.NewRequest("POST", "/api/v1/agents", nil)
				assertGCPIdentityExplicitBlock(t, srv, tt.wantExplicitBlockRejected, func() startContextInputs {
					return startContextInputs{
						Name: "agent-profile-block",
						Config: &CreateAgentConfig{
							Profile:     gcpIdentityDispatchOtherProfile,
							GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"},
						},
						HTTPRequest: r,
						Operation:   opCreate,
					}
				})
			})
		})
	}
}

// TestBuildStartContext_GCPIdentityCreateIgnoresSavedProfile pins that a
// create dispatch resolves its runtime and GCP identity classification from
// Config.Profile (falling back to the project's active profile when empty,
// inside resolveManagerForOpts itself) — never from the agent's own saved
// profile, even when one exists on disk for this agent name (e.g. a
// re-create after a crash, or a preserved directory from a prior run).
// opts.Profile (what ProvisionAgent, image resolution, and the saved profile
// written back by provision.go all use) is always in.Config.Profile on
// create, with no saved-profile fallback; the manager and GCP classification
// resolved here must agree with that, or a single create could run on one
// runtime while everything else about it is configured for another. Only
// start/restart fall back to the saved profile — see
// TestBuildStartContext_GCPIdentityUsesDispatchProfile_Start/_Restart below.
func TestBuildStartContext_GCPIdentityCreateIgnoresSavedProfile(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	// defaultRuntime (the project's active profile) is docker; the
	// non-default profile resolves to kubernetes.
	srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, "docker", gcpIdentityDispatchOtherProfile, "kubernetes")
	// This agent's own saved profile is the kubernetes one — different from
	// both the active profile and the (empty) Config.Profile the create
	// request below sends.
	writeSavedAgentProfile(t, dotScion, "agent-create-saved-profile-differs", gcpIdentityDispatchOtherProfile)

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	assertGCPIdentityDefaultMode(t, srv, "block", func() startContextInputs {
		return startContextInputs{
			Name: "agent-create-saved-profile-differs",
			// No Config.Profile: must resolve against the active profile
			// (docker, "block"), not the saved one (kubernetes, would be
			// "passthrough").
			Config:      &CreateAgentConfig{},
			HTTPRequest: r,
			Operation:   opCreate,
		}
	})
}

// TestBuildStartContext_GCPIdentityUsesDispatchProfile_Start covers the HTTP
// start path, where there is no Config.Profile — the profile comes from the
// agent's own saved profile (agent.GetSavedProfile), the same source
// handlers.go's startAgent already uses for manager resolution.
func TestBuildStartContext_GCPIdentityUsesDispatchProfile_Start(t *testing.T) {
	for _, tt := range gcpIdentityDispatchProfileCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("no identity configured", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				writeSavedAgentProfile(t, dotScion, "agent-saved-default", gcpIdentityDispatchOtherProfile)
				r := httptest.NewRequest("POST", "/api/v1/agents/agent-saved-default/start", nil)
				assertGCPIdentityDefaultMode(t, srv, tt.wantDefaultMode, func() startContextInputs {
					return startContextInputs{
						Name:        "agent-saved-default",
						HTTPRequest: r,
						Operation:   opHTTPStart,
					}
				})
			})

			t.Run("explicit block via resolvedEnv", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				writeSavedAgentProfile(t, dotScion, "agent-saved-block", gcpIdentityDispatchOtherProfile)
				r := httptest.NewRequest("POST", "/api/v1/agents/agent-saved-block/start", nil)
				assertGCPIdentityExplicitBlock(t, srv, tt.wantExplicitBlockRejected, func() startContextInputs {
					return startContextInputs{
						Name:        "agent-saved-block",
						ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block"},
						HTTPRequest: r,
						Operation:   opHTTPStart,
					}
				})
			})
		})
	}
}

// TestBuildStartContext_GCPIdentityUsesDispatchProfile_Restart is the
// restart-path twin of the start-path test above.
func TestBuildStartContext_GCPIdentityUsesDispatchProfile_Restart(t *testing.T) {
	for _, tt := range gcpIdentityDispatchProfileCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("no identity configured", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				writeSavedAgentProfile(t, dotScion, "agent-saved-restart-default", gcpIdentityDispatchOtherProfile)
				r := httptest.NewRequest("POST", "/api/v1/agents/agent-saved-restart-default/restart", nil)
				assertGCPIdentityDefaultMode(t, srv, tt.wantDefaultMode, func() startContextInputs {
					return startContextInputs{
						Name:        "agent-saved-restart-default",
						HTTPRequest: r,
						Operation:   opHTTPRestart,
					}
				})
			})

			t.Run("explicit block via resolvedEnv", func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, tt.defaultRuntime, gcpIdentityDispatchOtherProfile, tt.profileRuntime)
				writeSavedAgentProfile(t, dotScion, "agent-saved-restart-block", gcpIdentityDispatchOtherProfile)
				r := httptest.NewRequest("POST", "/api/v1/agents/agent-saved-restart-block/restart", nil)
				assertGCPIdentityExplicitBlock(t, srv, tt.wantExplicitBlockRejected, func() startContextInputs {
					return startContextInputs{
						Name:        "agent-saved-restart-block",
						ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block"},
						HTTPRequest: r,
						Operation:   opHTTPRestart,
					}
				})
			})
		})
	}
}

// TestBuildStartContext_GCPIdentityForceRuntimeOverridesProfile pins that
// ForceRuntime takes priority over a dispatch's profile, exactly as
// resolveManagerForOpts's own ForceRuntime branch (handlers.go) does: a
// profile naming a different runtime than an operator's forced runtime must
// not be able to work around it.
func TestBuildStartContext_GCPIdentityForceRuntimeOverridesProfile(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	const otherProfile = "other-profile"
	// Settings declare a "docker" default and a "kubernetes" other profile,
	// but ForceRuntime below pins the broker to "docker" regardless.
	srv, _ := newTestServerForStartContextMultiProfile(t, cfg, "docker", otherProfile, "kubernetes")
	srv.config.ForceRuntime = "docker"

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "agent-force-runtime",
		Config: &CreateAgentConfig{
			Profile:     otherProfile, // would resolve to kubernetes via settings alone
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("expected block to be accepted since ForceRuntime pins docker regardless of the kubernetes profile, got %v", err)
	}
	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block', got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
}

// --- resolveWorktreeProvision tests ---

func TestResolveWorktreeProvision_Eligible(t *testing.T) {
	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
			Depth:  intPtr(1),
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
	})

	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		if result.ShouldProvision {
			t.Fatal("expected ShouldProvision=false when git is too old")
		}
		t.Skip("git < 2.47, worktree mode not eligible on this host")
	}

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true, got false (reason: %s)", result.Reason)
	}

	expectedPath := filepath.Join(projectDir, "workspace", "worktrees", "agent-1")
	if result.WorktreePath != expectedPath {
		t.Errorf("expected WorktreePath=%q, got %q", expectedPath, result.WorktreePath)
	}

	expectedRoot := filepath.Join(projectDir, "workspace")
	if result.ProjectRoot != expectedRoot {
		t.Errorf("expected ProjectRoot=%q, got %q", expectedRoot, result.ProjectRoot)
	}

	pi := result.ProvisionInput
	if pi.Mode != store.SharingModeWorktreePerAgent {
		t.Errorf("expected Mode=worktree-per-agent, got %v", pi.Mode)
	}
	if pi.ProjectID != "proj-1" {
		t.Errorf("expected ProjectID='proj-1', got %q", pi.ProjectID)
	}
	if pi.AgentID != "agent-1" {
		t.Errorf("expected AgentID='agent-1', got %q", pi.AgentID)
	}
	if pi.GitClone == nil || pi.GitClone.URL != "https://github.com/org/repo.git" {
		t.Errorf("expected GitClone.URL set, got %v", pi.GitClone)
	}
	if pi.Locker != nil {
		t.Error("expected Locker=nil for node-local single-broker")
	}
}

func TestResolveWorktreeProvision_BranchOverridesAgentName(t *testing.T) {
	projectDir := t.TempDir()
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.47, worktree mode not eligible on this host")
	}

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		ProjectPath:   projectDir,
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
		AgentName:     "test-agent",
		Branch:        "feature-branch",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true, reason: %s", result.Reason)
	}
	if result.ProvisionInput.AgentName != "feature-branch" {
		t.Errorf("expected AgentName='feature-branch' (from Branch), got %q", result.ProvisionInput.AgentName)
	}
}

func TestResolveWorktreeProvision_WrongMode(t *testing.T) {
	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModePerAgent,
		GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		ProjectPath:   "/some/path",
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false for non-worktree mode")
	}
	if !strings.Contains(result.Reason, "not worktree-per-agent") {
		t.Errorf("expected reason to mention mode mismatch, got %q", result.Reason)
	}
}

func TestResolveWorktreeProvision_NoGitClone(t *testing.T) {
	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      nil,
		ProjectPath:   "/some/path",
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false when GitClone is nil")
	}
	if !strings.Contains(result.Reason, "not git-backed") {
		t.Errorf("expected reason to mention non-git, got %q", result.Reason)
	}
}

// TestResolveWorktreeProvision_MissingIDs is the defense-in-depth guard for
// GoogleCloudPlatform/scion#1931's worktree-per-agent start path: without
// both AgentID and ProjectID, provision.WorktreePath(base, "") resolves to
// the shared "worktrees" parent directory every agent's worktree lives
// under, not a per-agent path. Provisioning must be skipped entirely (fall
// back to clone-per-agent) rather than ever touching that shared path.
func TestResolveWorktreeProvision_MissingIDs(t *testing.T) {
	base := worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		ProjectPath:   "/some/path",
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
	}

	missingAgentID := base
	missingAgentID.AgentID = ""
	if result := resolveWorktreeProvision(missingAgentID); result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false when AgentID is empty")
	} else if !strings.Contains(result.Reason, "AgentID") {
		t.Errorf("expected reason to mention AgentID, got %q", result.Reason)
	}

	missingProjectID := base
	missingProjectID.ProjectID = ""
	if result := resolveWorktreeProvision(missingProjectID); result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false when ProjectID is empty")
	} else if !strings.Contains(result.Reason, "ProjectID") {
		t.Errorf("expected reason to mention ProjectID, got %q", result.Reason)
	}

	if result := resolveWorktreeProvision(base); !result.ShouldProvision {
		eligible, _ := runtime.WorktreeModeEligible()
		if eligible {
			t.Errorf("expected ShouldProvision=true when both IDs are set, reason: %s", result.Reason)
		}
	}
}

// TestTryProvisionWorktree_MissingIdentityOnStart_FailsClosed proves a
// start dispatch (never a create) with no valid agent identity fails the
// request instead of silently falling back to an in-container clone, which
// would mount a fresh, empty workspace over whatever this agent's real
// worktree holds — a create with the same missing identity still falls
// back, since a fresh create has no existing worktree to protect.
// TestResolveWorktreeProvision_InvalidIDsRejected is the table test for an
// AgentID or ProjectID that is any of the listed invalid or malformed
// values: each must be rejected, never reaching path construction.
func TestResolveWorktreeProvision_InvalidIDsRejected(t *testing.T) {
	projectDir := t.TempDir()
	invalidValues := []string{
		"..",
		".",
		"../../x",
		"a/b",
		"/tmp/abs",
		"agent-1/",
		"",
		"a\\b",
		"a\x00b",
	}
	for _, id := range invalidValues {
		t.Run("agentID="+id, func(t *testing.T) {
			result := resolveWorktreeProvision(worktreeProvisionInput{
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
				ProjectPath:   projectDir,
				ProjectID:     "proj-1",
				AgentID:       id,
			})
			if result.ShouldProvision {
				t.Fatalf("expected ShouldProvision=false for AgentID=%q", id)
			}
			if !result.MissingIdentity {
				t.Errorf("expected MissingIdentity=true for AgentID=%q, reason: %s", id, result.Reason)
			}
		})
		t.Run("projectID="+id, func(t *testing.T) {
			result := resolveWorktreeProvision(worktreeProvisionInput{
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
				ProjectPath:   projectDir,
				ProjectID:     id,
				AgentID:       "agent-1",
			})
			if result.ShouldProvision {
				t.Fatalf("expected ShouldProvision=false for ProjectID=%q", id)
			}
			if !result.MissingIdentity {
				t.Errorf("expected MissingIdentity=true for ProjectID=%q, reason: %s", id, result.Reason)
			}
		})
	}

	// A literal-looking "%2e%2e" is an ordinary, if unusual, directory
	// name, since nothing decodes it, and must be accepted like any other
	// opaque ID.
	if !isSingleCleanPathElement("%2e%2e") {
		t.Error(`expected "%2e%2e" to be a single clean path element (a literal name, not interpreted)`)
	}
}

func TestTryProvisionWorktree_MissingIdentityOnStart_FailsClosed(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name:          "some-agent",
		AgentID:       "", // missing
		ProjectID:     "p1",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	}, opts, map[string]string{}, srv.runtime.Name())

	if err == nil {
		t.Fatal("expected tryProvisionWorktree to fail closed when AgentID is missing on a start dispatch")
	}
	if ok {
		t.Error("expected ok=false alongside the error")
	}
	if opts.GitClone != nil {
		t.Error("expected no fallback to an in-container clone")
	}

	// The same missing-identity case on a create dispatch still falls back.
	opts2 := &api.StartOptions{}
	ok2, err2 := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name:          "some-agent",
		AgentID:       "",
		ProjectID:     "p1",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opCreate,
	}, opts2, map[string]string{}, srv.runtime.Name())
	if err2 != nil {
		t.Fatalf("expected create to fall back cleanly, got error: %v", err2)
	}
	if ok2 {
		t.Error("expected ok=false (fallback), got true")
	}
}

func TestResolveWorktreeProvision_GitTooOld_Fallback(t *testing.T) {
	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
			Depth:  intPtr(1),
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
		eligibilityOverride: func() (bool, string) {
			return false, "git >= 2.47.0 required for worktree-per-agent mode (--relative-paths), found 2.39.0"
		},
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false when git is too old")
	}
	if !strings.Contains(result.Reason, "2.47") {
		t.Errorf("expected reason to mention git 2.47 requirement, got %q", result.Reason)
	}
	if result.ProvisionInput.ProjectID != "" {
		t.Error("expected empty ProvisionInput when ineligible")
	}
}

func TestResolveWorktreeProvision_KubernetesNodeLocal_Rejected(t *testing.T) {
	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
		RuntimeName: "kubernetes",
		eligibilityOverride: func() (bool, string) {
			return true, ""
		},
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false for Kubernetes without NFS backend")
	}
	if !strings.Contains(result.Reason, "Kubernetes") {
		t.Errorf("expected reason to mention Kubernetes, got %q", result.Reason)
	}
	if !strings.Contains(result.Reason, "NFS") {
		t.Errorf("expected reason to mention NFS requirement, got %q", result.Reason)
	}
	if result.ProvisionInput.ProjectID != "" {
		t.Error("expected empty ProvisionInput when rejected")
	}
}

// TestResolveWorktreeProvision_KubernetesAliases_Rejected: every recognized
// spelling of the Kubernetes runtime skips host-side provisioning.
func TestResolveWorktreeProvision_KubernetesAliases_Rejected(t *testing.T) {
	for _, name := range []string{"k8s", "remote"} {
		t.Run(name, func(t *testing.T) {
			result := resolveWorktreeProvision(worktreeProvisionInput{
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				GitClone:      &api.GitCloneConfig{URL: "https://github.com/org/repo.git", Branch: "main"},
				ProjectPath:   t.TempDir(),
				ProjectID:     "proj-1",
				AgentID:       "agent-1",
				AgentName:     "test-agent",
				RuntimeName:   name,
				eligibilityOverride: func() (bool, string) {
					return true, ""
				},
			})
			if result.ShouldProvision {
				t.Fatalf("expected ShouldProvision=false for runtime %q", name)
			}
			if !strings.Contains(result.Reason, "Kubernetes") {
				t.Errorf("expected reason to mention Kubernetes, got %q", result.Reason)
			}
		})
	}
}

func TestResolveWorktreeProvision_DockerRuntime_NotRejected(t *testing.T) {
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.47, worktree mode not eligible on this host")
	}

	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
		RuntimeName: "docker",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true for Docker runtime, got false (reason: %s)", result.Reason)
	}
}

func TestResolveWorktreeProvision_EmptyRuntime_NotRejected(t *testing.T) {
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.47, worktree mode not eligible on this host")
	}

	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		AgentID:     "agent-1",
		RuntimeName: "",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true for empty RuntimeName, got false (reason: %s)", result.Reason)
	}
}

func TestResolveWorktreeProvision_FullCloneDepth(t *testing.T) {
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.47, worktree mode not eligible on this host")
	}

	projectDir := t.TempDir()
	originalGC := &api.GitCloneConfig{
		URL:    "https://github.com/org/repo.git",
		Branch: "main",
		Depth:  intPtr(1),
	}

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      originalGC,
		ProjectPath:   projectDir,
		ProjectID:     "proj-1",
		ProjectSlug:   "my-project",
		AgentID:       "agent-1",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true, reason: %s", result.Reason)
	}

	if result.ProvisionInput.GitClone.Depth == nil || *result.ProvisionInput.GitClone.Depth != 0 {
		t.Errorf("expected GitClone.Depth=0 (full clone), got %v", result.ProvisionInput.GitClone.Depth)
	}

	if originalGC.Depth == nil || *originalGC.Depth != 1 {
		t.Errorf("original GitClone.Depth was mutated: got %v, want 1", originalGC.Depth)
	}
}

// requireWorktreeGit skips the test when the host's git is too old for
// worktree-per-agent mode (--relative-paths requires git >= 2.47). CI's git
// is new enough; this keeps the suite green on an older host git (for
// example, stock Ubuntu 24.04 ships git 2.43).
func requireWorktreeGit(t *testing.T) {
	t.Helper()
	if eligible, reason := runtime.WorktreeModeEligible(); !eligible {
		t.Skip("worktree mode not eligible on this host: " + reason)
	}
}

// initBareRepoWithCommit creates a bare git repo (default branch main) seeded
// with one commit, and returns its path for use as a GitClone URL.
func initBareRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	wc := filepath.Join(dir, "wc")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, strings.TrimSpace(string(out)))
		}
	}
	run("init", "--bare", "-b", "main", bare)
	run("clone", bare, wc)
	if err := os.WriteFile(filepath.Join(wc, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", wc, "add", "-A")
	run("-C", wc, "commit", "-m", "init")
	run("-C", wc, "push", "origin", "main")
	return bare
}

// TestTryProvisionWorktree_JoinResolvesSharedPath verifies that when agent-b
// is provisioned with --branch pointing to an already-checked-out branch
// (agent-a's), provisioning succeeds as a JOIN and opts.Workspace is set to
// agent-a's worktree path (not WorktreePath(base, agent-b)).
func TestTryProvisionWorktree_JoinResolvesSharedPath(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Set up the shared base + agent-a's worktree on branch "agent-a".
	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: "agent-a",
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a", AgentName: "agent-a", GitClone: gc,
	}); err != nil {
		t.Fatalf("setup agent-a: %v", err)
	}
	base := resolved.HostPath
	agentAWt := provision.WorktreePath(base, "agent-a")
	if _, err := os.Stat(agentAWt); err != nil {
		t.Fatalf("agent-a worktree missing after setup: %v", err)
	}

	// Provision agent-b with --branch "agent-a" → should JOIN, not fail.
	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-b", AgentID: "agent-b",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc, Branch: "agent-a"},
	}, opts, map[string]string{}, srv.runtime.Name())
	if err != nil {
		t.Fatalf("tryProvisionWorktree returned an error: %v", err)
	}

	if !ok {
		t.Fatal("expected JOIN to succeed, got ok=false (fell back to clone-per-agent)")
	}

	// opts.Workspace must point to agent-a's worktree (the shared path).
	if opts.Workspace != agentAWt {
		t.Errorf("opts.Workspace = %q, want %q (agent-a's worktree)", opts.Workspace, agentAWt)
	}

	// No separate worktree created for agent-b.
	agentBWt := provision.WorktreePath(base, "agent-b")
	if _, err := os.Stat(agentBWt); !os.IsNotExist(err) {
		t.Errorf("agent-b should NOT have its own worktree, stat err=%v", err)
	}

	// Both agents registered as sharers.
	sharers, wtPath, err := provision.ListSharers(base, "agent-a")
	if err != nil {
		t.Fatalf("ListSharers: %v", err)
	}
	if wtPath != agentAWt {
		t.Errorf("registry worktreePath = %q, want %q", wtPath, agentAWt)
	}
	if len(sharers) != 2 {
		t.Fatalf("expected 2 sharers, got %d: %v", len(sharers), sharers)
	}

	// Shared base and agent-a worktree are intact.
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		t.Errorf("shared base .git was destroyed: %v", err)
	}
	if _, err := os.Stat(agentAWt); err != nil {
		t.Errorf("agent-a worktree was destroyed: %v", err)
	}
}

// TestTryProvisionWorktree_JoinTargetPreExisting_FailsInsteadOfFallback
// covers a JOIN agent whose own WorktreePath(base, "agent-b") never exists
// (it shares agent-a's worktree instead): the preExisted check also
// consults the sharer registry, so it still recognizes that a real, live
// worktree it is about to attach to already exists. A provisioning failure
// on the JOIN fails the start — no removal (there is nothing at agent-b's
// own path to remove anyway), and no silent fallback to an in-container
// clone, which would abandon agent-a's live worktree without ever mounting
// it for agent-b.
func TestTryProvisionWorktree_JoinTargetPreExisting_FailsInsteadOfFallback(t *testing.T) {
	requireWorktreeGit(t)
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the read-only sharer dir fault injection below never fails")
	}
	t.Setenv("SCION_HOST_UID", "")

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Set up the shared base + agent-a's worktree on branch "agent-a", as
	// the JOIN target.
	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: "agent-a",
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a", AgentName: "agent-a", GitClone: gc,
	}); err != nil {
		t.Fatalf("setup agent-a: %v", err)
	}
	base := resolved.HostPath
	agentAWt := provision.WorktreePath(base, "agent-a")
	if _, err := os.Stat(agentAWt); err != nil {
		t.Fatalf("agent-a worktree missing after setup: %v", err)
	}

	// Make the sharer registry directory read-only: ListSharers (a read of
	// an existing, valid marker) still succeeds and reports agent-a's
	// worktree, but RegisterSharer's write to add agent-b as a sharer fails
	// — a failure that happens only because a real JOIN target exists.
	sharerDir := filepath.Join(base, ".git", "scion-sharers")
	if err := os.Chmod(sharerDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sharerDir, 0o755) })

	// Simulate un-pushed work in agent-a's live worktree.
	const unpushedContent = "package main // un-pushed change\n"
	unpushedFile := filepath.Join(agentAWt, "unpushed.go")
	if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Agent-b attempts to JOIN branch "agent-a": must fail outright.
	opts := &api.StartOptions{}
	ok, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-b", AgentID: "agent-b",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc, Branch: "agent-a"},
	}, opts, map[string]string{}, srv.runtime.Name())

	if err == nil {
		t.Fatalf("expected tryProvisionWorktree to fail when the JOIN target's registration write fails, got ok=%v", ok)
	}
	if ok {
		t.Error("expected ok=false alongside the error")
	}

	// Agent-a's worktree and its un-pushed file must survive untouched.
	if _, statErr := os.Stat(agentAWt); statErr != nil {
		t.Errorf("agent-a's worktree must survive a failed JOIN, stat error: %v", statErr)
	}
	got, readErr := os.ReadFile(unpushedFile)
	if readErr != nil {
		t.Fatalf("un-pushed file must survive a failed JOIN, but reading it failed: %v", readErr)
	}
	if string(got) != unpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
	}
}

// TestBuildStartContext_WorktreePerAgentOnStart_TakesWorktreePathAndReusesIt
// proves a start dispatch (Operation opHTTPStart) with WorkspaceMode
// worktree-per-agent and GitClone set takes create's worktree-per-agent
// path, not the in-container clone path — and that re-running start against
// the same agent reuses the existing worktree instead of recreating it
// (GoogleCloudPlatform/scion#1931).
func TestBuildStartContext_WorktreePerAgentOnStart_TakesWorktreePathAndReusesIt(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	in := startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	}

	sc1, err := srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("first buildStartContext failed: %v", err)
	}
	if sc1.Opts.GitClone != nil {
		t.Errorf("expected GitClone to be suppressed by the worktree path, got %+v", sc1.Opts.GitClone)
	}
	firstWorkspace := sc1.Opts.Workspace
	if firstWorkspace == "" {
		t.Fatal("expected a worktree Workspace path to be set")
	}
	if _, err := os.Stat(firstWorkspace); err != nil {
		t.Fatalf("expected worktree to exist on disk: %v", err)
	}

	// Simulate un-pushed work in the worktree between the two starts.
	const unpushedContent = "package main // un-pushed change\n"
	unpushedFile := filepath.Join(firstWorkspace, "unpushed.go")
	if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Re-run start for the same agent: must reuse the existing worktree, not
	// recreate it or fall back to the in-container clone path.
	sc2, err := srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("second buildStartContext (reuse) failed: %v", err)
	}
	if sc2.Opts.GitClone != nil {
		t.Errorf("expected GitClone to remain suppressed on reuse, got %+v", sc2.Opts.GitClone)
	}
	if sc2.Opts.Workspace != firstWorkspace {
		t.Errorf("expected start to reuse the same worktree path %q, got %q", firstWorkspace, sc2.Opts.Workspace)
	}

	// Reuse must not run any destructive git operation (e.g. git clean -fdx)
	// against the existing worktree: the un-pushed file must still be there.
	got, err := os.ReadFile(unpushedFile)
	if err != nil {
		t.Fatalf("un-pushed file must survive a worktree-reuse start, but reading it failed: %v", err)
	}
	if string(got) != unpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
	}
}

// TestBuildStartContext_WorktreePerAgentEnvParity extends
// TestStartAndCreate_GitWorkspaceEnvParity (clone-per-agent) to
// worktree-per-agent: create and start, each provisioning a fresh agent's
// own worktree for the first time, must produce identical
// SCION_WORKSPACE_MODE/SCION_WORKSPACE_GIT env, and both must omit
// SCION_GIT_CLONE_URL since the worktree path suppresses the in-container
// clone on both operations.
func TestBuildStartContext_WorktreePerAgentEnvParity(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	createProjectPath := filepath.Join(t.TempDir(), "proj-create")
	if err := os.MkdirAll(createProjectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	createSC, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:          "agent-create",
		AgentID:       "agent-create",
		ProjectID:     "p1",
		ProjectSlug:   "proj-create",
		ProjectPath:   createProjectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opCreate,
	})
	if err != nil {
		t.Fatalf("create buildStartContext failed: %v", err)
	}

	startProjectPath := filepath.Join(t.TempDir(), "proj-start")
	if err := os.MkdirAll(startProjectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	startSC, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:          "agent-start",
		AgentID:       "agent-start",
		ProjectID:     "p2",
		ProjectSlug:   "proj-start",
		ProjectPath:   startProjectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	})
	if err != nil {
		t.Fatalf("start buildStartContext failed: %v", err)
	}

	for _, key := range []string{"SCION_WORKSPACE_MODE", "SCION_WORKSPACE_GIT", "SCION_GIT_CLONE_URL"} {
		createVal, createOK := createSC.Opts.Env[key]
		startVal, startOK := startSC.Opts.Env[key]
		if createOK != startOK || createVal != startVal {
			t.Errorf("%s: create=%q(present=%v) start=%q(present=%v), want identical", key, createVal, createOK, startVal, startOK)
		}
	}
	if _, ok := createSC.Opts.Env["SCION_GIT_CLONE_URL"]; ok {
		t.Errorf("expected SCION_GIT_CLONE_URL absent for create worktree-per-agent, got %q", createSC.Opts.Env["SCION_GIT_CLONE_URL"])
	}
	if createSC.Opts.GitClone != nil {
		t.Errorf("expected create's in-container GitClone to be suppressed by the worktree path, got %+v", createSC.Opts.GitClone)
	}
	if startSC.Opts.GitClone != nil {
		t.Errorf("expected start's in-container GitClone to be suppressed by the worktree path, got %+v", startSC.Opts.GitClone)
	}
}

// TestBuildStartContext_WorktreePerAgentStart_ProvisioningFailureNeverRemovesExistingWorktree
// covers GoogleCloudPlatform/scion#1931's worktree-reuse path: on a second
// start (or restart) for an agent whose worktree already exists, a
// provisioning failure must fail the request and must never touch the
// existing worktree, since it may hold un-pushed work.
func TestBuildStartContext_WorktreePerAgentStart_ProvisioningFailureNeverRemovesExistingWorktree(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	in := startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	}

	sc1, err := srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("first buildStartContext failed: %v", err)
	}
	worktreePath := sc1.Opts.Workspace
	if worktreePath == "" {
		t.Fatal("expected a worktree Workspace path to be set")
	}

	// Simulate un-pushed work in the agent's live worktree.
	const unpushedContent = "package main // un-pushed change\n"
	unpushedFile := filepath.Join(worktreePath, "unpushed.go")
	if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Corrupt the sharer marker so RegisterSharer's re-registration on the
	// next call fails: base is two levels above the per-agent worktree
	// (<base>/worktrees/<agentID>).
	base := filepath.Dir(filepath.Dir(worktreePath))
	markerPath := filepath.Join(base, ".git", "scion-sharers", "agent-a.json")
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	// Start again: provisioning must fail (corrupt marker), and the request
	// itself must fail — no removal, no fallback to an in-container clone.
	_, err = srv.buildStartContext(context.Background(), in)
	if err == nil {
		t.Fatal("expected buildStartContext to fail when provisioning the existing worktree errors")
	}

	// The client-facing error must be generic: the underlying ProvisionShared
	// error (which can embed a raw clone URL, e.g. from git's own error text)
	// is logged server-side only, never returned to the caller.
	wantErr := `worktree-per-agent: provisioning failed for the existing worktree of agent "agent-a"; the existing workspace was left untouched`
	if err.Error() != wantErr {
		t.Errorf("error = %q, want %q", err.Error(), wantErr)
	}

	// The worktree and its un-pushed file must be untouched.
	if _, statErr := os.Stat(worktreePath); statErr != nil {
		t.Errorf("expected the existing worktree to survive the provisioning failure, stat error: %v", statErr)
	}
	got, readErr := os.ReadFile(unpushedFile)
	if readErr != nil {
		t.Fatalf("un-pushed file must survive a failed re-provision, but reading it failed: %v", readErr)
	}
	if string(got) != unpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
	}
}

// TestBuildStartContext_WorktreePerAgentStart_MissingMarkersFailsInsteadOfSelfHeal
// is the fault-injection guard for GoogleCloudPlatform/scion#1931: when the
// provisioning sentinel and the shared base's .git are both missing (as
// provision.ProvisionShared's own self-heal expects for a first-time
// provision), but this agent's worktree already exists on disk with
// un-pushed work, the start must fail with a clear error instead of letting
// ProvisionShared's gitCloneWorkspace -> removeDirContents wipe the shared
// base — and everything under it, including this worktree — while still
// returning success.
func TestBuildStartContext_WorktreePerAgentStart_MissingMarkersFailsInsteadOfSelfHeal(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	in := startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	}

	sc1, err := srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("first buildStartContext failed: %v", err)
	}
	worktreePath := sc1.Opts.Workspace
	if worktreePath == "" {
		t.Fatal("expected a worktree Workspace path to be set")
	}

	const unpushedContent = "package main // un-pushed change\n"
	unpushedFile := filepath.Join(worktreePath, "unpushed.go")
	if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Remove both markers ProvisionShared checks for "already provisioned":
	// the sentinel file (in the project root, the parent of the shared
	// "workspace" dir) and the shared base's own .git.
	base := filepath.Dir(filepath.Dir(worktreePath)) // .../workspace
	sentinelPath := filepath.Join(filepath.Dir(base), provision.ProvisionSentinelFile)
	if err := os.Remove(sentinelPath); err != nil {
		t.Fatalf("failed to remove sentinel for fault injection: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(base, ".git")); err != nil {
		t.Fatalf("failed to remove base .git for fault injection: %v", err)
	}

	// Start again: must fail closed, not self-heal by wiping the base.
	_, err = srv.buildStartContext(context.Background(), in)
	if err == nil {
		t.Fatal("expected buildStartContext to fail when the sentinel and base .git are both missing")
	}

	// The worktree and its un-pushed file must survive: ProvisionShared's
	// self-heal (removeDirContents on the shared base) must never have run.
	if _, statErr := os.Stat(worktreePath); statErr != nil {
		t.Errorf("expected the existing worktree to survive, stat error: %v", statErr)
	}
	got, readErr := os.ReadFile(unpushedFile)
	if readErr != nil {
		t.Fatalf("un-pushed file must survive, but reading it failed: %v", readErr)
	}
	if string(got) != unpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
	}
}

// TestBuildStartContext_WorktreePerAgentStart_SingleMissingMarkerFailsClosed
// pins worktreeBaseIsProvisioned's contract for each marker independently:
// removing only the sentinel, or only the base's .git, must each alone
// still fail the start closed. The joint test above (removing both) does
// not distinguish which check did the work.
func TestBuildStartContext_WorktreePerAgentStart_SingleMissingMarkerFailsClosed(t *testing.T) {
	requireWorktreeGit(t)

	for _, tc := range []struct {
		name           string
		removeSentinel bool
		removeGit      bool
	}{
		{name: "sentinel only missing", removeSentinel: true},
		{name: "base .git only missing", removeGit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			bare := initBareRepoWithCommit(t)
			gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

			projectPath := filepath.Join(t.TempDir(), "proj")
			if err := os.MkdirAll(projectPath, 0o755); err != nil {
				t.Fatal(err)
			}

			in := startContextInputs{
				Name:          "agent-a",
				AgentID:       "agent-a",
				ProjectID:     "p1",
				ProjectSlug:   "proj",
				ProjectPath:   projectPath,
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				Config:        &CreateAgentConfig{GitClone: gc},
				Operation:     opHTTPStart,
			}

			sc1, err := srv.buildStartContext(context.Background(), in)
			if err != nil {
				t.Fatalf("first buildStartContext failed: %v", err)
			}
			worktreePath := sc1.Opts.Workspace
			if worktreePath == "" {
				t.Fatal("expected a worktree Workspace path to be set")
			}

			const unpushedContent = "package main // un-pushed change\n"
			unpushedFile := filepath.Join(worktreePath, "unpushed.go")
			if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0644); err != nil {
				t.Fatal(err)
			}

			base := filepath.Dir(filepath.Dir(worktreePath)) // .../workspace
			if tc.removeSentinel {
				sentinelPath := filepath.Join(filepath.Dir(base), provision.ProvisionSentinelFile)
				if err := os.Remove(sentinelPath); err != nil {
					t.Fatalf("failed to remove sentinel for fault injection: %v", err)
				}
			}
			if tc.removeGit {
				if err := os.RemoveAll(filepath.Join(base, ".git")); err != nil {
					t.Fatalf("failed to remove base .git for fault injection: %v", err)
				}
			}

			_, err = srv.buildStartContext(context.Background(), in)
			if err == nil {
				t.Fatalf("expected buildStartContext to fail when %s", tc.name)
			}

			if _, statErr := os.Stat(worktreePath); statErr != nil {
				t.Errorf("expected the existing worktree to survive, stat error: %v", statErr)
			}
			got, readErr := os.ReadFile(unpushedFile)
			if readErr != nil {
				t.Fatalf("un-pushed file must survive, but reading it failed: %v", readErr)
			}
			if string(got) != unpushedContent {
				t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
			}
		})
	}
}

// TestBuildStartContext_WorktreePerAgentCreate_ProvisioningFailureCleansUpPartial
// proves create's own partial-worktree cleanup on a provisioning failure is
// unchanged: when the agent's worktree does not exist yet (a fresh create),
// a failure still removes only what this call created.
func TestBuildStartContext_WorktreePerAgentCreate_ProvisioningFailureCleansUpPartial(t *testing.T) {
	requireWorktreeGit(t)
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the read-only sharer dir fault injection below never fails")
	}

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Force RegisterSharer to fail on this agent's very first provision, by
	// pre-creating a plain file where the scion-sharers directory must go.
	// The shared base clone doesn't exist yet, so create it via the resolve
	// path first: run the same resolution buildStartContext uses.
	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: "agent-a",
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	base := resolved.HostPath
	if err := os.MkdirAll(filepath.Dir(filepath.Join(base, ".git")), 0o755); err != nil {
		t.Fatal(err)
	}
	// Clone the shared base ourselves so we control it before provisioning.
	cloneCmd := exec.Command("git", "clone", bare, base)
	if out, cloneErr := cloneCmd.CombinedOutput(); cloneErr != nil {
		t.Fatalf("git clone: %v: %s", cloneErr, out)
	}
	// Pre-create the marker directory read-only, so ListSharers (a read of a
	// not-yet-existing file, which is not an error) still lets git worktree
	// add succeed, but the subsequent RegisterSharer's write into this same
	// directory fails — reproducing a failure that happens only after this
	// call has already created the worktree on disk.
	sharerDir := filepath.Join(base, ".git", "scion-sharers")
	if err := os.MkdirAll(sharerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sharerDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sharerDir, 0o755) })

	in := startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opCreate,
	}

	sc, err := srv.buildStartContext(context.Background(), in)
	// The worktree provisioning failure falls back to clone-per-agent (a
	// fresh agent has no existing worktree to protect), so the overall
	// request still succeeds — it is not the same failure mode as the
	// existing-worktree case above.
	if err != nil {
		t.Fatalf("buildStartContext should fall back to clone-per-agent, not fail outright: %v", err)
	}
	if sc.Opts.GitClone == nil {
		t.Error("expected fallback to in-container clone mode (GitClone set) when worktree provisioning fails on a fresh create")
	}

	// This call's own partial worktree must have been cleaned up.
	worktreePath := filepath.Join(base, "worktrees", "agent-a")
	if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
		t.Errorf("expected the partial worktree created by this call to be cleaned up, stat error: %v", statErr)
	}
}

// TestWorktreeWorkspace_RepoRootDerivesToBase validates that the container
// dual-mount inputs resolve correctly for the worktree layout WITHOUT any
// explicit opts.RepoRoot (api.StartOptions has no such field). pkg/agent/run.go
// derives repoRoot from the workspace itself: IsGitRepoDir(worktree) is true
// (git rev-parse --is-inside-work-tree works through the worktree .git pointer
// file), and GetCommonGitDir(worktree) returns the SHARED base .git, so
// repoRoot = filepath.Dir(commonDir) == the base checkout. The worktree then
// sits at <base>/worktrees/<id>, giving a non-".." relative path that triggers
// common.go's .git + worktree dual-mount. (Regression guard for the #350 review
// claim that opts.RepoRoot must be set explicitly.)
func TestWorktreeWorkspace_RepoRootDerivesToBase(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: "agent-a",
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a", AgentName: "agent-a", GitClone: gc,
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	base := resolved.HostPath // <projectPath>/workspace — the shared base checkout
	worktree := provision.WorktreePath(base, "agent-a")

	// Replicate pkg/agent/run.go's repoRoot derivation from the workspace.
	if !util.IsGitRepoDir(worktree) {
		t.Fatal("IsGitRepoDir(worktree) = false; run.go would not derive repoRoot from the worktree")
	}
	commonDir, err := util.GetCommonGitDir(worktree)
	if err != nil {
		t.Fatalf("GetCommonGitDir(worktree): %v", err)
	}
	repoRoot := filepath.Dir(commonDir)
	if repoRoot != base {
		t.Errorf("derived repoRoot = %q, want base %q", repoRoot, base)
	}

	// The dual-mount in common.go only fires when rel(repoRoot, workspace) is a
	// non-".." subpath — confirm the worktree is nested inside the base.
	rel, err := filepath.Rel(repoRoot, worktree)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if rel != filepath.Join("worktrees", "agent-a") {
		t.Errorf("rel(repoRoot, worktree) = %q, want %q", rel, filepath.Join("worktrees", "agent-a"))
	}
	if strings.HasPrefix(rel, "..") {
		t.Errorf("rel %q starts with .. — common.go dual-mount would NOT fire", rel)
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

func intPtr(i int) *int { return &i }

// worktreeBaseIsProvisioned accepts the sentinel in the input's own sentinel
// directory (the base's parent by default), in the project's provisioning
// state directory, or in the base itself (#2670), and still requires the
// base's .git.
func TestWorktreeBaseIsProvisioned_SentinelLocations(t *testing.T) {
	for name, dirOf := range map[string]func(base string) string{
		"parent":    filepath.Dir,
		"state dir": provision.ProjectStateDir,
		"base":      func(base string) string { return base },
	} {
		t.Run(name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), "workspace")
			if err := os.MkdirAll(filepath.Join(base, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			in := provision.ProvisionInput{Resolved: provision.ResolvedWorkspace{HostPath: base}}
			if worktreeBaseIsProvisioned(in) {
				t.Fatal("provisioned without a sentinel")
			}
			dir := dirOf(base)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, provision.ProvisionSentinelFile), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if !worktreeBaseIsProvisioned(in) {
				t.Errorf("sentinel in %s not accepted", dir)
			}
			if err := os.RemoveAll(filepath.Join(base, ".git")); err != nil {
				t.Fatal(err)
			}
			if worktreeBaseIsProvisioned(in) {
				t.Error("provisioned without the base's .git")
			}
		})
	}
}
