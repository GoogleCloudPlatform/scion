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

package agentplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func manifestFor(name, version string) string {
	return `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": "` + name +
		`", "version": "` + version + `", "description": "` + name + ` plugin"}`
}

func readIndex(t *testing.T, home string) *Index {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, InputsRel, IndexFileName))
	if err != nil {
		t.Fatal(err)
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	return &idx
}

func TestStageGolden(t *testing.T) {
	base := t.TempDir()
	writeFiles(t, base, map[string]string{
		"tpl/plugins/acme/plugin.json": `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
			"name": "acme.tools", "version": "1.2.0", "description": "Acme tools", "foo": true}`,
		"tpl/plugins/acme/skills/deploy/SKILL.md": "---\nname: deploy\n---\n",
		"tpl/plugins/acme/skills/review/SKILL.md": "---\nname: review\n---\n",
		"tpl/plugins/acme/skills/notaskill/x.md":  "x",
		"tpl/plugins/acme/bin/srv*":               "#!/bin/sh\n",
		"tpl/plugins/acme/mcp.json": `{"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json", "mcpServers": {
			"local": {"type": "stdio", "command": "./bin/srv", "args": ["${PLUGIN_DATA}/x"]},
			"old": {"type": "sse", "url": "http://example.com/sse"}}}`,
	})
	home := t.TempDir()
	res, err := Stage(context.Background(), StageRequest{
		Chain:              []TemplateSource{{Name: "my-template", Dir: filepath.Join(base, "tpl"), Refs: []api.AgentPluginReference{{Path: "plugins/acme"}}}},
		AgentHome:          home,
		ReservedSkillNames: map[string]bool{"acme.tools": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Index == nil || len(res.Index.Plugins) != 1 {
		t.Fatalf("index = %+v", res.Index)
	}
	wantDiags := []string{
		`agent_plugins: plugin "acme.tools": unknown manifest field "foo" ignored`,
		`agent_plugins: plugin "acme.tools": mcp server "old" skipped: "url" must use https unless the host is localhost or a loopback address`,
	}
	if len(res.Diagnostics) != len(wantDiags) {
		t.Fatalf("diagnostics = %v", res.Diagnostics)
	}
	for i, d := range res.Diagnostics {
		if d.String() != wantDiags[i] {
			t.Errorf("diag %d = %q, want %q", i, d.String(), wantDiags[i])
		}
	}

	got, err := os.ReadFile(filepath.Join(home, InputsRel, IndexFileName))
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "plugins.golden.json")
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("plugins.json mismatch (run with -update):\n%s", got)
	}

	staged := filepath.Join(home, InputsRel, PluginsDirName, "acme.tools")
	if fi, err := os.Stat(filepath.Join(staged, "bin/srv")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("staged exec = %v %v", fi, err)
	}
}

func TestStageChildOverridesParent(t *testing.T) {
	base := t.TempDir()
	writeFiles(t, base, map[string]string{
		"parent/p/plugin.json":       manifestFor("shared", "1.0.0"),
		"parent/only/plugin.json":    manifestFor("parent-only", "1.0.0"),
		"child/p2/plugin.json":       manifestFor("shared", "2.0.0"),
		"child/p2/skills/s/SKILL.md": "s",
	})
	home := t.TempDir()
	// Stale content from an earlier run must disappear.
	writeFiles(t, home, map[string]string{filepath.Join(InputsRel, PluginsDirName, "stale/plugin.json"): "{}"})
	res, err := Stage(context.Background(), StageRequest{
		Chain: []TemplateSource{
			{Name: "parent", Dir: filepath.Join(base, "parent"), Refs: []api.AgentPluginReference{{Path: "p"}, {Path: "only"}}},
			{Name: "child", Dir: filepath.Join(base, "child"), Refs: []api.AgentPluginReference{{Path: "p2"}}},
		},
		AgentHome: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].String() != `agent_plugins: plugin "shared": from template child replaces the one from parent` {
		t.Fatalf("diagnostics = %v", res.Diagnostics)
	}
	idx := readIndex(t, home)
	if len(idx.Plugins) != 2 || idx.Plugins[0].Name != "shared" || idx.Plugins[0].Version != "2.0.0" ||
		idx.Plugins[0].Source.Template != "child" || idx.Plugins[1].Name != "parent-only" {
		t.Fatalf("index = %+v", idx)
	}
	if len(idx.Plugins[0].Skills) != 1 || idx.Plugins[0].Skills[0] != "s" {
		t.Errorf("skills = %v", idx.Plugins[0].Skills)
	}
	if _, err := os.Stat(filepath.Join(home, InputsRel, PluginsDirName, "stale")); !os.IsNotExist(err) {
		t.Errorf("stale bundle survived: %v", err)
	}
}

func TestStageRequiredVsOptional(t *testing.T) {
	base := t.TempDir()
	writeFiles(t, base, map[string]string{
		"tpl/good/plugin.json":    manifestFor("good", "1"),
		"tpl/badjson/plugin.json": "{",
		"tpl/newer/plugin.json":   `{"$schema": "https://agent-plugins.org/schemas/1.1.0/plugin.schema.json", "name": "newer"}`,
	})
	tpl := filepath.Join(base, "tpl")
	cases := []struct {
		path, wantErr string
	}{
		{"missing", `template tpl: plugin dir "missing" not found or is a symlink`},
		{"../x", `template tpl: path "../x" must be relative to the template directory`},
		{"badjson", `plugin at "badjson": plugin.json: invalid JSON`},
		{"newer", `plugin "newer": unsupported Agent Plugins version "https://agent-plugins.org/schemas/1.1.0/plugin.schema.json"; Scion supports 1.0.0`},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			home := t.TempDir()
			_, err := Stage(context.Background(), StageRequest{
				Chain:     []TemplateSource{{Name: "tpl", Dir: tpl, Refs: []api.AgentPluginReference{{Path: "good"}, {Path: c.path}}}},
				AgentHome: home,
			})
			if err == nil || !strings.HasPrefix(err.Error(), c.wantErr) {
				t.Fatalf("required: err = %v, want prefix %q", err, c.wantErr)
			}
			res, err := Stage(context.Background(), StageRequest{
				Chain:     []TemplateSource{{Name: "tpl", Dir: tpl, Refs: []api.AgentPluginReference{{Path: "good"}, {Path: c.path, Optional: true}}}},
				AgentHome: home,
			})
			if err != nil {
				t.Fatalf("optional: %v", err)
			}
			if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, c.wantErr) ||
				!strings.HasSuffix(res.Diagnostics[0].Message, "(optional plugin skipped)") {
				t.Fatalf("optional diagnostics = %v", res.Diagnostics)
			}
			if res.Index == nil || len(res.Index.Plugins) != 1 || res.Index.Plugins[0].Name != "good" {
				t.Fatalf("index = %+v", res.Index)
			}
		})
	}
}

