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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for the template env fallback (RunConfig.FallbackEnv,
// ptone/scion#4524): every runtime applies it as the LAST step of its env
// assembly, so auth env, harness env, env-type secrets and RunConfig.Env all
// win, a suppressed key is never set, and a non-colliding key is set.

func TestFallbackEnvEntries(t *testing.T) {
	got := FallbackEnvEntries(map[string]string{
		"NEW":       "n",
		"EMITTED":   "template",
		"SUPPRESS":  "template",
		"EMPTY":     "",
		"BAD=KEY":   "x",
		"":          "x",
		"A_LITERAL": "${HOME}",
	}, []string{"SUPPRESS"}, map[string]bool{"EMITTED": true})
	want := []string{"A_LITERAL=${HOME}", "NEW=n"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FallbackEnvEntries = %v, want %v", got, want)
	}
	if FallbackEnvEntries(nil, nil, nil) != nil {
		t.Error("nil fallback: want nil")
	}
}

// fallbackFixture is the shared table for the per-runtime tests: every
// colliding key's fallback value is "template".
var fallbackFixture = struct {
	fallback   map[string]string
	suppressed []string
}{
	fallback: map[string]string{
		"AUTH_KEY":    "template",
		"SECRET_KEY":  "template",
		"CFG_KEY":     "template",
		"HARNESS_KEY": "template",
		"SUPP_KEY":    "template",
		"NEW_KEY":     "from-template",
	},
	suppressed: []string{"SUPP_KEY"},
}

// fallbackRunConfig returns cfg with every source of the fallback fixture set.
func fallbackRunConfig(cfg RunConfig) RunConfig {
	cfg.Harness = &mockHarness{command: []string{"agent"}, env: map[string]string{"HARNESS_KEY": "harness"}}
	cfg.ResolvedAuth = &api.ResolvedAuth{Method: "api-key", EnvVars: map[string]string{"AUTH_KEY": "auth"}}
	cfg.ResolvedSecrets = []api.ResolvedSecret{{Name: "s", Type: "environment", Target: "SECRET_KEY", Value: "secret"}}
	cfg.Env = append(cfg.Env, "CFG_KEY=cfg")
	cfg.FallbackEnv = fallbackFixture.fallback
	cfg.SuppressedFallbackKeys = fallbackFixture.suppressed
	return cfg
}

// assertFallbackOutcome checks the effective env (last value per key).
func assertFallbackOutcome(t *testing.T, env map[string]string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got, ok := env[k]; !ok || got != v {
			t.Errorf("%s = %q (present %v), want %q", k, got, ok, v)
		}
	}
	if v, ok := env["SUPP_KEY"]; ok {
		t.Errorf("suppressed key SUPP_KEY = %q set by the fallback", v)
	}
	for k, v := range env {
		if v == "template" {
			t.Errorf("%s took the fallback value although another source set it", k)
		}
	}
}

// effectiveArgsEnv returns the env a container CLI ends up with: the last
// -e/--env value per key, from flags before the image (or "--").
func effectiveArgsEnv(args []string, stop string) map[string]string {
	env := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == stop {
			break
		}
		if args[i] == "-e" || args[i] == "--env" {
			if k, v, ok := strings.Cut(args[i+1], "="); ok {
				env[k] = v
			}
			i++
		}
	}
	return env
}

func TestBuildCommonRunArgs_FallbackEnvIsLast(t *testing.T) {
	cfg := fallbackRunConfig(minimalRunConfig())
	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	assertFallbackOutcome(t, effectiveArgsEnv(args, cfg.Image), map[string]string{
		"AUTH_KEY": "auth", "SECRET_KEY": "secret", "CFG_KEY": "cfg", "HARNESS_KEY": "harness", "NEW_KEY": "from-template",
	})
	// Every env flag precedes the image argument.
	img := -1
	for i, a := range args {
		if a == cfg.Image {
			img = i
			break
		}
	}
	for i, a := range args {
		if a == "NEW_KEY=from-template" && (img < 0 || i > img) {
			t.Errorf("fallback env flag after the image argument: %v", args)
		}
	}
}

