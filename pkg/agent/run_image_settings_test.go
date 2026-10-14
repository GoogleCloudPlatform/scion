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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestStartImageRegistryRewriteAfterOptsImage(t *testing.T) {
	// Verify that the image_registry rewrite is applied AFTER the opts.Image
	// override, so a bare image from the dispatch request gets rewritten.
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: default-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
    image_registry: us-docker.pkg.dev/my-project/scion
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
		ImageExistsFunc: func(ctx context.Context, image string) (bool, error) {
			return false, nil
		},
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		Image:       "scion-custom:latest",
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	want := "us-docker.pkg.dev/my-project/scion/scion-custom:latest"
	if capturedConfig.Image != want {
		t.Errorf("expected image %q after registry rewrite of opts.Image, got %q",
			want, capturedConfig.Image)
	}
}

// --- ptone/scion#2156: Hub settings image / imagePullPolicy precedence ---
//
// ProvisionAgent bakes the harness-config file's own `image` (and, per the
// fix below, a Hub settings harness_configs.<h>.image) into
// finalScionCfg.Image as a fallback base, with an explicit template/inline
// `image:` merged in on top as an override. Both end up as a non-empty
// finalScionCfg.Image, indistinguishable from each other by value alone —
// which matters because for an EXISTING agent, finalScionCfg.Image is
// whatever was persisted in scion-agent.json at a previous provision, and
// may now be stale relative to current settings. Start's own
// image-resolution chain resolves the on-disk file default and the current
// settings value fresh on every call, so it must determine "is there a
// genuine template/inline override" directly (by re-reading the template
// chain and inline config) rather than by
// comparing finalScionCfg.Image against anything — a value comparison can't
// tell a stale persisted fallback apart from a real override. These tests
// pin the fixed precedence end to end through Manager.Start, capturing the
// runtime.RunConfig that would become the container/pod spec.

// TestStart_SettingsImageOverridesHarnessConfigFileDefault: with no
// template/inline override, a Hub settings harness_configs.<h>.image must
// win over the harness-config file's own `image:` default.
func TestStart_SettingsImageOverridesHarnessConfigFileDefault(t *testing.T) {
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
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.Image != "settings-pinned:v1" {
		t.Errorf("expected settings image to win over the harness-config file default, got %q", capturedConfig.Image)
	}
}

// TestStart_DispatchImageStillOutranksSettings: the agent-create request /
// dispatch --image (opts.Image, tier 1 in the precedence table) must still
// outrank a Hub settings image (ptone/scion#2156).
func TestStart_DispatchImageStillOutranksSettings(t *testing.T) {
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
		Image:       "dispatch-pinned:v9",
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.Image != "dispatch-pinned:v9" {
		t.Errorf("expected dispatch --image to outrank the settings image, got %q", capturedConfig.Image)
	}
}

// TestStart_TemplateImageAndPullPolicyIgnored: a template's `image:` /
// `kubernetes.imagePullPolicy` are environment keys, ignored
// (ptone/scion#4524), so the Hub settings values apply.
func TestStart_TemplateImageAndPullPolicyIgnored(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness", "image": "template-pinned:v2", "kubernetes": {"imagePullPolicy": "Never"}}`), 0644)

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

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.Image != "settings-pinned:v1" {
		t.Errorf("image = %q, want the settings image (the template's is ignored)", capturedConfig.Image)
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Always" {
		t.Errorf("imagePullPolicy = %q, want the settings Always (the template's is ignored)", gotPolicy)
	}
}

