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
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#4561 (TS-2d): settings- and profile-derived
// environment values (image, image pull policy, resources, volumes) are not
// persisted to scion-agent.json at provision; Start applies them from
// current settings, and the requester's own values stay persisted.

type envSettings struct {
	image, pullPolicy, settingsVolSource        string
	defaultMemReq, profileMemLimit, overrideCPU string
}

func (s envSettings) yaml() string {
	return `schema_version: "1"
active_profile: local
default_resources:
  requests:
    memory: ` + s.defaultMemReq + `
profiles:
  local:
    runtime: docker
    resources:
      limits:
        memory: ` + s.profileMemLimit + `
    harness_overrides:
      test-harness:
        resources:
          requests:
            cpu: "` + s.overrideCPU + `"
harness_configs:
  test-harness:
    harness: generic
    image: ` + s.image + `
    image_pull_policy: ` + s.pullPolicy + `
    volumes:
      - source: ` + s.settingsVolSource + `
        target: /mnt/settings
`
}

var envSettingsV1 = envSettings{
	image: "settings-image:v1", pullPolicy: "Always", settingsVolSource: "/host/settings-a",
	defaultMemReq: "1Gi", profileMemLimit: "2Gi", overrideCPU: "1",
}

var envSettingsV2 = envSettings{
	image: "settings-image:v2", pullPolicy: "Never", settingsVolSource: "/host/settings-b",
	defaultMemReq: "512Mi", profileMemLimit: "3Gi", overrideCPU: "2",
}

