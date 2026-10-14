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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestHarnessAuthOverrideFlag(t *testing.T) {
	// Verify that HarnessAuth in StartOptions takes highest priority,
	// overriding the auth_selected_type from scion-agent.json.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	t.Run("override changes auth_selected_type from api-key to vertex-ai", func(t *testing.T) {
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				return "mock-id", nil
			},
		}

		agentDir := filepath.Join(projectScionDir, "agents", "auth-override")
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"auth_selectedType": "api-key"
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "auth-override",
			ProjectPath: projectScionDir,
			NoAuth:      true,
			HarnessAuth: "vertex-ai",
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		// The override is applied in-memory to finalScionCfg.AuthSelectedType
		// before container launch. Verify the scion-agent.json was updated.
		data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
		if err != nil {
			t.Fatalf("failed to read scion-agent.json: %v", err)
		}
		if !strings.Contains(string(data), `"vertex-ai"`) {
			t.Errorf("expected scion-agent.json to contain vertex-ai, got: %s", string(data))
		}
	})
}

func TestHarnessAuthCorruptedValueNotPersisted(t *testing.T) {
	// Regression test: when opts.HarnessAuth contains a harness implementation
	// name (e.g. "container-script" from corrupted scion-agent.json), it must
	// NOT be persisted to scion-agent.json. The guards at run.go:493 and
	// run.go:754 prevent this.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	for _, implName := range []string{"container-script", "generic", "builtin", "passthrough"} {
		t.Run(implName, func(t *testing.T) {
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					return "mock-id", nil
				},
			}

			agentName := "corrupt-" + implName
			agentDir := filepath.Join(projectScionDir, "agents", agentName)
			_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
			// Simulate corrupted scion-agent.json with harness implementation name
			_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
				"harness": "gemini",
				"auth_selectedType": "`+implName+`"
			}`), 0644)

			mgr := NewManager(mockRT)
			_, err := mgr.Start(context.Background(), api.StartOptions{
				Name:        agentName,
				ProjectPath: projectScionDir,
				NoAuth:      true,
				HarnessAuth: implName, // corrupted value from Hub
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
			if err != nil {
				t.Fatalf("failed to read scion-agent.json: %v", err)
			}
			// The corrupted implementation name must NOT appear as auth_selectedType.
			if strings.Contains(string(data), `"`+implName+`"`) {
				t.Errorf("scion-agent.json still contains corrupted value %q: %s", implName, string(data))
			}
		})
	}
}

func TestBuildAuthEnvOverlay_DoesNotMutateBaseEnv(t *testing.T) {
	baseEnv := map[string]string{
		"EXISTING_KEY": "existing-value",
		"API_KEY":      "explicit-value",
	}
	secrets := []api.ResolvedSecret{
		{
			Name:   "API_KEY",
			Type:   "environment",
			Target: "API_KEY",
			Value:  "secret-value",
			Source: "user",
		},
		{
			Name:   "GEMINI_API_KEY",
			Type:   "environment",
			Target: "GEMINI_API_KEY",
			Value:  "secret-api-key-value",
			Source: "user",
		},
	}

	overlay := buildAuthEnvOverlay(baseEnv, secrets)

	if baseEnv["API_KEY"] != "explicit-value" {
		t.Errorf("base env mutated: API_KEY = %q, want %q", baseEnv["API_KEY"], "explicit-value")
	}
	if _, ok := baseEnv["GEMINI_API_KEY"]; ok {
		t.Error("base env mutated: unexpected GEMINI_API_KEY entry")
	}
	if overlay["API_KEY"] != "explicit-value" {
		t.Errorf("overlay API_KEY = %q, want %q", overlay["API_KEY"], "explicit-value")
	}
	if overlay["GEMINI_API_KEY"] != "secret-api-key-value" {
		t.Errorf("overlay GEMINI_API_KEY = %q, want %q", overlay["GEMINI_API_KEY"], "secret-api-key-value")
	}
}

func TestBuildAuthEnvOverlay_EmptyValueOverriddenBySecret(t *testing.T) {
	// Empty-value passthrough markers in baseEnv should be overridden by
	// secrets so that auth resolution can detect the credential.
	baseEnv := map[string]string{
		"GEMINI_API_KEY": "",
		"EXISTING_KEY":   "keep-me",
	}
	secrets := []api.ResolvedSecret{
		{
			Name:   "GEMINI_API_KEY",
			Type:   "environment",
			Target: "GEMINI_API_KEY",
			Value:  "secret-api-key-value",
			Source: "user",
		},
	}

	overlay := buildAuthEnvOverlay(baseEnv, secrets)

	if overlay["GEMINI_API_KEY"] != "secret-api-key-value" {
		t.Errorf("overlay GEMINI_API_KEY = %q, want %q (secret should override empty passthrough)", overlay["GEMINI_API_KEY"], "secret-api-key-value")
	}
	if overlay["EXISTING_KEY"] != "keep-me" {
		t.Errorf("overlay EXISTING_KEY = %q, want %q", overlay["EXISTING_KEY"], "keep-me")
	}
	// Ensure baseEnv was not mutated
	if baseEnv["GEMINI_API_KEY"] != "" {
		t.Error("base env mutated: GEMINI_API_KEY should still be empty")
	}
}

func TestFilterResolvedSecretsForResolvedAuth(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{Name: "GEMINI_API_KEY", Type: "environment", Target: "GEMINI_API_KEY", Value: "gemini"},
		{Name: "gcloud-adc", Type: "file", Target: "/home/scion/.config/gcloud/application_default_credentials.json", Value: "adc"},
		{Name: "NOT_AUTH_SECRET", Type: "environment", Target: "NOT_AUTH_SECRET", Value: "keep"},
	}
	resolved := &api.ResolvedAuth{
		Method: "api-key",
		EnvVars: map[string]string{
			"GEMINI_API_KEY": "gemini",
		},
	}

	filtered := filterResolvedSecretsForResolvedAuth(secrets, resolved, nil)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 secrets after filtering, got %d", len(filtered))
	}

	got := make(map[string]struct{}, len(filtered))
	for _, s := range filtered {
		got[s.Name] = struct{}{}
	}
	if _, ok := got["GEMINI_API_KEY"]; !ok {
		t.Error("expected GEMINI_API_KEY to be kept")
	}
	if _, ok := got["NOT_AUTH_SECRET"]; !ok {
		t.Error("expected NOT_AUTH_SECRET to be kept")
	}
	if _, ok := got["gcloud-adc"]; ok {
		t.Error("expected gcloud-adc to be dropped for api-key auth")
	}
}

func TestIsAuthEnvKey_GCPSharedKeys(t *testing.T) {
	// GCP shared fields are always auth-related regardless of config
	gcpKeys := []string{
		"GOOGLE_CLOUD_PROJECT", "GCP_PROJECT", "ANTHROPIC_VERTEX_PROJECT_ID",
		"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION",
	}
	for _, key := range gcpKeys {
		if !isAuthEnvKey(key) {
			t.Errorf("isAuthEnvKey(%q) = false, want true", key)
		}
	}
	// Per-provider keys are no longer built-in — they come from config
	if isAuthEnvKey("GEMINI_API_KEY") {
		t.Error("isAuthEnvKey(GEMINI_API_KEY) without config = true, want false")
	}
	if isAuthEnvKey("RANDOM_ENV_VAR") {
		t.Error("isAuthEnvKey(RANDOM_ENV_VAR) = true, want false")
	}
}

func TestIsAuthEnvKey_ConfigDrivenKeys(t *testing.T) {
	configKeys := map[string]struct{}{
		"COPILOT_GITHUB_TOKEN": {},
		"GH_TOKEN":             {},
		"GITHUB_TOKEN":         {},
		"GEMINI_API_KEY":       {},
	}

	if !isAuthEnvKey("COPILOT_GITHUB_TOKEN", configKeys) {
		t.Error("isAuthEnvKey(COPILOT_GITHUB_TOKEN, configKeys) = false, want true")
	}
	if !isAuthEnvKey("GH_TOKEN", configKeys) {
		t.Error("isAuthEnvKey(GH_TOKEN, configKeys) = false, want true")
	}
	// Config-declared per-provider keys work with config keys present
	if !isAuthEnvKey("GEMINI_API_KEY", configKeys) {
		t.Error("isAuthEnvKey(GEMINI_API_KEY, configKeys) = false, want true")
	}
	// GCP shared keys always work
	if !isAuthEnvKey("GOOGLE_CLOUD_PROJECT", configKeys) {
		t.Error("isAuthEnvKey(GOOGLE_CLOUD_PROJECT, configKeys) = false, want true")
	}
	// Unknown key is still not auth
	if isAuthEnvKey("RANDOM_VAR", configKeys) {
		t.Error("isAuthEnvKey(RANDOM_VAR, configKeys) = true, want false")
	}
}

func TestConfigAuthEnvKeySet(t *testing.T) {
	authMeta := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"api-key": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}},
				},
			},
			"vertex-ai": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GOOGLE_CLOUD_PROJECT"}},
				},
			},
		},
	}

	keys := configAuthEnvKeySet(authMeta)
	expected := []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "GOOGLE_CLOUD_PROJECT"}
	for _, k := range expected {
		if _, ok := keys[k]; !ok {
			t.Errorf("expected key %q in configAuthEnvKeySet result", k)
		}
	}

	nilKeys := configAuthEnvKeySet(nil)
	if nilKeys != nil {
		t.Errorf("expected nil for nil authMeta, got %v", nilKeys)
	}
}

func TestFilterResolvedSecretsForResolvedAuth_ConfigDrivenKeys(t *testing.T) {
	configKeys := map[string]struct{}{
		"COPILOT_GITHUB_TOKEN": {},
		"GH_TOKEN":             {},
	}

	secrets := []api.ResolvedSecret{
		{Name: "COPILOT_GITHUB_TOKEN", Type: "environment", Target: "COPILOT_GITHUB_TOKEN", Value: "ghp_test"},
		{Name: "GH_TOKEN", Type: "environment", Target: "GH_TOKEN", Value: "gh_test"},
		{Name: "SOME_OTHER_SECRET", Type: "environment", Target: "SOME_OTHER_SECRET", Value: "other"},
	}

	resolved := &api.ResolvedAuth{
		Method: "api-key",
		EnvVars: map[string]string{
			"COPILOT_GITHUB_TOKEN": "ghp_test",
		},
	}

	filtered := filterResolvedSecretsForResolvedAuth(secrets, resolved, configKeys)

	got := make(map[string]struct{}, len(filtered))
	for _, s := range filtered {
		got[s.Name] = struct{}{}
	}

	if _, ok := got["COPILOT_GITHUB_TOKEN"]; !ok {
		t.Error("expected COPILOT_GITHUB_TOKEN to be kept (required by resolved auth)")
	}
	if _, ok := got["GH_TOKEN"]; ok {
		t.Error("expected GH_TOKEN to be dropped (config-driven auth key not required by resolved auth)")
	}
	if _, ok := got["SOME_OTHER_SECRET"]; !ok {
		t.Error("expected SOME_OTHER_SECRET to be kept (not an auth key)")
	}
}

func TestProfileEnvVisibleInAuthOverlay(t *testing.T) {
	// Regression: profile env vars must be injected into opts.Env BEFORE
	// buildAuthEnvOverlay is called. Previously the overlay was built first,
	// so profile-provided vars like GOOGLE_CLOUD_PROJECT were invisible to
	// GatherAuthWithEnv, causing auth resolution to fail.
	optsEnv := map[string]string{"EXISTING": "val"}

	// Simulate profile env injection (what Start does before buildAuthEnvOverlay)
	profileEnv := map[string]string{
		"GOOGLE_CLOUD_PROJECT": "my-project",
		"GOOGLE_CLOUD_REGION":  "us-central1",
	}
	for k, v := range profileEnv {
		if _, exists := optsEnv[k]; !exists {
			optsEnv[k] = v
		}
	}

	overlay := buildAuthEnvOverlay(optsEnv, nil)

	if got := overlay["GOOGLE_CLOUD_PROJECT"]; got != "my-project" {
		t.Errorf("GOOGLE_CLOUD_PROJECT in auth overlay = %q, want %q", got, "my-project")
	}
	if got := overlay["GOOGLE_CLOUD_REGION"]; got != "us-central1" {
		t.Errorf("GOOGLE_CLOUD_REGION in auth overlay = %q, want %q", got, "us-central1")
	}
}

func TestStartInjectsProfileEnvForAuth(t *testing.T) {
	// When the resolved harness config defines env vars like GOOGLE_CLOUD_PROJECT
	// and GOOGLE_CLOUD_REGION, Start() should inject them into opts.Env so that
	// GatherAuthWithEnv can see them during local (non-broker) auth resolution.
	//
	// NOTE ON THE NAME: this test is still called ...ProfileEnvForAuth, but its
	// fixture declares the env under harness_configs.<hc>.env, NOT under
	// profiles.<p>.env. G3-full retired profiles.<p>.env as an injection point
	// into harness-config resolution, so the old fixture could no longer reach
	// opts.Env at all. The fixture was migrated to the documented migration path
	// and EVERY ASSERTION BELOW IS BYTE-IDENTICAL to the pre-G3-full version --
	// that is the point: the injection behaviour under test did not change, only
	// the key you declare the env under. The name is left alone deliberately so
	// that this commit does not rename a test other agents are tracking by name;
	// renaming it is a follow-up, not a silent drive-by.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	// Clear env vars that would interfere
	for _, k := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
		if old, ok := os.LookupEnv(k); ok {
			defer func() { _ = os.Setenv(k, old) }()
			_ = os.Unsetenv(k)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config on disk (claude type)
	hcDir := filepath.Join(globalScionDir, "harness-configs", "claude-cfg")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "claude-cfg"}`), 0644)

	// Global versioned settings. The auth env vars are declared under
	// harness_configs.claude-cfg.env; they were under profiles.vertex.env until
	// G3-full removed profiles.<p>.env as an injection point.
	//
	// The assertions are unchanged across the migration. The property this test
	// exists for — settings-declared auth env reaches GatherAuthWithEnv during
	// local, non-broker auth resolution — survives G3-full; only the key it is
	// spelled with moved. harness_overrides is deliberately not used.
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: vertex
profiles:
  vertex:
    runtime: docker
