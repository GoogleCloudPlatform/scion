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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestPickImage_OrderAndReplacementLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	img, src := pickImage(logger, "a1", []imageCandidate{
		{source: imageTierHarnessConfigFile, image: "file:1"},
		{source: imageTierSettings, image: ""},
		{source: imageTierTemplate, image: "tpl:2"},
		{source: imageTierInline, image: "tpl:2"}, // same image: not a replacement
		{source: imageTierProfileOverride, image: "profile:3"},
		{source: imageTierRequest, image: ""},
	})
	if img != "profile:3" || src != imageTierProfileOverride {
		t.Fatalf("pickImage = (%q, %q), want (profile:3, %s)", img, src, imageTierProfileOverride)
	}
	out := buf.String()
	if got := strings.Count(out, "lower-tier image replaced"); got != 2 {
		t.Fatalf("expected 2 replacement log lines (file->template, template->profile), got %d:\n%s", got, out)
	}
	for _, want := range []string{"replaced_value=file:1", "replaced_value=tpl:2", "value=profile:3", `"profile harness_overrides outranks inline config"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q:\n%s", want, out)
		}
	}
}

func TestPickImage_NoCandidates(t *testing.T) {
	img, src := pickImage(nil, "a1", []imageCandidate{{source: imageTierTemplate}})
	if img != "" || src != "" {
		t.Fatalf("pickImage = (%q, %q), want empty", img, src)
	}
}

// imagePrecedenceFixture writes a global .scion with a harness-config file
// default, a template with templateImage (omitted when empty), and the given
// settings.yaml, and returns the project .scion dir.
func imagePrecedenceFixture(t *testing.T, templateImage, settingsYAML string) string {
	t.Helper()
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	tplJSON := `{"default_harness_config": "test-harness"}`
	if templateImage != "" {
		tplJSON = `{"default_harness_config": "test-harness", "image": "` + templateImage + `"}`
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplJSON), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

// startCapturingImage runs Start against a mock runtime and returns the
// image handed to the runtime plus everything logged through slog.Default.
func startCapturingImage(t *testing.T, opts api.StartOptions) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

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
	opts.BrokerMode = true
	opts.NoAuth = true
	if _, err := NewManager(mockRT).Start(context.Background(), opts); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	return captured.Image, buf.String()
}

const profileOverrideSettings = `schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
    harness_overrides:
      test-harness:
        image: profile-pinned:v4
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
`

// TestStart_ExplicitProfileOverrideImageBeatsTemplateImage pins
// ptone/scion#1799: an explicitly set profiles.<p>.harness_overrides.<hc>.image
// outranks a template's image, and the replacement is logged at Info.
func TestStart_ExplicitProfileOverrideImageBeatsTemplateImage(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", profileOverrideSettings)

	img, logs := startCapturingImage(t, api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		Profile:     "staging",
	})
	if img != "profile-pinned:v4" {
		t.Fatalf("expected the explicit profile override to beat the template image, got %q", img)
	}
	if !strings.Contains(logs, "lower-tier image replaced") ||
		!strings.Contains(logs, "replaced_value=template-pinned:v2") ||
		!strings.Contains(logs, " value=profile-pinned:v4") ||
		!strings.Contains(logs, "level=INFO") {
		t.Errorf("expected an Info log naming the replaced template image, got:\n%s", logs)
	}
}

// TestStart_PlainSettingsImageDoesNotBeatTemplateImage pins the other half
// of the ptone/scion#1799 decision: the plain harness_configs.<hc>.image
// default does NOT gain priority over a template image.
func TestStart_PlainSettingsImageDoesNotBeatTemplateImage(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
`)
	img, logs := startCapturingImage(t, api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
	})
	if img != "template-pinned:v2" {
		t.Fatalf("expected the template image to beat the plain settings default, got %q", img)
	}
	if !strings.Contains(logs, "replaced_value=settings-pinned:v1") {
		t.Errorf("expected an Info log naming the replaced settings image, got:\n%s", logs)
	}
}

// TestStart_ExplicitProfileOverrideImageBeatsInlineImage: the inline config
// tier also ranks below an explicit profile override.
func TestStart_ExplicitProfileOverrideImageBeatsInlineImage(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", profileOverrideSettings)

	img, _ := startCapturingImage(t, api.StartOptions{
		Name:         "test-agent",
		ProjectPath:  projectScionDir,
		Profile:      "staging",
		InlineConfig: &api.ScionConfig{Image: "inline-pinned:v3"},
	})
	if img != "profile-pinned:v4" {
		t.Fatalf("expected the explicit profile override to beat the inline image, got %q", img)
	}
}

