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
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// Fixture values only; none of these name a real project or region.
const (
	authFixtureProject = "fixture-vertex-project"
	authFixtureRegion  = "fixture-region-1"
)

// claudeVertexAuthNames are the names the claude vertex-ai ResolveAuth sets
// that the harness's env overlay also writes (as placeholder text when no
// staged secret holds the value), plus the GCP region it forwards.
var claudeVertexAuthNames = []string{
	"ANTHROPIC_VERTEX_PROJECT_ID",
	"CLOUD_ML_REGION",
	"CLAUDE_CODE_USE_VERTEX",
	"GOOGLE_CLOUD_REGION",
}

// claudeVertexWant is the env each runtime must deliver for
// claudeVertexAuthNames from the claude vertex-ai fixture.
var claudeVertexWant = map[string]string{
	"ANTHROPIC_VERTEX_PROJECT_ID": authFixtureProject,
	"CLOUD_ML_REGION":             authFixtureRegion,
	"CLAUDE_CODE_USE_VERTEX":      "1",
	"GOOGLE_CLOUD_REGION":         authFixtureRegion,
}

// claudeVertexResolvedAuth runs the claude harness's ResolveAuth for
// vertex-ai on the fixture project and region.
func claudeVertexResolvedAuth(t *testing.T) *api.ResolvedAuth {
	t.Helper()
	h, err := harness.NewContainerScriptHarness(t.TempDir(), config.HarnessConfigEntry{
		Harness:     "claude",
		Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
	})
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}
	ra, err := h.ResolveAuth(api.AuthConfig{
		SelectedType:       "vertex-ai",
		GoogleCloudProject: authFixtureProject,
		GoogleCloudRegion:  authFixtureRegion,
	})
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	for _, name := range claudeVertexAuthNames {
		if ra.EnvVars[name] != claudeVertexWant[name] {
			t.Fatalf("fixture: ResolveAuth did not set %s to the fixture value", name)
		}
	}
	return ra
}

// dockerEnvFor returns the effective container env (last -e per name) that
// buildCommonRunArgs gives cfg.
func dockerEnvFor(t *testing.T, cfg RunConfig) map[string]string {
	t.Helper()
	cfg.Harness = &mockHarness{command: []string{"echo"}, env: map[string]string{}}
	cfg.Name = "test-agent"
	cfg.UnixUsername = "scion"
	cfg.Image = "scion-agent:latest"
	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	return effectiveArgsEnv(args, cfg.Image)
}

// k8sEnvFor returns the agent container env the Kubernetes runtime gives cfg.
func k8sEnvFor(t *testing.T, cfg RunConfig) map[string]string {
	t.Helper()
	cfg.Name = "resolved-auth"
	cfg.Image = "example.com/image:v1"
	cfg.UnixUsername = "scion"
	cfg.Harness = &EnvHarness{}
	rt, _, _ := newTestK8sRuntime()
	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	env := map[string]string{}
	for _, c := range pod.Spec.Containers {
		if c.Name != agentContainerName {
			continue
		}
		for _, ev := range c.Env {
			env[ev.Name] = ev.Value
		}
	}
	return env
}

// cloudRunEnvFor runs cfg's settings through CloudRunRuntime.Run against a
// fake Instances API and returns the created instance's env (crInstanceEnv
// fails on a duplicate name).
func cloudRunEnvFor(t *testing.T, cfg RunConfig) map[string]string {
	t.Helper()
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	run := runConfigForTest()
	run.UnixUsername = "scion"
	run.Env = cfg.Env
	run.ResolvedAuth = cfg.ResolvedAuth
	run.ResolvedSecrets = cfg.ResolvedSecrets
	run.FallbackEnv = cfg.FallbackEnv
	if _, err := rt.Run(context.Background(), run); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return crInstanceEnv(t, fake)
}