// envFixture writes a global .scion with a harness-config file (image, pull
// policy and a volume), a template with no environment keys and the given
// settings, and returns the project .scion dir and the settings path.
func envFixture(t *testing.T, s envSettings) (string, string) {
	t.Helper()
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_") {
			k := strings.SplitN(e, "=", 2)[0]
			t.Setenv(k, "")
			os.Unsetenv(k) //nolint:errcheck
		}
	}
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	mustMkdir(t, hcDir)
	mustWrite(t, filepath.Join(hcDir, "config.yaml"), `harness: generic
user: scion
image: file-image:v1
image_pull_policy: IfNotPresent
volumes:
  - source: /host/file
    target: /mnt/file
`)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	mustMkdir(t, tplDir)
	mustWrite(t, filepath.Join(tplDir, "scion-agent.json"), `{"default_harness_config": "test-harness"}`)
	settingsPath := filepath.Join(globalScionDir, "settings.yaml")
	mustWrite(t, settingsPath, s.yaml())

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	mustMkdir(t, projectScionDir)
	return projectScionDir, settingsPath
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// startEnv runs Manager.Start for agent "env-agent" and returns the run
// config handed to the runtime.
func startEnv(t *testing.T, projectScionDir string, mutate func(*api.StartOptions)) runtime.RunConfig {
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
	opts := api.StartOptions{
		Name:        "env-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	}
	if mutate != nil {
		mutate(&opts)
	}
	if _, err := NewManager(mockRT).Start(context.Background(), opts); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	return captured
}

func persistedEnvAgentConfig(t *testing.T, projectScionDir string) (api.ScionConfig, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(projectScionDir, "agents", "env-agent", "scion-agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, string(data)
}

func volumesAt(vols []api.VolumeMount, target string) []api.VolumeMount {
	var out []api.VolumeMount
	for _, v := range vols {
		if v.Target == target {
			out = append(out, v)
		}
	}
	return out
}

// assertEnvApplied checks the run config carries the settings values of s
// (with the harness-config file volume).
func assertEnvApplied(t *testing.T, rc runtime.RunConfig, s envSettings) {
	t.Helper()
	if rc.Image != s.image {
		t.Errorf("image = %q, want %q", rc.Image, s.image)
	}
	if got := pullPolicyOf(rc); got != s.pullPolicy {
		t.Errorf("image pull policy = %q, want %q", got, s.pullPolicy)
	}
	if rc.Resources == nil {
		t.Fatalf("resources = nil, want the settings tiers")
	}
	if got := rc.Resources.Requests.Memory; got != s.defaultMemReq {
		t.Errorf("requests.memory = %q, want %q (default_resources)", got, s.defaultMemReq)
	}
	if got := rc.Resources.Limits.Memory; got != s.profileMemLimit {
		t.Errorf("limits.memory = %q, want %q (profile resources)", got, s.profileMemLimit)
	}
	if got := rc.Resources.Requests.CPU; got != s.overrideCPU {
		t.Errorf("requests.cpu = %q, want %q (profile harness_overrides)", got, s.overrideCPU)
	}
	if rc.Resources.Limits.CPU == "" {
		t.Errorf("limits.cpu empty, want the built-in default")
	}
	sv := volumesAt(rc.Volumes, "/mnt/settings")
	if len(sv) != 1 || sv[0].Source != s.settingsVolSource {
		t.Errorf("volumes at /mnt/settings = %+v, want one from %q", sv, s.settingsVolSource)
	}
	if fv := volumesAt(rc.Volumes, "/mnt/file"); len(fv) != 1 {
		t.Errorf("volumes at /mnt/file = %+v, want one from the harness-config file", fv)
	}
}

// Acceptance 1 and 3: a newly provisioned agent's scion-agent.json has no
// settings-, profile- or harness-config-derived image, pull policy,
// resources or volumes, while the first start still applies them.
func TestProvision_EnvironmentValuesNotPersisted(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)

	rc := startEnv(t, project, nil)
	assertEnvApplied(t, rc, envSettingsV1)

	cfg, raw := persistedEnvAgentConfig(t, project)
	if cfg.Image != "" {
		t.Errorf("persisted image = %q, want none", cfg.Image)
	}
	if cfg.Kubernetes != nil {
		t.Errorf("persisted kubernetes = %+v, want none (image pull policy)", cfg.Kubernetes)
	}
	if cfg.Resources != nil {
		t.Errorf("persisted resources = %+v, want none", cfg.Resources)
	}
	for _, target := range []string{"/mnt/settings", "/mnt/file"} {
		if v := volumesAt(cfg.Volumes, target); len(v) != 0 {
			t.Errorf("persisted volumes at %s = %+v, want none", target, v)
		}
	}
	if cfg.Harness == "" || cfg.HarnessConfig != "test-harness" {
		t.Errorf("persisted config lost its job keys: %s", raw)
	}
}

// Acceptance 2 and 3: changing the settings and profile after provisioning
// takes effect on the next start, for every moved value.
func TestStart_EnvironmentSettingsChangeAppliesOnNextStart(t *testing.T) {
	project, settingsPath := envFixture(t, envSettingsV1)
	assertEnvApplied(t, startEnv(t, project, nil), envSettingsV1)

	mustWrite(t, settingsPath, envSettingsV2.yaml())
	assertEnvApplied(t, startEnv(t, project, nil), envSettingsV2)

	// A Start that rewrites scion-agent.json (here the corrupted
	// auth_selectedType repair) still persists no environment value.
	cfgPath := filepath.Join(project, "agents", "env-agent", "scion-agent.json")
	cfg, _ := persistedEnvAgentConfig(t, project)
	cfg.AuthSelectedType = "generic"
	data, _ := json.Marshal(cfg)
	mustWrite(t, cfgPath, string(data))
	startEnv(t, project, nil)
	cfg, raw := persistedEnvAgentConfig(t, project)
	if cfg.AuthSelectedType != "" {
		t.Fatalf("test precondition: Start did not rewrite scion-agent.json: %s", raw)
	}
	if cfg.Image != "" || cfg.Kubernetes != nil || cfg.Resources != nil || len(volumesAt(cfg.Volumes, "/mnt/settings")) != 0 {
		t.Errorf("Start's scion-agent.json rewrite persisted environment values: %s", raw)
	}
}

// Values the requester supplied directly (inline config) stay persisted
// and keep their rank on a restart without them.
func TestProvision_RequesterEnvironmentValuesPersisted(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)
	inline := &api.ScionConfig{
		Image:      "inline-image:v1",
		Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "IfNotPresent"},
		Resources:  &api.ResourceSpec{Limits: api.ResourceList{Memory: "4Gi"}},
		Volumes:    []api.VolumeMount{{Source: "/host/inline", Target: "/mnt/inline"}},
	}
	startEnv(t, project, func(o *api.StartOptions) { o.InlineConfig = inline })

	cfg, raw := persistedEnvAgentConfig(t, project)
	if cfg.Image != "inline-image:v1" {
		t.Errorf("persisted image = %q, want the inline image", cfg.Image)
	}
	if cfg.Kubernetes == nil || cfg.Kubernetes.ImagePullPolicy != "IfNotPresent" {
		t.Errorf("persisted kubernetes = %+v, want the inline pull policy", cfg.Kubernetes)
	}
	if cfg.Resources == nil || cfg.Resources.Limits.Memory != "4Gi" || cfg.Resources.Requests != (api.ResourceList{}) || cfg.Resources.Limits.CPU != "" {
		t.Errorf("persisted resources = %+v, want only the inline limits.memory", cfg.Resources)
	}
	if v := volumesAt(cfg.Volumes, "/mnt/inline"); len(v) != 1 {
		t.Errorf("persisted inline volume missing: %s", raw)
	}
	if v := volumesAt(cfg.Volumes, "/mnt/settings"); len(v) != 0 {
		t.Errorf("settings volume persisted: %s", raw)
	}

	// A restart with no inline config.
	rc := startEnv(t, project, nil)
	if rc.Image != "inline-image:v1" {
		t.Errorf("restart image = %q, want the inline image", rc.Image)
	}
	if got := pullPolicyOf(rc); got != "IfNotPresent" {
		t.Errorf("restart pull policy = %q, want the inline one", got)
	}
	if rc.Resources == nil || rc.Resources.Limits.Memory != "4Gi" || rc.Resources.Requests.CPU != "1" {
		t.Errorf("restart resources = %+v, want inline limits.memory 4Gi over profile, harness override requests.cpu 1", rc.Resources)
	}
	if v := volumesAt(rc.Volumes, "/mnt/inline"); len(v) != 1 {
		t.Errorf("restart volumes = %+v, want the inline volume once", rc.Volumes)
	}
	if v := volumesAt(rc.Volumes, "/mnt/settings"); len(v) != 1 {
		t.Errorf("restart volumes = %+v, want the settings volume once", rc.Volumes)
	}
}

