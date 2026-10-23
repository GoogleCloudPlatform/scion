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
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// effectiveConfigFixture is a project whose template bundles a generic
// harness-config, with HOME and the working directory in a temp dir.
func effectiveConfigFixture(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)
	globalScionDir := filepath.Join(tmpDir, ".scion")
	tplDir := filepath.Join(globalScionDir, "templates", "alpha")
	hcDir := filepath.Join(tplDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: alpha-user\nimage: alpha-image:v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

// TestStart_ReportsEffectiveHarnessAndModel (WP1 slice 3 addendum v12
// section 5): both of Start's return paths, the listed container and the
// started-but-not-listed fallback, carry the harness and the effective
// model Start resolved.
func TestStart_ReportsEffectiveHarnessAndModel(t *testing.T) {
	for _, listed := range []bool{false, true} {
		name := "not listed"
		if listed {
			name = "listed"
		}
		t.Run(name, func(t *testing.T) {
			project := effectiveConfigFixture(t)
			var ran atomic.Bool
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					if listed && ran.Load() {
						return []api.AgentInfo{{ContainerID: "mock-id", Name: "eff-agent", Phase: "running", ContainerStatus: "Up"}}, nil
					}
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					ran.Store(true)
					return "mock-id", nil
				},
			}
			info, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
				Name: "eff-agent", Template: "alpha", ProjectPath: project, BrokerMode: true, NoAuth: true,
				Env: map[string]string{"SCION_MODEL": "model-x-1"},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if info.ContainerID != "mock-id" && listed {
				t.Fatalf("fixture: the listed path was not taken (%+v)", info)
			}
			if info.Harness != "generic" {
				t.Errorf("Harness = %q, want generic", info.Harness)
			}
			if info.Model != "model-x-1" {
				t.Errorf("Model = %q, want model-x-1", info.Model)
			}
		})
	}
}

// TestStartedTemplateHash: the template hash comes from the image
// provenance record when the agent has one, otherwise from agent info, and
// is unknown ("") for a template resolved by name.
func TestStartedTemplateHash(t *testing.T) {
	info := &api.ScionConfig{Info: &api.AgentInfo{TemplateHash: "sha256:info"}}
	if got := startedTemplateHash(&imageProvenance{TemplateHash: "sha256:prov"}, info); got != "sha256:prov" {
		t.Errorf("with provenance: %q", got)
	}
	if got := startedTemplateHash(&imageProvenance{}, info); got != "" {
		t.Errorf("provenance without a hash must not fall back to agent info: %q", got)
	}
	if got := startedTemplateHash(nil, info); got != "sha256:info" {
		t.Errorf("without provenance: %q", got)
	}
	if got := startedTemplateHash(nil, nil); got != "" {
		t.Errorf("nothing known: %q", got)
	}
}

// TestStart_AlreadyRunningReportsNoEffectiveConfig (review wp1s3-r1 R1,
// addendum v13 section 5): a start with no task that finds the agent
// already running resolves no configuration, so it returns the running
// entry with Harness, Model and TemplateHash unset, even if the listing
// carried them, and starts nothing.
func TestStart_AlreadyRunningReportsNoEffectiveConfig(t *testing.T) {
	project := effectiveConfigFixture(t)
	var ran atomic.Bool
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				ContainerID: "running-id", Name: "eff-agent", Phase: "running", RunID: "run-b",
				Harness: "listed-harness", Model: "listed-model", TemplateHash: "sha256:listed",
			}}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			ran.Store(true)
			return "mock-id", nil
		},
	}
	info, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name: "eff-agent", Template: "alpha", ProjectPath: project, BrokerMode: true, NoAuth: true,
		Env: map[string]string{"SCION_MODEL": "model-x-1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ran.Load() || info.ContainerID != "running-id" || info.RunID != "run-b" {
		t.Fatalf("fixture: the already-running path was not taken (ran=%v, %+v)", ran.Load(), info)
	}
	if info.Harness != "" || info.Model != "" || info.TemplateHash != "" {
		t.Errorf("an already-running start reports effective fields: harness %q model %q template hash %q", info.Harness, info.Model, info.TemplateHash)
	}
}