func TestStageNothingDeclared(t *testing.T) {
	home := t.TempDir()
	res, err := Stage(context.Background(), StageRequest{AgentHome: home})
	if err != nil || res.Index != nil {
		t.Fatalf("res %+v err %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(home, InputsRel, IndexFileName)); !os.IsNotExist(err) {
		t.Fatalf("index written: %v", err)
	}
}

func TestStageLimitLeavesNoPartialDir(t *testing.T) {
	base := t.TempDir()
	writeFiles(t, base, map[string]string{
		"tpl/big/plugin.json": manifestFor("big", "1"),
		"tpl/big/data":        strings.Repeat("x", 4096),
	})
	tpl := filepath.Join(base, "tpl")
	home := t.TempDir()
	lim := Limits{MaxFiles: 100, MaxBytes: 1024, MaxDepth: 4}
	_, err := Stage(context.Background(), StageRequest{
		Chain:     []TemplateSource{{Name: "tpl", Dir: tpl, Refs: []api.AgentPluginReference{{Path: "big"}}}},
		AgentHome: home, Limits: lim,
	})
	if err == nil || err.Error() != `plugin "big": bundle exceeds 1024 bytes` {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(home, InputsRel, PluginsDirName))
	if len(entries) != 0 {
		t.Fatalf("partial dirs left: %v", entries)
	}
	res, err := Stage(context.Background(), StageRequest{
		Chain:     []TemplateSource{{Name: "tpl", Dir: tpl, Refs: []api.AgentPluginReference{{Path: "big", Optional: true}}}},
		AgentHome: home, Limits: lim,
	})
	if err != nil || res.Index != nil || len(res.Diagnostics) != 1 {
		t.Fatalf("optional: res %+v err %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(home, InputsRel, PluginsDirName)); !os.IsNotExist(err) {
		t.Fatalf("empty plugins dir left: %v", err)
	}
}

func TestStageSymlinkInsideBundleWarns(t *testing.T) {
	base := t.TempDir()
	writeFiles(t, base, map[string]string{"tpl/p/plugin.json": manifestFor("p", "1")})
	if err := os.Symlink("/etc/passwd", filepath.Join(base, "tpl/p/pw")); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	res, err := Stage(context.Background(), StageRequest{
		Chain:     []TemplateSource{{Name: "tpl", Dir: filepath.Join(base, "tpl"), Refs: []api.AgentPluginReference{{Path: "p"}}}},
		AgentHome: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].String() != `agent_plugins: plugin "p": skipped symlink or special file pw (not followed)` {
		t.Fatalf("diagnostics = %v", res.Diagnostics)
	}
	if res.Index.Plugins[0].SkillsDirConflict {
		t.Error("unexpected skills_dir_conflict")
	}
}
