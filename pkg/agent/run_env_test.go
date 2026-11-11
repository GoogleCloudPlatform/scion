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
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestBuildAgentEnv(t *testing.T) {
	// Setup host env for inheritance test
	t.Setenv("INHERITED_KEY", "inherited-value")

	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"NORMAL_KEY":     "normal-value",
			"INHERITED_KEY":  "${INHERITED_KEY}",
			"EMPTY_CFG_KEY":  "",               // Should be omitted
			"OVERRIDDEN_KEY": "original-value", // Should be omitted because of override
		},
	}

	extraEnv := map[string]string{
		"EXTRA_KEY":       "extra-value",
		"OVERRIDDEN_KEY":  "", // Should cause omission
		"EMPTY_EXTRA_KEY": "", // Should be omitted
	}

	env, warnings, missingKeys, _ := buildAgentEnv(scionCfg, extraEnv, nil, false)

	expected := map[string]string{
		"NORMAL_KEY":    "normal-value",
		"INHERITED_KEY": "inherited-value",
		"EXTRA_KEY":     "extra-value",
	}

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if len(env) != len(expected) {
		t.Errorf("expected %d env vars, got %d: %v", len(expected), len(env), env)
	}

	if len(warnings) != 3 {
		t.Errorf("expected 3 warnings, got %d: %v", len(warnings), warnings)
	}

	if len(missingKeys) != 3 {
		t.Errorf("expected 3 missing keys, got %d: %v", len(missingKeys), missingKeys)
	}

	for k, v := range expected {
		if envMap[k] != v {
			t.Errorf("expected env[%s] = %q, got %q", k, v, envMap[k])
		}
	}

	// Explicitly check for omitted keys
	omitted := []string{"EMPTY_CFG_KEY", "OVERRIDDEN_KEY", "EMPTY_EXTRA_KEY"}
	for _, k := range omitted {
		if _, ok := envMap[k]; ok {
			t.Errorf("expected key %s to be omitted, but it was present", k)
		}
	}
}

func TestBuildAgentEnv_MissingKeysReturned(t *testing.T) {
	// Verify that buildAgentEnv returns the names of keys that could not
	// be resolved, so the caller can treat them as errors.
	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"GOOD_KEY":    "good-value",
			"MISSING_ONE": "",
			"MISSING_TWO": "",
		},
	}

	env, _, missingKeys, _ := buildAgentEnv(scionCfg, nil, nil, false)

	if len(env) != 1 {
		t.Errorf("expected 1 env var, got %d: %v", len(env), env)
	}
	if len(missingKeys) != 2 {
		t.Fatalf("expected 2 missing keys, got %d: %v", len(missingKeys), missingKeys)
	}

	sort.Strings(missingKeys)
	if missingKeys[0] != "MISSING_ONE" || missingKeys[1] != "MISSING_TWO" {
		t.Errorf("unexpected missing keys: %v", missingKeys)
	}
}

func TestStartBrokerMode_EmptyEnvNotFatal(t *testing.T) {
	// In broker mode, empty env vars from scion-agent.json that the hub
	// didn't resolve (e.g., profile-level keys irrelevant to the selected
	// harness) should produce warnings but NOT block agent start.
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

	var capturedEnv []string
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedEnv = config.Env
			return "mock-id", nil
		},
	}

	// Write scion-agent.json with empty env vars (simulating profile-level
	// passthrough markers that are irrelevant to the selected harness)
	agentDir := filepath.Join(projectScionDir, "agents", "broker-test")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
		"harness": "generic",
		"env": {
			"GEMINI_API_KEY": "",
			"OPENAI_API_KEY": "",
			"GOOD_KEY": "good-value"
		}
	}`), 0644)

	mgr := NewManager(mockRT)

	// In broker mode, empty env vars should NOT cause an error
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "broker-test",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Env: map[string]string{
			"GEMINI_API_KEY": "resolved-from-hub",
		},
	})
	if err != nil {
		t.Fatalf("Start in BrokerMode should not fail on empty env vars, got: %v", err)
	}

	// Verify GEMINI_API_KEY was resolved from hub env
	envMap := make(map[string]string)
	for _, e := range capturedEnv {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	if envMap["GEMINI_API_KEY"] != "resolved-from-hub" {
		t.Errorf("GEMINI_API_KEY = %q, want %q", envMap["GEMINI_API_KEY"], "resolved-from-hub")
	}
	if envMap["GOOD_KEY"] != "good-value" {
		t.Errorf("GOOD_KEY = %q, want %q", envMap["GOOD_KEY"], "good-value")
	}
	// OPENAI_API_KEY should be omitted (empty and not resolved)
	if _, ok := envMap["OPENAI_API_KEY"]; ok {
		t.Error("expected OPENAI_API_KEY to be omitted, but it was present")
	}
}

func TestStartLocalMode_EmptyEnvIsFatal(t *testing.T) {
	// In local (non-broker) mode, empty env vars that can't be resolved
	// should still cause a fatal error.
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

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", "local-test")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
		"harness": "generic",
		"env": {
			"MISSING_KEY": ""
		}
	}`), 0644)

	mgr := NewManager(mockRT)

	// In local mode (BrokerMode=false), empty env vars should be fatal
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "local-test",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to fail on empty env vars in local mode")
	}
	if !strings.Contains(err.Error(), "MISSING_KEY") {
		t.Errorf("expected error to mention MISSING_KEY, got: %v", err)
	}
}

