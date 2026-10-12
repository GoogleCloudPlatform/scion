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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestStart_RestartAfterInlineOnlyCreate_ExplicitInlineSurvivesWithoutLiveInlineConfig
// pins ptone/scion#2156: an agent created with an inline-config image/pull
// policy and no template pin has that explicit choice persisted to
// agent-info.json (AgentInfo.ExplicitImage / .ExplicitImagePullPolicy). A
// local restart with no --config (opts.InlineConfig nil, unlike a
// hub-dispatched restart, which resends AppliedConfig.InlineConfig) must
// still honour it — not silently fall back to a Hub settings value, which is
// a lower tier than an explicit override, live or persisted.
func TestStart_RestartAfterInlineOnlyCreate_ExplicitInlineSurvivesWithoutLiveInlineConfig(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
    image_pull_policy: Always
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	// Create with an explicit inline image and pull policy; no template pin.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		InlineConfig: &api.ScionConfig{
			Image:      "inline-pinned:v3",
			Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "Never"},
		},
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "inline-pinned:v3" {
		t.Fatalf("precondition failed: create-time image = %q, want inline-pinned:v3", capturedConfig.Image)
	}

	// Restart with no --config: opts.InlineConfig is nil, as on a plain local
	// `scion start <existing-agent>`.
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "inline-pinned:v3" {
		t.Errorf("restart image = %q, want the create-time explicit inline image %q (not the settings value)",
			capturedConfig.Image, "inline-pinned:v3")
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("restart imagePullPolicy = %q, want the create-time explicit inline value %q (not the settings value)",
			gotPolicy, "Never")
	}
}

// TestStart_RestartWithHarnessAuthOnly_ExplicitInlineImageStillSurvives pins
// ptone/scion#2156: a restart request can make opts.InlineConfig-derived
// startInlineConfig non-nil for reasons that have nothing to do with image —
// --harness-auth alone does this — and that must not defeat the persisted
// create-time inline image/pull-policy fallback. The fallback is keyed on
// whether the CURRENT request's inline config sets that specific field, not
// on whether some inline config object exists at all.
func TestStart_RestartWithHarnessAuthOnly_ExplicitInlineImageStillSurvives(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
    image_pull_policy: Always
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	// Create with an explicit inline image and pull policy; no template pin.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		InlineConfig: &api.ScionConfig{
			Image:      "inline-pinned:v3",
			Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "Never"},
		},
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "inline-pinned:v3" {
		t.Fatalf("precondition failed: create-time image = %q, want inline-pinned:v3", capturedConfig.Image)
	}

	// Restart with --harness-auth only: opts.HarnessAuth makes run.go's
	// startInlineConfig non-nil (used for the GetAgent merge), but
	// opts.InlineConfig itself is nil — the create-time inline image/pull
	// policy must still apply, not the settings values.
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		HarnessAuth: "api-key",
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "inline-pinned:v3" {
		t.Errorf("restart (--harness-auth only) image = %q, want the create-time explicit inline image %q",
			capturedConfig.Image, "inline-pinned:v3")
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("restart (--harness-auth only) imagePullPolicy = %q, want the create-time explicit inline value %q",
			gotPolicy, "Never")
	}
}

