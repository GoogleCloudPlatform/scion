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
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for ignoring environment keys in templates (ptone/scion#4524).

const templateEnvSettingsYAML = `schema_version: "1"
default_harness_config: test-harness
harness_configs:
  test-harness:
    harness: test-harness
`

// templateEnvTemplateJSON sets every environment key, reserved env keys
// (including the harness-config's credential key) and a set of job keys.
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

// wantTemplateEnvIgnored is the ignored-key list templateEnvTemplateJSON
// gives with the credential-declaring harness-config.
var wantTemplateEnvIgnored = []string{
	"env.OTEL_EXPORTER_OTLP_ENDPOINT",
	"env.SCION_HUB_URL",
	"env.TEST_HARNESS_API_KEY",
	"env.TZ",
	"hub", "image", "kubernetes", "resources", "telemetry", "volumes",
}

// templateEnvFixture builds a project whose limits-tpl template sets
// environment keys and whose test-harness harness-config declares
// TEST_HARNESS_API_KEY as a credential key.
func templateEnvFixture(t *testing.T) string {
	t.Helper()
	projectScionDir := hubDefaultsFixture(t, templateEnvSettingsYAML, templateEnvTemplateJSON)
	home, _ := os.UserHomeDir()
	hcConfig := `harness: test-harness
image: test-image:latest
auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: [TEST_HARNESS_API_KEY]
`
	if err := os.WriteFile(filepath.Join(home, ".scion", "harness-configs", "test-harness", "config.yaml"), []byte(hcConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

// assertNoTemplateEnvironment checks that cfg carries none of the template's
// environment values and keeps its job keys.
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
	for _, k := range []string{"SCION_HUB_URL", "OTEL_EXPORTER_OTLP_ENDPOINT", "TZ", "TEST_HARNESS_API_KEY"} {
		if v, ok := cfg.Env[k]; ok {
			t.Errorf("%s: reserved env %s=%q from the template survived", label, k, v)
		}
	}
	if cfg.Model != "tpl-model" {
		t.Errorf("%s: model = %q, want the template's job key tpl-model", label, cfg.Model)
	}
	if cfg.Env["TPL_JOB_FLAG"] != "keep" {
		t.Errorf("%s: non-reserved env TPL_JOB_FLAG = %q, want keep", label, cfg.Env["TPL_JOB_FLAG"])
	}
}

func wantTemplateEnvWarning() string {
	return "template limits-tpl sets environment key(s) " + strings.Join(wantTemplateEnvIgnored, ", ") +
		"; ignored (environment comes from settings)"
}

// Acceptance: the provisioned agent's config, and its stored
// scion-agent.json, contain none of the template's environment keys; each
// is reported once as a warning; job keys are unaffected.
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

	if got, want := warns.Warnings(), []string{wantTemplateEnvWarning()}; !reflect.DeepEqual(got, want) {
		t.Errorf("warnings = %q,\nwant %q", got, want)
	}
}

// Acceptance: a restart of an agent whose template sets environment keys
// does not reintroduce them (GetAgent merges the template chain under the
// stored config), and reports them once. Fails if GetAgent skips stripping.
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

func TestStripTemplateEnvironment_DedupesWarnings(t *testing.T) {
	ctx, warns := withTemplateEnvWarnings(context.Background())
	tpl := &config.Template{Name: "dup", Path: t.TempDir()}
	cfg := &api.ScionConfig{Image: "x"}
	_ = stripTemplateEnvironment(ctx, tpl, cfg, nil)
	_ = stripTemplateEnvironment(ctx, tpl, cfg, nil)
	want := []string{"template dup sets environment key(s) image; ignored (environment comes from settings)"}
	if got := warns.Warnings(); !reflect.DeepEqual(got, want) {
		t.Errorf("warnings = %q, want %q", got, want)
	}
}
