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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agentplugin"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

func TestAgentPluginsEnabled(t *testing.T) {
	broker := func(exps ...string) context.Context {
		ctx := api.ContextWithBrokerMode(context.Background())
		return api.ContextWithHubAgentDefaults(ctx, &api.HubAgentDefaults{Experiments: exps})
	}
	local := &config.VersionedSettings{LocalExperiments: []string{experiments.AgentPlugins}}
	cases := []struct {
		name     string
		ctx      context.Context
		settings *config.VersionedSettings
		want     bool
	}{
		{"broker, hub on", broker(experiments.AgentPlugins), nil, true},
		{"broker, hub off", broker(), nil, false},
		{"broker, hub off, local_experiments set", broker(experiments.K8sNFSHome), local, false},
		{"broker, no hub defaults, local_experiments set", api.ContextWithBrokerMode(context.Background()), local, false},
		{"solo, local_experiments set", context.Background(), local, true},
		{"solo, other local experiment", context.Background(), &config.VersionedSettings{LocalExperiments: []string{"hub.other"}}, false},
		{"solo, no settings", context.Background(), nil, false},
		{"solo, hub defaults on but not broker", api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{Experiments: []string{experiments.AgentPlugins}}), nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentPluginsEnabled(c.ctx, c.settings); got != c.want {
				t.Errorf("agentPluginsEnabled = %v, want %v", got, c.want)
			}
		})
	}
}

func TestAddStartWarning(t *testing.T) {
	ctx, c := withTemplateEnvWarnings(context.Background())
	addStartWarning(ctx, "agent_plugins: a")
	addStartWarning(ctx, "agent_plugins: a")
	addStartWarning(ctx, "agent_plugins: b")
	if got := c.Warnings(); !slices.Equal(got, []string{"agent_plugins: a", "agent_plugins: b"}) {
		t.Fatalf("warnings = %v", got)
	}
	quiet := withQuietTemplateEnvWarnings(context.Background())
	addStartWarning(quiet, "dropped")
	if got := templateEnvWarningsFrom(quiet).Warnings(); len(got) != 0 {
		t.Fatalf("quiet collector kept %v", got)
	}
}

func TestReservedAndResolvedSkillNames(t *testing.T) {
	rec := &SkillResolutionRecord{Skills: []SkillResolutionEntry{
		{Name: "a", InstalledPath: "/h/.claude/skills/alias-a"},
		{Name: "b", As: "bee"},
		{Name: "c"},
	}}
	got := reservedSkillNames([]string{"tpl"}, []string{"plat", ""}, resolvedSkillNames(rec))
	for _, n := range []string{"tpl", "plat", "alias-a", "bee", "c"} {
		if !got[n] {
			t.Errorf("%s missing from %v", n, got)
		}
	}
	if got[""] || len(got) != 5 {
		t.Errorf("reserved = %v", got)
	}
	if names := platformSkillNames(); len(names) == 0 {
		t.Error("platformSkillNames returned nothing")
	}
}

