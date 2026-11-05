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
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// Fixture values only; none of these name a real project, region or
// credential.
const (
	authFixtureProject     = "fixture-vertex-project"
	authFixtureRegion      = "fixture-region-1"
	authFixtureConfigValue = "placeholder-auth-config-value"
	authCanaryAPIKey       = "placeholder-canary-api-key-7285"
	authCanaryOAuthToken   = "placeholder-canary-oauth-token-7285"
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
// vertex-ai on the fixture project and region, with one config-driven auth
// value (auth.EnvVars) that ResolveAuth copies into its env.
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
		EnvVars:            map[string]string{"EXAMPLE_AUTH_CONFIG_VALUE": authFixtureConfigValue},
	})
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	for _, name := range claudeVertexAuthNames {
		if ra.EnvVars[name] != claudeVertexWant[name] {
			t.Fatalf("fixture: ResolveAuth did not set %s to the fixture value", name)
		}
	}
	if ra.EnvVars["EXAMPLE_AUTH_CONFIG_VALUE"] != authFixtureConfigValue {
		t.Fatal("fixture: ResolveAuth did not copy the config-driven auth value")
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

// cloudRunRunFor runs cfg's settings through CloudRunRuntime.Run against a
// fake Instances API and returns the created instance's plain env
// (crInstanceEnv fails on a duplicate name) and the resolved-auth entries
// of its decoded staged blob (nil when there are none).
func cloudRunRunFor(t *testing.T, cfg RunConfig) (plain, staged map[string]string) {
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
	plain = crInstanceEnv(t, fake)
	return plain, crStagedResolvedAuth(t, plain)
}

// cloudRunEnvFor is cloudRunRunFor's plain env.
func cloudRunEnvFor(t *testing.T, cfg RunConfig) map[string]string {
	t.Helper()
	plain, _ := cloudRunRunFor(t, cfg)
	return plain
}

// crStagedResolvedAuth decodes the staged blob in a Cloud Run instance env
// and returns its resolved-auth entries (nil without a blob).
func crStagedResolvedAuth(t *testing.T, plain map[string]string) map[string]string {
	t.Helper()
	blob, ok := plain[stagedsecrets.EnvVar]
	if !ok {
		return nil
	}
	staged, err := stagedsecrets.Decode(blob)
	if err != nil {
		t.Fatalf("decode %s: %v", stagedsecrets.EnvVar, err)
	}
	return staged.ResolvedAuthEnv
}

// cloudRunEffective returns the value a Cloud Run harness receives for a
// resolved-auth name: the plain env for a discovery name, the staged
// resolved-auth entries for every other name.
func cloudRunEffective(plain, staged map[string]string, name string) (string, bool) {
	if resolvedAuthPlainEnvNames[name] {
		v, ok := plain[name]
		return v, ok
	}
	v, ok := staged[name]
	return v, ok
}

// The same resolved auth gives the same effective env on Docker,
// Kubernetes and Cloud Run. On Cloud Run the discovery names are plain env
// and every other entry is in the staged blob. Cloud Run follows Docker's
// rules, so the colliding and empty-value cases are checked for those two;
// Kubernetes lets resolved auth win over config.Env and keeps empty
// values, so it is checked only for the vertex names, which no other
// source sets.
func TestResolvedAuthEnv_ParityAcrossRuntimes(t *testing.T) {
	t.Run("claude-vertex-ai", func(t *testing.T) {
		ra := claudeVertexResolvedAuth(t)
		cfg := RunConfig{ResolvedAuth: ra}
		docker, k8s := dockerEnvFor(t, cfg), k8sEnvFor(t, cfg)
		plain, staged := cloudRunRunFor(t, cfg)
		for runtimeName, env := range map[string]map[string]string{"docker": docker, "kubernetes": k8s, "cloudrun": plain} {
			for _, name := range claudeVertexAuthNames {
				if got, ok := env[name]; !ok || got != claudeVertexWant[name] {
					t.Errorf("%s: %s = %q (present %v), want the fixture value %q", runtimeName, name, got, ok, claudeVertexWant[name])
				}
			}
		}
		// Every resolved-auth entry with a value: Docker's value equals the
		// value the Cloud Run harness receives.
		for name, v := range ra.EnvVars {
			if v == "" {
				continue
			}
			got, ok := cloudRunEffective(plain, staged, name)
			if docker[name] != v || !ok || got != docker[name] {
				t.Errorf("%s: docker %q, cloudrun %q (present %v), want both %q", name, docker[name], got, ok, v)
			}
		}
	})

	// The same vertex names on Cloud Run with the claude harness itself
	// set, so its harness env (ptone/scion#7295) is applied too: the
	// resolved-auth values are unchanged and the harness env reaches the
	// instance.
	t.Run("claude-vertex-ai-cloudrun-with-harness", func(t *testing.T) {
		h, err := harness.NewContainerScriptHarness(t.TempDir(), config.HarnessConfigEntry{
			Harness:     "claude",
			Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
		})
		if err != nil {
			t.Fatalf("NewContainerScriptHarness: %v", err)
		}
		fake := &fakeInstancesClient{getErr: notFoundErr()}
		rt := newFakeCloudRunRuntime(t, fake)
		run := runConfigForTest()
		run.UnixUsername = "scion"
		run.Harness = h
		run.ResolvedAuth = claudeVertexResolvedAuth(t)
		if _, err := rt.Run(context.Background(), run); err != nil {
			t.Fatalf("Run: %v", err)
		}
		env := crInstanceEnv(t, fake)
		for _, name := range claudeVertexAuthNames {
			if got, ok := env[name]; !ok || got != claudeVertexWant[name] {
				t.Errorf("%s = %q (present %v), want the fixture value %q", name, got, ok, claudeVertexWant[name])
			}
		}
		if env["SCION_AGENT_NAME"] != run.Name {
			t.Errorf("SCION_AGENT_NAME = %q, want the harness env value %q", env["SCION_AGENT_NAME"], run.Name)
		}
	})

	t.Run("docker-cloudrun-config-env-and-secret-win", func(t *testing.T) {
		cfg := RunConfig{
			Env: []string{"GOOGLE_CLOUD_PROJECT=from-config-env", "EXAMPLE_CFG_NAME=from-config-env"},
			ResolvedAuth: &api.ResolvedAuth{EnvVars: map[string]string{
				"GOOGLE_CLOUD_PROJECT": "from-resolved-auth",
				"GOOGLE_CLOUD_REGION":  "from-resolved-auth",
				"EXAMPLE_CFG_NAME":     "from-resolved-auth",
				"EXAMPLE_AUTH_SECRET":  "from-resolved-auth",
				"CLOUD_ML_REGION":      authFixtureRegion,
			}},
			ResolvedSecrets: []api.ResolvedSecret{
				{Name: "auth-secret", Type: "environment", Target: "EXAMPLE_AUTH_SECRET", Value: "from-secret"},
				{Name: "region-secret", Type: "environment", Target: "GOOGLE_CLOUD_REGION", Value: "from-secret"},
			},
		}
		want := map[string]string{
			"GOOGLE_CLOUD_PROJECT": "from-config-env",
			"GOOGLE_CLOUD_REGION":  "from-secret",
			"EXAMPLE_CFG_NAME":     "from-config-env",
			"EXAMPLE_AUTH_SECRET":  "from-secret",
			"CLOUD_ML_REGION":      authFixtureRegion,
		}
		docker := dockerEnvFor(t, cfg)
		plain, staged := cloudRunRunFor(t, cfg)
		for name, w := range want {
			if docker[name] != w {
				t.Errorf("docker: %s = %q, want %q", name, docker[name], w)
			}
			if got, ok := plain[name]; !ok || got != w {
				t.Errorf("cloudrun: %s = %q (present %v), want %q", name, got, ok, w)
			}
		}
		// A name config.Env or an env-type secret set is not staged either.
		for _, name := range []string{"EXAMPLE_CFG_NAME", "EXAMPLE_AUTH_SECRET"} {
			if _, ok := staged[name]; ok {
				t.Errorf("cloudrun: %s is in the staged resolved-auth entries although another source set it", name)
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
// auth discovery names as plain env with their real values.
func TestCloudRunRun_ClaudeVertexAIResolvedAuthEnv(t *testing.T) {
	env := cloudRunEnvFor(t, RunConfig{ResolvedAuth: claudeVertexResolvedAuth(t)})
	for _, name := range claudeVertexAuthNames {
		if got, ok := env[name]; !ok || got != claudeVertexWant[name] {
			t.Errorf("%s = %q (present %v), want the fixture value %q", name, got, ok, claudeVertexWant[name])
		}
	}
	for _, name := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION", "SCION_HARNESS_SELECTED_AUTH"} {
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
	// The vertex-ai entries of harnesses/claude/provision.py
	// _build_env_overlay when no staged secret holds the project or region
	// (its _resolve falls back to "${NAME}" text). Keep in step with it.
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

// Resolved-auth credential values never appear in the plain instance env:
// they are in the staged blob's resolved-auth entries.
func TestCloudRunRun_CredentialResolvedAuthOnlyInStagedBlob(t *testing.T) {
	plain, staged := cloudRunRunFor(t, RunConfig{ResolvedAuth: &api.ResolvedAuth{EnvVars: map[string]string{
		"ANTHROPIC_API_KEY":       authCanaryAPIKey,
		"CLAUDE_CODE_OAUTH_TOKEN": authCanaryOAuthToken,
		"CLAUDE_CODE_USE_VERTEX":  "1",
	}}})
	for _, name := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := plain[name]; ok {
			t.Errorf("%s is a plain instance env entry", name)
		}
	}
	for k, v := range plain {
		if strings.Contains(v, authCanaryAPIKey) || strings.Contains(v, authCanaryOAuthToken) {
			t.Errorf("the plain instance env entry %s carries a canary credential value", k)
		}
	}
	if staged["ANTHROPIC_API_KEY"] != authCanaryAPIKey || staged["CLAUDE_CODE_OAUTH_TOKEN"] != authCanaryOAuthToken {
		t.Error("the canary credentials are not in the staged resolved-auth entries")
	}
	if plain["CLAUDE_CODE_USE_VERTEX"] != "1" {
		t.Error("control: the discovery name CLAUDE_CODE_USE_VERTEX is not plain env")
	}
	if _, ok := staged["CLAUDE_CODE_USE_VERTEX"]; ok {
		t.Error("the discovery name CLAUDE_CODE_USE_VERTEX is also staged")
	}
}

// The resolved-auth path cannot set a reserved name, replace the staged
// blob, a runtime key or an env-type secret, in either the plain env or
// the staged entries.
func TestCloudRunRun_ResolvedAuthKeepsReservedAndSecretEnv(t *testing.T) {
	cfg := RunConfig{
		ResolvedAuth: &api.ResolvedAuth{EnvVars: map[string]string{
			stagedsecrets.HubTokenStagedEnvVar: "1",
			stagedsecrets.EnvVar:               "from-resolved-auth",
			"SCION_HOST_UID":                   "0",
			"EXAMPLE_API_KEY":                  "from-resolved-auth",
			"ORDINARY_CONTROL":                 "keep",
		}},
		ResolvedSecrets: crTestResolvedSecrets(),
		Env:             []string{"PRESET_KEY=from-env"},
	}
	plain, staged := cloudRunRunFor(t, cfg)
	if _, ok := plain[stagedsecrets.HubTokenStagedEnvVar]; ok {
		t.Error("the reserved marker reached the plain instance env through resolved auth")
	}
	if plain[stagedsecrets.EnvVar] == "from-resolved-auth" {
		t.Errorf("resolved auth replaced %s", stagedsecrets.EnvVar)
	}
	if plain["SCION_HOST_UID"] == "0" {
		t.Error("resolved auth replaced SCION_HOST_UID")
	}
	if plain["EXAMPLE_API_KEY"] != crTestEnvSecretValue {
		t.Errorf("EXAMPLE_API_KEY does not carry the env-type secret value (resolved auth won: %v)", plain["EXAMPLE_API_KEY"] == "from-resolved-auth")
	}
	if _, ok := plain["ORDINARY_CONTROL"]; ok {
		t.Error("a non-discovery resolved-auth entry is plain env")
	}
	for _, name := range []string{stagedsecrets.HubTokenStagedEnvVar, stagedsecrets.EnvVar, "SCION_HOST_UID", "EXAMPLE_API_KEY"} {
		if _, ok := staged[name]; ok {
			t.Errorf("%s is in the staged resolved-auth entries", name)
		}
	}
	if staged["ORDINARY_CONTROL"] != "keep" {
		t.Error("control: ORDINARY_CONTROL is not in the staged resolved-auth entries")
	}
	assertResolvedSecretsInEnv(t, plain)
}

// The staged resolved-auth entries count against the blob's size cap, and
// an oversize blob names their class, never a value.
func TestCloudRunRun_ResolvedAuthCountsAgainstStagedCap(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	cfg := runConfigForTest()
	cfg.UnixUsername = "scion"
	cfg.ResolvedAuth = &api.ResolvedAuth{EnvVars: map[string]string{
		"EXAMPLE_LARGE_AUTH_VALUE": strings.Repeat("a", cloudRunMaxEnvValueBytes),
	}}
	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run accepted a staged blob over the cap")
	}
	if !strings.Contains(err.Error(), stagedClassResolvedAuth) {
		t.Errorf("oversize error does not name the %q class: %v", stagedClassResolvedAuth, err)
	}
	if strings.Contains(err.Error(), "aaaaaaaa") || strings.Contains(err.Error(), "EXAMPLE_LARGE_AUTH_VALUE") {
		t.Error("oversize error carries the entry's value or name")
	}
	if len(fake.createReqs) != 0 {
		t.Error("an instance was created despite the oversize blob")
	}
}

// A resolved-auth name counts as set for the template fallback, as on
// Docker: the fallback neither overrides an applied plain value nor adds a
// second copy of the name, and it does not set a name the staged data
// carries (that name is never plain env either).
func TestBuildCloudRunInstance_FallbackDoesNotOverrideResolvedAuth(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := RunConfig{
		Image:        "test-image:latest",
		Labels:       map[string]string{"agent_id": "agent-1"},
		ResolvedAuth: &api.ResolvedAuth{EnvVars: map[string]string{"CLOUD_ML_REGION": authFixtureRegion, "EXAMPLE_STAGED_AUTH": "from-resolved-auth"}},
		FallbackEnv:  map[string]string{"CLOUD_ML_REGION": "template", "EXAMPLE_STAGED_AUTH": "template", "NEW_KEY": "from-template"},
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
	if v, ok := env["EXAMPLE_STAGED_AUTH"]; ok {
		t.Errorf("EXAMPLE_STAGED_AUTH is instance env (template value %v); a staged resolved-auth name must be neither plain env nor a fallback", v == "template")
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

// splitResolvedAuthEnv sends the discovery names to plain env and every
// other entry to the staged entries, and drops a reserved name from the
// staged entries even when nothing else set it (taken nil).
func TestSplitResolvedAuthEnv(t *testing.T) {
	env := map[string]string{
		"GOOGLE_CLOUD_PROJECT":                 authFixtureProject,
		"CLAUDE_CODE_USE_VERTEX":               "1",
		"ANTHROPIC_API_KEY":                    authCanaryAPIKey,
		stagedsecrets.HubTokenStagedEnvVar:     "1",
		stagedsecrets.ResolvedAuthStagedEnvVar: "1",
		stagedsecrets.EnvVar:                   "x",
	}
	for _, k := range cloudRunRuntimeEnvKeys {
		env[k] = "x"
	}
	plain, staged := splitResolvedAuthEnv(&api.ResolvedAuth{EnvVars: env}, nil)
	wantPlain := []string{"CLAUDE_CODE_USE_VERTEX=1", "GOOGLE_CLOUD_PROJECT=" + authFixtureProject}
	if !slices.Equal(plain, wantPlain) {
		t.Errorf("plain = %v, want %v", plain, wantPlain)
	}
	if len(staged) != 1 || staged["ANTHROPIC_API_KEY"] != authCanaryAPIKey {
		t.Errorf("staged keys = %v, want only ANTHROPIC_API_KEY", slices.Sorted(maps.Keys(staged)))
	}
	if p, s := splitResolvedAuthEnv(nil, nil); p != nil || s != nil {
		t.Error("nil ResolvedAuth gave entries")
	}
}

// Every name the Cloud Run runtime sets on the instance itself, and the
// names sciontool init reads, is reserved for the staged resolved-auth
// entries on both ends (broker and init).
func TestReservedResolvedAuthEnvName_CoversCloudRunRuntimeKeys(t *testing.T) {
	if len(cloudRunRuntimeEnvKeys) == 0 {
		t.Fatal("control: cloudRunRuntimeEnvKeys is empty")
	}
	names := append([]string{stagedsecrets.EnvVar, stagedsecrets.HubTokenStagedEnvVar}, cloudRunRuntimeEnvKeys...)
	for _, name := range names {
		if !stagedsecrets.ReservedResolvedAuthEnvName(name) {
			t.Errorf("%s is set by the runtime or read by init but not reserved for staged resolved-auth entries", name)
		}
	}
}

// A resolved-auth entry named like an entry sciontool init removes from the
// harness environment is never staged: the broker split drops it, and Run
// serializes no such entry into the staged blob.
func TestCloudRunRun_ResolvedAuthDropsHarnessStrippedNames(t *testing.T) {
	names := []string{"PORT", "SCION_TRANSPORT_TOKEN", "SCION_TRANSPORT_TOKEN_EXPIRY", "SCION_EXTRA_PATH", "SCION_NATIVE_TELEMETRY_POLICY"}
	env := map[string]string{"EXAMPLE_AUTH_CONTROL": "keep"}
	for _, name := range names {
		env[name] = "placeholder-stripped-name-value"
	}
	_, split := splitResolvedAuthEnv(&api.ResolvedAuth{EnvVars: env}, nil)
	_, staged := cloudRunRunFor(t, RunConfig{ResolvedAuth: &api.ResolvedAuth{EnvVars: env}})
	for where, got := range map[string]map[string]string{"split": split, "Run staged blob": staged} {
		for _, name := range names {
			if _, ok := got[name]; ok {
				t.Errorf("%s: resolved-auth entry %s is staged", where, name)
			}
		}
		if got["EXAMPLE_AUTH_CONTROL"] != "keep" {
			t.Errorf("%s: control: an ordinary resolved-auth entry is not staged", where)
		}
	}
}

// The resolved-auth staged marker is reserved like the staged-token
// marker: no container environment carries it on Docker or Cloud Run, and
// no staged resolved-auth entry sets it.
func TestReservedAgentEnvName_ResolvedAuthStagedMarker(t *testing.T) {
	if !reservedAgentEnvName(stagedsecrets.ResolvedAuthStagedEnvVar) {
		t.Fatal("the resolved-auth staged marker is not a reserved agent env name")
	}
	cfg := RunConfig{
		Env:          []string{stagedsecrets.ResolvedAuthStagedEnvVar + "=1", "ORDINARY_CONTROL=keep"},
		ResolvedAuth: &api.ResolvedAuth{EnvVars: map[string]string{stagedsecrets.ResolvedAuthStagedEnvVar: "1", "EXAMPLE_AUTH_CONTROL": "keep"}},
	}
	docker := dockerEnvFor(t, cfg)
	plain, staged := cloudRunRunFor(t, cfg)
	for runtimeName, env := range map[string]map[string]string{"docker": docker, "cloudrun": plain} {
		if _, ok := env[stagedsecrets.ResolvedAuthStagedEnvVar]; ok {
			t.Errorf("%s: the resolved-auth staged marker reached the container environment", runtimeName)
		}
		if env["ORDINARY_CONTROL"] != "keep" {
			t.Errorf("%s: control: an ordinary env entry did not reach the container environment", runtimeName)
		}
	}
	if _, ok := staged[stagedsecrets.ResolvedAuthStagedEnvVar]; ok {
		t.Error("cloudrun: the resolved-auth staged marker is a staged resolved-auth entry")
	}
	if staged["EXAMPLE_AUTH_CONTROL"] != "keep" {
		t.Error("cloudrun: control: an ordinary resolved-auth entry is not staged")
	}
}
