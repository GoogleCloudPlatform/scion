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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
			if !info.ConfigResolved {
				t.Error("ConfigResolved = false on a resolving return path")
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
				ContainerID: "running-id", Name: "eff-agent", Phase: "running", RunID: "run-b", ProjectID: "proj-eff",
				Harness: "listed-harness", Model: "listed-model", TemplateHash: "sha256:listed",
				ConfigResolved: true,
			}}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			ran.Store(true)
			return "mock-id", nil
		},
	}
	info, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name: "eff-agent", Template: "alpha", ProjectPath: project, BrokerMode: true, NoAuth: true,
		// The pre-start match is project-scoped: the listed entry must be
		// this project's.
		Env: map[string]string{"SCION_MODEL": "model-x-1", "SCION_PROJECT_ID": "proj-eff"},
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
	if info.ConfigResolved {
		t.Error("ConfigResolved = true on the already-running return")
	}
}

// startEffective starts name with opts on a fresh mock runtime and returns
// the AgentInfo and the container's SCION_HARNESS ("" and false when the
// container env does not set it), failing on a start error.
func startEffective(t *testing.T, opts api.StartOptions) (*api.AgentInfo, string, bool) {
	t.Helper()
	var captured runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	}
	opts.BrokerMode, opts.NoAuth = true, true
	info, err := NewManager(mockRT).Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, kv := range captured.Env {
		if v, ok := strings.CutPrefix(kv, "SCION_HARNESS="); ok {
			return info, v, true
		}
	}
	return info, "", false
}

// TestStart_EffectiveHarnessIsTheResolvedHarness (addendum v14 section 5;
// vm-deploy and token-analyst-2 ACK 07:19-07:20): the effective harness is
// Name() of the harness Start resolved and launched with, not the stored
// harness name. A start that switches harness-config reports the new
// harness although the agent's stored config still names the old one.
func TestStart_EffectiveHarnessIsTheResolvedHarness(t *testing.T) {
	project := effectiveConfigFixture(t)
	other := filepath.Join(os.Getenv("HOME"), ".scion", "templates", "alpha", "harness-configs", "other-hc")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "config.yaml"), []byte("harness: switched-h\nuser: alpha-user\nimage: alpha-image:v1\ncapabilities: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, _, _ := startEffective(t, api.StartOptions{Name: "eff-switch", Template: "alpha", ProjectPath: project})
	if first.Harness != "generic" {
		t.Fatalf("first start: Harness = %q, want generic", first.Harness)
	}
	switched, containerHarness, _ := startEffective(t, api.StartOptions{Name: "eff-switch", Template: "alpha", ProjectPath: project, HarnessConfig: "other-hc"})
	if switched.HarnessConfig != "other-hc" {
		t.Fatalf("fixture: the start did not switch harness-config (%q)", switched.HarnessConfig)
	}
	if switched.Harness != "switched-h" || !switched.ConfigResolved {
		t.Errorf("switched start: Harness = %q, ConfigResolved = %v; want the resolved harness switched-h", switched.Harness, switched.ConfigResolved)
	}
	// Since upstream #4281 (the fix for ptone/scion#5946) the container's
	// SCION_HARNESS is the launched harness too, so it agrees with the
	// effective harness the hub records.
	if containerHarness != switched.Harness {
		t.Errorf("container SCION_HARNESS = %q; want the launched harness %q (#4281)", containerHarness, switched.Harness)
	}
}

// TestStart_EffectiveHarnessGenericFallback (token-analyst-2 fixture 2): a
// harness-config with no declarative metadata resolves to the plain
// Generic harness (harness.Resolve's legacy fallback), whatever harness it
// names. The effective harness is the one that ran, "generic", not the
// stored name the config declared.
func TestStart_EffectiveHarnessGenericFallback(t *testing.T) {
	project := effectiveConfigFixture(t)
	hc := filepath.Join(os.Getenv("HOME"), ".scion", "templates", "alpha", "harness-configs", "test-harness", "config.yaml")
	if err := os.WriteFile(hc, []byte("harness: named-h\nuser: alpha-user\nimage: alpha-image:v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, containerHarness, _ := startEffective(t, api.StartOptions{Name: "eff-fallback", Template: "alpha", ProjectPath: project})
	if info.HarnessConfig != "test-harness" {
		t.Fatalf("fixture: harness-config %q", info.HarnessConfig)
	}
	if info.Harness != "generic" || !info.ConfigResolved {
		t.Errorf("Generic fallback: Harness = %q, ConfigResolved = %v; want generic (the resolved Generic implementation), true", info.Harness, info.ConfigResolved)
	}
	// Since upstream #4281 the container's SCION_HARNESS is the launched
	// harness (generic), not the declared name.
	if containerHarness != info.Harness {
		t.Errorf("container SCION_HARNESS = %q; want the launched harness %q (#4281)", containerHarness, info.Harness)
	}
}

// TestStart_EffectiveHarnessLegacyNoHarnessConfig (review wp1s3-r3 O-2): a
// legacy agent whose stored config names neither a harness nor a
// harness-config, with no template or settings default, starts on
// harness.New's Generic (the no-harness-config branch). It reports the
// resolved Generic as its effective harness, with the marker set. Since
// upstream #4281 the container's SCION_HARNESS is that launched harness too.
func TestStart_EffectiveHarnessLegacyNoHarnessConfig(t *testing.T) {
	project := effectiveConfigFixture(t)
	if first, _, _ := startEffective(t, api.StartOptions{Name: "eff-legacy", Template: "alpha", ProjectPath: project}); first.Harness != "generic" {
		t.Fatalf("fixture: first start Harness %q", first.Harness)
	}
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, ".scion", "templates", "alpha", "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"),
		[]byte("schema_version: \"1\"\ndefault_harness_config: \"\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stored := filepath.Join(project, "agents", "eff-legacy", "scion-agent.json")
	data, err := os.ReadFile(stored)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "harness")
	delete(raw, "harness_config")
	delete(raw, "default_harness_config")
	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored, out, 0o644); err != nil {
		t.Fatal(err)
	}

	info, containerHarness, set := startEffective(t, api.StartOptions{Name: "eff-legacy", Template: "alpha", ProjectPath: project, Image: "img:legacy"})
	if info.HarnessConfig != "" {
		t.Fatalf("fixture: a harness-config resolved (%q)", info.HarnessConfig)
	}
	if info.Harness != "generic" || !info.ConfigResolved {
		t.Errorf("legacy start: Harness = %q, ConfigResolved = %v; want generic, true", info.Harness, info.ConfigResolved)
	}
	// Since upstream #4281 the container gets the launched harness even
	// when no harness is stored.
	if !set || containerHarness != info.Harness {
		t.Errorf("container SCION_HARNESS = %q (set %v); want the launched harness %q (#4281)", containerHarness, set, info.Harness)
	}
}

// TestStart_FailedResolutionReportsNothing (token-analyst-2 fixture 3): a
// start whose harness resolution fails (an unusable provisioner) returns
// no AgentInfo at all, so there is no echo and no evidence to invent.
func TestStart_FailedResolutionReportsNothing(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-bad", unusableBuiltinHC)
	runs := 0
	info, err := policyTestManager(&runs).Start(context.Background(), api.StartOptions{
		Name: "fresh", ProjectPath: e.scion, HarnessConfig: "hc-bad", NoAuth: true,
	})
	if err == nil || info != nil {
		t.Fatalf("Start = (%+v, %v); want an error and no AgentInfo", info, err)
	}
	if runs != 0 {
		t.Errorf("container ran %d times; want none", runs)
	}
}
