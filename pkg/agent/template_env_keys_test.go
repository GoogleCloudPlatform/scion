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
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for template environment keys at provisioning (ptone/scion#4524):
// environment keys and reserved SCION_* env are ignored and reported, and a
// template's env never reaches the agent config or scion-agent.json (it is
// the separate template env layer; see template_env_fill_test.go).

const templateEnvSettingsYAML = `schema_version: "1"
default_harness_config: test-harness
harness_configs:
  test-harness:
    harness: test-harness
`

// templateEnvTemplateJSON sets every environment key, a reserved SCION_*
// env key, tool env (OTEL_*, TZ, a credential-looking key, a plain key) and
// a job key.
const templateEnvTemplateJSON = `{
	"default_harness_config": "test-harness",
	"model": "tpl-model",
	"telemetry": {"cloud": {"endpoint": "template-otel:4317"}},
	"hub": {"endpoint": "http://template-hub"},
	"image": "template-image:1",
	"resources": {"requests": {"cpu": "4"}},
	"kubernetes": {"imagePullPolicy": "Always"},
	"volumes": [{"source": "/tpl-src", "target": "/tpl-dst"}],
	"env": {
		"SCION_HUB_URL": "http://template-hub",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://template-otel",
		"TZ": "Pacific/Auckland",
		"TEST_HARNESS_API_KEY": "template-secret",
		"TPL_JOB_FLAG": "keep"
	}
}`

// templateEnvKeysInTemplate are the template's env keys, none of which may
// appear in the agent config.
var templateEnvKeysInTemplate = []string{"SCION_HUB_URL", "OTEL_EXPORTER_OTLP_ENDPOINT", "TZ", "TEST_HARNESS_API_KEY", "TPL_JOB_FLAG"}

// wantTemplateEnvIgnored is the ignored-key list of templateEnvTemplateJSON:
// the six environment keys and the reserved SCION_* env key.
var wantTemplateEnvIgnored = []string{
	"env.SCION_HUB_URL",
	"hub", "image", "kubernetes", "resources", "telemetry", "volumes",
}

func templateEnvFixture(t *testing.T) string {
	t.Helper()
	return hubDefaultsFixture(t, templateEnvSettingsYAML, templateEnvTemplateJSON)
}

// assertNoTemplateEnvironment checks that cfg carries none of the template's
// environment values and none of its env, and keeps its job keys.
func assertNoTemplateEnvironment(t *testing.T, label string, cfg *api.ScionConfig) {
	t.Helper()
	if cfg == nil {
		t.Fatalf("%s: nil config", label)
	}
	// Settings-level telemetry may still be present; only the template's
	// own value (marked by its endpoint) must not be.
	if cfg.Telemetry != nil && cfg.Telemetry.Cloud != nil && cfg.Telemetry.Cloud.Endpoint == "template-otel:4317" {
		t.Errorf("%s: telemetry from the template survived: %+v", label, cfg.Telemetry.Cloud)
	}
	if cfg.Hub != nil && cfg.Hub.Endpoint == "http://template-hub" {
		t.Errorf("%s: hub from the template survived: %+v", label, cfg.Hub)
	}
	if cfg.Image == "template-image:1" {
		t.Errorf("%s: image from the template survived", label)
	}
	if cfg.Resources != nil && cfg.Resources.Requests.CPU == "4" {
		t.Errorf("%s: resources from the template survived: %+v", label, cfg.Resources)
	}
	if cfg.Kubernetes != nil && cfg.Kubernetes.ImagePullPolicy == "Always" {
		t.Errorf("%s: kubernetes from the template survived: %+v", label, cfg.Kubernetes)
	}
	for _, v := range cfg.Volumes {
		if v.Source == "/tpl-src" || v.Target == "/tpl-dst" {
			t.Errorf("%s: volume from the template survived: %+v", label, v)
		}
	}
	for _, k := range templateEnvKeysInTemplate {
		if v, ok := cfg.Env[k]; ok {
			t.Errorf("%s: template env %s=%q reached the agent config (template env is a separate layer)", label, k, v)
		}
	}
	if cfg.Model != "tpl-model" {
		t.Errorf("%s: model = %q, want the template's job key tpl-model", label, cfg.Model)
	}
}

func wantTemplateEnvWarning() string {
	return config.TemplateEnvironmentKeysWarning("limits-tpl", wantTemplateEnvIgnored)
}

// Acceptance 3 and 10: the provisioned agent's config, and its stored
// scion-agent.json, contain none of the template's environment keys and
// none of its env; the ignored keys are reported once; job keys are
// unaffected.
func TestProvision_TemplateEnvironmentKeysIgnored(t *testing.T) {
	projectScionDir := templateEnvFixture(t)
	ctx, warns := withTemplateEnvWarnings(context.Background())

	agentDir, _, _, cfg, err := GetAgent(ctx, "env-agent", "limits-tpl", "", "test-harness", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent (provision): %v", err)
	}
	assertNoTemplateEnvironment(t, "provisioned config", cfg)

	stored, err := (&config.Template{Path: agentDir}).LoadConfig()
	if err != nil {
		t.Fatalf("load stored scion-agent.json: %v", err)
	}
	assertNoTemplateEnvironment(t, "stored scion-agent.json", stored)
	raw, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range templateEnvKeysInTemplate {
		if strings.Contains(string(raw), k) {
			t.Errorf("scion-agent.json mentions template env key %s:\n%s", k, raw)
		}
	}

	if got, want := warns.Warnings(), []string{wantTemplateEnvWarning()}; !reflect.DeepEqual(got, want) {
		t.Errorf("warnings = %q,\nwant %q", got, want)
	}
}

// A restart of an agent whose template sets environment keys and env does
// not reintroduce them under the stored config (GetAgent merges the
// template chain under it), and reports the ignored keys once. Fails if
// GetAgent skips templateJobConfig.
func TestGetAgent_RestartDoesNotReintroduceTemplateEnvironmentKeys(t *testing.T) {
	projectScionDir := templateEnvFixture(t)
	if _, _, _, _, err := GetAgent(context.Background(), "env-agent", "limits-tpl", "", "test-harness", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("GetAgent (provision): %v", err)
	}

	ctx, warns := withTemplateEnvWarnings(context.Background())
	_, _, _, cfg, err := GetAgent(ctx, "env-agent", "", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent (restart): %v", err)
	}
	assertNoTemplateEnvironment(t, "restart config", cfg)
	if got, want := warns.Warnings(), []string{wantTemplateEnvWarning()}; !reflect.DeepEqual(got, want) {
		t.Errorf("restart warnings = %q,\nwant %q", got, want)
	}
}

// PreflightResolve repeats the resolution the real provisioning does; it
// must not report the ignored keys itself.
func TestPreflightResolve_TemplateEnvironmentKeysQuiet(t *testing.T) {
	projectScionDir := templateEnvFixture(t)
	ctx, warns := withTemplateEnvWarnings(context.Background())
	if err := PreflightResolve(ctx, api.StartOptions{Name: "env-agent", Template: "limits-tpl", HarnessConfig: "test-harness", ProjectPath: projectScionDir}); err != nil {
		t.Fatalf("PreflightResolve: %v", err)
	}
	if got := warns.Warnings(); len(got) != 0 {
		t.Errorf("preflight reported warnings %q, want none", got)
	}
}

func TestTemplateJobConfig_DropsEnvAndDedupesWarnings(t *testing.T) {
	ctx, warns := withTemplateEnvWarnings(context.Background())
	tpl := &config.Template{Name: "dup", Path: t.TempDir()}
	cfg := &api.ScionConfig{Model: "m", Image: "x", Env: map[string]string{"TOOL": "1"}}
	job := templateJobConfig(ctx, tpl, cfg)
	_ = templateJobConfig(ctx, tpl, cfg)
	if job.Env != nil || job.Image != "" || job.Model != "m" {
		t.Errorf("job config = %+v, want model only (no env, no image)", job)
	}
	if cfg.Env["TOOL"] != "1" || cfg.Image != "x" {
		t.Errorf("input config was modified: %+v", cfg)
	}
	want := []string{"template dup sets environment key(s) image; ignored (environment comes from settings)"}
	if got := warns.Warnings(); !reflect.DeepEqual(got, want) {
		t.Errorf("warnings = %q, want %q", got, want)
	}
	// A template with env only: no warning, and still no env in the job config.
	if job := templateJobConfig(ctx, tpl, &api.ScionConfig{Env: map[string]string{"TOOL": "1"}}); job.Env != nil {
		t.Errorf("env-only template: job Env = %v, want nil", job.Env)
	}
	if got := warns.Warnings(); len(got) != 1 {
		t.Errorf("env-only template added a warning: %q", got)
	}
}

// A collector built without its seen map still records and dedupes.
func TestTemplateJobConfig_ZeroValueCollector(t *testing.T) {
	c := &templateEnvWarnings{}
	ctx := context.WithValue(context.Background(), templateEnvWarningsKey{}, c)
	tpl := &config.Template{Name: "z", Path: t.TempDir()}
	_ = templateJobConfig(ctx, tpl, &api.ScionConfig{Image: "x"})
	_ = templateJobConfig(ctx, tpl, &api.ScionConfig{Image: "x"})
	if got := c.Warnings(); len(got) != 1 {
		t.Errorf("warnings = %q, want exactly one", got)
	}
}

// writeTemplateJSON writes a global template with the given config.
func writeTemplateJSON(t *testing.T, name string, cfg map[string]any) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".scion", "templates", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