harness_configs:
  claude-cfg:
    harness: claude
    env:
      GOOGLE_CLOUD_PROJECT: my-gcp-project
      GOOGLE_CLOUD_REGION: us-central1
runtimes:
  docker:
    type: docker
`), 0644)

	// Create project directory
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	// Capture the RunConfig
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			capturedConfig = cfg
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Convert env slice to map
	envMap := make(map[string]string)
	for _, e := range capturedConfig.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if got := envMap["GOOGLE_CLOUD_PROJECT"]; got != "my-gcp-project" {
		t.Errorf("GOOGLE_CLOUD_PROJECT = %q, want %q", got, "my-gcp-project")
	}
	if got := envMap["GOOGLE_CLOUD_REGION"]; got != "us-central1" {
		t.Errorf("GOOGLE_CLOUD_REGION = %q, want %q", got, "us-central1")
	}
}

// --- Gap 3 / Phase 5: harness-config env and the BrokerMode gate ---------
//
// These tests exercise resolveAuthEnvOverlay, the extracted
// injection-then-overlay sequence that Start runs immediately before
// GatherAuthWithEnv. They deliberately assert on the AUTH OVERLAY and not on
// the container environment: harness-config env already reaches the container
// in broker mode via provision.go's ungated finalScionCfg merge, so a
// container-env assertion is green before the fix and discriminates nothing.
// See design §0.2.

// g3TestSettings builds settings with one harness config and one profile,
// so tests can tell the two sources apart.
func g3TestSettings(hcEnv map[string]string) *config.VersionedSettings {
	return &config.VersionedSettings{
		SchemaVersion: "1",
		ActiveProfile: "vertex",
		HarnessConfigs: map[string]config.HarnessConfigEntry{
			"claude-cfg": {Harness: "claude", Env: hcEnv},
		},
		Profiles: map[string]config.V1ProfileConfig{
			"vertex": {Runtime: "docker"},
		},
	}
}

// TestStart_BrokerMode_HarnessConfigEnv_VisibleToAuthOverlay is the
// discriminating test for Gap 3. Before the fix the !opts.BrokerMode gate
// skipped injection entirely for hub-dispatched agents, so credentials the
// harness config declares were invisible to GatherAuthWithEnv.
func TestStart_BrokerMode_HarnessConfigEnv_VisibleToAuthOverlay(t *testing.T) {
	settings := g3TestSettings(map[string]string{
		"GOOGLE_CLOUD_PROJECT": "hc-project",
		"GOOGLE_CLOUD_REGION":  "us-central1",
	})

	opts := api.StartOptions{
		Name:       "test-agent",
		BrokerMode: true, // hub-dispatched: every agent in a hub deployment
		Env:        map[string]string{"EXISTING": "val"},
	}

	overlay, _ := resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

	if got := overlay["GOOGLE_CLOUD_PROJECT"]; got != "hc-project" {
		t.Errorf("auth overlay GOOGLE_CLOUD_PROJECT = %q, want %q "+
			"(harness-config env must be visible to GatherAuthWithEnv in broker mode)",
			got, "hc-project")
	}
	if got := overlay["GOOGLE_CLOUD_REGION"]; got != "us-central1" {
		t.Errorf("auth overlay GOOGLE_CLOUD_REGION = %q, want %q", got, "us-central1")
	}
	if got := overlay["EXISTING"]; got != "val" {
		t.Errorf("auth overlay EXISTING = %q, want %q (pre-existing keys must survive)", got, "val")
	}
}

// TestStart_BrokerMode_HubEnvNotClobberedByHarnessConfigEnv guards the
// only-if-absent guard: hub-resolved env is already in opts.Env by the time
// injection runs, and it must win over the harness config's value.
func TestStart_BrokerMode_HubEnvNotClobberedByHarnessConfigEnv(t *testing.T) {
	settings := g3TestSettings(map[string]string{
		"GOOGLE_CLOUD_PROJECT": "hc-project",
		"HC_ONLY":              "hc-value",
	})

	opts := api.StartOptions{
		Name:       "test-agent",
		BrokerMode: true,
		// Hub-supplied value, placed in opts.Env by start_context.go.
		Env: map[string]string{"GOOGLE_CLOUD_PROJECT": "hub-project"},
	}

	overlay, _ := resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

	if got := overlay["GOOGLE_CLOUD_PROJECT"]; got != "hub-project" {
		t.Errorf("auth overlay GOOGLE_CLOUD_PROJECT = %q, want %q (hub value must win)",
			got, "hub-project")
	}
	// opts.Env is what is later projected into the container, so assert there
	// too: acceptance criterion 15 is about the value that reaches the agent.
	if got := opts.Env["GOOGLE_CLOUD_PROJECT"]; got != "hub-project" {
		t.Errorf("opts.Env GOOGLE_CLOUD_PROJECT = %q, want %q (hub value must reach the container)",
			got, "hub-project")
	}
	// Polarity control: the non-colliding key still gets injected, proving the
	// guard is per-key and not an all-or-nothing bail-out.
	if got := overlay["HC_ONLY"]; got != "hc-value" {
		t.Errorf("auth overlay HC_ONLY = %q, want %q", got, "hc-value")
	}
}

// TestResolveAuthEnvOverlay_NoHarnessConfigMeansNoInjection verifies that
// with no harness config named, nothing is injected into the auth overlay.
// profiles.<p>.env has been fully removed from the struct, so there is no
// remaining path for profile env to reach the overlay.
func TestResolveAuthEnvOverlay_NoHarnessConfigMeansNoInjection(t *testing.T) {
	settings := g3TestSettings(nil)

	opts := api.StartOptions{Name: "test-agent", BrokerMode: true}

	overlay, _ := resolveAuthEnvOverlay(&opts, settings, "vertex", "" /* no harness config */)

	if len(overlay) != 0 {
		t.Errorf("auth overlay = %v, want empty (with no harness config named, nothing should be injected)", overlay)
	}
}

// TestResolveAuthEnvOverlay_OnlyHarnessConfigEnvArrives verifies that the auth
// overlay receives only harness-config env. profiles.<p>.env has been fully
// removed from V1ProfileConfig, so there is no profile env to arrive.
func TestResolveAuthEnvOverlay_OnlyHarnessConfigEnvArrives(t *testing.T) {
	settings := g3TestSettings(map[string]string{"HC_ONLY": "hc-value"})

	opts := api.StartOptions{Name: "test-agent", BrokerMode: true}

	overlay, _ := resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

	if got := overlay["HC_ONLY"]; got != "hc-value" {
		t.Fatalf("existence control failed: auth overlay HC_ONLY = %q, want %q — "+
			"the harness config was not resolved",
			got, "hc-value")
	}
}

// TestResolveAuthEnvOverlay_NilSettings guards the nil path.
func TestResolveAuthEnvOverlay_NilSettings(t *testing.T) {
	opts := api.StartOptions{Name: "test-agent", BrokerMode: true, Env: map[string]string{"A": "1"}}

	overlay, _ := resolveAuthEnvOverlay(&opts, nil, "vertex", "claude-cfg")

	if got := overlay["A"]; got != "1" {
		t.Errorf("auth overlay A = %q, want %q", got, "1")
	}
}

// --- Gap 3 follow-up: the RANK limb -----------------------------------------
//
// Design §0.2 item 2: Gap 3 has a rank limb as well as a presence limb. The
// presence limb (harness-config env becomes VISIBLE to the auth overlay in
// broker mode) is pinned by the tests above. The rank limb is pinned here.
//
// Mechanism: resolveAuthEnvOverlay injects harness-config env into opts.Env
// through the *api.StartOptions pointer, and Start later calls
// buildAgentEnv(finalScionCfg, opts.Env) at run.go:807 — where opts.Env is
// extraEnv and OVERRIDES scionCfg.Env. Template env is a separate
// lowest-precedence fill applied after all of these (ptone/scion#4524), so
// harness-config env outranks it in the container in both modes.

// rankProbeFixture stands up a tmp HOME in which the SAME key is declared by
// both the template (RANK_PROBE=from-template, the template env fill) and
// the settings harness config (RANK_PROBE=from-harness-config, which reaches
// opts.Env only via resolveAuthEnvOverlay). Returns the project .scion path.
func rankProbeFixture(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	_ = os.Chdir(tmpDir)

	originalHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", originalHome) })
	_ = os.Setenv("HOME", tmpDir)

	if old, ok := os.LookupEnv("RANK_PROBE"); ok {
		t.Cleanup(func() { _ = os.Setenv("RANK_PROBE", old) })
		_ = os.Unsetenv("RANK_PROBE")
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "claude-cfg")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"),
		[]byte("harness: claude\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Template declares RANK_PROBE — the template env layer, applied as the
	// final fill after buildAgentEnv (ptone/scion#4524).
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"),
		[]byte(`{"default_harness_config": "claude-cfg", "env": {"RANK_PROBE": "from-template"}}`), 0644)

	// Settings harness config declares the SAME key with a different value.
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: vertex
profiles:
  vertex:
    runtime: docker
harness_configs:
  claude-cfg:
    harness: claude
    env:
      RANK_PROBE: from-harness-config
runtimes:
  docker:
    type: docker
`), 0644)

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	return projectScionDir
}