// The same resolved auth gives the same env on Docker, Kubernetes and
// Cloud Run for the claude vertex-ai names. Cloud Run follows Docker's
// rules through resolvedAuthEnvEntries, so the colliding and empty-value
// cases are checked for those two; Kubernetes lets resolved auth win over
// config.Env and keeps empty values, so it is checked only where no other
// source sets the name.
func TestResolvedAuthEnv_ParityAcrossRuntimes(t *testing.T) {
	t.Run("claude-vertex-ai", func(t *testing.T) {
		cfg := RunConfig{ResolvedAuth: claudeVertexResolvedAuth(t)}
		envs := map[string]map[string]string{
			"docker":     dockerEnvFor(t, cfg),
			"kubernetes": k8sEnvFor(t, cfg),
			"cloudrun":   cloudRunEnvFor(t, cfg),
		}
		for runtimeName, env := range envs {
			for _, name := range claudeVertexAuthNames {
				if got, ok := env[name]; !ok || got != claudeVertexWant[name] {
					t.Errorf("%s: %s = %q (present %v), want the fixture value %q", runtimeName, name, got, ok, claudeVertexWant[name])
				}
			}
		}
	})

	t.Run("docker-cloudrun-config-env-and-secret-win", func(t *testing.T) {
		cfg := RunConfig{
			Env: []string{"GOOGLE_CLOUD_PROJECT=from-config-env"},
			ResolvedAuth: &api.ResolvedAuth{EnvVars: map[string]string{
				"GOOGLE_CLOUD_PROJECT": "from-resolved-auth",
				"EXAMPLE_AUTH_SECRET":  "from-resolved-auth",
				"CLOUD_ML_REGION":      authFixtureRegion,
			}},
			ResolvedSecrets: []api.ResolvedSecret{
				{Name: "auth-secret", Type: "environment", Target: "EXAMPLE_AUTH_SECRET", Value: "from-secret"},
			},
		}
		want := map[string]string{
			"GOOGLE_CLOUD_PROJECT": "from-config-env",
			"EXAMPLE_AUTH_SECRET":  "from-secret",
			"CLOUD_ML_REGION":      authFixtureRegion,
		}
		docker, cloudrun := dockerEnvFor(t, cfg), cloudRunEnvFor(t, cfg)
		for name, w := range want {
			if docker[name] != w {
				t.Errorf("docker: %s = %q, want %q", name, docker[name], w)
			}
			if got, ok := cloudrun[name]; !ok || got != w {
				t.Errorf("cloudrun: %s = %q (present %v), want %q", name, got, ok, w)
			}
		}
	})

	t.Run("docker-cloudrun-empty-value-skipped", func(t *testing.T) {
		cfg := RunConfig{ResolvedAuth: &api.ResolvedAuth{EnvVars: map[string]string{
			"CLOUD_ML_REGION":        "",
			"CLAUDE_CODE_USE_VERTEX": "1",
		}}}
		for runtimeName, env := range map[string]map[string]string{
			"docker":   dockerEnvFor(t, cfg),
			"cloudrun": cloudRunEnvFor(t, cfg),
		} {
			if _, ok := env["CLOUD_ML_REGION"]; ok {
				t.Errorf("%s: an empty resolved-auth value reached the container env", runtimeName)
			}
			if env["CLAUDE_CODE_USE_VERTEX"] != "1" {
				t.Errorf("%s: control: CLAUDE_CODE_USE_VERTEX = %q, want %q", runtimeName, env["CLAUDE_CODE_USE_VERTEX"], "1")
			}
		}
	})
}

// A Cloud Run instance for a claude vertex-ai agent carries the resolved
// auth names with their real values.
func TestCloudRunRun_ClaudeVertexAIResolvedAuthEnv(t *testing.T) {
	env := cloudRunEnvFor(t, RunConfig{ResolvedAuth: claudeVertexResolvedAuth(t)})
	for _, name := range claudeVertexAuthNames {
		if got, ok := env[name]; !ok || got != claudeVertexWant[name] {
			t.Errorf("%s = %q (present %v), want the fixture value %q", name, got, ok, claudeVertexWant[name])
		}
	}
	for _, name := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"} {
		if _, ok := env[name]; !ok {
			t.Errorf("%s missing from the instance env", name)
		}
	}
}

// The harness's env overlay holds placeholder text for the vertex names
// when no staged secret has their value. sciontool's MergeEnvOverlay adds
// an overlay name only when the runtime env lacks it, so with resolved
// auth applied the effective env keeps the real values.
func TestCloudRunRun_ResolvedAuthWinsOverPlaceholderOverlay(t *testing.T) {
	env := cloudRunEnvFor(t, RunConfig{ResolvedAuth: claudeVertexResolvedAuth(t)})
	runtimeEnv := make([]string, 0, len(env))
	for k, v := range env {
		runtimeEnv = append(runtimeEnv, k+"="+v)
	}
	slices.Sort(runtimeEnv)
	overlay := map[string]string{
		"ANTHROPIC_VERTEX_PROJECT_ID": "${GOOGLE_CLOUD_PROJECT}",
		"CLOUD_ML_REGION":             "${GOOGLE_CLOUD_LOCATION}",
		"CLAUDE_CODE_USE_VERTEX":      "1",
	}
	merged := hooks.MergeEnvOverlay(runtimeEnv, overlay)

	effective := map[string]string{}
	for _, e := range merged {
		k, v, _ := strings.Cut(e, "=")
		if _, dup := effective[k]; dup {
			t.Errorf("%s appears more than once in the merged env", k)
		}
		effective[k] = v
	}
	for _, name := range []string{"ANTHROPIC_VERTEX_PROJECT_ID", "CLOUD_ML_REGION", "CLAUDE_CODE_USE_VERTEX"} {
		if effective[name] != claudeVertexWant[name] {
			t.Errorf("%s = %q after the overlay merge, want the fixture value %q", name, effective[name], claudeVertexWant[name])
		}
	}
	for k, v := range effective {
		if strings.Contains(v, "${") {
			t.Errorf("%s carries placeholder text after the overlay merge", k)
		}
	}
}

