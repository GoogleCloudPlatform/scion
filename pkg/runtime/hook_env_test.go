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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

type forgedMarkerHarness struct {
	harness.Generic
}

func (h *forgedMarkerHarness) GetEnv(agentName, agentHome, unixUsername string) map[string]string {
	out := h.Generic.GetEnv(agentName, agentHome, unixUsername)
	out[envpolicy.HookWithheldEnvVar] = "FORGED_FROM_HARNESS"
	out["HARNESS_KEY"] = "from-harness"
	return out
}

func forgedMarkerRunConfig(runtimeName string, withheld []string) RunConfig {
	return RunConfig{
		Harness:         &forgedMarkerHarness{},
		Name:            "test-agent",
		UnixUsername:    "scion",
		Image:           "scion-agent:latest",
		RuntimeName:     runtimeName,
		Labels:          map[string]string{"agent_id": "agent-1", "scion.name": "test-agent"},
		Env:             []string{"BUNDLE_KEY=v", envpolicy.HookWithheldEnvVar + "=FORGED_FROM_ENV"},
		FallbackEnv:     map[string]string{envpolicy.HookWithheldEnvVar: "FORGED_FROM_FALLBACK", "FALLBACK_KEY": "fb"},
		ResolvedSecrets: []api.ResolvedSecret{{Type: "environment", Target: envpolicy.HookWithheldEnvVar, Value: "FORGED_FROM_SECRET"}},
		HookWithheldEnv: withheld,
	}
}

func TestReservedAgentEnvName_IncludesHookWithheldMarker(t *testing.T) {
	if !reservedAgentEnvName(envpolicy.HookWithheldEnvVar) {
		t.Fatalf("reservedAgentEnvName(%q) = false, want true", envpolicy.HookWithheldEnvVar)
	}
}

func TestBuildCommonRunArgs_HookWithheldEnvIsReservedAndEmittedLast(t *testing.T) {
	for _, rtName := range []string{"docker", "podman", "container"} {
		t.Run(rtName, func(t *testing.T) {
			cfg := forgedMarkerRunConfig(rtName, []string{"Z_KEY", "BUNDLE_KEY", "BUNDLE_KEY"})
			args, err := buildCommonRunArgs(cfg)
			if err != nil {
				t.Fatalf("buildCommonRunArgs failed: %v", err)
			}
			if rtName == "container" {
				// Apple container strips unsupported flags from args[3:]; the
				// marker must survive that pass.
				args = stripUnsupportedAppleFlags(args[3:])
			}

			var envArgs []string
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-e" {
					envArgs = append(envArgs, args[i+1])
					i++
				}
			}

			wantEntry := envpolicy.HookWithheldEnvVar + "=BUNDLE_KEY,Z_KEY"
			var markerEntries []string
			for _, e := range envArgs {
				if k, _, _ := strings.Cut(e, "="); k == envpolicy.HookWithheldEnvVar {
					markerEntries = append(markerEntries, e)
				}
			}
			if len(markerEntries) != 1 || markerEntries[0] != wantEntry {
				t.Fatalf("marker -e entries = %v, want [%s]", markerEntries, wantEntry)
			}
			if envArgs[len(envArgs)-1] != wantEntry {
				t.Errorf("last -e entry = %q, want %q (must be emitted last)", envArgs[len(envArgs)-1], wantEntry)
			}
		})
	}
}

func TestPodmanSecretEnvNames_ExcludesHookWithheldMarker(t *testing.T) {
	cfg := forgedMarkerRunConfig("podman", []string{"BUNDLE_KEY"})
	names := podmanSecretEnvNames(cfg)
	if names[envpolicy.HookWithheldEnvVar] {
		t.Errorf("podmanSecretEnvNames unexpectedly includes %q", envpolicy.HookWithheldEnvVar)
	}
}

func TestBuildCommonRunArgs_HookWithheldEnvOmittedWhenEmptyEvenIfForged(t *testing.T) {
	for _, rtName := range []string{"docker", "podman", "container"} {
		t.Run(rtName, func(t *testing.T) {
			cfg := forgedMarkerRunConfig(rtName, nil)
			args, err := buildCommonRunArgs(cfg)
			if err != nil {
				t.Fatalf("buildCommonRunArgs failed: %v", err)
			}
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-e" {
					if k, _, _ := strings.Cut(args[i+1], "="); k == envpolicy.HookWithheldEnvVar {
						t.Errorf("unexpected %s emitted when HookWithheldEnv is empty: %q", envpolicy.HookWithheldEnvVar, args[i+1])
					}
					i++
				}
			}
		})
	}
}