// An agent provisioned before ptone/scion#4561 has the settings values of
// that time baked into scion-agent.json. After a settings change it mounts
// the current settings volume once (never the stale one, never both), and
// the current profile resources apply over default ones as before.
func TestStart_LegacyBakedEnvironmentValues(t *testing.T) {
	project, settingsPath := envFixture(t, envSettingsV1)
	startEnv(t, project, nil)

	// Simulate the pre-change persisted config.
	cfgPath := filepath.Join(project, "agents", "env-agent", "scion-agent.json")
	cfg, _ := persistedEnvAgentConfig(t, project)
	cfg.Image = "settings-image:v1"
	cfg.Kubernetes = &api.KubernetesConfig{ImagePullPolicy: "Always"}
	cfg.Volumes = append(cfg.Volumes,
		api.VolumeMount{Source: "/host/settings-a", Target: "/mnt/settings"},
		api.VolumeMount{Source: "/host/file", Target: "/mnt/file"},
	)
	data, _ := json.Marshal(cfg)
	mustWrite(t, cfgPath, string(data))

	mustWrite(t, settingsPath, envSettingsV2.yaml())
	rc := startEnv(t, project, nil)

	sv := volumesAt(rc.Volumes, "/mnt/settings")
	if len(sv) != 1 || sv[0].Source != "/host/settings-b" {
		t.Errorf("volumes at /mnt/settings = %+v, want only the current /host/settings-b", sv)
	}
	if fv := volumesAt(rc.Volumes, "/mnt/file"); len(fv) != 1 {
		t.Errorf("volumes at /mnt/file = %+v, want one", fv)
	}
	if rc.Image != "settings-image:v2" {
		t.Errorf("image = %q, want the current settings image", rc.Image)
	}
	if got := pullPolicyOf(rc); got != "Never" {
		t.Errorf("pull policy = %q, want the current settings one", got)
	}
}

func TestMergeEnvironmentVolumes(t *testing.T) {
	settings := []api.VolumeMount{{Source: "/s", Target: "/mnt/a"}, {Source: "/sw", Target: "/workspace"}}
	file := []api.VolumeMount{{Source: "/f", Target: "/mnt/b"}}
	own := []api.VolumeMount{
		{Source: "/stale", Target: "/mnt/a"},
		{Source: "/ws", Target: "/workspace"},
		{Source: "/o", Target: "/mnt/c"},
	}
	got := mergeEnvironmentVolumes(settings, file, own)
	want := []api.VolumeMount{
		{Source: "/s", Target: "/mnt/a"},
		{Source: "/sw", Target: "/workspace"},
		{Source: "/f", Target: "/mnt/b"},
		{Source: "/ws", Target: "/workspace"},
		{Source: "/o", Target: "/mnt/c"},
	}
	if len(got) != len(want) {
		t.Fatalf("mergeEnvironmentVolumes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("volume %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
