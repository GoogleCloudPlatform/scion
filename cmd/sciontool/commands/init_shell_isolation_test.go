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

//go:build unix

package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
	"gopkg.in/yaml.v3"
)

func TestIsShellExecContainer(t *testing.T) {
	idleReady := `{"strategy":"idle"}`
	delayReady := `{"strategy":"delay","timeout_ms":15000}`

	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{
			name: "nil getenv",
			env:  nil,
			want: false,
		},
		{
			name: "SCION_HARNESS=shell",
			env:  map[string]string{"SCION_HARNESS": "shell"},
			want: true,
		},
		{
			name: "SCION_HARNESS=shell with idle ready",
			env:  map[string]string{"SCION_HARNESS": "shell", state.HarnessReadyEnv: idleReady},
			want: true,
		},
		{
			name: "SCION_HARNESS=claude",
			env:  map[string]string{"SCION_HARNESS": "claude"},
			want: false,
		},
		{
			name: "SCION_HARNESS unset and SCION_HARNESS_READY=idle",
			env:  map[string]string{state.HarnessReadyEnv: idleReady},
			want: true,
		},
		{
			name: "SCION_HARNESS unset and SCION_HARNESS_READY=delay",
			env:  map[string]string{state.HarnessReadyEnv: delayReady},
			want: false,
		},
		{
			name: "SCION_HARNESS=claude wins when set even with idle ready",
			env:  map[string]string{"SCION_HARNESS": "claude", state.HarnessReadyEnv: idleReady},
			want: false,
		},
		{
			name: "both unset",
			env:  map[string]string{},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var getenv func(string) string
			if tc.env != nil {
				getenv = func(k string) string { return tc.env[k] }
			}
			if got := isShellExecContainer(getenv); got != tc.want {
				t.Errorf("isShellExecContainer() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRemoveEnvFileNoFollow(t *testing.T) {
	t.Run("removes existing regular scion-env", func(t *testing.T) {
		home := t.TempDir()
		scionDir := filepath.Join(home, ".scion")
		if err := os.MkdirAll(scionDir, 0o755); err != nil {
			t.Fatal(err)
		}
		envPath := filepath.Join(scionDir, "scion-env")
		if err := os.WriteFile(envPath, []byte("export SCION_AGENT_ID=abc\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := removeEnvFileNoFollow(home); err != nil {
			t.Fatalf("removeEnvFileNoFollow: %v", err)
		}
		if _, err := os.Lstat(envPath); !os.IsNotExist(err) {
			t.Fatalf("expected scion-env to be removed, lstat err=%v", err)
		}
	})

	t.Run("missing .scion dir or file is a no-op", func(t *testing.T) {
		home := t.TempDir()
		if err := removeEnvFileNoFollow(home); err != nil {
			t.Fatalf("missing .scion dir: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := removeEnvFileNoFollow(home); err != nil {
			t.Fatalf("missing scion-env: %v", err)
		}
	})

	t.Run("unlinks symlink leaf without touching target", func(t *testing.T) {
		home := t.TempDir()
		scionDir := filepath.Join(home, ".scion")
		if err := os.MkdirAll(scionDir, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(target, []byte("keep-me"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(scionDir, "scion-env")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := removeEnvFileNoFollow(home); err != nil {
			t.Fatalf("removeEnvFileNoFollow on symlink leaf: %v", err)
		}
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Fatalf("expected symlink leaf to be removed, lstat err=%v", err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "keep-me" {
			t.Fatalf("target file was modified or removed: data=%q err=%v", got, err)
		}
	})

	t.Run("refuses symlinked .scion directory", func(t *testing.T) {
		home := t.TempDir()
		victimDir := t.TempDir()
		victimFile := filepath.Join(victimDir, "scion-env")
		if err := os.WriteFile(victimFile, []byte("keep-me"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victimDir, filepath.Join(home, ".scion")); err != nil {
			t.Fatal(err)
		}
		if err := removeEnvFileNoFollow(home); err == nil {
			t.Fatal("expected error for symlinked .scion directory, got nil")
		}
		if got, err := os.ReadFile(victimFile); err != nil || string(got) != "keep-me" {
			t.Fatalf("victim file was modified or removed: data=%q err=%v", got, err)
		}
	})
}

func TestSeedShellCLIContext(t *testing.T) {
	t.Run("seeds plain endpoint with mode 0600 and chowns to target UID/GID", func(t *testing.T) {
		home := t.TempDir()
		var chownedUID, chownedGID int
		origChown := shellContextSeedChown
		shellContextSeedChown = func(_ int, uid, gid int) error {
			chownedUID, chownedGID = uid, gid
			return nil
		}
		t.Cleanup(func() { shellContextSeedChown = origChown })

		origOwner := scionDirOwnerUID
		scionDirOwnerUID = func(int) (uint32, error) { return 1000, nil }
		t.Cleanup(func() { scionDirOwnerUID = origOwner })

		env := map[string]string{"SCION_HUB_ENDPOINT": "HTTPS://Hub.Example.COM:443/"}
		if err := seedShellCLIContext(home, 1000, 1001, func(k string) string { return env[k] }); err != nil {
			t.Fatalf("seedShellCLIContext: %v", err)
		}

		ctxPath := filepath.Join(home, ".scion", "context.yaml")
		info, err := os.Stat(ctxPath)
		if err != nil {
			t.Fatalf("stat context.yaml: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode = %#o, want 0600", got)
		}
		if chownedUID != 1000 || chownedGID != 1001 {
			t.Errorf("fchown got uid=%d gid=%d, want 1000:1001", chownedUID, chownedGID)
		}

		data, err := os.ReadFile(ctxPath)
		if err != nil {
			t.Fatal(err)
		}
		var parsed shellSeedCLIContext
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("unmarshal seeded context.yaml: %v", err)
		}
		if parsed.Version != 1 || parsed.CurrentHub != "hub" {
			t.Fatalf("parsed = %+v", parsed)
		}
		if entry := parsed.Hubs["hub"]; entry == nil || entry.Endpoint != "https://hub.example.com" {
			t.Fatalf("hubs[hub] = %+v, want https://hub.example.com", entry)
		}
		if strings.Contains(string(data), "transport") {
			t.Errorf("P1 plain-endpoint seed must not include transport block:\n%s", data)
		}
	})

	t.Run("falls back to SCION_HUB_URL when SCION_HUB_ENDPOINT is unset", func(t *testing.T) {
		home := t.TempDir()
		env := map[string]string{"SCION_HUB_URL": "http://localhost:9810/"}
		if err := seedShellCLIContext(home, 0, 0, func(k string) string { return env[k] }); err != nil {
			t.Fatalf("seedShellCLIContext: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(home, ".scion", "context.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var parsed shellSeedCLIContext
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.CurrentHub != "localhost" || parsed.Hubs["localhost"].Endpoint != "http://localhost:9810" {
			t.Fatalf("parsed = %+v", parsed)
		}
	})

	t.Run("skips when no hub endpoint is set", func(t *testing.T) {
		home := t.TempDir()
		if err := seedShellCLIContext(home, 0, 0, func(string) string { return "" }); err != nil {
			t.Fatalf("seedShellCLIContext: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, ".scion", "context.yaml")); !os.IsNotExist(err) {
			t.Fatalf("expected no context.yaml when endpoint unset, err=%v", err)
		}
	})

	t.Run("never overwrites an existing context.yaml (byte-identical)", func(t *testing.T) {
		home := t.TempDir()
		scionDir := filepath.Join(home, ".scion")
		if err := os.MkdirAll(scionDir, 0o755); err != nil {
			t.Fatal(err)
		}
		existing := "version: 1\ncurrent_hub: custom\nhubs:\n  custom:\n    endpoint: https://other.example.com\n"
		ctxPath := filepath.Join(scionDir, "context.yaml")
		if err := os.WriteFile(ctxPath, []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}

		env := map[string]string{"SCION_HUB_ENDPOINT": "https://hub.example.com"}
		if err := seedShellCLIContext(home, 0, 0, func(k string) string { return env[k] }); err != nil {
			t.Fatalf("seedShellCLIContext on existing file: %v", err)
		}
		got, err := os.ReadFile(ctxPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != existing {
			t.Fatalf("existing context.yaml was modified:\ngot:\n%s\nwant:\n%s", got, existing)
		}
	})

	t.Run("does not follow or overwrite a symlink at context.yaml", func(t *testing.T) {
		home := t.TempDir()
		scionDir := filepath.Join(home, ".scion")
		if err := os.MkdirAll(scionDir, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(scionDir, "context.yaml")); err != nil {
			t.Fatal(err)
		}

		env := map[string]string{"SCION_HUB_ENDPOINT": "https://hub.example.com"}
		if err := seedShellCLIContext(home, 0, 0, func(k string) string { return env[k] }); err != nil {
			t.Fatalf("seedShellCLIContext with symlink leaf: %v", err)
		}
		got, err := os.ReadFile(victim)
		if err != nil || string(got) != "untouched" {
			t.Fatalf("symlink target was modified: data=%q err=%v", got, err)
		}
	})

	t.Run("refuses symlinked .scion directory", func(t *testing.T) {
		home := t.TempDir()
		victimDir := t.TempDir()
		if err := os.Symlink(victimDir, filepath.Join(home, ".scion")); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"SCION_HUB_ENDPOINT": "https://hub.example.com"}
		if err := seedShellCLIContext(home, 0, 0, func(k string) string { return env[k] }); err == nil {
			t.Fatal("expected error for symlinked .scion directory, got nil")
		}
		if _, err := os.Stat(filepath.Join(victimDir, "context.yaml")); !os.IsNotExist(err) {
			t.Fatalf("context.yaml must not be written through symlinked .scion dir, err=%v", err)
		}
	})
}

// TestRunInit_ShellIsolationOrderingAndBehavior verifies the P1 ordering
// invariant (scope item 7): for a shell execution container, stale
// ~/.scion/scion-env is unlinked, ~/.scion/context.yaml is seeded, and
// supervisor.Config.WorkloadIsolation is enabled BEFORE sup.Run is called, and
// the launched workload receives zero SCION_* environment variables.
func TestRunInit_ShellIsolationOrderingAndBehavior(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	t.Cleanup(hub.SetTokenHome(agentHome))
	_ = captureSciontoolLog(t)

	origExitCodePath := harnessExitCodePath
	harnessExitCodePath = filepath.Join(t.TempDir(), "no-harness-exit-code")
	t.Cleanup(func() { harnessExitCodePath = origExitCodePath })
	origTrigger := limitsTriggerPath
	limitsTriggerPath = filepath.Join(t.TempDir(), "limits-exceeded")
	t.Cleanup(func() { limitsTriggerPath = origTrigger })

	// Pre-plant a stale scion-env to prove init unlinks it before sup.Run.
	scionDir := filepath.Join(agentHome, ".scion")
	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	envFilePath := filepath.Join(scionDir, "scion-env")
	if err := os.WriteFile(envFilePath, []byte("export SCION_AGENT_ID=stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctxFilePath := filepath.Join(scionDir, "context.yaml")

	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv("SCION_HUB_ENDPOINT", "https://Hub.Example.COM:443/")
	t.Setenv("SCION_AGENT_ID", "agent-shell-1")
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_AUTH_TOKEN", "should-not-leak")
	t.Setenv("SCION_AUTO_EXPOSE", "true")
	t.Setenv("USER_CUSTOM_VAR", "keep-this")

	// Snapshot state immediately before sup.Run is launched.
	hookCalled := false
	var hookWorkloadIsolation bool
	var envFileGoneBeforeRun bool
	var ctxExistsBeforeRun bool
	origHook := beforeSupervisorRunForTest
	beforeSupervisorRunForTest = func(cfg supervisor.Config) {
		hookCalled = true
		hookWorkloadIsolation = cfg.WorkloadIsolation
		_, err := os.Lstat(envFilePath)
		envFileGoneBeforeRun = os.IsNotExist(err)
		_, err = os.Stat(ctxFilePath)
		ctxExistsBeforeRun = err == nil
	}
	t.Cleanup(func() { beforeSupervisorRunForTest = origHook })

	envOut := filepath.Join(t.TempDir(), "child-env.txt")
	code := RunInit([]string{"sh", "-c", `env > "$1"; sleep 0.2`, "sh", envOut},
		InitRunOptions{DisableTermSignalForwarding: true, DisableReExec: true})
	if code != 0 {
		t.Fatalf("RunInit() = %d, want 0", code)
	}

	if !hookCalled {
		t.Fatal("beforeSupervisorRunForTest was not called")
	}
	if !hookWorkloadIsolation {
		t.Error("supervisor.Config.WorkloadIsolation was false before sup.Run")
	}
	if !envFileGoneBeforeRun {
		t.Error("stale ~/.scion/scion-env still existed when sup.Run was called")
	}
	if !ctxExistsBeforeRun {
		t.Error("~/.scion/context.yaml did not exist when sup.Run was called")
	}

	// Verify seeded context.yaml content.
	ctxBytes, err := os.ReadFile(ctxFilePath)
	if err != nil {
		t.Fatalf("read seeded context.yaml: %v", err)
	}
	var parsed shellSeedCLIContext
	if err := yaml.Unmarshal(ctxBytes, &parsed); err != nil {
		t.Fatalf("unmarshal seeded context.yaml: %v", err)
	}
	if parsed.CurrentHub != "hub" || parsed.Hubs["hub"] == nil || parsed.Hubs["hub"].Endpoint != "https://hub.example.com" {
		t.Errorf("seeded context.yaml = %+v, want hub -> https://hub.example.com", parsed)
	}

	// Verify child process saw zero SCION_* env vars and kept USER_CUSTOM_VAR.
	childEnvBytes, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read child env dump: %v", err)
	}
	sawUserVar := false
	for _, line := range strings.Split(string(childEnvBytes), "\n") {
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		if strings.HasPrefix(k, "SCION_") {
			t.Errorf("shell workload inherited SCION_* variable %s", k)
		}
		if k == "USER_CUSTOM_VAR" && v == "keep-this" {
			sawUserVar = true
		}
	}
	if !sawUserVar {
		t.Error("shell workload lost non-SCION USER_CUSTOM_VAR")
	}
}

// TestRunInit_NonShellKeepsEnvFileAndScionEnv verifies A10: non-shell (LLM)
// agents still write ~/.scion/scion-env, do not seed ~/.scion/context.yaml, and
// keep WorkloadIsolation disabled so SCION_* variables reach the child.
func TestRunInit_NonShellKeepsEnvFileAndScionEnv(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	t.Cleanup(hub.SetTokenHome(agentHome))
	_ = captureSciontoolLog(t)

	origExitCodePath := harnessExitCodePath
	harnessExitCodePath = filepath.Join(t.TempDir(), "no-harness-exit-code")
	t.Cleanup(func() { harnessExitCodePath = origExitCodePath })
	origTrigger := limitsTriggerPath
	limitsTriggerPath = filepath.Join(t.TempDir(), "limits-exceeded")
	t.Cleanup(func() { limitsTriggerPath = origTrigger })

	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")
	t.Setenv("SCION_AGENT_ID", "agent-llm-1")

	var hookWorkloadIsolation bool
	origHook := beforeSupervisorRunForTest
	beforeSupervisorRunForTest = func(cfg supervisor.Config) {
		hookWorkloadIsolation = cfg.WorkloadIsolation
	}
	t.Cleanup(func() { beforeSupervisorRunForTest = origHook })

	envOut := filepath.Join(t.TempDir(), "child-env.txt")
	code := RunInit([]string{"sh", "-c", `env > "$1"; sleep 0.2`, "sh", envOut},
		InitRunOptions{DisableTermSignalForwarding: true, DisableReExec: true})
	if code != 0 {
		t.Fatalf("RunInit() = %d, want 0", code)
	}

	if hookWorkloadIsolation {
		t.Error("non-shell agent must have WorkloadIsolation=false")
	}
	if _, err := os.Stat(filepath.Join(agentHome, ".scion", "scion-env")); err != nil {
		t.Errorf("non-shell agent must still write ~/.scion/scion-env: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentHome, ".scion", "context.yaml")); !os.IsNotExist(err) {
		t.Errorf("non-shell agent must not seed ~/.scion/context.yaml: err=%v", err)
	}
	childEnvBytes, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read child env dump: %v", err)
	}
	if !strings.Contains(string(childEnvBytes), "SCION_AGENT_ID=agent-llm-1") {
		t.Error("non-shell child did not inherit SCION_AGENT_ID")
	}
}
