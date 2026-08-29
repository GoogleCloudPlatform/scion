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

package runtime

import (
	"context"
	"embed"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// -----------------------------------------------------------------------
// Basic identity tests (from P1)
// -----------------------------------------------------------------------

func TestCloudRunSandboxRuntime_Name(t *testing.T) {
	rt := NewCloudRunSandboxRuntime(nil)
	if rt.Name() != "cloudrun-sandbox" {
		t.Errorf("Name() = %q, want %q", rt.Name(), "cloudrun-sandbox")
	}
}

func TestCloudRunSandboxRuntime_ExecUser(t *testing.T) {
	rt := NewCloudRunSandboxRuntime(nil)
	if rt.ExecUser() != "scion" {
		t.Errorf("ExecUser() = %q, want %q", rt.ExecUser(), "scion")
	}
}

// -----------------------------------------------------------------------
// Stubs that remain not-yet-implemented (P4 scope)
// -----------------------------------------------------------------------

func TestCloudRunSandboxRuntime_P4Methods(t *testing.T) {
	rt := NewCloudRunSandboxRuntime(nil)
	ctx := context.Background()

	// P4 methods are implemented but require the sandbox binary to run.
	// In unit tests without the sandbox binary, they return an exec error
	// (not "not yet implemented"). Verify they attempt to exec the right
	// command rather than returning a stub error.

	t.Run("Attach_NotFoundInState", func(t *testing.T) {
		err := rt.Attach(ctx, "nonexistent-sandbox")
		if err == nil {
			t.Errorf("Attach() expected error for nonexistent sandbox, got nil")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("Attach() error = %v, want 'not found' error", err)
		}
	})

	t.Run("GetLogs_NoSandboxBinary", func(t *testing.T) {
		_, err := rt.GetLogs(ctx, "x")
		// Without the sandbox binary, this fails with an exec error.
		if err == nil {
			t.Errorf("GetLogs() expected error without sandbox binary, got nil")
		}
		// Should NOT contain "not yet implemented" — the stub is gone.
		if strings.Contains(err.Error(), "not yet implemented") {
			t.Errorf("GetLogs() still returns stub error: %v", err)
		}
	})

	t.Run("Exec_NoSandboxBinary", func(t *testing.T) {
		_, err := rt.Exec(ctx, "x", []string{"/bin/ls"})
		if err == nil {
			t.Errorf("Exec() expected error without sandbox binary, got nil")
		}
		if strings.Contains(err.Error(), "not yet implemented") {
			t.Errorf("Exec() still returns stub error: %v", err)
		}
	})
}

// -----------------------------------------------------------------------
// Image methods (no-op, from P1)
// -----------------------------------------------------------------------

func TestCloudRunSandboxRuntime_ImageMethodsNoOp(t *testing.T) {
	rt := NewCloudRunSandboxRuntime(nil)
	ctx := context.Background()

	t.Run("ImageExists", func(t *testing.T) {
		exists, err := rt.ImageExists(ctx, "any-image")
		if err != nil {
			t.Errorf("ImageExists() error = %v, want nil", err)
		}
		if !exists {
			t.Errorf("ImageExists() = %v, want true (omni-image)", exists)
		}
	})

	t.Run("ImageID", func(t *testing.T) {
		id, err := rt.ImageID(ctx, "any-image")
		if err != nil {
			t.Errorf("ImageID() error = %v, want nil", err)
		}
		if id != "omni-image" {
			t.Errorf("ImageID() = %q, want %q", id, "omni-image")
		}
	})

	t.Run("RemoveImage", func(t *testing.T) {
		err := rt.RemoveImage(ctx, "any-image")
		if err != nil {
			t.Errorf("RemoveImage() error = %v, want nil (no-op)", err)
		}
	})

	t.Run("PullImage", func(t *testing.T) {
		err := rt.PullImage(ctx, "any-image")
		if err != nil {
			t.Errorf("PullImage() error = %v, want nil (no-op)", err)
		}
	})
}

// -----------------------------------------------------------------------
// Sync (no-op)
// -----------------------------------------------------------------------

func TestCloudRunSandboxRuntime_Sync_NoOp(t *testing.T) {
	rt := NewCloudRunSandboxRuntime(nil)
	ctx := context.Background()

	err := rt.Sync(ctx, "test-sandbox", SyncTo)
	if err != nil {
		t.Errorf("Sync() error = %v, want nil (no-op, filesystem shared via bind mounts)", err)
	}
}

// -----------------------------------------------------------------------
// SandboxLauncherAvailable
// -----------------------------------------------------------------------

func TestSandboxLauncherAvailable(t *testing.T) {
	// The default sandbox binary path should not exist in the test environment
	if SandboxLauncherAvailable() {
		t.Skip("sandbox binary found at default path; cannot test absence")
	}

	t.Run("absent", func(t *testing.T) {
		if SandboxLauncherAvailable() {
			t.Error("SandboxLauncherAvailable() = true, want false (no binary at default path)")
		}
	})
}

// -----------------------------------------------------------------------
// Factory integration
// -----------------------------------------------------------------------

func TestGetRuntime_CloudRunSandbox_DirectProfileName(t *testing.T) {
	t.Setenv("PATH", "")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_GROVE", "")

	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	tmpWd := t.TempDir()
	if err := os.Chdir(tmpWd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	r := GetRuntime("", "cloudrun-sandbox")
	if _, ok := r.(*CloudRunSandboxRuntime); !ok {
		t.Fatalf("expected *CloudRunSandboxRuntime from direct profile name, got %T", r)
	}
}

func TestGetRuntime_CloudRunSandbox_SettingsBased(t *testing.T) {
	t.Setenv("PATH", "")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	settings := `{
		"schema_version": "1",
		"active_profile": "sandbox",
		"runtimes": {
			"crs": {
				"type": "cloudrun-sandbox"
			}
		},
		"profiles": {
			"sandbox": {
				"runtime": "crs"
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(globalDir, "settings.json"), []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	tmpWd := t.TempDir()
	if err := os.Chdir(tmpWd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	r := GetRuntime("", "")
	if _, ok := r.(*CloudRunSandboxRuntime); !ok {
		t.Fatalf("expected *CloudRunSandboxRuntime from settings, got %T", r)
	}
}

func TestGetRuntime_CloudRunInstance_Precedence_Over_Docker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Cloud Run Instance detection only applies on Linux")
	}

	t.Setenv("PATH", "")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_GROVE", "")
	t.Setenv("CLOUD_RUN_INSTANCE", "instance-1")
	t.Setenv("K_SERVICE", "")

	oldWd, _ := os.Getwd()
	tmpWd := t.TempDir()
	if err := os.Chdir(tmpWd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	// CLOUD_RUN_INSTANCE is set but the sandbox binary is not at the default
	// path, so the factory should pick cloudrun-instances (CloudRunRuntime).
	r := GetRuntime("", "")
	if _, ok := r.(*CloudRunRuntime); !ok {
		t.Errorf("expected *CloudRunRuntime (cloudrun-instances) when CLOUD_RUN_INSTANCE is set without sandbox binary, got %T", r)
	}
}

// -----------------------------------------------------------------------
// Constructor with config
// -----------------------------------------------------------------------

func TestNewCloudRunSandboxRuntime_WithConfig(t *testing.T) {
	cfg := &config.V1CloudRunSandboxConfig{
		SandboxBin: "/usr/bin/test-sandbox",
	}
	rt := NewCloudRunSandboxRuntime(cfg)
	if rt.bin != "/usr/bin/test-sandbox" {
		t.Errorf("bin = %q, want %q", rt.bin, "/usr/bin/test-sandbox")
	}
}

func TestNewCloudRunSandboxRuntime_NilConfig(t *testing.T) {
	rt := NewCloudRunSandboxRuntime(nil)
	if rt.bin != DefaultSandboxBin {
		t.Errorf("bin = %q, want default %q", rt.bin, DefaultSandboxBin)
	}
}

// -----------------------------------------------------------------------
// State store tests
// -----------------------------------------------------------------------

func TestSandboxStateStore_AddGetRemove(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	ss := newSandboxStateStore(stateFile)

	entry := &sandboxStateEntry{
		SandboxName: "test-sandbox",
		AgentID:     "test-agent",
		Project:     "test-project",
		ProjectID:   "proj-123",
		CreatedAt:   time.Now(),
		AgentHome:   "/scion/agents/test-agent/home",
		Workspace:   "/scion/agents/test-agent/workspace",
	}

	// Add
	ss.add(entry)
	got := ss.get("test-sandbox")
	if got == nil {
		t.Fatal("get() returned nil after add()")
	}
	if got.AgentID != "test-agent" {
		t.Errorf("AgentID = %q, want %q", got.AgentID, "test-agent")
	}

	// List
	all := ss.list()
	if len(all) != 1 {
		t.Fatalf("list() returned %d entries, want 1", len(all))
	}

	// Remove
	ss.remove("test-sandbox")
	got = ss.get("test-sandbox")
	if got != nil {
		t.Errorf("get() returned non-nil after remove()")
	}
}

func TestSandboxStateStore_MarkStopped(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	ss := newSandboxStateStore(stateFile)

	entry := &sandboxStateEntry{
		SandboxName: "sb1",
		AgentID:     "agent1",
		CreatedAt:   time.Now(),
	}
	ss.add(entry)

	now := time.Now()
	code := 137
	ss.markStopped("sb1", &code, &now)

	got := ss.get("sb1")
	if got == nil {
		t.Fatal("get() returned nil")
	}
	if !got.Stopped {
		t.Error("Stopped = false, want true")
	}
	if got.ExitCode == nil || *got.ExitCode != 137 {
		t.Errorf("ExitCode = %v, want 137", got.ExitCode)
	}
	if got.StoppedAt == nil {
		t.Error("StoppedAt = nil, want non-nil")
	}
}

func TestSandboxStateStore_MarkStopped_NilExitCode(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	ss := newSandboxStateStore(stateFile)

	entry := &sandboxStateEntry{
		SandboxName: "sb1",
		AgentID:     "agent1",
	}
	ss.add(entry)

	now := time.Now()
	ss.markStopped("sb1", nil, &now)

	got := ss.get("sb1")
	if got == nil {
		t.Fatal("get() returned nil")
	}
	if !got.Stopped {
		t.Error("Stopped = false, want true")
	}
	if got.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil (C5: nil means unknown)", got.ExitCode)
	}
}

func TestSandboxStateStore_Persistence(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")

	// Write with one store instance.
	ss1 := newSandboxStateStore(stateFile)
	ss1.add(&sandboxStateEntry{
		SandboxName: "persist-test",
		AgentID:     "agent-persist",
		Project:     "proj",
	})

	// Read with a new store instance.
	ss2 := newSandboxStateStore(stateFile)
	got := ss2.get("persist-test")
	if got == nil {
		t.Fatal("persisted entry not found after reload")
	}
	if got.AgentID != "agent-persist" {
		t.Errorf("AgentID = %q, want %q", got.AgentID, "agent-persist")
	}
}

func TestSandboxStateStore_List_Snapshot(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	ss := newSandboxStateStore(stateFile)

	ss.add(&sandboxStateEntry{SandboxName: "a", AgentID: "agent-a"})
	ss.add(&sandboxStateEntry{SandboxName: "b", AgentID: "agent-b"})

	list := ss.list()
	if len(list) != 2 {
		t.Fatalf("list() returned %d entries, want 2", len(list))
	}

	// Mutating the returned entries should not affect the store.
	list[0].AgentID = "mutated"
	original := ss.get("a")
	if original.AgentID == "mutated" {
		t.Error("list() returned live references instead of copies")
	}
}

// -----------------------------------------------------------------------
// sanitizeSandboxName tests
// -----------------------------------------------------------------------

func TestSanitizeSandboxName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my-agent", "my-agent"},
		{"My_Agent.1", "my-agent-1"},
		{"UPPERCASE", "uppercase"},
		{"a/b/c", "a-b-c"},
		{"---leading---", "leading"},
		{"", "sandbox"},
		{strings.Repeat("a", 100), strings.Repeat("a", 63)},
		{"hello world!", "hello-world"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeSandboxName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeSandboxName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSanitizeSandboxName_TrailingHyphenAfterTruncation(t *testing.T) {
	// 63rd character is a hyphen after sanitization — trim must happen AFTER truncation.
	input := strings.Repeat("a", 62) + "!" // '!' -> '-', producing 63 chars ending in '-'
	got := sanitizeSandboxName(input)
	if strings.HasSuffix(got, "-") {
		t.Errorf("sanitizeSandboxName(%q) = %q, ends with trailing hyphen", input, got)
	}
	want := strings.Repeat("a", 62)
	if got != want {
		t.Errorf("sanitizeSandboxName(%q) = %q, want %q", input, got, want)
	}
}

// -----------------------------------------------------------------------
// mountsFor tests
// -----------------------------------------------------------------------

func TestMountsFor_BasicPaths(t *testing.T) {
	paths := scionPaths{
		root:      "/scion",
		agentHome: "/scion/agents/test-agent/home",
		workspace: "/scion/agents/test-agent/workspace",
	}

	mounts := mountsFor(paths, nil)
	// No tmux mount: AF_UNIX can't cross gVisor boundary (§4.4a).
	if len(mounts) != 2 {
		t.Fatalf("mountsFor() returned %d mounts, want 2", len(mounts))
	}

	wantMounts := []string{
		"type=bind,source=/scion/agents/test-agent/home,destination=/home/scion",
		"type=bind,source=/scion/agents/test-agent/workspace,destination=/workspace",
	}
	for i, want := range wantMounts {
		if mounts[i] != want {
			t.Errorf("mounts[%d] = %q, want %q", i, mounts[i], want)
		}
	}
}

// TestMountsFor_WorkspaceMountedAtStandardPath verifies the workspace is
// mounted at /workspace inside the sandbox (not at the host-side
// /scion/agents/<slug>/workspace path). This is the fix for #43: without
// this remapping, sciontool init tried to git-clone into /workspace on the
// read-only rootfs, killing every git-linked agent on arrival.
func TestMountsFor_WorkspaceMountedAtStandardPath(t *testing.T) {
	paths := scionPaths{
		root:      "/scion",
		agentHome: "/scion/agents/slug/home",
		workspace: "/scion/agents/slug/workspace",
	}
	mounts := mountsFor(paths, nil)

	// Find the workspace mount.
	var wsMount string
	for _, m := range mounts {
		if strings.Contains(m, "workspace") {
			wsMount = m
			break
		}
	}
	if wsMount == "" {
		t.Fatal("no workspace mount found")
	}

	// Destination must be /workspace, NOT the host-side path.
	wantDest := "destination=/workspace"
	if !strings.Contains(wsMount, wantDest) {
		t.Errorf("workspace mount destination wrong:\n  got:  %s\n  want: ...%s", wsMount, wantDest)
	}

	// Source must be the host-side path.
	wantSrc := "source=/scion/agents/slug/workspace"
	if !strings.Contains(wsMount, wantSrc) {
		t.Errorf("workspace mount source wrong:\n  got:  %s\n  want: ...%s", wsMount, wantSrc)
	}
}

func TestMountsFor_WithSharedDirs(t *testing.T) {
	paths := scionPaths{
		root:      "/scion",
		agentHome: "/scion/agents/test-agent/home",
		workspace: "/scion/agents/test-agent/workspace",
	}
	sharedDirs := []api.SharedDir{
		{Name: "build-cache"},
		{Name: "artifacts"},
	}

	mounts := mountsFor(paths, sharedDirs)
	// 2 agent mounts (home, workspace) + 2 shared dirs = 4. No tmux mount (§4.4a).
	if len(mounts) != 4 {
		t.Fatalf("mountsFor() returned %d mounts, want 4 (2 agent + 2 shared)", len(mounts))
	}

	// Shared dirs should be at indices 2 and 3.
	if !strings.Contains(mounts[2], "build-cache") {
		t.Errorf("mounts[2] = %q, want shared dir build-cache", mounts[2])
	}
	if !strings.Contains(mounts[3], "artifacts") {
		t.Errorf("mounts[3] = %q, want shared dir artifacts", mounts[3])
	}
}

// -----------------------------------------------------------------------
// envFor tests
// -----------------------------------------------------------------------

func TestEnvFor_BasicEnv(t *testing.T) {
	cfg := RunConfig{
		Env:       []string{"KEY1=VAL1", "KEY2=VAL2"},
		Project:   "my-project",
		ProjectID: "proj-123",
	}
	paths := scionPaths{}

	env := envFor(cfg, paths)

	// Check broker-provided env.
	if env["KEY1"] != "VAL1" {
		t.Errorf("KEY1 = %q, want %q", env["KEY1"], "VAL1")
	}
	if env["KEY2"] != "VAL2" {
		t.Errorf("KEY2 = %q, want %q", env["KEY2"], "VAL2")
	}

	// TMUX_TMPDIR should NOT be set (§4.4a: AF_UNIX can't cross gVisor).
	if _, ok := env["TMUX_TMPDIR"]; ok {
		t.Errorf("TMUX_TMPDIR should not be set (§4.4a), got %q", env["TMUX_TMPDIR"])
	}

	// Check project identity.
	if env["SCION_PROJECT"] != "my-project" {
		t.Errorf("SCION_PROJECT = %q, want %q", env["SCION_PROJECT"], "my-project")
	}
	if env["SCION_PROJECT_ID"] != "proj-123" {
		t.Errorf("SCION_PROJECT_ID = %q, want %q", env["SCION_PROJECT_ID"], "proj-123")
	}
	if env["SCION_GROVE"] != "my-project" {
		t.Errorf("SCION_GROVE = %q, want %q", env["SCION_GROVE"], "my-project")
	}
	if env["SCION_GROVE_ID"] != "proj-123" {
		t.Errorf("SCION_GROVE_ID = %q, want %q", env["SCION_GROVE_ID"], "proj-123")
	}

	// Check UID/GID are set.
	if env["SCION_HOST_UID"] == "" {
		t.Error("SCION_HOST_UID not set")
	}
	if env["SCION_HOST_GID"] == "" {
		t.Error("SCION_HOST_GID not set")
	}
}

func TestEnvFor_PATH(t *testing.T) {
	cfg := RunConfig{}
	paths := scionPaths{}

	env := envFor(cfg, paths)
	if env["PATH"] == "" {
		t.Error("PATH not set; sandbox has no PATH by default (AC-0 retest finding)")
	}
	if !strings.Contains(env["PATH"], "/usr/bin") {
		t.Errorf("PATH = %q, expected to contain /usr/bin", env["PATH"])
	}
}

func TestEnvFor_PATH_OverridableByHarness(t *testing.T) {
	cfg := RunConfig{
		Harness: &mockHarness{
			env: map[string]string{"PATH": "/custom/bin"},
		},
	}
	paths := scionPaths{}

	env := envFor(cfg, paths)
	if env["PATH"] != "/custom/bin" {
		t.Errorf("PATH = %q, want harness override %q", env["PATH"], "/custom/bin")
	}
}

func TestEnvFor_WorkspacePath(t *testing.T) {
	cfg := RunConfig{}
	paths := scionPaths{}

	env := envFor(cfg, paths)

	// SCION_WORKSPACE_PATH must be set to /workspace so sciontool init
	// resolves the writable bind mount (not the read-only rootfs). This
	// is the sandbox equivalent of Docker's --workdir /workspace (#43).
	if env["SCION_WORKSPACE_PATH"] != sandboxWorkspace {
		t.Errorf("SCION_WORKSPACE_PATH = %q, want %q", env["SCION_WORKSPACE_PATH"], sandboxWorkspace)
	}
}

func TestEnvFor_WorkspaceBackend(t *testing.T) {
	cfg := RunConfig{
		WorkspaceBackendName: "nfs",
	}
	paths := scionPaths{}

	env := envFor(cfg, paths)
	if env["SCION_WORKSPACE_BACKEND"] != "nfs" {
		t.Errorf("SCION_WORKSPACE_BACKEND = %q, want %q", env["SCION_WORKSPACE_BACKEND"], "nfs")
	}
}

func TestEnvArgs_Sorted(t *testing.T) {
	env := map[string]string{
		"C_KEY": "c",
		"A_KEY": "a",
		"B_KEY": "b",
	}

	args := envArgs(env)
	if len(args) != 3 {
		t.Fatalf("envArgs() returned %d entries, want 3", len(args))
	}
	if args[0] != "A_KEY=a" {
		t.Errorf("args[0] = %q, want %q", args[0], "A_KEY=a")
	}
	if args[1] != "B_KEY=b" {
		t.Errorf("args[1] = %q, want %q", args[1], "B_KEY=b")
	}
	if args[2] != "C_KEY=c" {
		t.Errorf("args[2] = %q, want %q", args[2], "C_KEY=c")
	}
}

// -----------------------------------------------------------------------
// P3a behaviour-identity test (#127)
//
// P3a adds env classifications alongside the env map but changes nothing
// the sandbox sees. This test pins that claim: given a canonical set of
// broker-provided env vars (every key from the env-writer inventory),
// envFor() + envArgs() produce the exact same sorted argv as before P3a.
//
// envFor() only reads cfg.Env ([]string) and adds its own fixed keys
// (PATH, HOME, etc.). The classification map lives on a separate struct
// that envFor() never sees. This test ensures no accidental cross-wiring
// was introduced.
// -----------------------------------------------------------------------

// TestP3a_EnvForBehaviourIdentity verifies that envFor() + envArgs()
// produce byte-identical output for a canonical broker env, proving P3a
// changes nothing the sandbox sees.
func TestP3a_EnvForBehaviourIdentity(t *testing.T) {
	// Canonical env: every classified key from the env-writer inventory.
	// These are the KEY=VALUE strings the broker passes via RunConfig.Env.
	// The test uses sentinels for secrets, real-looking values for plain.
	brokerEnv := []string{
		// Hub-side plain (H1-H4, H9, H11-H13, H15-audience/expiry/mode)
		"SCION_MODEL=claude-4",
		"SCION_THINKING_LEVEL=high",
		"SCION_HUB_NAME=test-hub",
		"SCION_USER_GITHUB_TOKEN=1",
		"SCION_GITHUB_APP_ENABLED=true",
		"SCION_GITHUB_TOKEN_EXPIRY=2026-08-29T12:00:00Z",
		"SCION_GITHUB_TOKEN_PATH=/tmp/gh-token",
		"SCION_TRANSPORT_AUDIENCE=https://test.example.com",
		"SCION_TRANSPORT_TOKEN_EXPIRY=2026-08-29T12:00:00Z",
		"SCION_TRANSPORT_MODE=direct",
		// Hub-side secret-fetchable (H5-H8)
		"GEMINI_API_KEY=FAKE-KEY-SENTINEL-not-a-real-credential",
		// Hub-side secret-injected (H10, H14, H15-token)
		"GITHUB_TOKEN=FAKE-KEY-SENTINEL-not-a-real-credential",
		"SCION_DEV_TOKEN=FAKE-KEY-SENTINEL-not-a-real-credential",
		"SCION_TRANSPORT_TOKEN=FAKE-AUTH-SENTINEL-not-a-real-credential",
		// Broker-side plain (B1-B22 plain subset)
		"SCION_AGENT_ID=agent-001",
		// Note: SCION_GROVE_ID and SCION_PROJECT_ID are set by envFor() from
		// cfg.ProjectID, overwriting any broker-env value. They appear in
		// envForOwnKeys below, not here.
		//   "SCION_GROVE_ID" → cfg.ProjectID
		//   "SCION_PROJECT_ID" → cfg.ProjectID
		"SCION_AGENT_SLUG=test-agent",
		"SCION_HUB_ENDPOINT=https://hub.example.com",
		"SCION_WORKSPACE_MODE=clone-per-agent",
		"SCION_WORKSPACE_GIT=true",
		"SCION_METADATA_MODE=gce",
		"SCION_METADATA_SA_EMAIL=sa@proj.iam.gserviceaccount.com",
		"SCION_METADATA_PROJECT_ID=proj-001",
		"SCION_BROKER_ID=broker-001",
		"SCION_CREATOR=user@example.com",
		"SCION_DEBUG=1",
		// Broker-side secret-injected (B-auth, B-clone)
		"SCION_AUTH_TOKEN=FAKE-AUTH-SENTINEL-not-a-real-credential",
		"SCION_GIT_CLONE_URL=https://FAKE-KEY-SENTINEL-not-a-real-credential@github.com/org/repo.git",
	}

	cfg := RunConfig{
		Env:       brokerEnv,
		Project:   "test-project",
		ProjectID: "proj-123",
	}

	env := envFor(cfg, scionPaths{})
	argv := envArgs(env)

	// Verify every broker-provided key passes through unchanged.
	for _, entry := range brokerEnv {
		parts := strings.SplitN(entry, "=", 2)
		key, val := parts[0], parts[1]
		if got, ok := env[key]; !ok {
			t.Errorf("broker key %q missing from envFor() output", key)
		} else if got != val {
			t.Errorf("broker key %q: got %q, want %q", key, got, val)
		}
	}

	// Verify envFor()-added keys are present (these are fixed by envFor,
	// not by the broker, and must not disappear due to P3a).
	envForOwnKeys := []string{
		"PATH", "HOME", "USER", "LOGNAME",
		"SCION_PROJECT", "SCION_GROVE",
		"SCION_PROJECT_ID", "SCION_GROVE_ID",
		"SCION_HOST_UID", "SCION_HOST_GID",
		"SCION_WORKSPACE_PATH",
	}
	for _, key := range envForOwnKeys {
		if _, ok := env[key]; !ok {
			t.Errorf("envFor() own key %q missing from output", key)
		}
	}

	// Verify argv is sorted and deterministic (run twice, same result).
	argv2 := envArgs(envFor(cfg, scionPaths{}))
	if len(argv) != len(argv2) {
		t.Fatalf("envArgs() length changed: %d vs %d", len(argv), len(argv2))
	}
	for i := range argv {
		if argv[i] != argv2[i] {
			t.Errorf("envArgs()[%d] not deterministic: %q vs %q", i, argv[i], argv2[i])
		}
	}

	// Pin the total key count: broker keys + envFor-added keys.
	// If a future change adds or removes env vars, this count must be updated.
	// This catches accidental env writes from classification code.
	expectedKeyCount := len(brokerEnv) + len(envForOwnKeys)
	if len(env) != expectedKeyCount {
		// List unexpected keys for diagnosis.
		expected := make(map[string]bool)
		for _, entry := range brokerEnv {
			parts := strings.SplitN(entry, "=", 2)
			expected[parts[0]] = true
		}
		for _, k := range envForOwnKeys {
			expected[k] = true
		}
		for k := range env {
			if !expected[k] {
				t.Errorf("unexpected extra key in envFor() output: %q=%q", k, env[k])
			}
		}
		t.Errorf("envFor() produced %d keys, want %d", len(env), expectedKeyCount)
	}
}

// -----------------------------------------------------------------------
// buildEntrypoint tests
// -----------------------------------------------------------------------

type mockHarness struct {
	command []string
	env     map[string]string
}

func (h *mockHarness) Name() string { return "mock" }
func (h *mockHarness) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{}
}
func (h *mockHarness) GetCommand(task string, resume bool, baseArgs []string) []string {
	return h.command
}
func (h *mockHarness) GetEnv(agentName, agentHome, unixUsername string) map[string]string {
	return h.env
}
func (h *mockHarness) DefaultConfigDir() string                                        { return "" }
func (h *mockHarness) SkillsDir() string                                               { return "" }
func (h *mockHarness) HasSystemPrompt(agentHome string) bool                           { return false }
func (h *mockHarness) Provision(ctx context.Context, name, dir, home, ws string) error { return nil }
func (h *mockHarness) GetInterruptKey() string                                         { return "C-c" }
func (h *mockHarness) GetInterruptSequence() []string                                  { return nil }
func (h *mockHarness) GetHarnessEmbedsFS() (embed.FS, string)                          { return embed.FS{}, "" }
func (h *mockHarness) InjectAgentInstructions(agentHome string, content []byte) error  { return nil }
func (h *mockHarness) InjectSystemPrompt(agentHome string, content []byte) error       { return nil }
func (h *mockHarness) GetTelemetryEnv() map[string]string                              { return nil }
func (h *mockHarness) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error)      { return nil, nil }

func TestBuildEntrypoint_WithHarness(t *testing.T) {
	cfg := RunConfig{
		Harness: &mockHarness{
			command: []string{"claude", "--agent"},
		},
	}
	entrypoint, err := buildEntrypoint(cfg)
	if err != nil {
		t.Fatalf("buildEntrypoint() error = %v", err)
	}

	// Should start with /bin/sh -c. argv[0] MUST be an absolute path —
	// the sandbox launcher resolves it
	// before PATH is set (see envFor), so bare "sh" silently fails.
	if len(entrypoint) < 3 {
		t.Fatalf("entrypoint too short: %v", entrypoint)
	}
	if entrypoint[0] != "/bin/sh" || entrypoint[1] != "-c" {
		t.Errorf("entrypoint[0:2] = %v, want [/bin/sh -c]", entrypoint[:2])
	}
	if !strings.HasPrefix(entrypoint[0], "/") {
		t.Errorf("entrypoint argv[0] must be an absolute path, got %q", entrypoint[0])
	}

	// The command should contain sciontool init and the tmux poll loop.
	// No symlink chain: agent home is mounted at /home/scion by mountsFor().
	cmd := entrypoint[2]
	for _, pattern := range []string{
		"sciontool init",
		"tmux new-session -d -s scion -n agent",
		"new-window -t scion -n shell",
		"select-window -t scion:agent",
		"while tmux has-session -t scion",
		"claude",
		"echo $? >",
		// #22 entrypoint output capture preserved (log only; no .rc file).
		entrypointLogFile,
	} {
		if !strings.Contains(cmd, pattern) {
			t.Errorf("entrypoint command missing pattern %q\nfull: %s", pattern, cmd)
		}
	}
	// Must NOT contain the old symlink chain (rootfs is read-only) or
	// attach-session (no TTY in sandbox).
	for _, absent := range []string{
		"rm -rf /home/scion",
		"ln -sfn",
		"attach-session -t scion",
	} {
		if strings.Contains(cmd, absent) {
			t.Errorf("entrypoint command should NOT contain %q\nfull: %s", absent, cmd)
		}
	}
}

func TestBuildEntrypoint_NoHarness(t *testing.T) {
	cfg := RunConfig{}
	_, err := buildEntrypoint(cfg)
	if err == nil {
		t.Error("buildEntrypoint() with no harness should return error")
	}
}

func TestBuildEntrypoint_NoAuth(t *testing.T) {
	cfg := RunConfig{
		NoAuth:        true,
		NoAuthMessage: "Please configure auth",
	}
	entrypoint, err := buildEntrypoint(cfg)
	if err != nil {
		t.Fatalf("buildEntrypoint() error = %v", err)
	}

	// argv[0] must be an absolute path (sandbox PATH resolution).
	if !strings.HasPrefix(entrypoint[0], "/") {
		t.Errorf("entrypoint argv[0] must be an absolute path, got %q", entrypoint[0])
	}

	cmd := entrypoint[2]
	if !strings.Contains(cmd, "Please configure auth") {
		t.Error("entrypoint command should contain no-auth message")
	}
	// No symlink chain: agent home is mounted at /home/scion by mountsFor().
	if strings.Contains(cmd, "ln -sfn") {
		t.Error("entrypoint command should NOT contain symlink setup (rootfs is read-only)")
	}
}

// -----------------------------------------------------------------------
// prepareScionLayout tests
// -----------------------------------------------------------------------

func TestPrepareScionLayout_CreatesDirectories(t *testing.T) {
	rootDir := t.TempDir()
	cfg := RunConfig{
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
			{Name: "artifacts"},
		},
	}

	paths, err := prepareScionLayout(rootDir, "test-agent", cfg)
	if err != nil {
		t.Fatalf("prepareScionLayout() error = %v", err)
	}

	// Verify paths exist.
	for _, dir := range []string{paths.agentHome, paths.workspace} {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			t.Errorf("directory not created: %s", dir)
		}
	}

	// Verify shared dirs exist.
	for _, name := range []string{"build-cache", "artifacts"} {
		sdPath := filepath.Join(rootDir, "shared", name)
		if _, err := os.Stat(sdPath); os.IsNotExist(err) {
			t.Errorf("shared dir not created: %s", sdPath)
		}
	}
}

func TestPrepareScionLayout_PathStructure(t *testing.T) {
	rootDir := t.TempDir()
	paths, err := prepareScionLayout(rootDir, "my-agent", RunConfig{})
	if err != nil {
		t.Fatalf("prepareScionLayout() error = %v", err)
	}

	wantHome := filepath.Join(rootDir, "agents", "my-agent", "home")
	wantWs := filepath.Join(rootDir, "agents", "my-agent", "workspace")

	if paths.agentHome != wantHome {
		t.Errorf("agentHome = %q, want %q", paths.agentHome, wantHome)
	}
	if paths.workspace != wantWs {
		t.Errorf("workspace = %q, want %q", paths.workspace, wantWs)
	}
}

func TestPrepareScionLayout_RelocatesHomeDir(t *testing.T) {
	rootDir := t.TempDir()
	homeDir := t.TempDir()

	// Create a file in the original home dir.
	if err := os.WriteFile(filepath.Join(homeDir, "agent-info.json"), []byte(`{"test": true}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := RunConfig{HomeDir: homeDir}
	paths, err := prepareScionLayout(rootDir, "test-agent", cfg)
	if err != nil {
		t.Fatalf("prepareScionLayout() error = %v", err)
	}

	// The file should now be at the /scion path.
	data, err := os.ReadFile(filepath.Join(paths.agentHome, "agent-info.json"))
	if err != nil {
		t.Fatalf("agent-info.json not found at scion path: %v", err)
	}
	if string(data) != `{"test": true}` {
		t.Errorf("agent-info.json content = %q, want %q", string(data), `{"test": true}`)
	}

	// The original homeDir should be a symlink to the /scion path.
	link, err := os.Readlink(homeDir)
	if err != nil {
		t.Fatalf("original homeDir is not a symlink: %v", err)
	}
	if link != paths.agentHome {
		t.Errorf("symlink target = %q, want %q", link, paths.agentHome)
	}
}

// -----------------------------------------------------------------------
// List tests
// -----------------------------------------------------------------------

func TestCloudRunSandboxRuntime_List_ReturnsStateEntries(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	rt := &CloudRunSandboxRuntime{
		bin:          "/nonexistent",
		state:        newSandboxStateStore(stateFile),
		rootDir:      t.TempDir(),
		watchCancels: make(map[string]context.CancelFunc),
	}

	rt.state.add(&sandboxStateEntry{
		SandboxName: "sb-1",
		AgentID:     "agent-one",
		Project:     "test-proj",
		ProjectID:   "proj-1",
		Template:    "default",
		Image:       "omni-image",
		Labels:      map[string]string{"scion.name": "agent-one"},
	})
	rt.state.add(&sandboxStateEntry{
		SandboxName: "sb-2",
		AgentID:     "agent-two",
		Project:     "test-proj",
		ProjectID:   "proj-1",
		Image:       "omni-image",
		Stopped:     true,
	})

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("List() returned %d agents, want 2", len(agents))
	}

	// Find running agent.
	var running, stopped *api.AgentInfo
	for i := range agents {
		if agents[i].ContainerID == "sb-1" {
			running = &agents[i]
		}
		if agents[i].ContainerID == "sb-2" {
			stopped = &agents[i]
		}
	}

	if running == nil {
		t.Fatal("running agent (sb-1) not found in list")
	}
	if running.Phase != "running" || running.ContainerStatus != "running" {
		t.Errorf("running agent: phase=%q status=%q, want running/running", running.Phase, running.ContainerStatus)
	}
	if running.Runtime != "cloudrun-sandbox" {
		t.Errorf("runtime = %q, want %q", running.Runtime, "cloudrun-sandbox")
	}

	if stopped == nil {
		t.Fatal("stopped agent (sb-2) not found in list")
	}
	if stopped.Phase != "stopped" || stopped.ContainerStatus != "stopped" {
		t.Errorf("stopped agent: phase=%q status=%q, want stopped/stopped", stopped.Phase, stopped.ContainerStatus)
	}

	// §9.1b stopgap: ExitCode should not be set on AgentInfo (field doesn't exist yet).
	// When #1260 merges, this test should be updated.
}

func TestCloudRunSandboxRuntime_List_LabelFilter(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	rt := &CloudRunSandboxRuntime{
		bin:          "/nonexistent",
		state:        newSandboxStateStore(stateFile),
		rootDir:      t.TempDir(),
		watchCancels: make(map[string]context.CancelFunc),
	}

	rt.state.add(&sandboxStateEntry{
		SandboxName: "sb-1",
		AgentID:     "agent-one",
		Project:     "proj-a",
		Labels:      map[string]string{"env": "prod"},
	})
	rt.state.add(&sandboxStateEntry{
		SandboxName: "sb-2",
		AgentID:     "agent-two",
		Project:     "proj-b",
		Labels:      map[string]string{"env": "staging"},
	})

	// Filter by label.
	agents, err := rt.List(context.Background(), map[string]string{"env": "prod"})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("List() with filter returned %d agents, want 1", len(agents))
	}
	if agents[0].ContainerID != "sb-1" {
		t.Errorf("filtered agent = %q, want %q", agents[0].ContainerID, "sb-1")
	}
}

