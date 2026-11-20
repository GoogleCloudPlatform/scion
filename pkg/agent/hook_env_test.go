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
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestComputeHookWithheldEnv_WithBuildAgentEnv(t *testing.T) {
	bundleEnv := map[string]string{
		"BUNDLE_ONLY":            "from-bundle",
		"GITHUB_TOKEN":           "",
		"INLINE_WIN":             "from-bundle",
		"GOOGLE_CLOUD_PROJECT":   "bundle-proj",
		"EMPTY_AUTH_OR_SECRET":   "",
		"MY${UNSET_TEST_VAR}KEY": "expanded-key-val",
	}
	inlineEnv := map[string]string{
		"INLINE_WIN": "from-inline",
	}
	callerEnv := map[string]string{
		"GITHUB_TOKEN":  "ghp_from_hub",
		"OPERATOR_ONLY": "keep",
	}

	// Simulate ProvisionAgent merge: inline overrides bundle in ScionConfig.Env.
	mergedCfgEnv := make(map[string]string, len(bundleEnv)+len(inlineEnv))
	for k, v := range bundleEnv {
		mergedCfgEnv[k] = v
	}
	for k, v := range inlineEnv {
		mergedCfgEnv[k] = v
	}

	agentEnv, _, _, _ := buildAgentEnv(&api.ScionConfig{Env: mergedCfgEnv}, callerEnv, nil, true)

	got := computeHookWithheldEnv(bundleEnv, agentEnv, callerEnv, inlineEnv)
	want := []string{"BUNDLE_ONLY", "GOOGLE_CLOUD_PROJECT", "MYKEY"}
	if !slices.Equal(got, want) {
		t.Errorf("computeHookWithheldEnv() = %v, want %v (agentEnv=%v)", got, want, agentEnv)
	}
}

func TestComputeHookWithheldEnv_EmptyBundle(t *testing.T) {
	got := computeHookWithheldEnv(nil, []string{"FOO=bar"}, nil, nil)
	if len(got) != 0 {
		t.Errorf("computeHookWithheldEnv(nil, ...) = %v, want empty", got)
	}
}

func TestStart_HookWithheldEnv_EndToEnd(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hcConfig := strings.Join([]string{
		"harness: generic",
		"user: scion",
		"image: test-image:latest",
		"env:",
		"  BUNDLE_ONLY: from-bundle",
		"  GITHUB_TOKEN: \"\"",
		"  GOOGLE_CLOUD_PROJECT: bundle-proj",
		"  INLINE_WIN: from-bundle",
		"  \"MY${UNSET_TEST_VAR}KEY\": from-expanded-key",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hcConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	settings := "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var captured runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return nil, nil
		},
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	})

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:          "hook-env-test",
		ProjectPath:   projectScionDir,
		Template:      "default",
		HarnessConfig: "test-harness",
		BrokerMode:    true,
		NoAuth:        true,
		Env: map[string]string{
			"GITHUB_TOKEN":  "ghp_from_hub",
			"OPERATOR_ONLY": "operator-val",
		},
		InlineConfig: &api.ScionConfig{
			Env: map[string]string{
				"INLINE_WIN": "from-inline",
			},
		},
		ResolvedSecrets: []api.ResolvedSecret{
			{Type: "environment", Target: "GOOGLE_CLOUD_PROJECT", Value: "secret-proj"},
		},
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	wantWithheld := []string{"BUNDLE_ONLY", "GOOGLE_CLOUD_PROJECT", "MYKEY"}
	if !slices.Equal(captured.HookWithheldEnv, wantWithheld) {
		t.Errorf("RunConfig.HookWithheldEnv = %v, want %v (RunConfig.Env=%v)", captured.HookWithheldEnv, wantWithheld, captured.Env)
	}

	envMap := make(map[string]string, len(captured.Env))
	for _, e := range captured.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			envMap[k] = v
		}
	}
	if envMap["BUNDLE_ONLY"] != "from-bundle" {
		t.Errorf("RunConfig.Env[BUNDLE_ONLY] = %q, want %q", envMap["BUNDLE_ONLY"], "from-bundle")
	}
	if envMap["GOOGLE_CLOUD_PROJECT"] != "bundle-proj" {
		t.Errorf("RunConfig.Env[GOOGLE_CLOUD_PROJECT] = %q, want %q", envMap["GOOGLE_CLOUD_PROJECT"], "bundle-proj")
	}
	if envMap["GITHUB_TOKEN"] != "ghp_from_hub" {
		t.Errorf("RunConfig.Env[GITHUB_TOKEN] = %q, want %q", envMap["GITHUB_TOKEN"], "ghp_from_hub")
	}
	if envMap["INLINE_WIN"] != "from-inline" {
		t.Errorf("RunConfig.Env[INLINE_WIN] = %q, want %q", envMap["INLINE_WIN"], "from-inline")
	}
	if envMap["MYKEY"] != "from-expanded-key" {
		t.Errorf("RunConfig.Env[MYKEY] = %q, want %q", envMap["MYKEY"], "from-expanded-key")
	}
}