// The resolved-auth path cannot put a reserved name, or a name the staged
// secrets or an env-type secret set, into the instance env with its own
// value.
func TestCloudRunRun_ResolvedAuthKeepsReservedAndSecretEnv(t *testing.T) {
	cfg := RunConfig{
		ResolvedAuth: &api.ResolvedAuth{EnvVars: map[string]string{
			stagedsecrets.HubTokenStagedEnvVar: "1",
			stagedsecrets.EnvVar:               "from-resolved-auth",
			"EXAMPLE_API_KEY":                  "from-resolved-auth",
			"ORDINARY_CONTROL":                 "keep",
		}},
		ResolvedSecrets: crTestResolvedSecrets(),
		Env:             []string{"PRESET_KEY=from-env"},
	}
	env := cloudRunEnvFor(t, cfg)
	if _, ok := env[stagedsecrets.HubTokenStagedEnvVar]; ok {
		t.Errorf("the reserved marker reached the instance env through resolved auth")
	}
	if env[stagedsecrets.EnvVar] == "from-resolved-auth" {
		t.Errorf("resolved auth replaced %s", stagedsecrets.EnvVar)
	}
	if env["EXAMPLE_API_KEY"] != crTestEnvSecretValue {
		t.Errorf("EXAMPLE_API_KEY does not carry the env-type secret value (resolved auth won: %v)", env["EXAMPLE_API_KEY"] == "from-resolved-auth")
	}
	if env["ORDINARY_CONTROL"] != "keep" {
		t.Errorf("control: ORDINARY_CONTROL = %q, want %q", env["ORDINARY_CONTROL"], "keep")
	}
	assertResolvedSecretsInEnv(t, env)
}

// A resolved-auth name counts as set for the template env fallback, as on
// Docker: the fallback neither overrides the applied value nor adds a
// second copy of the name.
func TestBuildCloudRunInstance_FallbackDoesNotOverrideResolvedAuth(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := RunConfig{
		Image:        "test-image:latest",
		Labels:       map[string]string{"agent_id": "agent-1"},
		ResolvedAuth: &api.ResolvedAuth{EnvVars: map[string]string{"CLOUD_ML_REGION": authFixtureRegion}},
		FallbackEnv:  map[string]string{"CLOUD_ML_REGION": "template", "NEW_KEY": "from-template"},
	}
	inst := rt.buildCloudRunInstance(cfg, 1000, 1000, nil)
	count := map[string]int{}
	env := map[string]string{}
	for _, ev := range inst.Containers[0].Env {
		count[ev.GetName()]++
		env[ev.GetName()] = ev.GetValue()
	}
	if count["CLOUD_ML_REGION"] != 1 {
		t.Errorf("CLOUD_ML_REGION appears %d times, want 1", count["CLOUD_ML_REGION"])
	}
	if env["CLOUD_ML_REGION"] != authFixtureRegion {
		t.Errorf("CLOUD_ML_REGION = %q, want the resolved-auth value %q", env["CLOUD_ML_REGION"], authFixtureRegion)
	}
	if env["NEW_KEY"] != "from-template" {
		t.Errorf("control: NEW_KEY = %q, want %q", env["NEW_KEY"], "from-template")
	}
}

// resolvedAuthEnvEntries skips empty values and taken names and returns the
// rest sorted by name.
func TestResolvedAuthEnvEntries(t *testing.T) {
	ra := &api.ResolvedAuth{EnvVars: map[string]string{
		"B_NAME": "b",
		"A_NAME": "a",
		"EMPTY":  "",
		"TAKEN":  "t",
	}}
	got := resolvedAuthEnvEntries(ra, func(name string) bool { return name == "TAKEN" })
	want := []string{"A_NAME=a", "B_NAME=b"}
	if !slices.Equal(got, want) {
		t.Errorf("resolvedAuthEnvEntries = %v, want %v", got, want)
	}
	if got := resolvedAuthEnvEntries(ra, nil); !slices.Equal(got, []string{"A_NAME=a", "B_NAME=b", "TAKEN=t"}) {
		t.Errorf("nil taken: resolvedAuthEnvEntries = %v", got)
	}
	if got := resolvedAuthEnvEntries(nil, nil); got != nil {
		t.Errorf("nil ResolvedAuth: resolvedAuthEnvEntries = %v, want nil", got)
	}
}