// TestStart_RestartAfterSettingsImageAdded_SettingsWinsOverStalePersistedFileDefault
// pins ptone/scion#2156: an agent provisioned BEFORE a Hub settings image
// existed has the harness-config file's default persisted in its
// scion-agent.json. Restarting that agent after an operator adds
// harness_configs.<h>.image to settings must pick up the settings image, not
// keep re-running the stale persisted file default. A fresh single-Start
// test cannot exercise this: ProvisionAgent's settings-precedence fix
// already makes finalScionCfg.Image correct on a first, from-scratch
// provision, regardless of how Start treats it afterward — only a genuine
// restart of an EXISTING agent distinguishes "settings win" from "the file
// default happens to be baked into the persisted config too".
func TestStart_RestartAfterSettingsImageAdded_SettingsWinsOverStalePersistedFileDefault(t *testing.T) {
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
	noSettingsImage := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`)
	withSettingsImage := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
`)
	_ = os.WriteFile(settingsPath, noSettingsImage, 0644)

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
	startOpts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	}

	// First Start: no settings image yet, so the agent's persisted
	// scion-agent.json ends up with the harness-config file's default.
	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	if capturedConfig.Image != "file-default:latest" {
		t.Fatalf("precondition failed: first-provision image = %q, want the harness-config file default", capturedConfig.Image)
	}

	// An operator now pins harness_configs.test-harness.image in settings.
	_ = os.WriteFile(settingsPath, withSettingsImage, 0644)

	// Second Start (restart of the now-existing agent) must pick up the
	// settings image, not the stale persisted file default.
	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}
	if capturedConfig.Image != "settings-pinned:v1" {
		t.Errorf("restart image = %q, want the newly-added settings image %q (not the stale persisted file default)",
			capturedConfig.Image, "settings-pinned:v1")
	}
}

// TestStart_RestartOfExistingAgent_ResolvedKubernetesServiceAccountNameOverridesPersistedValue
// is the exact regression case a dedicated StartOptions field exists to fix:
// GetAgent, for an EXISTING agent, builds its config from the template chain
// plus the PERSISTED scion-agent.json — never from InlineConfig. A fresh
// single-Start test cannot exercise this (ProvisionAgent's own template
// merge already gets a first provision right); only a genuine second Start
// against an already-provisioned agent, whose scion-agent.json now holds a
// ServiceAccountName from that first provision, distinguishes "the override
// still applies against a persisted value" from "it only ever applied
// against a live template".
func TestStart_RestartOfExistingAgent_ResolvedKubernetesServiceAccountNameOverridesPersistedValue(t *testing.T) {
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
	// A template's kubernetes block is an ignored environment key
	// (ptone/scion#4524); the create-time ServiceAccountName comes from the
	// inline (request) config, which provision persists to scion-agent.json.
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	createInline := &api.ScionConfig{Kubernetes: &api.KubernetesConfig{ServiceAccountName: "template-ksa"}}

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: k8s
profiles:
  k8s:
    runtime: kubernetes
runtimes:
  kubernetes:
    type: kubernetes
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

	// First Start (fresh provision): no resolved KSA yet, so the agent's
	// persisted scion-agent.json ends up with the create-time ServiceAccountName.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:         "test-agent",
		ProjectPath:  projectScionDir,
		BrokerMode:   true,
		NoAuth:       true,
		InlineConfig: createInline,
	}); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	if capturedConfig.Kubernetes == nil || capturedConfig.Kubernetes.ServiceAccountName != "template-ksa" {
		got := ""
		if capturedConfig.Kubernetes != nil {
			got = capturedConfig.Kubernetes.ServiceAccountName
		}
		t.Fatalf("precondition failed: first-provision ServiceAccountName = %q, want the create-time 'template-ksa'", got)
	}

	// Second Start (restart of the now-existing agent), with the broker
	// having resolved a KSA mapping for this dispatch. It must win over the
	// value now persisted in scion-agent.json from the first provision, not
	// be silently ignored the way an InlineConfig-only value would be.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:                                 "test-agent",
		ProjectPath:                          projectScionDir,
		BrokerMode:                           true,
		NoAuth:                               true,
		ResolvedKubernetesServiceAccountName: "resolved-ksa",
	}); err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}
	if capturedConfig.Kubernetes == nil || capturedConfig.Kubernetes.ServiceAccountName != "resolved-ksa" {
		got := ""
		if capturedConfig.Kubernetes != nil {
			got = capturedConfig.Kubernetes.ServiceAccountName
		}
		t.Errorf("restart ServiceAccountName = %q, want the resolved KSA %q (not the stale persisted template value)", got, "resolved-ksa")
	}
}

