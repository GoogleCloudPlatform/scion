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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
)

// TestWorkloadChildEnv_Golden verifies that for an agent combining bundle,
// settings-hc, template, inline, and hub env, the workload environment
// produced by supervisor.ChildEnv carries every expected key with only the
// intended §3.5 differences (no SCION_HOOK_WITHHELD_ENV marker, refused
// SCION_* keys dropped, grandfathered hints rewritten, literal bundle values).
func TestWorkloadChildEnv_Golden(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("HOST_SECRET", "must-not-leak")

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hcConfig := strings.Join([]string{
		"harness: generic",
		"user: scion",
		"image: test-image:latest",
		"command:",
		"  base: [\"echo\"]",
		"env:",
		"  BUNDLE_ONLY: from-bundle",
		"  BUNDLE_LITERAL: \"${HOST_SECRET}/literal\"",
		"  BUNDLE_EMPTY_HOST: \"\"",
		"  SHARED_LAYER: from-bundle",
		"  SCION_EXTRA_PATH: /evil/bundle",
		"  SCION_LIVENESS_HARNESS_ROOT_DEPTH: \"2\"",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hcConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{
		"default_harness_config": "test-harness",
		"env": {
			"TPL_ONLY": "from-template",
			"SHARED_LAYER": "from-template"
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	settings := strings.Join([]string{
		"schema_version: \"1\"",
		"active_profile: local",
		"profiles:",
		"  local:",
		"    runtime: docker",
		"harness_configs:",
		"  test-harness:",
		"    env:",
		"      SETTINGS_ONLY: from-settings",
		"      SHARED_LAYER: from-settings",
		"",
	}, "\n")
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

	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:          "golden-workload",
		ProjectPath:   projectScionDir,
		Template:      "default",
		HarnessConfig: "test-harness",
		BrokerMode:    true,
		NoAuth:        true,
		Env: map[string]string{
			"HUB_ONLY":     "from-hub",
			"SHARED_LAYER": "from-hub",
		},
		InlineConfig: &api.ScionConfig{
			Env: map[string]string{
				"INLINE_ONLY": "from-inline",
			},
		},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Simulate container env assembly (RunConfig.Env + FallbackEnv entries for
	// absent keys, with SCION_HOOK_WITHHELD_ENV already unset by initenv.Load)
	// and pass it to supervisor.ChildEnv.
	containerEnv := append([]string{"PATH=/usr/bin", "HOME=/root"}, captured.Env...)
	existing := runEnvMap(containerEnv)
	for k, v := range captured.FallbackEnv {
		if _, ok := existing[k]; !ok {
			containerEnv = append(containerEnv, k+"="+v)
		}
	}

	childEnvSlice, err := supervisor.ChildEnv(supervisor.Config{
		UID:      1000,
		GID:      1000,
		Username: "scion",
	}, containerEnv)
	if err != nil {
		t.Fatalf("ChildEnv failed: %v", err)
	}
	childEnv := runEnvMap(childEnvSlice)

	wantSubset := map[string]string{
		"BUNDLE_ONLY":                   "from-bundle",
		"BUNDLE_LITERAL":                "${HOST_SECRET}/literal",
		"SETTINGS_ONLY":                 "from-settings",
		"TPL_ONLY":                      "from-template",
		"INLINE_ONLY":                   "from-inline",
		"HUB_ONLY":                      "from-hub",
		"SHARED_LAYER":                  "from-hub",
		envpolicy.HintLivenessRootDepth: "2",
	}
	for k, wantV := range wantSubset {
		if gotV := childEnv[k]; gotV != wantV {
			t.Errorf("ChildEnv[%s] = %q, want %q", k, gotV, wantV)
		}
	}
	for _, absent := range []string{
		"BUNDLE_EMPTY_HOST",
		"SCION_EXTRA_PATH",
		"SCION_LIVENESS_HARNESS_ROOT_DEPTH",
		envpolicy.HookWithheldEnvVar,
	} {
		if v, ok := childEnv[absent]; ok {
			t.Errorf("ChildEnv unexpectedly contains %s=%q", absent, v)
		}
	}
}
