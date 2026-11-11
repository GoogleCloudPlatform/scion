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
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const agentPluginsTestManifest = `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": "acme.tools", "version": "1.0.0"}`

const policyTestScriptedPlugins = policyTestScripted + "capabilities:\n  agent_plugins: { support: \"yes\" }\n"

// pluginTemplate writes template "plug" declaring one agent plugin.
func (e *policyTestEnv) pluginTemplate(t *testing.T) {
	t.Helper()
	dir := e.template(t, "plug", "agent_plugins:\n  - path: plugins/acme\n")
	writePluginFiles(t, dir, map[string]string{
		"plugins/acme/plugin.json":            agentPluginsTestManifest,
		"plugins/acme/skills/deploy/SKILL.md": "---\nname: deploy\n---\n",
		"plugins/acme/bin/srv*":               "#!/bin/sh\n",
	})
}

// enableLocalAgentPlugins adds local_experiments: [hub.agent_plugins] to the
// machine settings.
func (e *policyTestEnv) enableLocalAgentPlugins(t *testing.T) {
	t.Helper()
	path := filepath.Join(filepath.Dir(e.root), ".scion", "settings.yaml")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(data) == 0 {
		data = []byte("schema_version: \"1\"\n")
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, "local_experiments:\n  - hub.agent_plugins\n"...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *policyTestEnv) disableLocalAgentPlugins(t *testing.T) {
	t.Helper()
	path := filepath.Join(filepath.Dir(e.root), ".scion", "settings.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(data), "local_experiments:\n  - hub.agent_plugins\n", "", 1)
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func provisionWithWarnings(t *testing.T, e *policyTestEnv, name, hc string) []string {
	t.Helper()
	ctx, c := withTemplateEnvWarnings(context.Background())
	if _, _, _, err := ProvisionAgent(ctx, name, "plug", "", hc, e.scion, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	return c.Warnings()
}

func agentPluginWarnings(ws []string) []string {
	var out []string
	for _, w := range ws {
		if strings.HasPrefix(w, "agent_plugins:") {
			out = append(out, w)
		}
	}
	return out
}

func TestProvisionAgent_AgentPluginsGateOnStagesAndRecords(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-plugins", policyTestScriptedPlugins)
	e.pluginTemplate(t)
	e.enableLocalAgentPlugins(t)

	ws := provisionWithWarnings(t, e, "plugged", "hc-plugins")
	if got := agentPluginWarnings(ws); len(got) != 0 {
		t.Errorf("unexpected warnings: %v", got)
	}
	home := config.GetAgentHomePath(e.scion, "plugged")
	inputs := stagedInputsDir(home)
	for _, rel := range []string{"plugins.json", "plugins/acme.tools/plugin.json", "plugins/acme.tools/skills/deploy/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(inputs, rel)); err != nil {
			t.Errorf("staged inputs lack %s: %v", rel, err)
		}
	}
	agentDir := config.ResolveAgentDir(e.scion, "plugged")
	record := filepath.Join(agentDir, controlPlaneInputsDirName)
	if fi, err := os.Stat(filepath.Join(record, "plugins/acme.tools/bin/srv")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("record lacks the executable bundle file: %v %v", fi, err)
	}
	if !recordHasAgentPlugins(agentDir) {
		t.Error("record lacks plugins.json")
	}

	// A start with the experiment on restores the tree; the manifest names
	// the index.
	if _, err := policyTestManager(nil).Start(context.Background(), api.StartOptions{Name: "plugged", ProjectPath: e.scion, HarnessConfig: "hc-plugins", NoAuth: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inputs, "plugins/acme.tools/skills/deploy/SKILL.md")); err != nil {
		t.Errorf("start did not restore the staged bundle: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "manifest.json"))
	if err != nil || !strings.Contains(string(manifest), `"agent_plugins"`) {
		t.Errorf("manifest does not name the plugin index (err %v)", err)
	}

	// Turning the experiment off: the next start restores no plugins and
	// says so.
	e.disableLocalAgentPlugins(t)
	info, err := policyTestManager(nil).Start(context.Background(), api.StartOptions{Name: "plugged", ProjectPath: e.scion, HarnessConfig: "hc-plugins", NoAuth: true})
	if err != nil {
		t.Fatalf("Start (off): %v", err)
	}
	gateOff := "agent_plugins: ignored: experiment hub.agent_plugins is off; plugins Scion installed earlier, if any, are removed"
	if n := countWarning(info.Warnings, gateOff); n != 1 {
		t.Errorf("start warnings = %v, want %q once", info.Warnings, gateOff)
	}
	for _, rel := range []string{"plugins.json", "plugins"} {
		if _, err := os.Lstat(filepath.Join(inputs, rel)); !os.IsNotExist(err) {
			t.Errorf("start with the experiment off restored %s (err %v)", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(inputs, "instructions.md")); err != nil {
		t.Errorf("other inputs not restored: %v", err)
	}
}

func TestProvisionAgent_AgentPluginsGateOff(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-plugins", policyTestScriptedPlugins)
	e.pluginTemplate(t)

	ws := provisionWithWarnings(t, e, "unplugged", "hc-plugins")
	want := "agent_plugins: ignored: experiment hub.agent_plugins is off"
	if !slices.Contains(ws, want) {
		t.Errorf("warnings = %v, want %q", ws, want)
	}
	home := config.GetAgentHomePath(e.scion, "unplugged")
	if _, err := os.Lstat(filepath.Join(stagedInputsDir(home), "plugins")); !os.IsNotExist(err) {
		t.Errorf("plugins staged with the experiment off (err %v)", err)
	}
	if recordHasAgentPlugins(config.ResolveAgentDir(e.scion, "unplugged")) {
		t.Error("record has plugins with the experiment off")
	}
}

func TestProvisionAgent_AgentPluginsUnsupportedHarnessStagesAndWarns(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	e.projectHC(t, "hc-no", policyTestScripted+"capabilities:\n  agent_plugins: { support: \"no\", reason: \"test harness\" }\n")
	e.pluginTemplate(t)
	e.enableLocalAgentPlugins(t)

	ws := agentPluginWarnings(provisionWithWarnings(t, e, "old-hc", "hc-scripted"))
	if len(ws) != 1 || !strings.Contains(ws[0], `harness-config "hc-scripted" predates Agent Plugins support`) {
		t.Errorf("absent capability: warnings = %v", ws)
	}
	ws = agentPluginWarnings(provisionWithWarnings(t, e, "no-hc", "hc-no"))
	if len(ws) != 1 || ws[0] != `agent_plugins: harness "hc-no" does not support Agent Plugins; plugins staged but not installed (test harness)` {
		t.Errorf("capability no: warnings = %v", ws)
	}
	for _, name := range []string{"old-hc", "no-hc"} {
		home := config.GetAgentHomePath(e.scion, name)
		if _, err := os.Stat(filepath.Join(stagedInputsDir(home), "plugins.json")); err != nil {
			t.Errorf("%s: plugins not staged: %v", name, err)
		}
	}
}

func TestProvisionAgent_AgentPluginsRequiredFailureFailsProvision(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-plugins", policyTestScriptedPlugins)
	e.template(t, "plug", "agent_plugins:\n  - path: plugins/missing\n")
	e.enableLocalAgentPlugins(t)
	_, _, _, err := ProvisionAgent(context.Background(), "broken", "plug", "", "hc-plugins", e.scion, "", "", "", "")
	if err == nil || !strings.Contains(err.Error(), `agent plugins: template plug: plugin dir "plugins/missing" not found or is a symlink`) {
		t.Fatalf("err = %v", err)
	}
}

func countWarning(ws []string, want string) int {
	n := 0
	for _, w := range ws {
		if w == want {
			n++
		}
	}
	return n
}

// A start that switches to a harness-config without the capability warns,
// and the bundle is still restored for a later switch back.
func TestStart_AgentPluginsHarnessSwitchWarns(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-plugins", policyTestScriptedPlugins)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	e.pluginTemplate(t)
	e.enableLocalAgentPlugins(t)
	provisionWithWarnings(t, e, "switch", "hc-plugins")

	info, err := policyTestManager(nil).Start(context.Background(), api.StartOptions{Name: "switch", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := `agent_plugins: harness-config "hc-scripted" predates Agent Plugins support; plugins staged but not installed (run scion harness-config reset hc-scripted for the bundled defaults, or scion harness-config upgrade hc-scripted to keep local edits)`
	if n := countWarning(info.Warnings, want); n != 1 {
		t.Errorf("start warnings = %v, want %q once", info.Warnings, want)
	}
	home := config.GetAgentHomePath(e.scion, "switch")
	if _, err := os.Stat(filepath.Join(stagedInputsDir(home), "plugins.json")); err != nil {
		t.Errorf("plugins not restored after the switch: %v", err)
	}
}

// Inline-only agent_plugins (no template declares any) are reported, not
// silently ignored.
func TestProvisionAgent_InlineOnlyAgentPluginsWarn(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-plugins", policyTestScriptedPlugins)
	inline := &api.ScionConfig{AgentPlugins: []api.AgentPluginReference{{Path: "plugins/x"}}}
	provision := func(name string) []string {
		ctx, c := withTemplateEnvWarnings(context.Background())
		if _, _, _, err := ProvisionAgent(ctx, name, "", "", "hc-plugins", e.scion, "", "", "", "", inline); err != nil {
			t.Fatalf("ProvisionAgent: %v", err)
		}
		return agentPluginWarnings(c.Warnings())
	}
	if got := provision("inline-off"); !slices.Equal(got, []string{"agent_plugins: ignored: experiment hub.agent_plugins is off"}) {
		t.Errorf("gate off: warnings = %v", got)
	}
	e.enableLocalAgentPlugins(t)
	if got := provision("inline-on"); !slices.Equal(got, []string{`agent_plugins: entry "plugins/x" ignored: only template-relative paths are supported`}) {
		t.Errorf("gate on: warnings = %v", got)
	}
}