// TestStart_KubernetesBlockIdentityReplacesServiceAccount covers GCP
// identity "block" on Kubernetes (ptone/scion#4034) on create and on a
// restart of an existing agent whose persisted scion-agent.json holds a
// template serviceAccountName: the block ServiceAccount replaces it, an
// empty block ServiceAccount clears it (the namespace default), and the
// RunConfig is marked so the runtime turns off the token mount and adds the
// Workload Identity node selector. Without the block identity the template
// value is kept and the RunConfig is not marked.
func TestStart_KubernetesBlockIdentityReplacesServiceAccount(t *testing.T) {
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
	// A template's kubernetes block is an ignored environment key
	// (ptone/scion#4524); the persisted serviceAccountName comes from the
	// create's inline (request) config.
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	createInline := &api.ScionConfig{Kubernetes: &api.KubernetesConfig{ServiceAccountName: "template-ksa"}}

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: k8s
profiles:
  k8s:
    runtime: kubernetes
runtimes:
  kubernetes:
    type: kubernetes
`), 0644)

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
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

	saName := func() string {
		if capturedConfig.Kubernetes == nil {
			return ""
		}
		return capturedConfig.Kubernetes.ServiceAccountName
	}
	steps := []struct {
		name      string
		block     *api.KubernetesBlockIdentity
		wantSA    string
		wantBlock bool
	}{
		{"create with block service account", &api.KubernetesBlockIdentity{ServiceAccountName: "scion-block"}, "scion-block", true},
		{"restart with namespace default", &api.KubernetesBlockIdentity{}, "", true},
		{"restart without block", nil, "template-ksa", false},
	}
	for i, step := range steps {
		var inline *api.ScionConfig
		if i == 0 {
			inline = createInline
		}
		if _, err := mgr.Start(context.Background(), api.StartOptions{
			Name:                    "test-agent",
			ProjectPath:             projectScionDir,
			BrokerMode:              true,
			NoAuth:                  true,
			KubernetesBlockIdentity: step.block,
			InlineConfig:            inline,
		}); err != nil {
			t.Fatalf("%s: Start failed: %v", step.name, err)
		}
		if got := saName(); got != step.wantSA {
			t.Errorf("%s: ServiceAccountName = %q, want %q", step.name, got, step.wantSA)
		}
		if capturedConfig.KubernetesBlockIdentity != step.wantBlock {
			t.Errorf("%s: RunConfig.KubernetesBlockIdentity = %v, want %v", step.name, capturedConfig.KubernetesBlockIdentity, step.wantBlock)
		}
	}
}

// TestStart_RestartAfterSettingsPullPolicyRemoved_ClearsStalePersistedValue
// pins ptone/scion#2156: image_pull_policy must behave exactly like image on
// a restart — removing a Hub settings harness_configs.<h>.image_pull_policy
// must be honoured, not masked by the pull policy value ProvisionAgent
// persisted into scion-agent.json (via finalScionCfg.Kubernetes) at an
// earlier provision. Unlike image, RunConfig.Kubernetes is built by copying
// finalScionCfg.Kubernetes and overlaying resolvedPullPolicy, so this only
// holds if that overlay is unconditional.
func TestStart_RestartAfterSettingsPullPolicyRemoved_ClearsStalePersistedValue(t *testing.T) {
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
	withPullPolicy := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image_pull_policy: Always
`)
	withoutPullPolicy := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