func TestBuildCommonRunArgs_BundleEnvWinsOverAuthAndSecretAndIsWithheld(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RuntimeName:  "docker",
		Env:          []string{"GOOGLE_CLOUD_PROJECT=bundle-proj", "BUNDLE_ONLY=from-bundle"},
		ResolvedAuth: &api.ResolvedAuth{
			EnvVars: map[string]string{"GOOGLE_CLOUD_PROJECT": "auth-proj"},
		},
		ResolvedSecrets: []api.ResolvedSecret{
			{Type: "environment", Target: "GOOGLE_CLOUD_PROJECT", Value: "secret-proj"},
		},
		HookWithheldEnv: []string{"BUNDLE_ONLY", "GOOGLE_CLOUD_PROJECT"},
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	// On Docker, the last -e for a given key wins.
	effective := make(map[string]string)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" {
			if k, v, ok := strings.Cut(args[i+1], "="); ok {
				effective[k] = v
			}
			i++
		}
	}

	if got := effective["GOOGLE_CLOUD_PROJECT"]; got != "bundle-proj" {
		t.Errorf("effective GOOGLE_CLOUD_PROJECT = %q, want %q", got, "bundle-proj")
	}
	wantMarker := "BUNDLE_ONLY,GOOGLE_CLOUD_PROJECT"
	if got := effective[envpolicy.HookWithheldEnvVar]; got != wantMarker {
		t.Errorf("effective %s = %q, want %q", envpolicy.HookWithheldEnvVar, got, wantMarker)
	}
}

func TestBuildPod_HookWithheldEnvIsReservedAndEmittedLast(t *testing.T) {
	t.Run("emitted last and overrides forged sources", func(t *testing.T) {
		cfg := forgedMarkerRunConfig("kubernetes", []string{"Z_KEY", "BUNDLE_KEY", "BUNDLE_KEY"})
		pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
		if err != nil {
			t.Fatalf("buildPod: %v", err)
		}
		var envVars []string
		var markers []string
		for _, c := range pod.Spec.Containers {
			if c.Name != agentContainerName {
				continue
			}
			for _, ev := range c.Env {
				envVars = append(envVars, ev.Name+"="+ev.Value)
				if ev.Name == envpolicy.HookWithheldEnvVar {
					markers = append(markers, ev.Value)
				}
			}
		}
		if len(markers) != 1 || markers[0] != "BUNDLE_KEY,Z_KEY" {
			t.Fatalf("k8s marker values = %v, want [BUNDLE_KEY,Z_KEY]", markers)
		}
		wantLast := envpolicy.HookWithheldEnvVar + "=BUNDLE_KEY,Z_KEY"
		if len(envVars) == 0 || envVars[len(envVars)-1] != wantLast {
			t.Errorf("k8s last env = %q, want %q", envVars[len(envVars)-1], wantLast)
		}
	})

	t.Run("omitted when empty even if forged", func(t *testing.T) {
		cfg := forgedMarkerRunConfig("kubernetes", nil)
		pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
		if err != nil {
			t.Fatalf("buildPod: %v", err)
		}
		for _, c := range pod.Spec.Containers {
			if c.Name != agentContainerName {
				continue
			}
			for _, ev := range c.Env {
				if ev.Name == envpolicy.HookWithheldEnvVar {
					t.Errorf("unexpected %s=%q in k8s pod env", ev.Name, ev.Value)
				}
			}
		}
	})
}