func TestBuildPod_FallbackEnvIsLast(t *testing.T) {
	cfg := fallbackRunConfig(RunConfig{Name: "fallback", Image: "test-image", UnixUsername: "scion"})
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	env := map[string]string{}
	count := map[string]int{}
	for _, c := range pod.Spec.Containers {
		if c.Name != agentContainerName {
			continue
		}
		for _, ev := range c.Env {
			count[ev.Name]++
			if ev.ValueFrom != nil {
				env[ev.Name] = "<secret-ref>"
			} else {
				env[ev.Name] = ev.Value
			}
		}
	}
	for k, n := range count {
		if n > 1 {
			t.Errorf("env %s appears %d times", k, n)
		}
	}
	want := map[string]string{"AUTH_KEY": "auth", "CFG_KEY": "cfg", "HARNESS_KEY": "harness", "NEW_KEY": "from-template"}
	if v, ok := env["SECRET_KEY"]; ok && v == "template" {
		t.Errorf("SECRET_KEY took the fallback value over the env-type secret")
	}
	assertFallbackOutcome(t, env, want)
}

func TestBuildCloudRunInstance_FallbackEnvIsLast(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	// Cloud Run folds env-type secrets into cfg.Env before building the
	// instance (applyResolvedSecretsToEnv), so the secret arrives in Env.
	cfg := RunConfig{
		Image:   "test-image:latest",
		Labels:  map[string]string{"agent_id": "agent-1"},
		Harness: &mockHarness{command: []string{"agent"}},
		Env:     []string{"CFG_KEY=cfg", "SECRET_KEY=secret"},
		FallbackEnv: map[string]string{
			"CFG_KEY": "template", "SECRET_KEY": "template", "SCION_HOST_UID": "template",
			"SUPP_KEY": "template", "NEW_KEY": "from-template",
		},
		SuppressedFallbackKeys: []string{"SUPP_KEY"},
	}
	inst := rt.buildCloudRunInstance(cfg, 1000, 1000, nil)
	env := map[string]string{}
	count := map[string]int{}
	for _, ev := range inst.Containers[0].Env {
		count[ev.GetName()]++
		env[ev.GetName()] = ev.GetValue()
	}
	for k, n := range count {
		if n > 1 {
			t.Errorf("env %s appears %d times", k, n)
		}
	}
	assertFallbackOutcome(t, env, map[string]string{
		"CFG_KEY": "cfg", "SECRET_KEY": "secret", "SCION_HOST_UID": "1000", "NEW_KEY": "from-template",
	})
}

func TestBuildBootstrapEnv_FallbackEnvIsLast(t *testing.T) {
	cfg := fallbackRunConfig(RunConfig{Name: "fallback", UnixUsername: "scion"})
	env := buildBootstrapEnv(cfg)
	assertFallbackOutcome(t, env, map[string]string{
		"AUTH_KEY": "auth", "SECRET_KEY": "secret", "CFG_KEY": "cfg", "HARNESS_KEY": "harness", "NEW_KEY": "from-template",
	})
}

func TestCloudRunSandboxRuntime_Run_FallbackEnvIsLast(t *testing.T) {
	tmpDir := t.TempDir()
	rootDir := filepath.Join(tmpDir, "scion")
	argsFile := filepath.Join(tmpDir, "sandbox-args")
	mockBin := filepath.Join(tmpDir, "sandbox")
	script := "#!/bin/sh\nif [ \"$1\" = \"run\" ]; then\n  printf '%s\\n' \"$@\" > " + argsFile + "\nfi\necho sandbox-ok\n"
	if err := os.WriteFile(mockBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(tmpDir, "agent-home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rt := &CloudRunSandboxRuntime{
		bin:          mockBin,
		state:        newSandboxStateStore(filepath.Join(tmpDir, "state.json")),
		rootDir:      rootDir,
		watchCancels: make(map[string]context.CancelFunc),
	}
	t.Cleanup(func() {
		rt.watchMu.Lock()
		for _, cancel := range rt.watchCancels {
			cancel()
		}
		rt.watchMu.Unlock()
		time.Sleep(100 * time.Millisecond) // let goroutines exit
	})
	cfg := fallbackRunConfig(RunConfig{
		Name:      "fallback-agent",
		HomeDir:   homeDir,
		Workspace: filepath.Join(tmpDir, "workspace"),
		Image:     "omni-image",
		Labels:    map[string]string{"scion.name": "fallback-agent"},
	})
	_ = os.MkdirAll(cfg.Workspace, 0o755)
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	assertFallbackOutcome(t, effectiveArgsEnv(args, "--"), map[string]string{
		"AUTH_KEY": "auth", "SECRET_KEY": "secret", "CFG_KEY": "cfg", "HARNESS_KEY": "harness", "NEW_KEY": "from-template",
	})
}
