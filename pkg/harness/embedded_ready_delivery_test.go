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

package harness

import (
	"io/fs"
	"reflect"
	"testing"
	"time"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// loadEmbeddedEntry reads harnesses/<name>/config.yaml from the embed FS and
// returns its raw bytes and parsed entry.
func loadEmbeddedEntry(t *testing.T, name string) ([]byte, config.HarnessConfigEntry) {
	t.Helper()
	data, err := fs.ReadFile(harnessesEmbed.FS, name+"/config.yaml")
	if err != nil {
		t.Fatalf("%s/config.yaml: read: %v", name, err)
	}
	entry, err := config.ParseHarnessConfigYAML(data)
	if err != nil {
		t.Fatalf("%s/config.yaml: parse: %v", name, err)
	}
	return data, entry
}

// TestEmbeddedHarnessConfigs_Validate checks that every embedded bundle
// config.yaml passes the JSON schema and every Go semantic check that
// LoadHarnessConfigDir runs.
func TestEmbeddedHarnessConfigs_Validate(t *testing.T) {
	names := AllHarnessNames()
	if len(names) == 0 {
		t.Fatal("AllHarnessNames() returned no harnesses")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			data, entry := loadEmbeddedEntry(t, name)
			verrs, err := config.ValidateHarnessConfig(data)
			if err != nil {
				t.Fatalf("schema validation error: %v", err)
			}
			if len(verrs) > 0 {
				t.Fatalf("schema violations: %v", verrs)
			}
			if err := entry.Thinking.Validate(); err != nil {
				t.Errorf("thinking block: %v", err)
			}
			if err := entry.Ready.Validate(); err != nil {
				t.Errorf("ready block: %v", err)
			}
			if err := entry.Delivery.Validate(); err != nil {
				t.Errorf("delivery block: %v", err)
			}
		})
	}
}

// TestEmbeddedHarnessConfigs_ValuesMatchLegacyTable pins every bundle's ready
// and delivery blocks to the behaviour they replace: the hook-mode harnesses
// of the in-container readiness table use hook (except copilot, whose
// sessionStart fires only on the first prompt), every other bundle uses
// delay, and every bundle's delivery equals the historical hard-coded Enter
// x3 at 300ms. The blocks must be written out explicitly, not left to
// defaults, so authors can see the values.
func TestEmbeddedHarnessConfigs_ValuesMatchLegacyTable(t *testing.T) {
	legacyHook := map[string]bool{
		"claude":     true,
		"gemini-cli": true,
		"grok-build": true,
	}
	wantHook := api.ReadySpec{Strategy: "hook", Settle: 1500 * time.Millisecond, Timeout: 90 * time.Second}
	wantDelay := api.ReadySpec{Strategy: "delay", Settle: 8 * time.Second}
	wantDelivery := api.DeliverySpec{SubmitKeys: []string{"Enter"}, ExtraSubmits: 2, ExtraSubmitDelay: 300 * time.Millisecond}

	seen := map[string]bool{}
	for _, name := range AllHarnessNames() {
		t.Run(name, func(t *testing.T) {
			_, entry := loadEmbeddedEntry(t, name)
			if entry.Ready == nil {
				t.Errorf("missing explicit ready block")
			}
			if entry.Delivery == nil {
				t.Errorf("missing explicit delivery block")
			}
			want := wantDelay
			if legacyHook[name] {
				want = wantHook
				seen[name] = true
			}
			if got := entry.ResolvedReady(); got != want {
				t.Errorf("ResolvedReady() = %+v, want %+v", got, want)
			}
			if got := entry.ResolvedDelivery(); !reflect.DeepEqual(got, wantDelivery) {
				t.Errorf("ResolvedDelivery() = %+v, want %+v", got, wantDelivery)
			}
			// Host delivery reads the spec through harness.New.
			if got := New(name).DeliverySpec(); !reflect.DeepEqual(got, wantDelivery) {
				t.Errorf("New(%q).DeliverySpec() = %+v, want %+v", name, got, wantDelivery)
			}
		})
	}
	for name := range legacyHook {
		if !seen[name] {
			t.Errorf("legacy hook-mode harness %q has no embedded bundle", name)
		}
	}
}

// TestEmbeddedHarnessConfigs_AgentPluginsCapability pins the agent_plugins
// capability of the bundles, declared in each bundle's agent-plugins.yaml
// sidecar (config.yaml must not carry it; ptone/scion#5196): codex installs
// natively (partial: sse skipped), copilot installs natively (yes), muse-code
// installs natively (partial), claude installs a skills-dir plugin (partial:
// wrapper and skip rule); the harnesses without native support say no.
func TestEmbeddedHarnessConfigs_AgentPluginsCapability(t *testing.T) {
	want := map[string]api.SupportLevel{
		"codex":       api.SupportPartial,
		"copilot":     api.SupportYes,
		"claude":      api.SupportPartial,
		"muse-code":   api.SupportPartial,
		"gemini-cli":  api.SupportNo,
		"grok-build":  api.SupportNo,
		"antigravity": api.SupportNo,
		"hermes":      api.SupportNo,
		"opencode":    api.SupportNo,
	}
	for name, level := range want {
		_, entry := loadEmbeddedEntry(t, name)
		if entry.Capabilities != nil && entry.Capabilities.AgentPlugins != (api.CapabilityField{}) {
			t.Errorf("%s: config.yaml declares capabilities.agent_plugins; it belongs in %s", name, config.AgentPluginsSidecarFile)
		}
		// Strict parse: the loaders degrade an invalid sidecar to "no", so
		// this is what catches a bad bundled file.
		data, err := fs.ReadFile(harnessesEmbed.FS, name+"/"+config.AgentPluginsSidecarFile)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		c, err := config.ParseAgentPluginsSidecar(data)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if c.Support != level {
			t.Errorf("%s: agent_plugins support = %q, want %q", name, c.Support, level)
		}
		if c.Reason == "" && level != api.SupportYes {
			t.Errorf("%s: agent_plugins has no reason", name)
		}
		// The built-in harness (harness.New, used when no harness-config dir
		// resolves) reads the same sidecar.
		if got := New(name).AdvancedCapabilities().AgentPlugins; got != c {
			t.Errorf("%s: New().AdvancedCapabilities().AgentPlugins = %+v, want %+v", name, got, c)
		}
	}
}