func TestBuildCloudRunInstance_HookWithheldEnvIsReservedAndEmittedLast(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("emitted last and overrides forged sources", func(t *testing.T) {
		cfg := forgedMarkerRunConfig("cloudrun", []string{"Z_KEY", "BUNDLE_KEY", "BUNDLE_KEY"})
		inst, err := rt.buildCloudRunInstance(cfg, 1000, 1000, nil)
		if err != nil {
			t.Fatalf("buildCloudRunInstance: %v", err)
		}
		envList := inst.Containers[0].Env
		var markers []string
		for _, ev := range envList {
			if ev.GetName() == envpolicy.HookWithheldEnvVar {
				markers = append(markers, ev.GetValue())
			}
		}
		if len(markers) != 1 || markers[0] != "BUNDLE_KEY,Z_KEY" {
			t.Fatalf("cloudrun marker values = %v, want [BUNDLE_KEY,Z_KEY]", markers)
		}
		last := envList[len(envList)-1]
		if last.GetName() != envpolicy.HookWithheldEnvVar || last.GetValue() != "BUNDLE_KEY,Z_KEY" {
			t.Errorf("cloudrun last env = %s=%q, want %s=BUNDLE_KEY,Z_KEY", last.GetName(), last.GetValue(), envpolicy.HookWithheldEnvVar)
		}
	})

	t.Run("omitted when empty even if forged", func(t *testing.T) {
		cfg := forgedMarkerRunConfig("cloudrun", nil)
		inst, err := rt.buildCloudRunInstance(cfg, 1000, 1000, nil)
		if err != nil {
			t.Fatalf("buildCloudRunInstance: %v", err)
		}
		for _, ev := range inst.Containers[0].Env {
			if ev.GetName() == envpolicy.HookWithheldEnvVar {
				t.Errorf("unexpected %s=%q in cloudrun env", ev.GetName(), ev.GetValue())
			}
		}
	})
}

func TestCloudRunSandboxRuntime_HookWithheldEnvIsReservedAndEmittedLast(t *testing.T) {
	runSandbox := func(t *testing.T, withheld []string) []string {
		t.Helper()
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
		wsDir := filepath.Join(tmpDir, "workspace")
		if err := os.MkdirAll(wsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		rt := &CloudRunSandboxRuntime{
			bin:          mockBin,
			state:        newSandboxStateStore(filepath.Join(tmpDir, "state.json")),
			rootDir:      rootDir,
			watchCancels: make(map[string]context.CancelFunc),
		}
		stopWatchersOnCleanup(t, rt.state.path, rt)
		cfg := forgedMarkerRunConfig("cloudrun-sandbox", withheld)
		cfg.HomeDir = homeDir
		cfg.Workspace = wsDir
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		data, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatalf("read recorded args: %v", err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}

	t.Run("emitted last and overrides forged sources", func(t *testing.T) {
		args := runSandbox(t, []string{"Z_KEY", "BUNDLE_KEY", "BUNDLE_KEY"})
		var envArgs []string
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--" {
				break
			}
			if args[i] == "--env" {
				envArgs = append(envArgs, args[i+1])
				i++
			}
		}
		wantEntry := envpolicy.HookWithheldEnvVar + "=BUNDLE_KEY,Z_KEY"
		var markers []string
		for _, e := range envArgs {
			if k, _, _ := strings.Cut(e, "="); k == envpolicy.HookWithheldEnvVar {
				markers = append(markers, e)
			}
		}
		if len(markers) != 1 || markers[0] != wantEntry {
			t.Fatalf("sandbox marker --env entries = %v, want [%s]", markers, wantEntry)
		}
		if envArgs[len(envArgs)-1] != wantEntry {
			t.Errorf("sandbox last --env entry = %q, want %q", envArgs[len(envArgs)-1], wantEntry)
		}
	})

	t.Run("omitted when empty even if forged", func(t *testing.T) {
		args := runSandbox(t, nil)
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--" {
				break
			}
			if args[i] == "--env" {
				if k, _, _ := strings.Cut(args[i+1], "="); k == envpolicy.HookWithheldEnvVar {
					t.Errorf("unexpected %s in sandbox --env: %q", envpolicy.HookWithheldEnvVar, args[i+1])
				}
				i++
			}
		}
	})
}

func TestBuildBootstrapEnv_HookWithheldEnvIsReservedAndAuthoritative(t *testing.T) {
	t.Run("authoritative marker overrides forged sources", func(t *testing.T) {
		cfg := forgedMarkerRunConfig("substrate", []string{"Z_KEY", "BUNDLE_KEY", "BUNDLE_KEY"})
		env := buildBootstrapEnv(cfg)
		if got := env[envpolicy.HookWithheldEnvVar]; got != "BUNDLE_KEY,Z_KEY" {
			t.Errorf("substrate bootstrap %s = %q, want %q", envpolicy.HookWithheldEnvVar, got, "BUNDLE_KEY,Z_KEY")
		}
	})

	t.Run("omitted when empty even if forged", func(t *testing.T) {
		cfg := forgedMarkerRunConfig("substrate", nil)
		env := buildBootstrapEnv(cfg)
		if got, ok := env[envpolicy.HookWithheldEnvVar]; ok {
			t.Errorf("unexpected %s=%q in substrate bootstrap env", envpolicy.HookWithheldEnvVar, got)
		}
	})
}