// TestStart_RequestImageBeatsProfileOverrideAndTemplate: an explicit
// --image / request image (opts.Image) still beats everything.
func TestStart_RequestImageBeatsProfileOverrideAndTemplate(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", profileOverrideSettings)

	img, logs := startCapturingImage(t, api.StartOptions{
		Name:         "test-agent",
		ProjectPath:  projectScionDir,
		Profile:      "staging",
		Image:        "flag-pinned:v9",
		InlineConfig: &api.ScionConfig{Image: "inline-pinned:v3"},
	})
	if img != "flag-pinned:v9" {
		t.Fatalf("expected --image to beat every other tier, got %q", img)
	}
	if !strings.Contains(logs, "replaced_value=profile-pinned:v4") {
		t.Errorf("expected an Info log naming the replaced profile image, got:\n%s", logs)
	}
}

// writeSettings replaces the global settings.yaml written by
// imagePrecedenceFixture (HOME is the fixture's temp dir).
func writeSettings(t *testing.T, settingsYAML string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
}

const noOverrideSettings = `schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
`

// startCapturingRun is startCapturingImage returning the whole RunConfig
// and the Start result.
func startCapturingRun(t *testing.T, opts api.StartOptions) (runtime.RunConfig, *api.AgentInfo) {
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
	opts.BrokerMode = true
	opts.NoAuth = true
	info, err := NewManager(mockRT).Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	return captured, info
}

func pullPolicyOf(cfg runtime.RunConfig) string {
	if cfg.Kubernetes == nil {
		return ""
	}
	return cfg.Kubernetes.ImagePullPolicy
}

// TestStart_RemovedProfilePinDoesNotLingerInTemplateSnapshot pins review
// finding 1 of ptone/scion#1799: a profile override present at provision
// must not be baked into the record a later Start falls back to when the
// template is unresolvable (the norm for a hub agent restarted on a broker
// with no local template). Removing the pin must bring back the
// template's own image, and the warning must describe what was used.
func TestStart_RemovedProfilePinDoesNotLingerInTemplateSnapshot(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "", profileOverrideSettings)
	tplDir := filepath.Join(os.Getenv("HOME"), ".scion", "templates", "named")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness", "image": "named-template:v7"}`), 0644); err != nil {
		t.Fatal(err)
	}

	run, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", Template: "named", ProjectPath: projectScionDir, Profile: "staging"})
	if run.Image != "profile-pinned:v4" {
		t.Fatalf("first start: image = %q, want the profile pin", run.Image)
	}

	if err := os.RemoveAll(tplDir); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, noOverrideSettings)

	run, info := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
	if run.Image != "named-template:v7" {
		t.Fatalf("restart after removing the pin: image = %q, want the template's own recorded image", run.Image)
	}
	if info == nil || !strings.Contains(strings.Join(info.Warnings, "\n"), "using the template's image recorded at an earlier provision") {
		t.Errorf("expected the template-snapshot warning, got %v", info)
	}
}

// TestStart_ExplicitImageStaysTopTierAcrossRestart pins review finding 2: a
// user's explicit image (--image, or a --config image the CLI promotes to
// opts.Image) ranks above an explicit profile override on the first start
// AND on a later plain restart.
func TestStart_ExplicitImageStaysTopTierAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create api.StartOptions
	}{
		{"--image", api.StartOptions{Image: "flag-pinned:v9"}},
		// The CLI promotes a --config image to opts.Image and also passes
		// the inline config (cmd/common.go).
		{"--config image", api.StartOptions{Image: "flag-pinned:v9", InlineConfig: &api.ScionConfig{Image: "flag-pinned:v9"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", profileOverrideSettings)
			create := tc.create
			create.Name, create.ProjectPath, create.Profile = "test-agent", projectScionDir, "staging"
			first, _ := startCapturingRun(t, create)
			restart, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
			if first.Image != "flag-pinned:v9" || restart.Image != "flag-pinned:v9" {
				t.Fatalf("first start = %q, restart = %q; want the explicit image on both", first.Image, restart.Image)
			}
		})
	}
}