// writePluginFiles writes path -> content under dir; a "*" suffix marks an
// executable file.
func writePluginFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, "*") {
			p, mode = strings.TrimSuffix(p, "*"), 0o755
		}
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotRestoreControlPlaneInputsPlugins(t *testing.T) {
	agentDir, agentHome := t.TempDir(), t.TempDir()
	inputs := stagedInputsDir(agentHome)
	writePluginFiles(t, inputs, map[string]string{
		"instructions.md":                 "hi",
		"plugins.json":                    `{"schema_version":1}`,
		"plugins/p/plugin.json":           "{}",
		"plugins/p/skills/s/SKILL.md":     "skill",
		"plugins/p/bin/tool*":             "#!/bin/sh\n",
		"plugins/p/deep/a/b/c.txt":        "deep",
		"plugins/other.tools/plugin.json": "{}",
	})
	if err := snapshotControlPlaneInputs(agentDir, agentHome); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(agentDir, controlPlaneInputsDirName)
	for _, rel := range []string{"instructions.md", "plugins.json", "plugins/p/deep/a/b/c.txt", "plugins/other.tools/plugin.json"} {
		if _, err := os.Stat(filepath.Join(record, rel)); err != nil {
			t.Errorf("record lacks %s: %v", rel, err)
		}
	}
	if !recordHasAgentPlugins(agentDir) {
		t.Error("recordHasAgentPlugins = false")
	}

	// Restore with plugins: the subtree comes back, exec bit included.
	if err := os.RemoveAll(inputs); err != nil {
		t.Fatal(err)
	}
	if err := restoreControlPlaneInputs(agentDir, agentHome, true); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(inputs, "plugins/p/bin/tool"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("restored tool = %v, %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(inputs, "plugins/p/deep/a/b/c.txt")); string(b) != "deep" {
		t.Errorf("restored deep file = %q", b)
	}
	if _, err := os.Stat(filepath.Join(inputs, "plugins.json")); err != nil {
		t.Errorf("plugins.json not restored: %v", err)
	}

	// Restore without plugins: neither the index nor the subtree.
	if err := restoreControlPlaneInputs(agentDir, agentHome, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inputs, "instructions.md")); err != nil {
		t.Errorf("instructions.md not restored: %v", err)
	}
	for _, rel := range []string{"plugins.json", "plugins"} {
		if _, err := os.Lstat(filepath.Join(inputs, rel)); !os.IsNotExist(err) {
			t.Errorf("%s restored with includePlugins=false (err %v)", rel, err)
		}
	}

	// A re-snapshot without staged plugins drops them from the record.
	if err := os.RemoveAll(filepath.Join(inputs, "plugins")); err != nil {
		t.Fatal(err)
	}
	if err := snapshotControlPlaneInputs(agentDir, agentHome); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(record, "plugins")); !os.IsNotExist(err) {
		t.Errorf("record kept plugins/ after re-snapshot (err %v)", err)
	}
}

func TestSnapshotControlPlaneInputsSkipsSymlinkedPlugins(t *testing.T) {
	agentDir, agentHome := t.TempDir(), t.TempDir()
	inputs := stagedInputsDir(agentHome)
	outside := t.TempDir()
	writePluginFiles(t, outside, map[string]string{"p/plugin.json": "SECRET"})
	writePluginFiles(t, inputs, map[string]string{"instructions.md": "hi"})
	if err := os.Symlink(outside, filepath.Join(inputs, "plugins")); err != nil {
		t.Fatal(err)
	}
	if err := snapshotControlPlaneInputs(agentDir, agentHome); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(agentDir, controlPlaneInputsDirName, "plugins")); !os.IsNotExist(err) {
		t.Fatalf("symlinked plugins dir was recorded (err %v)", err)
	}
}

// Round-2 review: the record copy applies the per-plugin limits to each
// plugin directory, as Stage did, not to the whole plugins/ tree.
func TestCopyPluginsTreeLimitsArePerPlugin(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writePluginFiles(t, src, map[string]string{
		"plugins/a/plugin.json": "{}", "plugins/a/f1": "1", "plugins/a/f2": "2",
		"plugins/b/plugin.json": "{}", "plugins/b/f1": "1", "plugins/b/f2": "2",
		// deep: depth 2 below its plugin root, at the depth limit.
		"plugins/deep/plugin.json": "{}", "plugins/deep/x/y/z": "z",
		// Not plugins: a leftover staging dir and a stray file.
		"plugins/.tmp-c/plugin.json": "{}", "plugins/stray": "s",
	})
	if err := os.Symlink(filepath.Join(src, "plugins", "a"), filepath.Join(src, "plugins", "linked")); err != nil {
		t.Fatal(err)
	}
	// Each plugin has at most 4 entries and depth 2; the whole tree has 12+
	// entries and depth 3, so a whole-tree limit would fail.
	lim := agentplugin.Limits{MaxFiles: 4, MaxBytes: 64, MaxDepth: 2}
	if err := copyPluginsTreeWithLimits(src, "plugins", dst, "rec/plugins", lim); err != nil {
		t.Fatalf("per-plugin copy failed: %v", err)
	}
	for _, rel := range []string{"a/f2", "b/f2", "deep/x/y/z"} {
		if _, err := os.Stat(filepath.Join(dst, "rec/plugins", rel)); err != nil {
			t.Errorf("%s not copied: %v", rel, err)
		}
	}
	for _, rel := range []string{".tmp-c", "stray", "linked"} {
		if _, err := os.Lstat(filepath.Join(dst, "rec/plugins", rel)); !os.IsNotExist(err) {
			t.Errorf("%s copied (err %v)", rel, err)
		}
	}

	// A single plugin over the limit still fails, naming the plugin.
	writePluginFiles(t, src, map[string]string{"plugins/big/f1": "1", "plugins/big/f2": "2", "plugins/big/f3": "3", "plugins/big/f4": "4", "plugins/big/f5": "5"})
	err := copyPluginsTreeWithLimits(src, "plugins", dst, "rec/plugins", lim)
	if err == nil || !strings.Contains(err.Error(), `copy agent plugin "big"`) || !agentplugin.IsLimitError(err) {
		t.Fatalf("over-limit plugin: err = %v", err)
	}
}

