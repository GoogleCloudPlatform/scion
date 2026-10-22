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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
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
	return New(cfg, mgr, rt, config.RuntimeBuildRecord{})
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
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
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
	srv.resolveAuxiliaryRuntime = withoutRecord(func(projectPath, agentName, profileFlag string) runtime.Runtime {
		return &runtime.MockRuntime{NameFunc: func() string { return otherRuntimeName }}
	})
	return srv, dotScion
}

// writeSavedAgentProfile writes an agent-info.json recording profile as the
// agent's saved profile, the on-disk source agent.GetSavedProfile reads from
// on start/restart dispatch (see pkg/agent/provision.go). dotScionDir is the
// project's .scion directory (as returned alongside projectPath from the
// test's own project setup), matching what config.GetAgentHomePath expects.
// The record names a harness-config, as a provisioned agent's does: GetAgent
// adopts an agent dir without scion-agent.json only from such a record and
// refuses one without it (ptone/scion#4438).
func writeSavedAgentProfile(t *testing.T, dotScionDir, agentName, profile string) {
	t.Helper()
	agentHome := config.GetAgentHomePath(dotScionDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	info := api.AgentInfo{Name: agentName, Profile: profile, HarnessConfig: "default"}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), data, 0o644); err != nil {
		t.Fatal(err)
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
	return New(cfg, mgr, rt, config.RuntimeBuildRecord{})
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

func intPtr(i int) *int { return &i }