// TestStart_RestartWithUnrelatedInlineConfigField_ExplicitInlineSurvivesPerField
// pins ptone/scion#2156: the explicit tier's fallback to the recorded
// create-time inline value is keyed per field (does THIS request's inline
// config set THIS field), not on whether any inline config object is
// present at all. A restart whose --config sets only an unrelated field
// (e.g. --model) must not drop the create-time inline image/pull-policy
// pin, and a restart whose --config sets only the image must still fall
// back to the recorded pull policy independently (field independence).
func TestStart_RestartWithUnrelatedInlineConfigField_ExplicitInlineSurvivesPerField(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
    image_pull_policy: Always
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	// Create with an explicit inline image and pull policy; no template pin.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		InlineConfig: &api.ScionConfig{
			Image:      "inline-pinned:v3",
			Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "Never"},
		},
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "inline-pinned:v3" {
		t.Fatalf("precondition failed: create-time image = %q, want inline-pinned:v3", capturedConfig.Image)
	}

	t.Run("restart with only an unrelated inline field (--model) keeps both recorded values", func(t *testing.T) {
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:         "test-agent",
			ProjectPath:  projectScionDir,
			BrokerMode:   true,
			NoAuth:       true,
			InlineConfig: &api.ScionConfig{Model: "some-model"},
		})
		if err != nil {
			t.Fatalf("restart Start failed: %v", err)
		}
		if capturedConfig.Image != "inline-pinned:v3" {
			t.Errorf("image = %q, want the recorded inline image %q", capturedConfig.Image, "inline-pinned:v3")
		}
		gotPolicy := ""
		if capturedConfig.Kubernetes != nil {
			gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
		}
		if gotPolicy != "Never" {
			t.Errorf("imagePullPolicy = %q, want the recorded inline value %q", gotPolicy, "Never")
		}
	})

	t.Run("restart with only inline image set falls back to the recorded pull policy independently", func(t *testing.T) {
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:         "test-agent",
			ProjectPath:  projectScionDir,
			BrokerMode:   true,
			NoAuth:       true,
			InlineConfig: &api.ScionConfig{Image: "other:v9"},
		})
		if err != nil {
			t.Fatalf("restart Start failed: %v", err)
		}
		if capturedConfig.Image != "other:v9" {
			t.Errorf("image = %q, want the current request's inline image %q", capturedConfig.Image, "other:v9")
		}
		gotPolicy := ""
		if capturedConfig.Kubernetes != nil {
			gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
		}
		if gotPolicy != "Never" {
			t.Errorf("imagePullPolicy = %q, want the recorded inline value %q (image and pull policy fall back independently)",
				gotPolicy, "Never")
		}
	})
}