`)
	_ = os.WriteFile(settingsPath, withPullPolicy, 0644)

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
	startOpts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	}

	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Always" {
		t.Fatalf("precondition failed: create-time imagePullPolicy = %q, want Always", gotPolicy)
	}

	// An operator removes harness_configs.test-harness.image_pull_policy.
	_ = os.WriteFile(settingsPath, withoutPullPolicy, 0644)

	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}
	gotPolicy = ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "" {
		t.Errorf("restart imagePullPolicy = %q, want empty (the settings value was removed, and there is no lower tier to fall back to)", gotPolicy)
	}
}

// TestStart_RestartAfterTemplatePullPolicyRemoved_FallsToSettings pins
// ptone/scion#2156 together with ptone/scion#4524: a template's
// kubernetes.imagePullPolicy pin is an ignored environment key, so the
// settings tier applies on the create and, after the pin is removed from the
// template, on the restart; nothing from the pin is persisted.
func TestStart_RestartAfterTemplatePullPolicyRemoved_FallsToSettings(t *testing.T) {
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
	withTemplatePin := `{"default_harness_config": "test-harness", "kubernetes": {"imagePullPolicy": "Never"}}`
	withoutTemplatePin := `{"default_harness_config": "test-harness"}`
	_ = os.WriteFile(tplPath, []byte(withTemplatePin), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
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
	startOpts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	}

	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Always" {
		t.Fatalf("create-time imagePullPolicy = %q, want the settings Always (the template pin is ignored)", gotPolicy)
	}

	// The template's pull-policy pin is removed (the template still exists
	// and still resolves; it just no longer sets kubernetes.imagePullPolicy).
	_ = os.WriteFile(tplPath, []byte(withoutTemplatePin), 0644)

	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}
	gotPolicy = ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Always" {
		t.Errorf("restart imagePullPolicy = %q, want the settings value %q (not the stale removed template pin)", gotPolicy, "Always")
	}
}

// TestStart_SettingsImagePullPolicyReachesRunConfigKubernetes: a Hub
// settings harness_configs.<h>.image_pull_policy default reaches
// runtime.RunConfig.Kubernetes.ImagePullPolicy — the field the Kubernetes
// runtime's buildPod reads to set the pod's container ImagePullPolicy — and
// a template kubernetes.imagePullPolicy, an ignored environment key
// (ptone/scion#4524), does not change it.
func TestStart_SettingsImagePullPolicyReachesRunConfigKubernetes(t *testing.T) {
	tests := []struct {
		name           string
		templateExtra  string
		wantPullPolicy string
	}{
		{
			name:           "settings default applies with no template override",
			wantPullPolicy: "Always",
		},
		{
			name:           "template imagePullPolicy is ignored",
			templateExtra:  `, "kubernetes": {"imagePullPolicy": "Never"}`,
			wantPullPolicy: "Always",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			tplJSON := `{"default_harness_config": "test-harness"` + tt.templateExtra + `}`
			_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplJSON), 0644)

			_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: k8s
profiles:
  k8s:
    runtime: kubernetes
harness_configs:
  test-harness:
    harness: generic
    image_pull_policy: Always
runtimes:
  kubernetes:
    type: kubernetes
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
				BrokerMode:  true,
				NoAuth:      true,
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			gotPolicy := ""
			if capturedConfig.Kubernetes != nil {
				gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
			}
			if gotPolicy != tt.wantPullPolicy {
				t.Errorf("RunConfig.Kubernetes.ImagePullPolicy = %q, want %q", gotPolicy, tt.wantPullPolicy)
			}
		})
	}
}

// TestStart_ResolvedKubernetesServiceAccountNameOverridesTemplate covers the
// three cases for opts.ResolvedKubernetesServiceAccountName (the broker's
// GCP-identity-mapped KSA for Kubernetes assign): it applies whether or not
// the agent config sets a Kubernetes config at all, always wins over a
// configured ServiceAccountName when non-empty, and leaves an existing
// configured value untouched when empty (not just "does nothing" — the
// configured value must survive, exactly as before this field existed).
//
// A template's kubernetes block is an ignored environment key
// (ptone/scion#4524), so the configured ServiceAccountName comes from the
// inline (request) config, the tier merged directly above the template.
func TestStart_ResolvedKubernetesServiceAccountNameOverridesTemplate(t *testing.T) {
	tests := []struct {
		name                       string
		inline                     *api.ScionConfig
		resolvedServiceAccountName string
		wantServiceAccountName     string
		wantKubernetesConfigAtAll  bool
	}{
		{
			name:                       "no template Kubernetes config at all, only the resolved KSA",
			resolvedServiceAccountName: "resolved-ksa",
			wantServiceAccountName:     "resolved-ksa",
			wantKubernetesConfigAtAll:  true,
		},
		{
			// MB: no template Kubernetes config, no resolved KSA (e.g. a
			// dispatch that never resolved a GCP identity mode "assign"
			// mapping), and no other resolved Kubernetes field (pull
			// policy) either — RunConfig.Kubernetes must stay nil, exactly
			// the pre-existing behavior for an agent with no Kubernetes
			// configuration at all. A missing nil-return guard here would
			// instead produce an all-zero-value *api.KubernetesConfig.
			name:                      "no template Kubernetes config and no resolved KSA stays nil",
			wantKubernetesConfigAtAll: false,
		},
		{
			name:                       "configured ServiceAccountName is overridden by the resolved KSA",
			inline:                     &api.ScionConfig{Kubernetes: &api.KubernetesConfig{ServiceAccountName: "template-ksa"}},
			resolvedServiceAccountName: "resolved-ksa",
			wantServiceAccountName:     "resolved-ksa",
			wantKubernetesConfigAtAll:  true,
		},
		{
			name:                      "empty resolved value leaves the configured ServiceAccountName untouched",
			inline:                    &api.ScionConfig{Kubernetes: &api.KubernetesConfig{ServiceAccountName: "template-ksa"}},
			wantServiceAccountName:    "template-ksa",
			wantKubernetesConfigAtAll: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			tplJSON := `{"default_harness_config": "test-harness"}`
			_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplJSON), 0644)

			_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: k8s
profiles:
  k8s:
    runtime: kubernetes
runtimes:
  kubernetes:
    type: kubernetes
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
				Name:                                 "test-agent",
				ProjectPath:                          projectScionDir,
				BrokerMode:                           true,
				NoAuth:                               true,
				InlineConfig:                         tt.inline,
				ResolvedKubernetesServiceAccountName: tt.resolvedServiceAccountName,
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			if tt.wantKubernetesConfigAtAll && capturedConfig.Kubernetes == nil {
				t.Fatal("expected RunConfig.Kubernetes to be set, got nil")
			}
			if !tt.wantKubernetesConfigAtAll && capturedConfig.Kubernetes != nil {
				t.Fatalf("expected RunConfig.Kubernetes to be nil, got %+v", capturedConfig.Kubernetes)
			}
			gotServiceAccountName := ""
			if capturedConfig.Kubernetes != nil {
				gotServiceAccountName = capturedConfig.Kubernetes.ServiceAccountName
			}
			if gotServiceAccountName != tt.wantServiceAccountName {
				t.Errorf("RunConfig.Kubernetes.ServiceAccountName = %q, want %q", gotServiceAccountName, tt.wantServiceAccountName)
			}
		})
	}
}

// TestStart_InlineConfigImageAndPullPolicyOutrankSettings: an inline config
// (opts.InlineConfig, e.g. a per-dispatch `--config` override) supplying an
// explicit image / kubernetes.imagePullPolicy must outrank a Hub settings
// value, on a first (fresh-provision) Start — the same rule a template
// override already has (ptone/scion#2156).
func TestStart_InlineConfigImageAndPullPolicyOutrankSettings(t *testing.T) {
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
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.Image != "inline-pinned:v3" {
		t.Errorf("expected inline-config image to outrank settings, got %q", capturedConfig.Image)
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("expected inline-config imagePullPolicy to outrank settings, got %q", gotPolicy)
	}
}