// rankProbeRunStart runs Start against a mock runtime and returns the CONTAINER
// env as a map. It deliberately reads runtime.RunConfig.Env — the container
// path — and not opts.Env: opts.Env is the thing being written, so asserting
// there would pass trivially. This is the mistake the abandoned §3.3.1 filter
// made (it satisfied an opts.Env assertion while the container still received
// the capability), and AC16a was rewritten because of it.
func rankProbeRunStart(t *testing.T, projectScionDir string, brokerMode bool) map[string]string {
	t.Helper()
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			capturedConfig = cfg
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "rank-probe-agent",
		Template:    "default",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		BrokerMode:  brokerMode,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	envMap := make(map[string]string)
	for _, e := range capturedConfig.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	return envMap
}

// TestBrokerMode_HarnessConfigEnvOutranksTemplateEnv pins the RANK limb of
// Gap 3 (design §0.2 item 2).
//
// 🔴 IF YOU ARE ABOUT TO PUT THE INJECTION IN resolveAuthEnvOverlay BACK BEHIND
// A MODE CHECK, THIS TEST IS WHY YOU MAY NOT. The rank shift is an intended
// deliverable, not an accident of the presence fix. Restoring a
// `!opts.BrokerMode` conjunct flips this assertion back to "from-template".
func TestBrokerMode_HarnessConfigEnvOutranksTemplateEnv(t *testing.T) {
	projectScionDir := rankProbeFixture(t)

	envMap := rankProbeRunStart(t, projectScionDir, true /* brokerMode */)

	if got := envMap["RANK_PROBE"]; got != "from-harness-config" {
		t.Errorf("container env RANK_PROBE = %q, want %q\n"+
			"harness-config env must OUTRANK template env in broker mode: it is injected "+
			"into opts.Env, which is extraEnv at buildAgentEnv (run.go:807) and overrides "+
			"finalScionCfg.Env. Getting %q back means the injection is gated by mode again.",
			got, "from-harness-config", got)
	}
}