// TestStart_RestartAfterTemplateImageEdited_LiveTemplateWinsOverCreateTimeSnapshot
// pins ptone/scion#2156: the value ProvisionAgent persists to
// agent-info.json for the explicit tier's restart fallback must be the
// INLINE config's contribution only, never the template's — a template is
// re-read live on every Start, so persisting its create-time value would
// let a stale template snapshot outrank the CURRENT template on every later
// restart. Here the agent is created with only a template image (no inline
// config at all), the template file is then edited, and a restart must
// pick up the new template value.
func TestStart_RestartAfterTemplateImageEdited_LiveTemplateWinsOverCreateTimeSnapshot(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplPath := filepath.Join(globalScionDir, "templates", "default", "scion-agent.json")
	_ = os.MkdirAll(filepath.Dir(tplPath), 0755)
	_ = os.WriteFile(tplPath, []byte(`{"default_harness_config": "test-harness", "image": "template-pinned:v1"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	// Create: template pins v1, no inline config at all.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "template-pinned:v1" {
		t.Fatalf("precondition failed: create-time image = %q, want template-pinned:v1", capturedConfig.Image)
	}

	// Edit the template to pin v2, then restart with no --config.
	_ = os.WriteFile(tplPath, []byte(`{"default_harness_config": "test-harness", "image": "template-pinned:v2"}`), 0644)

	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "template-pinned:v2" {
		t.Errorf("restart image = %q, want the current template's image %q (not a create-time snapshot)",
			capturedConfig.Image, "template-pinned:v2")
	}
}

// TestStart_RestartAfterTemplateDeleted_FallsBackToRecordedImage pins
// ptone/scion#2156: if an agent's template can no longer be resolved at all
// on a local restart (renamed or deleted since the agent was created — not
// merely "no image pin"), the restart must still run the image/pull-policy
// recorded at an earlier provision, matching pre-existing (origin/main)
// behaviour, rather than silently falling through to Hub settings or the
// file default just because the template disappeared.
func TestStart_RestartAfterTemplateDeleted_FallsBackToRecordedImage(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "custom")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness", "image": "tpl:v1", "kubernetes": {"imagePullPolicy": "Never"}}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	// Create from the "custom" template: image tpl:v1, pull policy Never.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		Template:    "custom",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "tpl:v1" {
		t.Fatalf("precondition failed: create-time image = %q, want tpl:v1", capturedConfig.Image)
	}

	// Delete the template entirely — renamed or removed since creation.
	if err := os.RemoveAll(tplDir); err != nil {
		t.Fatalf("failed to delete template dir: %v", err)
	}

	agentInfo, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "tpl:v1" {
		t.Errorf("restart image = %q, want the recorded image %q (matching origin/main's behaviour when the template is gone)",
			capturedConfig.Image, "tpl:v1")
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("restart imagePullPolicy = %q, want the recorded value %q", gotPolicy, "Never")
	}
	if agentInfo == nil || !strings.Contains(strings.Join(agentInfo.Warnings, "\n"), "could not be found") {
		t.Errorf("expected a warning about the unresolvable template, got warnings=%v", agentInfoWarningsOrNil(agentInfo))
	}

	// A live inline config must still outrank the unresolvable-template
	// snapshot — the same as it outranks a live template — for image and
	// pull policy independently. This is the ordering half of the
	// unresolvable-template fallback: the snapshot is applied BEFORE the
	// inline tier, not after, so it never gets a chance to override a
	// current request's own inline values.
	agentInfo, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		InlineConfig: &api.ScionConfig{
			Image:      "inline:v2",
			Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "IfNotPresent"},
		},
	})
	if err != nil {
		t.Fatalf("third Start (with inline config) failed: %v", err)
	}
	if capturedConfig.Image != "inline:v2" {
		t.Errorf("image with a live inline config = %q, want the inline value %q (inline must outrank the unresolvable-template snapshot)",
			capturedConfig.Image, "inline:v2")
	}
	gotPolicy = ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "IfNotPresent" {
		t.Errorf("imagePullPolicy with a live inline config = %q, want the inline value %q", gotPolicy, "IfNotPresent")
	}
	// Both fields came from the live inline config, not the snapshot, so the
	// "recorded at an earlier provision" warning must not appear.
	if agentInfo != nil && strings.Contains(strings.Join(agentInfo.Warnings, "\n"), "recorded at an earlier provision") {
		t.Errorf("did not expect a 'recorded at an earlier provision' warning when inline overrides both fields, got warnings=%v", agentInfo.Warnings)
	}

	// A dispatch --image (opts.Image) overrides only image, never pull
	// policy — there is no equivalent per-dispatch pull-policy flag. The
	// warning must then name only "pull policy" (the field the snapshot
	// still supplies), not "image and pull policy" (image came from
	// opts.Image, not the snapshot).
	agentInfo, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Image:       "dispatch:v9",
	})
	if err != nil {
		t.Fatalf("fourth Start (with dispatch --image) failed: %v", err)
	}
	if capturedConfig.Image != "dispatch:v9" {
		t.Errorf("image with dispatch --image = %q, want the dispatch value %q", capturedConfig.Image, "dispatch:v9")
	}
	gotPolicy = ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("imagePullPolicy with dispatch --image = %q, want the recorded snapshot value %q", gotPolicy, "Never")
	}
	if agentInfo == nil {
		t.Fatal("expected a non-nil AgentInfo")
	}
	joinedWarnings := strings.Join(agentInfo.Warnings, "\n")
	if !strings.Contains(joinedWarnings, "using the template's pull policy recorded") {
		t.Errorf("expected a warning naming only the pull policy as recorded, got warnings=%v", agentInfo.Warnings)
	}
	if strings.Contains(joinedWarnings, "image and pull policy") || strings.Contains(joinedWarnings, "using the template's image") {
		t.Errorf("did not expect the warning to name image (it came from dispatch --image, not the snapshot), got warnings=%v", agentInfo.Warnings)
	}
}

// agentInfoWarningsOrNil is a small helper so the warning-mismatch error
// message above doesn't panic on a nil agentInfo.
func agentInfoWarningsOrNil(info *api.AgentInfo) []string {
	if info == nil {
		return nil
	}
	return info.Warnings
}

// TestStart_RestartWithNoRecordedTemplate_NotTreatedAsUnresolvable pins the
// guard half of the unresolvable-template fallback: an EMPTY template chain
// must not be conflated with a template that failed to resolve. An agent
// whose Info.Template is empty (no template name to look up at all) has no
// template tier to fall back FROM in the first place — treating that the
// same as "the named template is gone" would pin whatever settings/file
// image happened to be baked into scion-agent.json at creation forever,
// which is the precedence bug ptone/scion#2156 exists to fix. Settings
// changed after creation must still apply on restart, and no "could not be
// found" warning should appear.
func TestStart_RestartWithNoRecordedTemplate_NotTreatedAsUnresolvable(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	settingsPath := filepath.Join(globalScionDir, "settings.yaml")
	settingsV1 := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-v1:latest
`)
	settingsV2 := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-v2:latest
`)
	_ = os.WriteFile(settingsPath, settingsV1, 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	}); err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "settings-v1:latest" {
		t.Fatalf("precondition failed: create-time image = %q, want settings-v1:latest", capturedConfig.Image)
	}

	// Simulate an agent with no recorded template name at all (e.g. an
	// agent predating template tracking, or one whose template was always
	// blank) — empty this agent's Info.Template directly on disk.
	agentInfoPath := filepath.Join(config.GetAgentHomePath(projectScionDir, "test-agent"), "agent-info.json")
	infoData, err := os.ReadFile(agentInfoPath)
	if err != nil {
		t.Fatalf("failed to read agent-info.json: %v", err)
	}
	var info api.AgentInfo
	if err := json.Unmarshal(infoData, &info); err != nil {
		t.Fatalf("failed to unmarshal agent-info.json: %v", err)
	}
	info.Template = ""
	updated, err := json.Marshal(&info)
	if err != nil {
		t.Fatalf("failed to re-marshal agent-info.json: %v", err)
	}
	if err := os.WriteFile(agentInfoPath, updated, 0644); err != nil {
		t.Fatalf("failed to write agent-info.json: %v", err)
	}

	// An operator changes the settings image.
	_ = os.WriteFile(settingsPath, settingsV2, 0644)

	agentInfo, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "settings-v2:latest" {
		t.Errorf("restart image = %q, want the new settings image %q (an empty recorded template must not pin the old settings-baked image)",
			capturedConfig.Image, "settings-v2:latest")
	}
	if agentInfo != nil && strings.Contains(strings.Join(agentInfo.Warnings, "\n"), "could not be found") {
		t.Errorf("did not expect an unresolvable-template warning when there was never a template to resolve, got warnings=%v", agentInfo.Warnings)
	}
}

// TestStart_ProfileOverrideImageAndPullPolicy: a profile's
// harness_overrides.<h> image/pull policy must outrank the base
// harness_configs.<h> entry when Start resolves the settings tier
// (ptone/scion#2156; ProvisionAgent's table test covers the provision-time
// path, this pins it at Start too).
func TestStart_ProfileOverrideImageAndPullPolicy(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
    harness_overrides:
      test-harness:
        image: profile-pinned:v4
        image_pull_policy: Never
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
    image_pull_policy: Always
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		Profile:     "staging",
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.Image != "profile-pinned:v4" {
		t.Errorf("expected profile harness_overrides image to outrank the base settings entry, got %q", capturedConfig.Image)
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("expected profile harness_overrides imagePullPolicy to outrank the base settings entry, got %q", gotPolicy)
	}
}

// TestStart_RestartWithNoProfile_ResolvesSettingsAgainstCreatedWithProfile
// pins ptone/scion#2156: a local restart with no --profile must resolve the
// Hub-settings tier against the profile the agent was actually CREATED with
// (finalScionCfg.Info.Profile), not whatever settings.active_profile happens
// to be — matching the broker's own restart-dispatch behavior
// (agent.GetSavedProfile). The agent here is created under "staging" (whose
// harness_overrides pins an image) while active_profile is a different
// profile with no such override; restarting with Profile: "" must still
// resolve to the staging image, not fall through to active_profile's.
func TestStart_RestartWithNoProfile_ResolvesSettingsAgainstCreatedWithProfile(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// active_profile is "local", which has no harness_overrides for
	// test-harness; "staging" (the created-with profile) pins one.
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
  staging:
    runtime: docker
    harness_overrides:
      test-harness:
        image: staging-pinned:v5
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	// Create under the "staging" profile.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		Profile:     "staging",
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "staging-pinned:v5" {
		t.Fatalf("precondition failed: create-time image = %q, want staging-pinned:v5", capturedConfig.Image)
	}

	// Restart with no --profile at all.
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "staging-pinned:v5" {
		t.Errorf("restart (no --profile) image = %q, want the created-with profile's image %q (not active_profile's)",
			capturedConfig.Image, "staging-pinned:v5")
	}
}
