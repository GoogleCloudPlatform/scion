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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// hubDefaultsFixture seeds a HOME with global settings.yaml (the broker's own
// defaults tier) and, optionally, a template that sets limits explicitly. It
// returns the project .scion dir to provision into.
//
// settingsYAML is written verbatim so a test can control exactly which broker
// defaults exist; templateJSON, when non-empty, becomes template "limits-tpl".
func hubDefaultsFixture(t *testing.T, settingsYAML, templateJSON string) string {
	t.Helper()
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	originalHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", originalHome) })
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	if err := os.MkdirAll(globalTemplatesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedTestHarnessConfig(t, globalScionDir, "test-harness", "test-harness")

	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	if templateJSON != "" {
		tplDir := filepath.Join(globalTemplatesDir, "limits-tpl")
		if err := os.MkdirAll(tplDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(templateJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

// 🔴 TestProvision_HubAgentDefaults_BelowTemplateAboveSettings is the rank test
// for Gap 2c, and the ladder is the point: a single-value assertion cannot tell
// the three tiers apart.
//
//	rung 1  template 100 + hub 50 + settings.yaml 10  →  100
//	rung 2            hub 50 + settings.yaml 10       →   50
//	rung 3                     settings.yaml 10       →   10
//
// Rung 1 is acceptance criterion 8, the inversion detector: if it returns 50 the
// hub defaults were stamped somewhere top-of-chain (InlineConfig — rejected
// alternative A5) instead of at the broker's low-precedence tier. Rung 2 is
// criterion 7/9 and rung 3 proves the hub tier sits above settings.yaml without
// replacing it.
func TestProvision_HubAgentDefaults_BelowTemplateAboveSettings(t *testing.T) {
	// LOAD-BEARING, and it is shared by all three rungs on purpose. Because this
	// one const stays live in every subtest, rung 1 runs template 100 + hub 50 +
	// BROKER 10 simultaneously and therefore asserts the (template, broker) pair
	// as well as (template, hub). That coverage is easy to delete by accident:
	// "rung 1 never looks at settings.yaml" is true of the assertion and false of
	// the fixture. Do not give the rungs separate settingsYAML values, and do not
	// drop default_max_turns here because rung 3 looks like its only consumer.
	const settingsYAML = `schema_version: "1"
default_harness_config: test-harness
default_max_turns: 10
default_max_model_calls: 11
default_max_duration: 10m
harness_configs:
  test-harness:
    harness: test-harness
`
	const templateJSON = `{
		"default_harness_config": "test-harness",
		"max_turns": 100,
		"max_model_calls": 101,
		"max_duration": "100m"
	}`

	hubDefaults := &api.HubAgentDefaults{
		MaxTurns:      50,
		MaxModelCalls: 51,
		MaxDuration:   "50m",
	}

	tests := []struct {
		name              string
		template          string
		hub               *api.HubAgentDefaults
		wantTurns         int
		wantCalls         int
		wantDuration      string
		wantExplanation   string
		seedTemplateFiles bool
	}{
		{
			name:         "template beats hub default and settings",
			template:     "limits-tpl",
			hub:          hubDefaults,
			wantTurns:    100,
			wantCalls:    101,
			wantDuration: "100m",
			wantExplanation: "criterion 8, and it decides TWO pairs at once: (template, hub) — a hub-wide " +
				"floor must never override a deliberate per-template value — and (template, broker), " +
				"because settingsYAML's default_max_turns 10 is live in this subtest too",
			seedTemplateFiles: true,
		},
		{
			name:         "hub default applies with no template value",
			hub:          hubDefaults,
			wantTurns:    50,
			wantCalls:    51,
			wantDuration: "50m",
			wantExplanation: "criteria 7 and 9, the (hub, broker) pair: hub default wins over " +
				"the broker's own settings.yaml",
		},
		{
			name:         "settings.yaml applies with no hub default",
			wantTurns:    10,
			wantCalls:    11,
			wantDuration: "10m",
			wantExplanation: "the broker's own defaults tier still works when the hub sends nothing " +
				"(file-mode / old-hub parity)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tplJSON := ""
			if tc.seedTemplateFiles {
				tplJSON = templateJSON
			}
			projectScionDir := hubDefaultsFixture(t, settingsYAML, tplJSON)

			ctx := context.Background()
			if tc.hub != nil {
				ctx = api.ContextWithHubAgentDefaults(ctx, tc.hub)
			}

			_, _, cfg, err := ProvisionAgent(ctx, "ladder-agent", tc.template, "", "test-harness",
				projectScionDir, "", "", "", "")
			if err != nil {
				t.Fatalf("ProvisionAgent: %v", err)
			}
			if cfg == nil {
				t.Fatal("ProvisionAgent returned nil config")
			}
			if cfg.MaxTurns != tc.wantTurns {
				t.Errorf("MaxTurns: want %d, got %d (%s)", tc.wantTurns, cfg.MaxTurns, tc.wantExplanation)
			}
			if cfg.MaxModelCalls != tc.wantCalls {
				t.Errorf("MaxModelCalls: want %d, got %d (%s)", tc.wantCalls, cfg.MaxModelCalls, tc.wantExplanation)
			}
			if cfg.MaxDuration != tc.wantDuration {
				t.Errorf("MaxDuration: want %q, got %q (%s)", tc.wantDuration, cfg.MaxDuration, tc.wantExplanation)
			}
		})
	}
}

// TestProvision_HubAgentDefaults_Resources covers the Resources field, which
// merges per-field rather than winning or losing whole. Hub defaults go in as
// MergeResourceSpec's BASE, so a field only the hub sets survives and a field
// the template also sets goes to the template.
//
// All THREE tiers are live here on purpose — template, hub, and the broker's own
// settings.yaml default_resources — so the fixture asserts a three-tier rank and
// not merely the (template, hub) pair. See the load-bearing note on settingsYAML.
func TestProvision_HubAgentDefaults_Resources(t *testing.T) {
	// LOAD-BEARING: default_resources must stay here, and must keep setting
	// fields the hub below also sets. It is the ONLY broker-tier resources value
	// anywhere in this file that runs alongside a hub value, and it is what makes
	// the (hub, broker) rank observable for Resources.
	//
	// Without it this test passes even when the hub tier is moved BELOW the
	// settings.yaml tier in provision.go — the exact refactor the comment at that
	// insertion point warns against. That reordering was mutation-probed and this
	// test survived it, because a fixture that declares no broker value cannot
	// tell you which of the two tiers won. Deleting these four lines as
	// "unused fixture noise" silently restores that blind spot.
	//
	// cpu "7" and disk "5Gi" are arbitrary but must differ from every other tier:
	// not the hub's "3"/"20Gi", not the template's memory, and not the "2" that
	// config.BuiltinDefaultResources() supplies at the bottom.
	const settingsYAML = `schema_version: "1"
default_harness_config: test-harness
default_resources:
  limits:
    cpu: "7"
  disk: "5Gi"
harness_configs:
  test-harness:
    harness: test-harness
`
	// Template sets memory only.
	const templateJSON = `{
		"default_harness_config": "test-harness",
		"resources": {
			"limits": {"memory": "8Gi"}
		}
	}`

	projectScionDir := hubDefaultsFixture(t, settingsYAML, templateJSON)

	// Hub sets CPU only, plus a memory value the template must beat.
	//
	// CPU is deliberately "3", not "2": config.BuiltinDefaultResources() fills
	// an unset CPU limit with "2" at the tier below, so asserting "2" here
	// would pass without the hub tier existing at all — the assertion has to
	// name a value only the hub could have supplied.
	ctx := api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{
		Resources: &api.ResourceSpec{
			Limits: api.ResourceList{CPU: "3", Memory: "1Gi"},
			Disk:   "20Gi",
		},
	})

	_, _, cfg, err := ProvisionAgent(ctx, "res-agent", "limits-tpl", "", "test-harness",
		projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	if cfg.Resources == nil {
		t.Fatal("Resources is nil; the hub's CPU-only default did not survive")
	}
	// CPU and Disk are the (hub, broker) rank: both tiers set them, the hub must
	// win. Getting "7" or "5Gi" here means the hub tier now runs BELOW
	// settings.yaml — a rank inversion, not a value change.
	if cfg.Resources.Limits.CPU != "3" {
		t.Errorf("Limits.CPU: want 3 from the hub default, got %q "+
			"(%q is the broker settings.yaml default, which the hub tier must outrank)",
			cfg.Resources.Limits.CPU, "7")
	}
	if cfg.Resources.Disk != "20Gi" {
		t.Errorf("Disk: want 20Gi from the hub default, got %q "+
			"(%q is the broker settings.yaml default, which the hub tier must outrank)",
			cfg.Resources.Disk, "5Gi")
	}
	// Memory is the (template, hub) rank in the other direction.
	if cfg.Resources.Limits.Memory != "8Gi" {
		t.Errorf("Limits.Memory: want 8Gi from the template, got %q", cfg.Resources.Limits.Memory)
	}
}

// TestProvision_HubAgentDefaults_AbsentIsUnchanged is the file-mode / old-hub
// parity guard on the broker side: with nothing on the context, provisioning
// must land on exactly the values the pre-change tree produced — the broker's
// own settings.yaml defaults, applied at the bottom.
func TestProvision_HubAgentDefaults_AbsentIsUnchanged(t *testing.T) {
	const settingsYAML = `schema_version: "1"
default_harness_config: test-harness
default_max_turns: 10
default_resources:
  limits:
    cpu: "1"
harness_configs:
  test-harness:
    harness: test-harness
`
	projectScionDir := hubDefaultsFixture(t, settingsYAML, "")

	_, _, cfg, err := ProvisionAgent(context.Background(), "nohub-agent", "", "", "test-harness",
		projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	if cfg.MaxTurns != 10 {
		t.Errorf("MaxTurns: want 10 from settings.yaml, got %d", cfg.MaxTurns)
	}
	if cfg.Resources == nil || cfg.Resources.Limits.CPU != "1" {
		t.Errorf("Resources: want cpu=1 from settings.yaml, got %+v", cfg.Resources)
	}
}

// TestProvision_HubAgentDefaults_ContextValueSurvivesProvisioning is a SMOKE
// TEST, NOT A PIN. Read this before trusting it.
//
// It does not — and structurally cannot — pin the defensive copy at
// provision.go (the `base := *hd.Resources` copy in the hub agent_defaults
// block). Review proved that by deleting the copy and watching this stay
// green. The reason is the `finalScionCfg = updatedCfg` reload near the end of
// ProvisionAgent: it reloads
// finalScionCfg from disk before returning, so the pointer this test inspects
// can never be the aliased one no matter what the merge did. Asserting on the
// written scion-agent.json does not help either — that file is marshalled from
// the config and re-read, which launders the aliasing the same way. Anything
// downstream of the reload is incapable of failing on this property.
//
// What it DOES check, which is still worth having: provisioning with hub
// resources on the context completes, produces a usable spec, and leaves the
// caller's *api.HubAgentDefaults unmodified.
//
// The actual guard on the copy is TestMergeResourceSpec_ReturnsBaseWhenOverrideNil
// in pkg/config, which fails when the REASON for the copy disappears. It lives
// there rather than here on purpose: identity cannot be asserted from anywhere
// downstream of ProvisionAgent's reload, so a copy of it in this package would
// be another test that cannot fail. Do not move it back.
func TestProvision_HubAgentDefaults_ContextValueSurvivesProvisioning(t *testing.T) {
	const settingsYAML = `schema_version: "1"
default_harness_config: test-harness
harness_configs:
  test-harness:
    harness: test-harness
`
	projectScionDir := hubDefaultsFixture(t, settingsYAML, "")

	hub := &api.HubAgentDefaults{Resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "3"}}}
	ctx := api.ContextWithHubAgentDefaults(context.Background(), hub)

	_, _, cfg, err := ProvisionAgent(ctx, "alias-agent", "", "", "test-harness",
		projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	if cfg.Resources == nil {
		t.Fatal("Resources is nil")
	}
	// Both of the following pass with the defensive copy removed, because of the
	// disk reload described above. They are kept as smoke coverage of the path,
	// not as evidence about aliasing.
	if cfg.Resources == hub.Resources {
		t.Fatal("provisioned config aliases the context's ResourceSpec")
	}
	if hub.Resources.Limits.CPU != "3" {
		t.Errorf("provisioning modified the caller's hub defaults: got %q", hub.Resources.Limits.CPU)
	}
}

// TestHubAgentDefaultsContext_RoundTrip covers the context plumbing itself,
// including the "absent" case every caller relies on for the file-mode path.
func TestHubAgentDefaultsContext_RoundTrip(t *testing.T) {
	if got := api.HubAgentDefaultsFromContext(context.Background()); got != nil {
		t.Errorf("want nil from a bare context, got %+v", got)
	}
	want := &api.HubAgentDefaults{MaxTurns: 50}
	got := api.HubAgentDefaultsFromContext(api.ContextWithHubAgentDefaults(context.Background(), want))
	if got != want {
		t.Errorf("want %+v, got %+v", want, got)
	}
}

// Compile-time reminder that the application point reads the same config helper
// the settings tier below it uses; if MergeResourceSpec's signature changes,
// this fails here rather than silently reordering the tiers.
var _ = config.MergeResourceSpec
