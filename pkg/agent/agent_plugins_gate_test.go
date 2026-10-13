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
