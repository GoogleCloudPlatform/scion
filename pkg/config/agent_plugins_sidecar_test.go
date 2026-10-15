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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"gopkg.in/yaml.v3"
)

const sidecarTestConfig = "harness: generic\nimage: scion-base:test\n"

func writeSidecarTestDir(t *testing.T, configYAML, sidecar string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if sidecar != "" {
		if err := os.WriteFile(filepath.Join(dir, AgentPluginsSidecarFile), []byte(sidecar), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestLoadHarnessConfigDir_AgentPluginsSidecar covers the precedence: the
// sidecar wins, capabilities.agent_plugins in config.yaml (Phase 1, still
// accepted) is the fallback, and neither means the config predates support.
func TestLoadHarnessConfigDir_AgentPluginsSidecar(t *testing.T) {
	legacy := sidecarTestConfig + "capabilities:\n  resume: { support: \"yes\" }\n  agent_plugins: { support: \"no\", reason: \"legacy\" }\n"
	tests := []struct {
		name    string
		config  string
		sidecar string
		want    api.CapabilityField
	}{
		{"sidecar only", sidecarTestConfig, "support: \"partial\"\nreason: from sidecar\n", api.CapabilityField{Support: api.SupportPartial, Reason: "from sidecar"}},
		{"legacy field only", legacy, "", api.CapabilityField{Support: api.SupportNo, Reason: "legacy"}},
		{"neither", sidecarTestConfig, "", api.CapabilityField{}},
		{"both, sidecar wins", legacy, "support: \"yes\"\n", api.CapabilityField{Support: api.SupportYes}},
		{"unknown sidecar keys ignored", sidecarTestConfig, "support: \"no\"\nreason: r\nfuture_key: 1\n", api.CapabilityField{Support: api.SupportNo, Reason: "r"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc, err := LoadHarnessConfigDir(writeSidecarTestDir(t, tt.config, tt.sidecar))
			if err != nil {
				t.Fatal(err)
			}
			var got api.CapabilityField
			if hc.Config.Capabilities != nil {
				got = hc.Config.Capabilities.AgentPlugins
			}
			if got != tt.want {
				t.Fatalf("agent_plugins = %+v, want %+v", got, tt.want)
			}
		})
	}

	// The sidecar does not disturb the other capabilities of config.yaml.
	hc, err := LoadHarnessConfigDir(writeSidecarTestDir(t, legacy, "support: \"yes\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if hc.Config.Capabilities.Resume.Support != api.SupportYes {
		t.Fatalf("resume = %+v, want yes", hc.Config.Capabilities.Resume)
	}
}

func TestLoadHarnessConfigDir_InvalidAgentPluginsSidecar(t *testing.T) {
	for _, sidecar := range []string{"support: \"maybe\"\n", "reason: no level\n", "support: [yes]\n"} {
		_, err := LoadHarnessConfigDir(writeSidecarTestDir(t, sidecarTestConfig, sidecar))
		if err == nil || !strings.Contains(err.Error(), AgentPluginsSidecarFile) {
			t.Errorf("sidecar %q: err = %v, want an %s error", sidecar, err, AgentPluginsSidecarFile)
		}
	}
}

// TestSeedHarnessConfigFromDir_AgentPluginsSidecarAtRoot seeds the bundled
// claude harness-config: the sidecar lands next to config.yaml, not under
// home/<config_dir>/ (which would copy it into the agent home), and a
// non-force reseed refreshes a stale sidecar like the provisioner scripts.
func TestSeedHarnessConfigFromDir_AgentPluginsSidecarAtRoot(t *testing.T) {
	target := filepath.Join(t.TempDir(), "claude")
	if err := SeedHarnessConfigFromDir(target, harnesses.FS, "claude", false); err != nil {
		t.Fatal(err)
	}
	bundled, err := fs.ReadFile(harnesses.FS, "claude/"+AgentPluginsSidecarFile)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, AgentPluginsSidecarFile))
	if err != nil {
		t.Fatalf("sidecar not at the bundle root: %v", err)
	}
	if string(got) != string(bundled) {
		t.Fatal("seeded sidecar differs from the bundled one")
	}
	if _, err := os.Stat(filepath.Join(target, "home", ".claude", AgentPluginsSidecarFile)); !os.IsNotExist(err) {
		t.Fatalf("sidecar also seeded under home/.claude: stat err = %v", err)
	}

	if err := os.WriteFile(filepath.Join(target, AgentPluginsSidecarFile), []byte("support: \"no\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SeedHarnessConfigFromDir(target, harnesses.FS, "claude", false); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(target, AgentPluginsSidecarFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(bundled) {
		t.Fatal("non-force reseed did not refresh a stale sidecar")
	}

	hc, err := LoadHarnessConfigDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if hc.Config.Capabilities == nil || hc.Config.Capabilities.AgentPlugins.Support != api.SupportPartial {
		t.Fatalf("seeded claude agent_plugins = %+v, want partial", hc.Config.Capabilities)
	}
}

// bundledHarnessNames lists the harnesses/<h> directories that hold a
// config.yaml.
func bundledHarnessNames(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(harnesses.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(harnesses.FS, e.Name()+"/config.yaml"); err == nil {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no bundled harness-configs found")
	}
	return names
}

// TestHarnessConfigs_ValidateAgainstPre3698Schema validates every bundled
// harnesses/<h>/config.yaml against the settings schema as it was before
// GoogleCloudPlatform/scion#3698 (testdata copy from commit 49382ae0c), the
// schema hubs, brokers and CLIs built before Agent Plugins embed. A key that
// schema rejects makes harness-config install/update fail on those binaries
// (ptone/scion#5196). New harness-config data that older binaries must not
// see belongs in a separate bundle file (like agent-plugins.yaml), not in
// config.yaml.
func TestHarnessConfigs_ValidateAgainstPre3698Schema(t *testing.T) {
	validateOld := pre3698SettingsValidator(t)
	for _, name := range bundledHarnessNames(t) {
		t.Run(name, func(t *testing.T) {
			data, err := fs.ReadFile(harnesses.FS, name+"/config.yaml")
			if err != nil {
				t.Fatal(err)
			}
			verrs, err := validateHarnessConfigWith(data, validateOld)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range verrs {
				t.Errorf("%s/config.yaml rejected by the pre-#3698 schema: %s", name, e.Error())
			}
		})
	}
}

// The old-schema check must be able to fail: the Phase 1 capability line is
// exactly what older binaries rejected.
func TestPre3698Schema_RejectsAgentPluginsCapability(t *testing.T) {
	validateOld := pre3698SettingsValidator(t)
	doc := sidecarTestConfig + "capabilities:\n  agent_plugins: { support: \"yes\" }\n"
	verrs, err := validateHarnessConfigWith([]byte(doc), validateOld)
	if err != nil {
		t.Fatal(err)
	}
	if len(verrs) == 0 {
		t.Fatal("pre-#3698 schema accepted capabilities.agent_plugins")
	}
	// The current schema still accepts it (deprecated, read as the fallback).
	if verrs, err := ValidateHarnessConfig([]byte(doc)); err != nil || len(verrs) != 0 {
		t.Fatalf("current schema: errs=%v err=%v", verrs, err)
	}
}

// pre3698SettingsValidator validates a settings document against the testdata
// copy of the settings schema at 49382ae0c, the parent of
// GoogleCloudPlatform/scion#3698.
func pre3698SettingsValidator(t *testing.T) func([]byte) ([]ValidationError, error) {
	t.Helper()
	const schemaName = "testdata/settings-v1.schema.pre-3698.json"
	schemaData, err := os.ReadFile(schemaName)
	if err != nil {
		t.Fatal(err)
	}
	return func(wrapped []byte) ([]ValidationError, error) {
		var doc interface{}
		if err := yaml.Unmarshal(wrapped, &doc); err != nil {
			return nil, err
		}
		return validateDocAgainstSchemaJSON(convertYAMLToJSON(doc), schemaName, schemaData)
	}
}

// TestUpgradeHarnessConfig_AgentPluginsSidecar: an upgrade adds a missing
// sidecar (a harness-config that predates Agent Plugins) and refreshes a stale
// one with a backup, like a provisioner script. It never writes
// capabilities.agent_plugins into config.yaml.
func TestUpgradeHarnessConfig_AgentPluginsSidecar(t *testing.T) {
	const bundled = "support: \"partial\"\nreason: bundled\n"
	for _, tt := range []struct {
		name     string
		existing string // "" means no sidecar
		action   string
	}{
		{"missing", "", "add_file"},
		{"stale", "support: \"no\"\nreason: old\n", "refresh_file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			existing := map[string]string{"config.yaml": upgradeFixtureConfig}
			if tt.existing != "" {
				existing[AgentPluginsSidecarFile] = tt.existing
			}
			hcDir, harnessesFS := upgradeFixture(t, "myh", existing)
			harnessesFS["myh/"+AgentPluginsSidecarFile] = &fstest.MapFile{Data: []byte(bundled)}
			plan, err := UpgradeHarnessConfig(hcDir, &MockHarness{NameVal: "myh"}, HarnessConfigUpgradeOptions{
				Now:         fixedTime,
				HarnessesFS: harnessesFS,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := actionsOfType(plan, tt.action)[AgentPluginsSidecarFile]; !ok {
				t.Errorf("no %s action for %s: %+v", tt.action, AgentPluginsSidecarFile, plan.Actions)
			}
			if tt.existing != "" && len(plan.Backups) == 0 {
				t.Error("stale sidecar replaced without a backup")
			}
			assertFileContents(t, hcDir, map[string]string{AgentPluginsSidecarFile: bundled})
			data, err := os.ReadFile(filepath.Join(hcDir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "agent_plugins") {
				t.Errorf("upgrade wrote agent_plugins into config.yaml:\n%s", data)
			}
		})
	}
}