// Two staged plugins whose combined size is over one plugin's limit go
// through snapshot and restore intact.
func TestSnapshotRestoreManyPlugins(t *testing.T) {
	agentDir, agentHome := t.TempDir(), t.TempDir()
	inputs := stagedInputsDir(agentHome)
	files := map[string]string{"plugins.json": `{"schema_version":1}`}
	for _, p := range []string{"one", "two"} {
		files["plugins/"+p+"/plugin.json"] = "{}"
		for i := 0; i < 6000; i++ {
			files[fmt.Sprintf("plugins/%s/d%d/f", p, i/500)+fmt.Sprint(i)] = ""
		}
	}
	writePluginFiles(t, inputs, files)
	if err := snapshotControlPlaneInputs(agentDir, agentHome); err != nil {
		t.Fatalf("snapshot of 2 x 6000-file plugins: %v", err)
	}
	if err := restoreControlPlaneInputs(agentDir, agentHome, true); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, rel := range []string{"plugins/one/d11/f5999", "plugins/two/d0/f0", "plugins.json"} {
		if _, err := os.Stat(filepath.Join(inputs, rel)); err != nil {
			t.Errorf("%s not restored: %v", rel, err)
		}
	}
}

func TestStartAgentPluginsWarning(t *testing.T) {
	newHC := func(c api.CapabilityField) api.Harness {
		return &capsHarness{caps: api.HarnessAdvancedCapabilities{AgentPlugins: c}}
	}
	yes := newHC(api.CapabilityField{Support: api.SupportPartial})
	absent := newHC(api.CapabilityField{})
	no := newHC(api.CapabilityField{Support: api.SupportNo, Reason: "r"})
	cases := []struct {
		name               string
		recordHas, include bool
		h                  api.Harness
		want               string
	}{
		{"no plugins recorded", false, true, absent, ""},
		{"no plugins recorded, gate off", false, false, yes, ""},
		{"gate off", true, false, yes, "agent_plugins: ignored: experiment hub.agent_plugins is off; plugins Scion installed earlier, if any, are removed"},
		{"supported", true, true, yes, ""},
		{"capability absent", true, true, absent, `agent_plugins: harness-config "hc" predates Agent Plugins support; plugins staged but not installed (reinstall it with scion harness-config install --force)`},
		{"capability no", true, true, no, `agent_plugins: harness "hc" does not support Agent Plugins; plugins staged but not installed (r)`},
	}
	for _, c := range cases {
		if got := startAgentPluginsWarning(c.recordHas, c.include, "hc", c.h); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// capsHarness is an api.Harness whose only meaningful method is
// AdvancedCapabilities.
type capsHarness struct {
	api.Harness
	caps api.HarnessAdvancedCapabilities
}

func (c *capsHarness) AdvancedCapabilities() api.HarnessAdvancedCapabilities { return c.caps }