func TestBuildAgentEnv_EmptyValuePassthrough(t *testing.T) {
	// When a config env entry has an empty value (no ${VAR} reference),
	// buildAgentEnv should implicitly look up the host env var of the same name.
	t.Setenv("HOST_AVAILABLE_KEY", "host-value")

	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"HOST_AVAILABLE_KEY": "", // empty → should pick up "host-value" from host
			"HOST_MISSING_KEY":   "", // empty → host doesn't have it → should be omitted
			"EXPLICIT_VALUE":     "explicit",
		},
	}

	env, warnings, missingKeys, _ := buildAgentEnv(scionCfg, nil, nil, false)

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if envMap["HOST_AVAILABLE_KEY"] != "host-value" {
		t.Errorf("expected HOST_AVAILABLE_KEY = %q, got %q", "host-value", envMap["HOST_AVAILABLE_KEY"])
	}
	if envMap["EXPLICIT_VALUE"] != "explicit" {
		t.Errorf("expected EXPLICIT_VALUE = %q, got %q", "explicit", envMap["EXPLICIT_VALUE"])
	}
	if _, ok := envMap["HOST_MISSING_KEY"]; ok {
		t.Error("expected HOST_MISSING_KEY to be omitted, but it was present")
	}

	// Only HOST_MISSING_KEY should produce a warning
	if len(warnings) != 1 {
		t.Errorf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if len(missingKeys) != 1 {
		t.Errorf("expected 1 missing key, got %d: %v", len(missingKeys), missingKeys)
	}
}

func TestBuildAgentEnv_ScionExtraPath(t *testing.T) {
	// SCION_EXTRA_PATH should pass through buildAgentEnv as a normal literal
	// env var (no special expansion needed since the value is a literal
	// container path like /home/scion/bin).
	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"SCION_EXTRA_PATH": "/home/scion/bin",
		},
	}

	env, warnings, _, _ := buildAgentEnv(scionCfg, nil, nil, false)

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if got, ok := envMap["SCION_EXTRA_PATH"]; !ok {
		t.Error("expected SCION_EXTRA_PATH to be present in env")
	} else if got != "/home/scion/bin" {
		t.Errorf("SCION_EXTRA_PATH = %q, want %q", got, "/home/scion/bin")
	}

	// No warnings expected for a literal value
	for _, w := range warnings {
		if strings.Contains(w, "SCION_EXTRA_PATH") {
			t.Errorf("unexpected warning for SCION_EXTRA_PATH: %s", w)
		}
	}
}