// -----------------------------------------------------------------------
// GetWorkspacePath tests
// -----------------------------------------------------------------------

func TestCloudRunSandboxRuntime_GetWorkspacePath(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	rt := &CloudRunSandboxRuntime{
		bin:          "/nonexistent",
		state:        newSandboxStateStore(stateFile),
		rootDir:      t.TempDir(),
		watchCancels: make(map[string]context.CancelFunc),
	}

	rt.state.add(&sandboxStateEntry{
		SandboxName: "sb-1",
		AgentID:     "agent-one",
		Workspace:   "/scion/agents/agent-one/workspace",
	})

	path, err := rt.GetWorkspacePath(context.Background(), "sb-1")
	if err != nil {
		t.Fatalf("GetWorkspacePath() error = %v", err)
	}
	if path != "/scion/agents/agent-one/workspace" {
		t.Errorf("GetWorkspacePath() = %q, want %q", path, "/scion/agents/agent-one/workspace")
	}
}

func TestCloudRunSandboxRuntime_GetWorkspacePath_NotFound(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	rt := &CloudRunSandboxRuntime{
		bin:          "/nonexistent",
		state:        newSandboxStateStore(stateFile),
		rootDir:      t.TempDir(),
		watchCancels: make(map[string]context.CancelFunc),
	}

	_, err := rt.GetWorkspacePath(context.Background(), "nonexistent")
	if err == nil {
		t.Error("GetWorkspacePath() for nonexistent sandbox should return error")
	}
}

