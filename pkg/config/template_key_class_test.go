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

package config

import (
	"io/fs"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Tests for the template key classification (ptone/scion#4524).

func TestStripTemplateEnvironmentKeys_AllEnvironmentKeys(t *testing.T) {
	cfg := &api.ScionConfig{
		HarnessConfig: "claude",
		Model:         "opus",
		Telemetry:     &api.TelemetryConfig{},
		Hub:           &api.AgentHubConfig{},
		Image:         "example.com/img:1",
		Resources:     &api.ResourceSpec{},
		Kubernetes:    &api.KubernetesConfig{},
		Volumes:       []api.VolumeMount{{Source: "/a", Target: "/b"}},
		Skills:        []api.SkillReference{{URI: "skill://x"}},
		Env: map[string]string{
			"SCION_HUB_URL":               "http://hub",
			"SCION_WORKSPACE_GIT":         "true",
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel",
			"TZ":                          "UTC",
			"ANTHROPIC_API_KEY":           "sk",
			"MY_FLAG":                     "1",
		},
	}
	orig := *cfg
	origEnv := map[string]string{}
	for k, v := range cfg.Env {
		origEnv[k] = v
	}

	got, ignored := StripTemplateEnvironmentKeys(cfg)

	// Only SCION_* env is reserved. OTEL_*, TZ and credential keys stay:
	// template env is a lowest-precedence fill applied by the agent, and TZ
	// is handled by the hub-only rule there.
	wantIgnored := []string{
		"env.SCION_HUB_URL",
		"env.SCION_WORKSPACE_GIT",
		"hub", "image", "kubernetes", "resources", "telemetry", "volumes",
	}
	if !reflect.DeepEqual(ignored, wantIgnored) {
		t.Errorf("ignored = %v, want %v", ignored, wantIgnored)
	}
	if got.Telemetry != nil || got.Hub != nil || got.Image != "" || got.Resources != nil || got.Kubernetes != nil || got.Volumes != nil {
		t.Errorf("environment keys survived: %+v", got)
	}
	wantEnv := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel",
		"TZ":                          "UTC",
		"ANTHROPIC_API_KEY":           "sk",
		"MY_FLAG":                     "1",
	}
	if !reflect.DeepEqual(got.Env, wantEnv) {
		t.Errorf("Env = %v, want every non-SCION_ key kept", got.Env)
	}
	if got.HarnessConfig != "claude" || got.Model != "opus" || len(got.Skills) != 1 {
		t.Errorf("job keys changed: %+v", got)
	}
	// The input is not modified.
	if !reflect.DeepEqual(cfg.Env, origEnv) || cfg.Image != orig.Image || cfg.Telemetry != orig.Telemetry || len(cfg.Volumes) != 1 {
		t.Errorf("input cfg was modified: %+v", cfg)
	}
}

func TestStripTemplateEnvironmentKeys_NoEnvironmentKeys(t *testing.T) {
	cfg := &api.ScionConfig{Model: "opus", Env: map[string]string{"MY_FLAG": "1", "ANTHROPIC_API_KEY": "sk"}}
	// A credential key is not reserved: template env is a fill.
	got, ignored := StripTemplateEnvironmentKeys(cfg)
	if ignored != nil || got != cfg {
		t.Errorf("got (%p, %v), want the input back and no ignored keys", got, ignored)
	}
	if got, ignored := StripTemplateEnvironmentKeys(nil); got != nil || ignored != nil {
		t.Errorf("nil cfg: got (%v, %v)", got, ignored)
	}
}

func TestStripTemplateEnvironmentKeys_OnlyReservedEnvDropsEnvMap(t *testing.T) {
	cfg := &api.ScionConfig{Env: map[string]string{"SCION_X": "1"}}
	got, ignored := StripTemplateEnvironmentKeys(cfg)
	if !reflect.DeepEqual(ignored, []string{"env.SCION_X"}) {
		t.Errorf("ignored = %v", ignored)
	}
	if got.Env != nil {
		t.Errorf("Env = %v, want nil", got.Env)
	}
}

func TestIsReservedTemplateEnvKey(t *testing.T) {
	for _, k := range []string{"SCION_HUB_URL", "SCION_WORKSPACE_GIT", "SCION_"} {
		if !IsReservedTemplateEnvKey(k) {
			t.Errorf("%s: want reserved", k)
		}
	}
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "TZ", "ANTHROPIC_API_KEY", "MY_SCION_X", "scion_x"} {
		if IsReservedTemplateEnvKey(k) {
			t.Errorf("%s: want not reserved", k)
		}
	}
}

// TestEmbeddedTemplatesSetNoEnvironmentKeys keeps the embedded (seeded)
// templates free of environment keys: a template describes the agent's job,
// not its operating environment.
func TestEmbeddedTemplatesSetNoEnvironmentKeys(t *testing.T) {
	checked := 0
	err := fs.WalkDir(EmbedsFS, "embeds", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.Contains(p, "/templates/") {
			return nil
		}
		switch path.Base(p) {
		case "scion-agent.yaml", "scion-agent.yml", "scion-agent.json":
		default:
			return nil
		}
		data, err := EmbedsFS.ReadFile(p)
		if err != nil {
			return err
		}
		cfg, err := ParseScionAgentConfig(path.Base(p), data)
		if err != nil {
			t.Errorf("%s: parse: %v", p, err)
			return nil
		}
		if keys := TemplateEnvironmentKeys(cfg); len(keys) > 0 {
			t.Errorf("embedded template %s sets environment key(s) %v; templates must set job keys only", p, keys)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no embedded template configs found")
	}
}

func TestTemplateEnvironmentKeysWarning(t *testing.T) {
	got := TemplateEnvironmentKeysWarning("web", []string{"env.TZ", "image"})
	want := "template web sets environment key(s) env.TZ, image; ignored (environment comes from settings)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The stripped copy never shares Env with the input, also when only
// top-level environment keys are dropped and no env key is reserved.
func TestStripTemplateEnvironmentKeys_CopyDoesNotAliasEnv(t *testing.T) {
	cfg := &api.ScionConfig{Image: "tpl:1", Env: map[string]string{"MY_FLAG": "1"}}
	got, ignored := StripTemplateEnvironmentKeys(cfg)
	if !reflect.DeepEqual(ignored, []string{"image"}) {
		t.Fatalf("ignored = %v, want [image]", ignored)
	}
	if !reflect.DeepEqual(got.Env, map[string]string{"MY_FLAG": "1"}) {
		t.Fatalf("Env = %v, want the non-reserved key kept", got.Env)
	}
	got.Env["MY_FLAG"] = "changed"
	got.Env["NEW"] = "x"
	if !reflect.DeepEqual(cfg.Env, map[string]string{"MY_FLAG": "1"}) {
		t.Errorf("mutating the stripped copy's Env changed the input: %v", cfg.Env)
	}
}

// A dropped key with a nil Env leaves Env nil in the copy.
func TestStripTemplateEnvironmentKeys_NilEnvStaysNil(t *testing.T) {
	got, _ := StripTemplateEnvironmentKeys(&api.ScionConfig{Image: "tpl:1"})
	if got.Env != nil {
		t.Errorf("Env = %v, want nil", got.Env)
	}
}