// TestStart_LegacyAgentRecordedInlineImageStaysTopTier: an agent
// provisioned before image provenance was recorded has only Info.ExplicitImage;
// that create-time request-level image keeps outranking a profile override.
func TestStart_LegacyAgentRecordedInlineImageStaysTopTier(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", profileOverrideSettings)
	startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging",
		Image: "inline-pinned:v3", InlineConfig: &api.ScionConfig{Image: "inline-pinned:v3"}})

	// Remove the broker-side provenance record, as for a pre-upgrade agent.
	provPath := filepath.Join(projectScionDir, "agents", "test-agent", imageProvenanceFile)
	if _, err := os.Stat(provPath); err != nil {
		t.Fatalf("expected broker-side image provenance at %s: %v", provPath, err)
	}
	if err := os.Remove(provPath); err != nil {
		t.Fatal(err)
	}

	restart, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
	if restart.Image != "inline-pinned:v3" {
		t.Fatalf("legacy restart: image = %q, want the recorded create-time inline image", restart.Image)
	}
}

// TestStart_ProfilePullPolicyMovesWithProfileImage pins the ptone decision
// on ptone/scion#1799: an explicitly set profile image_pull_policy takes the
// profile image's tier (above the template) only when the same override also
// sets an image, while the user's explicit
// inline pull policy still ranks above it and the plain settings policy
// stays below the template.
func TestStart_ProfilePullPolicyMovesWithProfileImage(t *testing.T) {
	const settingsWithPolicy = `schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
    harness_overrides:
      test-harness:
        image: profile-pinned:v4
        image_pull_policy: Always
harness_configs:
  test-harness:
    harness: generic
    image_pull_policy: IfNotPresent
`
	tplWithPolicy := func(t *testing.T, settingsYAML string) string {
		projectScionDir := imagePrecedenceFixture(t, "", settingsYAML)
		tpl := filepath.Join(os.Getenv("HOME"), ".scion", "templates", "default", "scion-agent.json")
		if err := os.WriteFile(tpl, []byte(`{"default_harness_config": "test-harness", "image": "template-pinned:v2", "kubernetes": {"imagePullPolicy": "Never"}}`), 0644); err != nil {
			t.Fatal(err)
		}
		return projectScionDir
	}

	t.Run("profile policy beats template policy", func(t *testing.T) {
		projectScionDir := tplWithPolicy(t, settingsWithPolicy)
		run, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"})
		if run.Image != "profile-pinned:v4" || pullPolicyOf(run) != "Always" {
			t.Fatalf("got image %q policy %q, want profile-pinned:v4 / Always", run.Image, pullPolicyOf(run))
		}
	})
	t.Run("explicit inline policy beats profile policy", func(t *testing.T) {
		projectScionDir := tplWithPolicy(t, settingsWithPolicy)
		run, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging",
			InlineConfig: &api.ScionConfig{Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "IfNotPresent"}}})
		if pullPolicyOf(run) != "IfNotPresent" {
			t.Fatalf("policy = %q, want the explicit inline IfNotPresent", pullPolicyOf(run))
		}
	})
	t.Run("profile policy without a profile image stays below template policy", func(t *testing.T) {
		projectScionDir := tplWithPolicy(t, `schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
    harness_overrides:
      test-harness:
        image_pull_policy: Always
harness_configs:
  test-harness:
    harness: generic
`)
		run, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"})
		if run.Image != "template-pinned:v2" || pullPolicyOf(run) != "Never" {
			t.Fatalf("got image %q policy %q, want the template's template-pinned:v2 / Never", run.Image, pullPolicyOf(run))
		}
	})
	t.Run("plain settings policy stays below template policy", func(t *testing.T) {
		projectScionDir := tplWithPolicy(t, noOverrideSettings+"    image_pull_policy: Always\n")
		run, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"})
		if pullPolicyOf(run) != "Never" {
			t.Fatalf("policy = %q, want the template's Never", pullPolicyOf(run))
		}
	})
}