// -----------------------------------------------------------------------
// Run tests (with mock binary)
// -----------------------------------------------------------------------

func TestCloudRunSandboxRuntime_Run_BuildsCommand(t *testing.T) {
	tmpDir := t.TempDir()
	rootDir := filepath.Join(tmpDir, "scion")
	argsFile := filepath.Join(tmpDir, "sandbox-args")

	// Create a mock sandbox binary that records its args.
	// Only record args for the "run" subcommand (not for subsequent
	// liveness probe "exec" calls). All calls succeed (rc=0).
	mockBin := filepath.Join(tmpDir, "sandbox")
	script := "#!/bin/sh\nif [ \"$1\" = \"run\" ]; then\n  printf '%s\\n' \"$@\" > " + argsFile + "\nfi\necho sandbox-ok\n"
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	homeDir := filepath.Join(tmpDir, "agent-home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}

	stateFile := filepath.Join(tmpDir, "state.json")
	rt := &CloudRunSandboxRuntime{
		bin:          mockBin,
		state:        newSandboxStateStore(stateFile),
		rootDir:      rootDir,
		watchCancels: make(map[string]context.CancelFunc),
	}

	cfg := RunConfig{
		Name:      "test-agent",
		HomeDir:   homeDir,
		Workspace: filepath.Join(tmpDir, "workspace"),
		Image:     "omni-image",
		Project:   "my-project",
		ProjectID: "proj-123",
		Harness: &mockHarness{
			command: []string{"claude", "--agent"},
			env:     map[string]string{"HARNESS_KEY": "harness-val"},
		},
		Labels: map[string]string{"scion.name": "test-agent"},
	}

	// Create workspace dir so copy doesn't error.
	_ = os.MkdirAll(cfg.Workspace, 0755)

	id, err := rt.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if id != "test-agent" {
		t.Errorf("Run() returned id = %q, want %q", id, "test-agent")
	}

	// Read recorded args.
	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("failed to read mock binary args: %v", err)
	}
	args := string(argsData)

	// Verify key args are present.
	for _, pattern := range []string{
		"run",
		"test-agent",
		"--detach",
		"--rootfs",
		"--write",
		"--allow-egress",
		"--mount",
		// Per-agent mounts: agent home mounted at /home/scion, workspace
		// at /workspace. No tmux mount (§4.4a: AF_UNIX can't cross gVisor
		// boundary). Workspace destination must be /workspace (not the
		// host-side path) so sciontool init's git clone writes to the
		// writable bind mount instead of the read-only rootfs (#43).
		"type=bind,source=" + rootDir + "/agents/test-agent/home,destination=/home/scion",
		"type=bind,source=" + rootDir + "/agents/test-agent/workspace,destination=/workspace",
		// Env vars via --env flags (FIX 2), not /usr/bin/env.
		"--env",
		"sciontool",
		"init",
	} {
		if !strings.Contains(args, pattern) {
			t.Errorf("sandbox command missing pattern %q\nfull args:\n%s", pattern, args)
		}
	}

	// /usr/bin/env should NOT be in args (FIX 2).
	if strings.Contains(args, "/usr/bin/env") {
		t.Errorf("sandbox command should not contain /usr/bin/env\nfull args:\n%s", args)
	}

	// Verify state store has the entry.
	entry := rt.state.get("test-agent")
	if entry == nil {
		t.Fatal("state store entry not found after Run()")
	}
	if entry.AgentID != "test-agent" {
		t.Errorf("state entry AgentID = %q, want %q", entry.AgentID, "test-agent")
	}
	if entry.Project != "my-project" {
		t.Errorf("state entry Project = %q, want %q", entry.Project, "my-project")
	}
}