func TestBuildAgentEnv_HubEndpointOverride(t *testing.T) {
	t.Run("scion config hub endpoint overrides extraEnv", func(t *testing.T) {
		scionCfg := &api.ScionConfig{
			Hub: &api.AgentHubConfig{
				Endpoint: "https://tunnel.example.com",
			},
		}

		// Simulate what Start() does: set hub endpoint in opts.Env from broker,
		// then override with scion config hub endpoint.
		extraEnv := map[string]string{
			"SCION_HUB_ENDPOINT": "http://localhost:9810",
			"SCION_HUB_URL":      "http://localhost:9810",
		}

		// Apply the override logic from Start()
		if scionCfg.Hub != nil && scionCfg.Hub.Endpoint != "" {
			extraEnv["SCION_HUB_ENDPOINT"] = scionCfg.Hub.Endpoint
			extraEnv["SCION_HUB_URL"] = scionCfg.Hub.Endpoint
		}

		env, _, _, _ := buildAgentEnv(scionCfg, extraEnv, nil, false)

		envMap := make(map[string]string)
		for _, e := range env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}

		if got := envMap["SCION_HUB_ENDPOINT"]; got != "https://tunnel.example.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://tunnel.example.com', got %q", got)
		}
		if got := envMap["SCION_HUB_URL"]; got != "https://tunnel.example.com" {
			t.Errorf("expected SCION_HUB_URL='https://tunnel.example.com', got %q", got)
		}
	})

	t.Run("no hub config preserves extraEnv", func(t *testing.T) {
		scionCfg := &api.ScionConfig{}
		extraEnv := map[string]string{
			"SCION_HUB_ENDPOINT": "https://hub.example.com",
			"SCION_HUB_URL":      "https://hub.example.com",
		}

		env, _, _, _ := buildAgentEnv(scionCfg, extraEnv, nil, false)

		envMap := make(map[string]string)
		for _, e := range env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}

		if got := envMap["SCION_HUB_ENDPOINT"]; got != "https://hub.example.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com', got %q", got)
		}
	})
}

func TestScionCreatorEnvVar(t *testing.T) {
	t.Run("SCION_CREATOR is set from OS user when not present", func(t *testing.T) {
		env := make(map[string]string)
		// Simulate the logic from Start(): if SCION_CREATOR is not set, set it from os/user
		if _, ok := env["SCION_CREATOR"]; !ok {
			if u, err := user.Current(); err == nil {
				env["SCION_CREATOR"] = u.Username
			}
		}

		if env["SCION_CREATOR"] == "" {
			t.Error("expected SCION_CREATOR to be set from OS user")
		}

		u, _ := user.Current()
		if env["SCION_CREATOR"] != u.Username {
			t.Errorf("expected SCION_CREATOR = %q, got %q", u.Username, env["SCION_CREATOR"])
		}
	})

	t.Run("SCION_CREATOR is preserved when already set", func(t *testing.T) {
		env := map[string]string{
			"SCION_CREATOR": "hub-user@example.com",
		}
		// Simulate the logic from Start(): if SCION_CREATOR is not set, set it from os/user
		if _, ok := env["SCION_CREATOR"]; !ok {
			if u, err := user.Current(); err == nil {
				env["SCION_CREATOR"] = u.Username
			}
		}

		if env["SCION_CREATOR"] != "hub-user@example.com" {
			t.Errorf("expected SCION_CREATOR = %q, got %q", "hub-user@example.com", env["SCION_CREATOR"])
		}
	})
}

// TestBuildAgentEnv_AuthoritativeMetadataModeWinsOverConfigEnv pins the seam
// between the broker's authoritative SCION_METADATA_MODE (opts.Env, passed
// here as extraEnv — see AgentManager.Start, which calls
// buildAgentEnv(finalScionCfg, opts.Env, opts.BrokerMode)) and RunConfig.Env,
// the field the docker/podman/k8s runtimes treat as already-decided (see the
// collision-skip in pkg/runtime). extraEnv must win over a conflicting
// template/harness-level scionCfg.Env value, the same precedence every other
// opts.Env key gets, so a template cannot re-decide the mode that reaches
// RunConfig.Env. brokerMode is true here to match the broker-mode start this
// seam is pinning; it is otherwise orthogonal to this test's assertion.
func TestBuildAgentEnv_AuthoritativeMetadataModeWinsOverConfigEnv(t *testing.T) {
	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
		},
	}
	extraEnv := map[string]string{
		"SCION_METADATA_MODE": "block",
	}

	env, _, _, _ := buildAgentEnv(scionCfg, extraEnv, nil, true)

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if got, want := envMap["SCION_METADATA_MODE"], "block"; got != want {
		t.Errorf("SCION_METADATA_MODE = %q, want %q (opts.Env must win over scionCfg.Env)", got, want)
	}
}