// TestWithProvisionedImage pins review finding 6: the provision-only
// response reports the image Start will run (request > profile > the
// provisioned inline/template/settings/file image), without changing the
// persisted config it was given.
func TestWithProvisionedImage(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "", profileOverrideSettings)
	base := &api.ScionConfig{Image: "template-pinned:v2", HarnessConfig: "test-harness"}

	got, _ := withProvisionedImage(api.StartOptions{Name: "a", ProjectPath: projectScionDir, Profile: "staging"}, "", base)
	if got.Image != "profile-pinned:v4" {
		t.Errorf("profile pin: got %q", got.Image)
	}
	if base.Image != "template-pinned:v2" {
		t.Errorf("input config was mutated: %q", base.Image)
	}
	got, _ = withProvisionedImage(api.StartOptions{Name: "a", ProjectPath: projectScionDir, Profile: "staging", Image: "request:v9"}, "", base)
	if got.Image != "request:v9" {
		t.Errorf("request image: got %q", got.Image)
	}
	writeSettings(t, noOverrideSettings)
	got, _ = withProvisionedImage(api.StartOptions{Name: "a", ProjectPath: projectScionDir, Profile: "staging"}, "", base)
	if got.Image != "template-pinned:v2" {
		t.Errorf("no pin: got %q", got.Image)
	}
}

// TestStart_InlineOnlyImageRanksTheSameOnRestart: a caller that passes an
// inline image but no request image (not the CLI, which promotes it) keeps
// it at the inline tier on the first start and on a plain restart alike, so
// the explicit profile override wins both times.
func TestStart_InlineOnlyImageRanksTheSameOnRestart(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", profileOverrideSettings)
	first, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging",
		InlineConfig: &api.ScionConfig{Image: "inline-pinned:v3"}})
	restart, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
	if first.Image != "profile-pinned:v4" || restart.Image != "profile-pinned:v4" {
		t.Fatalf("first start = %q, restart = %q; want the profile pin on both", first.Image, restart.Image)
	}
}

// TestStart_TemplateSnapshotUsesTemplatesOwnRecordedImage pins the
// provenance branch of the unresolvable-template fallback (round-2 finding
// 2): when the merged scion-agent.json image differs from the template's own
// value — here the template sets no image and a settings image applied at
// provision — a restart after the template is deleted and the settings
// image removed must not resurrect the old settings image from the merged
// record. It falls through to the live tiers (the harness-config file).
func TestStart_TemplateSnapshotUsesTemplatesOwnRecordedImage(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "", noOverrideSettings)
	tplDir := filepath.Join(os.Getenv("HOME"), ".scion", "templates", "named")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644); err != nil {
		t.Fatal(err)
	}

	run, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", Template: "named", ProjectPath: projectScionDir, Profile: "staging"})
	if run.Image != "settings-pinned:v1" {
		t.Fatalf("first start: image = %q, want the settings image", run.Image)
	}
	merged, err := os.ReadFile(filepath.Join(projectScionDir, "agents", "test-agent", "scion-agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), "settings-pinned:v1") {
		t.Fatalf("precondition: the merged scion-agent.json should carry the settings image, got %s", merged)
	}

	if err := os.RemoveAll(tplDir); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, `schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
`)
	run, info := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
	if run.Image != "file-default:latest" {
		t.Fatalf("restart: image = %q, want the harness-config file default (not the merged record's old settings image)", run.Image)
	}
	if info != nil && strings.Contains(strings.Join(info.Warnings, "\n"), "recorded at an earlier provision") {
		t.Errorf("no recorded template value won, so no snapshot warning expected, got %v", info.Warnings)
	}
}

// TestStart_AgentInfoImageFieldsDoNotChangeImageSelection: agent-info.json is
// in the agent home, outside broker-side agent state, so editing its image / provenance
// fields must not change the image Start selects: image provenance is
// recorded in broker-side agent state (see also
// TestStart_AgentInfoExplicitImageFieldsDoNotChangeSelection).
func TestStart_AgentInfoImageFieldsDoNotChangeImageSelection(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", profileOverrideSettings)
	first, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"})
	if first.Image != "profile-pinned:v4" {
		t.Fatalf("first start: image = %q, want the profile pin", first.Image)
	}

	infoPath := filepath.Join(projectScionDir, "agents", "test-agent", "home", "agent-info.json")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["image"] = "unrelated:v1"
	raw["imageProvenance"] = map[string]any{"requestImage": "unrelated:v1", "templateImage": "unrelated:v1"}
	out, _ := json.Marshal(raw)
	if err := os.WriteFile(infoPath, out, 0644); err != nil {
		t.Fatal(err)
	}

	restart, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
	if restart.Image != "profile-pinned:v4" {
		t.Fatalf("restart after editing agent-info.json: image = %q, want the profile pin unchanged", restart.Image)
	}
}

