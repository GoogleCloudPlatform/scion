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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/autoexpose"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"gopkg.in/yaml.v3"
)

type parsedSeedContext struct {
	Version    int    `yaml:"version"`
	CurrentHub string `yaml:"current_hub"`
	Hubs       map[string]*struct {
		Endpoint string `yaml:"endpoint"`
	} `yaml:"hubs"`
}

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
			name: "SCION_HARNESS=shell (READY absent, old-broker fallback)",
			env:  map[string]string{"SCION_HARNESS": "shell"},
			want: true,
		},
		{
			name: "SCION_HARNESS=shell with idle ready",
			env:  map[string]string{"SCION_HARNESS": "shell", state.HarnessReadyEnv: idleReady},
			want: true,
		},
		{
			name: "SCION_HARNESS=claude (READY absent)",
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
			name: "READY idle + SCION_HARNESS=claude => true (READY wins)",
			env:  map[string]string{"SCION_HARNESS": "claude", state.HarnessReadyEnv: idleReady},
			want: true,
		},
		{
			name: "READY delay + SCION_HARNESS=shell => false (READY wins)",
			env:  map[string]string{"SCION_HARNESS": "shell", state.HarnessReadyEnv: delayReady},
			want: false,
		},
		{
			name: "READY malformed + SCION_HARNESS=shell => false (fails closed)",
			env:  map[string]string{"SCION_HARNESS": "shell", state.HarnessReadyEnv: "not-json"},
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
			got := isShellExecContainer(getenv)
			if got != tc.want {
				t.Errorf("isShellExecContainer() = %v, want %v", got, tc.want)
			}

			// Equivalence invariant: isShellExecContainer and control.ExecEnabled
			// (with hub.command_requests) must agree on every input class.
			if tc.env != nil {
				execEnv := func(k string) string {
					if k == control.EnvHubExperiments {
						return control.ExperimentCommandRequests
					}
					return tc.env[k]
				}
				if execGot := control.ExecEnabled(execEnv); execGot != got {
					t.Errorf("predicate mismatch: isShellExecContainer=%v, control.ExecEnabled=%v", got, execGot)
				}
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
		var parsed parsedSeedContext
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
		var parsed parsedSeedContext
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

	for _, mode := range []string{"iap", "cloudrun_invoker"} {
		t.Run("skips in proxy transport mode "+mode, func(t *testing.T) {
			home := t.TempDir()
			env := map[string]string{
				"SCION_HUB_ENDPOINT":           "https://hub.example.com",
				transportauth.EnvTransportMode: mode,
			}
			if err := seedShellCLIContext(home, 0, 0, func(k string) string { return env[k] }); err != nil {
				t.Fatalf("seedShellCLIContext(%s): %v", mode, err)
			}
			if _, err := os.Stat(filepath.Join(home, ".scion", "context.yaml")); !os.IsNotExist(err) {
				t.Fatalf("expected no context.yaml in proxy mode %s, stat err=%v", mode, err)
			}
		})
	}

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
	t.Setenv(autoexpose.EnvAutoExposePorts, "true")
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
	var parsed parsedSeedContext
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

// TestRunInit_ShellIsolationSkipsDelegationAndAutoexpose verifies O1
// (ptone/scion#9743): with a configured hub client and auto-expose enabled,
// shellIsolation skips both startDelegationHelper (no delegation socket exists
// while the workload runs) and autoexpose. A non-shell control verifies both
// start under the same setup, so removing either !shellIsolation guard fails
// this test.
func TestRunInit_ShellIsolationSkipsDelegationAndAutoexpose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	runCase := func(t *testing.T, harness string) (sawSocket bool, logOutput string) {
		t.Helper()
		// Keep path short so Unix domain socket bind stays well under 108 bytes.
		agentHome, err := os.MkdirTemp("/tmp", "scion-iso-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(agentHome) })

		setupRunInitAsRootlessScion(t, agentHome)
		t.Cleanup(hub.SetTokenHome(agentHome))
		readLog := captureSciontoolLog(t)

		origExitCodePath := harnessExitCodePath
		harnessExitCodePath = filepath.Join(t.TempDir(), "no-harness-exit-code")
		t.Cleanup(func() { harnessExitCodePath = origExitCodePath })
		origTrigger := limitsTriggerPath
		limitsTriggerPath = filepath.Join(t.TempDir(), "limits-exceeded")
		t.Cleanup(func() { limitsTriggerPath = origTrigger })

		// Provide a token file so hub.NewClient() is configured and RunInit
		// enters the post-start hub block (autoexpose + startDelegationHelper).
		scionDir := filepath.Join(agentHome, ".scion")
		if err := os.MkdirAll(scionDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scionDir, hub.TokenFile), []byte("test-token"), 0o600); err != nil {
			t.Fatal(err)
		}

		t.Setenv("SCION_HARNESS", harness)
		t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
		t.Setenv("SCION_AGENT_ID", "agent-test-1")
		t.Setenv(autoexpose.EnvAutoExposePorts, "true")

		sockPath := delegationSocketPath()
		marker := filepath.Join(t.TempDir(), "saw-socket")
		// Wait past RunInit's 100ms immediate-exit check + hub post-start setup,
		// then check whether the delegation socket exists.
		script := `i=0; while [ $i -lt 15 ]; do if [ -S "$1" ]; then touch "$2"; break; fi; sleep 0.1; i=$((i+1)); done`
		code := RunInit([]string{"sh", "-c", script, "sh", sockPath, marker},
			InitRunOptions{DisableTermSignalForwarding: true, DisableReExec: true})
		if code != 0 {
			t.Fatalf("RunInit(%s) = %d, want 0", harness, code)
		}
		_, statErr := os.Stat(marker)
		return statErr == nil, readLog()
	}

	t.Run("shell container skips delegation helper and autoexpose", func(t *testing.T) {
		sawSocket, logOut := runCase(t, "shell")
		if sawSocket {
			t.Error("delegation socket existed during shell container run; startDelegationHelper must be skipped under shellIsolation")
		}
		if strings.Contains(logOut, "Started auto-expose port scanner") {
			t.Errorf("autoexpose started under shellIsolation:\n%s", logOut)
		}
	})

	t.Run("control: non-shell container starts delegation helper and autoexpose", func(t *testing.T) {
		sawSocket, logOut := runCase(t, "claude")
		if !sawSocket {
			t.Errorf("expected delegation socket during non-shell control run; log:\n%s", logOut)
		}
		if !strings.Contains(logOut, "Started auto-expose port scanner") {
			t.Errorf("expected autoexpose to start in non-shell control run; log:\n%s", logOut)
		}
	})
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

func TestResolveShellStartCmdArgs(t *testing.T) {
	getenv := func(k string) string {
		if k == "SCION_START_CMD" {
			return `tmux new-session -d -s scion 'sh -c "while :; do bash -l; done"'`
		}
		return ""
	}

	t.Run("rewrites FUSE wrapper to pass SCION_START_CMD via $1", func(t *testing.T) {
		in := []string{"sh", "-c", `mkdir -p /mnt && gcsfuse b /mnt && exec sh -c "$SCION_START_CMD"`}
		got := resolveShellStartCmdArgs(in, getenv)
		want := []string{
			"sh", "-c",
			`mkdir -p /mnt && gcsfuse b /mnt && exec sh -c "$1"`,
			"sh",
			`tmux new-session -d -s scion 'sh -c "while :; do bash -l; done"'`,
		}
		if len(got) != len(want) {
			t.Fatalf("len = %d, want %d: %q", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("arg[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("leaves non-FUSE / K8s / Cloud Run inline argv untouched", func(t *testing.T) {
		for _, in := range [][]string{
			{"sh", "-c", "tmux new-session -d -s scion"},
			{"sh", "-c", `true && exec sh -c "$SCION_START_CMD"`, "sh", "extra"},
			{"bash", "-c", `true && exec sh -c "$SCION_START_CMD"`},
			{"tmux", "new-session"},
		} {
			got := resolveShellStartCmdArgs(in, getenv)
			if len(got) != len(in) {
				t.Errorf("unexpected rewrite for %q -> %q", in, got)
			}
		}
	})
}

// TestRunInit_ShellIsolation_FUSEStartWrapper verifies Review 2 R1
// (ptone/scion#9743): an isolated shell launch whose child argv is the FUSE
// wrapper shape `sh -c 'true && exec sh -c "$SCION_START_CMD"'` with
// SCION_START_CMD set in PID 1's environment runs the command AND the
// environment inside the started command contains no SCION_* variable
// (including no SCION_START_CMD).
func TestRunInit_ShellIsolation_FUSEStartWrapper(t *testing.T) {
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

	outDir := t.TempDir()
	markerFile := filepath.Join(outDir, "started.marker")
	envOut := filepath.Join(outDir, "fuse-child-env.txt")

	// Include single quotes and && in SCION_START_CMD to mirror tmux start
	// commands and verify quoting safety through positional $1.
	startCmd := `printf '%s\n' 'TMUX_WOULD_START' > '` + markerFile + `' && env > '` + envOut + `' && sleep 0.2`

	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")
	t.Setenv("SCION_AGENT_ID", "agent-fuse-1")
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_START_CMD", startCmd)
	t.Setenv("USER_CUSTOM_VAR", "keep-this")

	fuseArgs := []string{"sh", "-c", `true && exec sh -c "$SCION_START_CMD"`}
	code := RunInit(fuseArgs, InitRunOptions{DisableTermSignalForwarding: true, DisableReExec: true})
	if code != 0 {
		t.Fatalf("RunInit(FUSE wrapper) = %d, want 0", code)
	}

	markerBytes, err := os.ReadFile(markerFile)
	if err != nil {
		t.Fatalf("SCION_START_CMD did not execute under FUSE wrapper: %v", err)
	}
	if strings.TrimSpace(string(markerBytes)) != "TMUX_WOULD_START" {
		t.Fatalf("marker = %q, want TMUX_WOULD_START", string(markerBytes))
	}

	envBytes, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read started command env: %v", err)
	}
	sawUserVar := false
	for _, line := range strings.Split(string(envBytes), "\n") {
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		if strings.HasPrefix(k, "SCION_") {
			t.Errorf("started command under FUSE wrapper inherited SCION_* variable %s", k)
		}
		if k == "USER_CUSTOM_VAR" && v == "keep-this" {
			sawUserVar = true
		}
	}
	if !sawUserVar {
		t.Error("started command under FUSE wrapper lost non-SCION USER_CUSTOM_VAR")
	}
}