// TestLocalMode_HarnessConfigEnvOutranksTemplateEnv is the polarity control.
//
// NOTE: local mode is UNCHANGED by this commit — injection always ran here, so
// harness-config env already outranked template env before the fix. This test
// therefore asserts continuity, not a change. Its value is that it fails if a
// future "fix" inverts the precedence globally instead of only for broker mode,
// which the broker-mode test alone could not distinguish.
func TestLocalMode_HarnessConfigEnvOutranksTemplateEnv(t *testing.T) {
	projectScionDir := rankProbeFixture(t)

	envMap := rankProbeRunStart(t, projectScionDir, false /* local mode */)

	if got := envMap["RANK_PROBE"]; got != "from-harness-config" {
		t.Errorf("container env RANK_PROBE = %q, want %q (local mode precedence is unchanged)",
			got, "from-harness-config")
	}
}

// TestResolveAuthEnvOverlay_MutatesCallerOptsEnv closes I-4.
//
// resolveAuthEnvOverlay has a TWO-PART contract: it returns the auth overlay,
// AND it mutates opts.Env through the *api.StartOptions pointer. The second
// half is what feeds the container (buildAgentEnv's extraEnv), and it is what
// makes the rank limb above work. Every other test in this file asserts on the
// RETURNED overlay, so the reviewer was able to change the signature to a value
// receiver with the entire pkg/agent suite still green.
//
// 🔴 MEASURED, NOT ASSUMED — and the result is counter-intuitive. Flipping the
// signature to a value receiver fails ONLY the nil-map subtest below. The
// pre-existing-map subtest still PASSES, because Go maps are reference types:
// the copied StartOptions carries the same underlying map, so opts.Env[k] = v
// on the copy is still visible to the caller. The pointer is load-bearing for
// exactly one thing — the `opts.Env = make(...)` allocation on the nil path,
// which assigns to a FIELD and is lost on a copy.
//
// So the nil-map subtest is the one that closes I-4. Keep it. An I-4 test
// written only against a pre-populated map would be green under the very
// regression it is meant to catch.
func TestResolveAuthEnvOverlay_MutatesCallerOptsEnv(t *testing.T) {
	settings := g3TestSettings(map[string]string{
		"GOOGLE_CLOUD_PROJECT": "hc-project",
	})

	// Pins the injection contract. NOTE this subtest does NOT detect a value
	// receiver — see the comment above. It is here for the contract, not as
	// the I-4 guard.
	t.Run("injects into a pre-existing caller map", func(t *testing.T) {
		opts := api.StartOptions{
			Name:       "test-agent",
			BrokerMode: true,
			Env:        map[string]string{"EXISTING": "val"},
		}

		_, _ = resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

		if got := opts.Env["GOOGLE_CLOUD_PROJECT"]; got != "hc-project" {
			t.Errorf("CALLER's opts.Env[GOOGLE_CLOUD_PROJECT] = %q, want %q — the pointer "+
				"receiver is load-bearing: opts.Env is projected into the container via "+
				"buildAgentEnv(finalScionCfg, opts.Env). A value receiver compiles and "+
				"returns the right overlay, but the container gets nothing.", got, "hc-project")
		}
	})

	// 🔴 THIS is the I-4 guard. It is the only subtest that goes red on a value
	// receiver. Do not "simplify" it into the case above.
	t.Run("allocates a nil caller map", func(t *testing.T) {
		// opts.Env = make(...) inside the function assigns to a field, so it
		// reaches the caller only through the pointer. This is also the shape
		// Start actually uses: a hub-dispatched agent with no ResolvedEnv
		// arrives with opts.Env == nil.
		opts := api.StartOptions{Name: "test-agent", BrokerMode: true, Env: nil}

		_, _ = resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

		if opts.Env == nil {
			t.Fatal("CALLER's opts.Env is still nil — the allocation inside " +
				"resolveAuthEnvOverlay did not reach the caller")
		}
		if got := opts.Env["GOOGLE_CLOUD_PROJECT"]; got != "hc-project" {
			t.Errorf("CALLER's opts.Env[GOOGLE_CLOUD_PROJECT] = %q, want %q", got, "hc-project")
		}
	})
}