// TestStart_AgentInfoExplicitImageFieldsDoNotChangeSelection: for an agent
// with broker-side image provenance, the create-time inline image and pull
// policy are read from that provenance, so editing agent-info.json's
// explicitImage / explicitImagePullPolicy display copies changes nothing.
func TestStart_AgentInfoExplicitImageFieldsDoNotChangeSelection(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "", noOverrideSettings)
	tpl := filepath.Join(os.Getenv("HOME"), ".scion", "templates", "default", "scion-agent.json")
	if err := os.WriteFile(tpl, []byte(`{"default_harness_config": "test-harness", "image": "template-pinned:v2", "kubernetes": {"imagePullPolicy": "Never"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	first, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"})
	if first.Image != "template-pinned:v2" || pullPolicyOf(first) != "Never" {
		t.Fatalf("first start: got %q / %q, want template-pinned:v2 / Never", first.Image, pullPolicyOf(first))
	}

	infoPath := filepath.Join(projectScionDir, "agents", "test-agent", "home", "agent-info.json")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["explicitImage"] = "unrelated:v1"
	raw["explicitImagePullPolicy"] = "Always"
	out, _ := json.Marshal(raw)
	if err := os.WriteFile(infoPath, out, 0644); err != nil {
		t.Fatal(err)
	}

	restart, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
	if restart.Image != "template-pinned:v2" || pullPolicyOf(restart) != "Never" {
		t.Fatalf("restart after editing agent-info.json: got %q / %q, want template-pinned:v2 / Never", restart.Image, pullPolicyOf(restart))
	}
}

// TestStart_InlineValuesRecordedBrokerSide: a create-time inline image and
// pull policy survive a plain restart through broker-side provenance.
func TestStart_InlineValuesRecordedBrokerSide(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", noOverrideSettings)
	startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging",
		InlineConfig: &api.ScionConfig{Image: "inline-pinned:v3", Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "IfNotPresent"}}})
	data, err := os.ReadFile(filepath.Join(projectScionDir, "agents", "test-agent", imageProvenanceFile))
	if err != nil {
		t.Fatal(err)
	}
	var p imageProvenance
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p.InlineImage != "inline-pinned:v3" || p.InlineImagePullPolicy != "IfNotPresent" {
		t.Fatalf("broker-side provenance = %+v, want the inline image and pull policy", p)
	}
	restart, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
	if restart.Image != "inline-pinned:v3" || pullPolicyOf(restart) != "IfNotPresent" {
		t.Fatalf("restart: got %q / %q, want the recorded inline values", restart.Image, pullPolicyOf(restart))
	}
}

// TestStart_AgentInfoProfileDoesNotChangeProfileOverride: the profile used for
// the profile harness_overrides image lookup is the provisioned profile
// recorded broker-side. Rewriting agent-info.json's profile to another
// configured profile whose override sets a different image changes nothing,
// whether the restart passes no profile (local) or the saved profile read
// from agent-info.json (as the broker's restart does via GetSavedProfile).
func TestStart_AgentInfoProfileDoesNotChangeProfileOverride(t *testing.T) {
	const twoProfiles = `schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
    harness_overrides:
      test-harness:
        image: profile-pinned:v4
  other:
    runtime: docker
    harness_overrides:
      test-harness:
        image: other-profile:v8
        image_pull_policy: Always
harness_configs:
  test-harness:
    harness: generic
`
	projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", twoProfiles)
	first, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"})
	if first.Image != "profile-pinned:v4" {
		t.Fatalf("first start: image = %q, want the staging pin", first.Image)
	}

	provPath := filepath.Join(projectScionDir, "agents", "test-agent", imageProvenanceFile)
	fi, err := os.Stat(provPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("image provenance mode = %o, want 0600", fi.Mode().Perm())
	}

	infoPath := filepath.Join(projectScionDir, "agents", "test-agent", "home", "agent-info.json")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["profile"] = "other"
	out, _ := json.Marshal(raw)
	if err := os.WriteFile(infoPath, out, 0644); err != nil {
		t.Fatal(err)
	}

	for _, restartProfile := range []string{"", "other"} {
		restart, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: restartProfile})
		if restart.Image != "profile-pinned:v4" || pullPolicyOf(restart) != "" {
			t.Fatalf("restart (profile %q) after rewriting agent-info.json profile: got %q / %q, want profile-pinned:v4 / no policy",
				restartProfile, restart.Image, pullPolicyOf(restart))
		}
	}
}

// rewriteAgentInfo applies edit to the agent's agent-info.json.
func rewriteAgentInfo(t *testing.T, projectScionDir string, edit func(map[string]any)) {
	t.Helper()
	infoPath := filepath.Join(projectScionDir, "agents", "test-agent", "home", "agent-info.json")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	edit(raw)
	out, _ := json.Marshal(raw)
	if err := os.WriteFile(infoPath, out, 0644); err != nil {
		t.Fatal(err)
	}
}

func writeTemplate(t *testing.T, name, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".scion", "templates", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestStart_AgentInfoTemplateDoesNotChangeTemplateTier: the template-tier
// image and pull policy come from the template recorded in broker-side
// provenance; rewriting agent-info.json's template to another project
// template changes nothing.
func TestStart_AgentInfoTemplateDoesNotChangeTemplateTier(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "", noOverrideSettings)
	writeTemplate(t, "alpha", `{"default_harness_config": "test-harness", "image": "alpha-image:v1", "kubernetes": {"imagePullPolicy": "Never"}}`)
	writeTemplate(t, "beta", `{"default_harness_config": "test-harness", "image": "beta-image:v2", "kubernetes": {"imagePullPolicy": "Always"}}`)

	first, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", Template: "alpha", ProjectPath: projectScionDir, Profile: "staging"})
	if first.Image != "alpha-image:v1" || pullPolicyOf(first) != "Never" {
		t.Fatalf("first start: got %q / %q", first.Image, pullPolicyOf(first))
	}
	rewriteAgentInfo(t, projectScionDir, func(raw map[string]any) { raw["template"] = "beta" })

	restart, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
	if restart.Image != "alpha-image:v1" || pullPolicyOf(restart) != "Never" {
		t.Fatalf("restart after rewriting agent-info.json template: got %q / %q, want alpha-image:v1 / Never", restart.Image, pullPolicyOf(restart))
	}
}

// TestStart_AgentInfoProfileDoesNotChangeImageRegistry: the profile-level
// image_registry rewrite uses the provisioned profile recorded in
// broker-side provenance, not agent-info.json's profile, even when a
// broker-style restart passes that saved profile as opts.Profile.
func TestStart_AgentInfoProfileDoesNotChangeImageRegistry(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "scion-test:latest", `schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
  reg:
    runtime: docker
    image_registry: registry.example.com/other
harness_configs:
  test-harness:
    harness: generic
`)
	first, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"})
	if first.Image != "scion-test:latest" {
		t.Fatalf("first start: image = %q, want the bare template image", first.Image)
	}
	rewriteAgentInfo(t, projectScionDir, func(raw map[string]any) { raw["profile"] = "reg" })

	saved := GetSavedProfile("test-agent", projectScionDir)
	if saved != "reg" {
		t.Fatalf("precondition: saved profile = %q, want reg", saved)
	}
	restart, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: saved})
	if restart.Image != "scion-test:latest" {
		t.Fatalf("broker-style restart after rewriting agent-info.json profile: image = %q, want it not rewritten to the other profile's registry", restart.Image)
	}
}

// TestStart_UnusableImageProvenanceFailsClosed: an image-provenance.json
// that exists but is unparseable or lacks the version marker fails the start
// with an actionable error; Start never falls back to agent-info.json.
func TestStart_UnusableImageProvenanceFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"unparseable", "{not json"},
		{"missing version", `{"requestImage": "x:v1"}`},
		{"wrong version", `{"version": 99}`},
		{"unreadable (a directory)", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", noOverrideSettings)
			startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"})
			provPath := filepath.Join(projectScionDir, "agents", "test-agent", imageProvenanceFile)
			if tc.body == "" {
				if err := os.Remove(provPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(provPath, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(provPath, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			promptPath := filepath.Join(projectScionDir, "agents", "test-agent", "prompt.md")
			deleted := false
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					// A stopped container for this agent: step 0 would delete it.
					return []api.AgentInfo{{Name: "test-agent", ContainerID: "stopped-1", Phase: "stopped"}}, nil
				},
				DeleteFunc: func(ctx context.Context, ref runtime.RunRef) error {
					deleted = true
					return nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					t.Fatal("runtime must not be called with unusable image provenance")
					return "", nil
				},
			}
			_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, Task: "new task for the agent"})
			if err == nil || !strings.Contains(err.Error(), "re-provision the agent") {
				t.Fatalf("expected an actionable image-provenance error, got %v", err)
			}
			// The message omits the host path (a broker returns it to the
			// Hub); the path is carried for logs only.
			if strings.Contains(err.Error(), provPath) {
				t.Errorf("error message must not contain the host path, got %v", err)
			}
			var pe *ImageProvenanceError
			if !errors.As(err, &pe) || pe.Path != provPath {
				t.Errorf("expected an *ImageProvenanceError carrying the path, got %#v", err)
			}
			if !errors.Is(err, config.ErrAgentStateConflict) || errors.Is(err, config.ErrAgentStateDirUnavailable) || !config.IsAgentStateConflict(err) {
				t.Errorf("expected an agent-state conflict (not an unavailable state dir), got %v", err)
			}
			// The pre-check runs before any side effect: the stopped
			// container is not cleaned up and the task is not staged.
			if deleted {
				t.Error("the stopped container must not be deleted before the provenance check")
			}
			if data, _ := os.ReadFile(promptPath); strings.Contains(string(data), "new task for the agent") {
				t.Error("prompt.md must not be written before the provenance check")
			}
		})
	}
}

// TestStart_RecordedRequestImageSurvivesMissingOrCorruptAgentInfo pins
// round-5 finding 1: the recorded explicit request image is replayed from
// broker-side provenance alone, so deleting or corrupting agent-info.json
// cannot drop it below an explicit profile override.
func TestStart_RecordedRequestImageSurvivesMissingOrCorruptAgentInfo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mangle func(path string) error
	}{
		{"deleted", os.Remove},
		{"corrupt", func(path string) error { return os.WriteFile(path, []byte("{not json"), 0644) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", profileOverrideSettings)
			first, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging", Image: "flag-pinned:v9"})
			if first.Image != "flag-pinned:v9" {
				t.Fatalf("first start: image = %q", first.Image)
			}
			if err := tc.mangle(filepath.Join(projectScionDir, "agents", "test-agent", "home", "agent-info.json")); err != nil {
				t.Fatal(err)
			}
			restart, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
			if restart.Image != "flag-pinned:v9" {
				t.Fatalf("restart with agent-info.json %s: image = %q, want the recorded explicit image", tc.name, restart.Image)
			}
		})
	}
}

// TestProvisionedProfile_AgentStateDirErrorHasNoHostPath: an unreadable
// project-id marker surfaces as an *fs.PathError from the root lookup; the
// resulting AgentStateDirError's message must not carry the host path (it
// can reach a broker response), only its Path field does.
func TestProvisionedProfile_AgentStateDirErrorHasNoHostPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := filepath.Join(t.TempDir(), "proj", ".scion")
	// A directory where the project-id file should be makes it unreadable.
	if err := os.MkdirAll(filepath.Join(projectDir, "project-id"), 0755); err != nil {
		t.Fatal(err)
	}
	_, _, err := ProvisionedProfile(projectDir, "some-agent", true, "", false)
	var de *AgentStateDirError
	if !errors.As(err, &de) {
		t.Fatalf("expected an *AgentStateDirError, got %v", err)
	}
	if strings.Contains(err.Error(), projectDir) {
		t.Errorf("message must not contain the host path: %v", err)
	}
	if !errors.Is(err, config.ErrAgentStateDirUnavailable) || !config.IsAgentStateConflict(err) {
		t.Errorf("expected an unavailable agent state dir, got %v", err)
	}
}

// TestProvisionedProfile_AgentStateDirErrorHasNoHostPath_InvalidProjectID:
// a project-id marker with an invalid ID fails the root lookup with an error
// that names the marker's host path but is not an *fs.PathError; the
// resulting AgentStateDirError's message must not carry that path either.
func TestProvisionedProfile_AgentStateDirErrorHasNoHostPath_InvalidProjectID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := filepath.Join(t.TempDir(), "proj", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "project-id"), []byte("not a valid id\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, _, err := ProvisionedProfile(projectDir, "some-agent", true, "", false)
	var de *AgentStateDirError
	if !errors.As(err, &de) {
		t.Fatalf("expected an *AgentStateDirError, got %v", err)
	}
	if strings.Contains(err.Error(), projectDir) {
		t.Errorf("message must not contain the host path: %v", err)
	}
	if !errors.Is(err, config.ErrAgentStateDirUnavailable) || !config.IsAgentStateConflict(err) {
		t.Errorf("expected an unavailable agent state dir, got %v", err)
	}
}