// TestCloudRunSandboxRuntime_Run_DeadOnArrival tests that Run() returns an
// error when the sandbox dies immediately after `sandbox run --detach`
// returns rc=0. This catches the "dead on arrival" scenario where the
// sandbox binary cannot resolve PATH (Finding #10) or fails for any other
// reason invisible to the run command's exit code.
func TestCloudRunSandboxRuntime_Run_DeadOnArrival(t *testing.T) {
	tmpDir := t.TempDir()
	rootDir := filepath.Join(tmpDir, "scion")

	// Mock binary: "run" succeeds, "exec" always fails (sandbox is dead),
	// "delete" succeeds (cleanup).
	mockBin := filepath.Join(tmpDir, "sandbox")
	script := `#!/bin/sh
case "$1" in
  run)    echo sandbox-ok; exit 0 ;;
  exec)   echo "sandbox not running"; exit 1 ;;
  delete) exit 0 ;;
  *)      exit 1 ;;
esac
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	homeDir := filepath.Join(tmpDir, "agent-home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}

	stateFile := filepath.Join(tmpDir, "state.json")
	rt := &CloudRunSandboxRuntime{
		bin:          mockBin,
		state:        newSandboxStateStore(stateFile),
		rootDir:      rootDir,
		watchCancels: make(map[string]context.CancelFunc),
	}

	cfg := RunConfig{
		Name:      "dead-agent",
		HomeDir:   homeDir,
		Workspace: filepath.Join(tmpDir, "workspace"),
		Image:     "omni-image",
		Project:   "my-project",
		ProjectID: "proj-123",
		Harness: &mockHarness{
			command: []string{"claude", "--agent"},
			env:     map[string]string{},
		},
		Labels: map[string]string{"scion.name": "dead-agent"},
	}

	_ = os.MkdirAll(cfg.Workspace, 0755)

	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() should return error for dead-on-arrival sandbox")
	}

	if !strings.Contains(err.Error(), "dead on arrival") {
		t.Errorf("Run() error should mention 'dead on arrival', got: %v", err)
	}

	// State store should NOT have an entry (sandbox was DOA).
	entry := rt.state.get("dead-agent")
	if entry != nil {
		t.Error("state store should not have entry for dead-on-arrival sandbox")
	}
}

// TestCloudRunSandboxRuntime_BuildEntrypoint_AbsolutePaths verifies that
// buildEntrypoint uses absolute paths for shell binaries (Finding #10).
func TestCloudRunSandboxRuntime_BuildEntrypoint_AbsolutePaths(t *testing.T) {
	cfg := RunConfig{
		Harness: &mockHarness{
			command: []string{"claude", "--agent"},
			env:     map[string]string{},
		},
	}
	entrypoint, err := buildEntrypoint(cfg)
	if err != nil {
		t.Fatalf("buildEntrypoint() error = %v", err)
	}
	// The first element must be an absolute path.
	if !strings.HasPrefix(entrypoint[0], "/") {
		t.Errorf("buildEntrypoint()[0] = %q, want absolute path (must start with /)", entrypoint[0])
	}
	// The entrypoint must not use bare 'sh' anywhere in the command.
	cmd := strings.Join(entrypoint, " ")
	// Check for bare 'sh' that is not preceded by '/' (would match 'sh -c' but not '/bin/sh -c')
	if strings.Contains(cmd, " sh -c ") && !strings.Contains(cmd, "/sh -c") {
		t.Errorf("buildEntrypoint() uses bare 'sh' instead of an absolute path:\n%s", cmd)
	}
}

// -----------------------------------------------------------------------
// Delete tests (with mock binary)
// -----------------------------------------------------------------------

func TestCloudRunSandboxRuntime_Delete(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "delete-args")

	// Mock binary that records its args and succeeds.
	mockBin := filepath.Join(tmpDir, "sandbox")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nexit 0\n"
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	stateFile := filepath.Join(tmpDir, "state.json")
	rt := &CloudRunSandboxRuntime{
		bin:          mockBin,
		state:        newSandboxStateStore(stateFile),
		rootDir:      filepath.Join(tmpDir, "scion"),
		watchCancels: make(map[string]context.CancelFunc),
	}

	rt.state.add(&sandboxStateEntry{SandboxName: "sb-del", AgentID: "agent-del"})

	err := rt.Delete(context.Background(), "sb-del")
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if entry := rt.state.get("sb-del"); entry != nil {
		t.Error("state entry still present after Delete()")
	}

	// Verify --force is used (FIX 4).
	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("failed to read mock args: %v", err)
	}
	if !strings.Contains(string(argsData), "--force") {
		t.Errorf("Delete() should use --force; args: %s", argsData)
	}
}

func TestCloudRunSandboxRuntime_Stop(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "stop-args")

	// Mock binary that records its args and succeeds.
	mockBin := filepath.Join(tmpDir, "sandbox")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nexit 0\n"
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	stateFile := filepath.Join(tmpDir, "state.json")
	rt := &CloudRunSandboxRuntime{
		bin:          mockBin,
		state:        newSandboxStateStore(stateFile),
		rootDir:      filepath.Join(tmpDir, "scion"),
		watchCancels: make(map[string]context.CancelFunc),
	}

	rt.state.add(&sandboxStateEntry{SandboxName: "sb-stop", AgentID: "agent-stop"})

	err := rt.Stop(context.Background(), "sb-stop")
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if entry := rt.state.get("sb-stop"); entry != nil {
		t.Error("state entry still present after Stop()")
	}

	// Verify --force is used (FIX 4).
	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("failed to read mock args: %v", err)
	}
	if !strings.Contains(string(argsData), "--force") {
		t.Errorf("Stop() should use --force; args: %s", argsData)
	}
}

// -----------------------------------------------------------------------
// relocateToScion tests
// -----------------------------------------------------------------------

func TestRelocateToScion_MovesContent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	_ = os.MkdirAll(src, 0755)
	_ = os.MkdirAll(dst, 0755)

	_ = os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello"), 0644)

	if err := relocateToScion(src, dst); err != nil {
		t.Fatalf("relocateToScion() error = %v", err)
	}

	// File should be at dst.
	data, err := os.ReadFile(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatalf("file not found at dst: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("file content = %q, want %q", string(data), "hello")
	}

	// src should be a symlink to dst.
	link, err := os.Readlink(src)
	if err != nil {
		t.Fatalf("src is not a symlink: %v", err)
	}
	if link != dst {
		t.Errorf("symlink target = %q, want %q", link, dst)
	}
}

func TestRelocateToScion_NonexistentSrc(t *testing.T) {
	src := filepath.Join(t.TempDir(), "nonexistent")
	dst := filepath.Join(t.TempDir(), "dst")
	_ = os.MkdirAll(dst, 0755)

	err := relocateToScion(src, dst)
	if err != nil {
		t.Errorf("relocateToScion() with nonexistent src should be no-op, got error = %v", err)
	}
}

func TestRelocateToScion_AlreadySymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	_ = os.MkdirAll(target, 0755)

	src := filepath.Join(t.TempDir(), "src-link")
	_ = os.Symlink(target, src)

	dst := filepath.Join(t.TempDir(), "dst")
	_ = os.MkdirAll(dst, 0755)

	err := relocateToScion(src, dst)
	if err != nil {
		t.Errorf("relocateToScion() with symlink src should be no-op, got error = %v", err)
	}

	// Symlink should still point to original target, not dst.
	link, _ := os.Readlink(src)
	if link != target {
		t.Errorf("symlink target changed to %q, want %q", link, target)
	}
}

// -----------------------------------------------------------------------
// waitForSandboxLiveness tests
// -----------------------------------------------------------------------

func TestWaitForSandboxLiveness_CancelledContext(t *testing.T) {
	// A pre-cancelled context must exit immediately without calling the probe.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var probeCalls atomic.Int32
	probe := func(ctx context.Context) error {
		probeCalls.Add(1)
		return nil
	}

	err := waitForSandboxLiveness(ctx, []time.Duration{time.Millisecond}, probe, "test")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("waitForSandboxLiveness() = %v, want context.Canceled", err)
	}
	if n := probeCalls.Load(); n != 0 {
		t.Errorf("probe was called %d times, want 0 (context was already cancelled)", n)
	}
}

func TestWaitForSandboxLiveness_CancelDuringSleep(t *testing.T) {
	// Cancel the context during the delay; the probe should not run.
	ctx, cancel := context.WithCancel(context.Background())

	var probeCalls atomic.Int32
	probe := func(ctx context.Context) error {
		probeCalls.Add(1)
		return nil
	}

	// Use a long delay so we can cancel during it.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	err := waitForSandboxLiveness(ctx, []time.Duration{5 * time.Second}, probe, "test")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("waitForSandboxLiveness() = %v, want context.Canceled", err)
	}
	if n := probeCalls.Load(); n != 0 {
		t.Errorf("probe was called %d times, want 0 (cancelled before probe)", n)
	}
}

func TestWaitForSandboxLiveness_Success(t *testing.T) {
	ctx := context.Background()

	probe := func(ctx context.Context) error {
		return nil
	}

	err := waitForSandboxLiveness(ctx, []time.Duration{time.Millisecond}, probe, "test")
	if err != nil {
		t.Errorf("waitForSandboxLiveness() = %v, want nil", err)
	}
}

func TestWaitForSandboxLiveness_RetriesThenSucceeds(t *testing.T) {
	ctx := context.Background()

	var probeCalls atomic.Int32
	probe := func(ctx context.Context) error {
		n := probeCalls.Add(1)
		if n < 3 {
			return errors.New("not ready")
		}
		return nil
	}

	delays := []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	err := waitForSandboxLiveness(ctx, delays, probe, "test")
	if err != nil {
		t.Errorf("waitForSandboxLiveness() = %v, want nil", err)
	}
	if n := probeCalls.Load(); n != 3 {
		t.Errorf("probe was called %d times, want 3", n)
	}
}

func TestWaitForSandboxLiveness_AllFail(t *testing.T) {
	ctx := context.Background()
	probeErr := errors.New("sandbox dead")

	probe := func(ctx context.Context) error {
		return probeErr
	}

	delays := []time.Duration{time.Millisecond, time.Millisecond}
	err := waitForSandboxLiveness(ctx, delays, probe, "test")
	if !errors.Is(err, probeErr) {
		t.Errorf("waitForSandboxLiveness() = %v, want %v", err, probeErr)
	}
}
