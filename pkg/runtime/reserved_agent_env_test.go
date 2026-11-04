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

package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// reservedEnvProbe is a non-secret env entry that sets the reserved marker,
// next to an ordinary entry every runtime must pass through (the positive
// control).
var reservedEnvProbe = []string{
	stagedsecrets.HubTokenStagedEnvVar + "=1",
	"ORDINARY_CONTROL=keep",
}

// reservedEnvProbeMap is reservedEnvProbe as a map, for the sources that
// supply env as a map: resolved auth and the harness.
func reservedEnvProbeMap() map[string]string {
	return map[string]string{
		stagedsecrets.HubTokenStagedEnvVar: "1",
		"ORDINARY_CONTROL":                 "keep",
	}
}

// envFromRunArgs returns the "-e" entries of container CLI run arguments.
func envFromRunArgs(args []string) map[string]string {
	env := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" {
			k, v, _ := strings.Cut(args[i+1], "=")
			env[k] = v
		}
	}
	return env
}

// assertReservedDropped checks an environment (name -> value) for the
// probe: the marker absent, the control present.
func assertReservedDropped(t *testing.T, runtimeName string, env map[string]string) {
	t.Helper()
	if _, ok := env[stagedsecrets.HubTokenStagedEnvVar]; ok {
		t.Errorf("%s: the reserved marker reached the container environment", runtimeName)
	}
	if env["ORDINARY_CONTROL"] != "keep" {
		t.Errorf("%s: control: an ordinary env entry did not reach the container environment", runtimeName)
	}
}

// The reserved marker name never reaches an agent container from the
// non-secret environment, on every runtime.
func TestReservedAgentEnvName_DroppedFromNonSecretEnvOnEveryRuntime(t *testing.T) {
	t.Run("docker/podman/apple", func(t *testing.T) {
		args, err := buildCommonRunArgs(RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			// The bare name (no "=") makes the container CLI copy the value
			// from the broker's own environment.
			Env: append(append([]string(nil), reservedEnvProbe...), stagedsecrets.HubTokenStagedEnvVar),
		})
		if err != nil {
			t.Fatalf("buildCommonRunArgs: %v", err)
		}
		env := map[string]string{}
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-e" {
				// A bare "-e NAME" records NAME with an empty value, so its
				// presence is checked too.
				k, v, _ := strings.Cut(args[i+1], "=")
				env[k] = v
			}
		}
		assertReservedDropped(t, "docker", env)
	})

	// Resolved auth and harness env are merged after config.Env.
	t.Run("docker-resolved-auth", func(t *testing.T) {
		args, err := buildCommonRunArgs(RunConfig{
			Harness:      &mockHarness{command: []string{"echo"}, env: map[string]string{}},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			ResolvedAuth: &api.ResolvedAuth{EnvVars: reservedEnvProbeMap()},
		})
		if err != nil {
			t.Fatalf("buildCommonRunArgs: %v", err)
		}
		assertReservedDropped(t, "docker", envFromRunArgs(args))
	})

	t.Run("docker-harness-env", func(t *testing.T) {
		args, err := buildCommonRunArgs(RunConfig{
			Harness:      &mockHarness{command: []string{"echo"}, env: reservedEnvProbeMap()},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
		})
		if err != nil {
			t.Fatalf("buildCommonRunArgs: %v", err)
		}
		assertReservedDropped(t, "docker", envFromRunArgs(args))
	})

	t.Run("kubernetes", func(t *testing.T) {
		rt, _, _ := newTestK8sRuntime()
		pod, err := rt.buildPod("default", RunConfig{
			Name:         "reserved-env",
			Image:        "example.com/image:v1",
			UnixUsername: "scion",
			Harness:      &EnvHarness{},
			Env:          append([]string(nil), reservedEnvProbe...),
		})
		if err != nil {
			t.Fatalf("buildPod: %v", err)
		}
		env := map[string]string{}
		for _, c := range pod.Spec.Containers {
			for _, ev := range c.Env {
				env[ev.Name] = ev.Value
			}
		}
		assertReservedDropped(t, "kubernetes", env)
	})

	t.Run("cloudrun", func(t *testing.T) {
		fake := &fakeInstancesClient{getErr: notFoundErr()}
		rt := newFakeCloudRunRuntime(t, fake)
		cfg := runConfigForTest()
		cfg.Env = append([]string(nil), reservedEnvProbe...)
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertReservedDropped(t, "cloudrun", crInstanceEnv(t, fake))
	})

	t.Run("cloudrun-sandbox", func(t *testing.T) {
		h := newSandboxSecretsHarness(t)
		h.cfg.Env = append([]string(nil), reservedEnvProbe...)
		if _, err := h.rt.Run(context.Background(), h.cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertReservedDropped(t, "cloudrun-sandbox", h.runEnv(t))
	})

	// Resolved auth is merged after config.Env and the harness env.
	t.Run("cloudrun-sandbox-resolved-auth", func(t *testing.T) {
		h := newSandboxSecretsHarness(t)
		h.cfg.ResolvedAuth = &api.ResolvedAuth{EnvVars: reservedEnvProbeMap()}
		if _, err := h.rt.Run(context.Background(), h.cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertReservedDropped(t, "cloudrun-sandbox", h.runEnv(t))
	})

	t.Run("cloudrun-sandbox-harness-env", func(t *testing.T) {
		h := newSandboxSecretsHarness(t)
		h.cfg.Harness = &mockHarness{command: []string{"gemini"}, env: reservedEnvProbeMap()}
		if _, err := h.rt.Run(context.Background(), h.cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertReservedDropped(t, "cloudrun-sandbox", h.runEnv(t))
	})

	t.Run("substrate", func(t *testing.T) {
		env := buildBootstrapEnv(RunConfig{Env: append([]string(nil), reservedEnvProbe...)})
		assertReservedDropped(t, "substrate", env)
	})
}
