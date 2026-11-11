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
	"testing"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Every bundled harness has an agent-plugins.yaml that passes the strict
// parse (the loaders would degrade a bad one to "no" silently).
func TestEmbeddedHarnessConfigs_AgentPluginsSidecarStrict(t *testing.T) {
	for _, name := range AllHarnessNames() {
		data, err := fs.ReadFile(harnessesEmbed.FS, name+"/"+config.AgentPluginsSidecarFile)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if _, err := config.ParseAgentPluginsSidecar(data); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The harnesses report the sidecar over the legacy config.yaml value, fall
// back to that value, and a sidecar alone does not replace the declarative
// harness's Generic defaults for the other capabilities.
func TestAdvancedCapabilities_AgentPluginsSidecar(t *testing.T) {
	sidecar := &api.CapabilityField{Support: api.SupportPartial, Reason: "sidecar"}
	legacy := &api.HarnessAdvancedCapabilities{
		Resume:       api.CapabilityField{Support: api.SupportYes},
		AgentPlugins: api.CapabilityField{Support: api.SupportNo, Reason: "legacy"},
	}
	prov := &config.HarnessProvisionerConfig{Type: "container-script"}
	tests := []struct {
		name  string
		entry config.HarnessConfigEntry
		want  api.CapabilityField
	}{
		{"sidecar wins", config.HarnessConfigEntry{Harness: "h", Capabilities: legacy, AgentPluginsSidecar: sidecar}, *sidecar},
		{"legacy fallback", config.HarnessConfigEntry{Harness: "h", Capabilities: legacy}, legacy.AgentPlugins},
		{"neither", config.HarnessConfigEntry{Harness: "h"}, api.CapabilityField{}},
		{"sidecar only", config.HarnessConfigEntry{Harness: "h", AgentPluginsSidecar: sidecar}, *sidecar},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewDeclarativeGenericHarness(tt.entry).AdvancedCapabilities().AgentPlugins; got != tt.want {
				t.Errorf("declarative: %+v, want %+v", got, tt.want)
			}
			entry := tt.entry
			entry.Provisioner = prov
			csh, err := NewContainerScriptHarness(t.TempDir(), entry)
			if err != nil {
				t.Fatal(err)
			}
			caps := csh.AdvancedCapabilities()
			if caps.AgentPlugins != tt.want {
				t.Errorf("container-script: %+v, want %+v", caps.AgentPlugins, tt.want)
			}
			if caps.Harness != "h" {
				t.Errorf("container-script: Harness = %q", caps.Harness)
			}
		})
	}

	// Sidecar only: the declarative harness keeps the Generic matrix.
	got := NewDeclarativeGenericHarness(config.HarnessConfigEntry{Harness: "h", AgentPluginsSidecar: sidecar}).AdvancedCapabilities()
	want := (&Generic{}).AdvancedCapabilities()
	want.Harness = "h"
	want.AgentPlugins = *sidecar
	if got != want {
		t.Errorf("declarative sidecar only: %+v, want Generic defaults %+v", got, want)
	}
}
