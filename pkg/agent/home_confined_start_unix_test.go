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

//go:build unix

package agent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/agentplugin"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// Start-path writes and removals under the agent home are confined to the
// home: a link at a leaf is replaced or removed itself (never written
// through or followed), a linked directory component is refused, and a
// recursive clear never descends through a link. Each test plants links to
// an "outside" directory and checks that directory is left exactly as it
// was.

type confinedHome struct {
	agentDir, home, outside string
	before                  string
}

func newConfinedHome(t *testing.T) *confinedHome {
	t.Helper()
	base := t.TempDir()
	c := &confinedHome{
		agentDir: filepath.Join(base, "agent"),
		home:     filepath.Join(base, "agent", "home"),
		outside:  filepath.Join(base, "outside"),
	}
	for _, d := range []string{c.home, filepath.Join(c.outside, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range map[string]string{
		"victim":                   "original",
		"sub/inner":                "inner",
		"instructions.md":          "outside instructions",
		"plugins.json":             "outside index",
		"TOKEN_A":                  "outside token",
		"20-harness-provision":     "outside hook",
		"harness/manifest.json":    "outside manifest",
		"hooks/pre-start.d/marker": "outside hook dir",
		// Sentinels at the exact paths a followed link at .scion or
		// .scion/hooks would reach.
		"hooks/pre-start.d/20-harness-provision": "outside hook",
		"pre-start.d/20-harness-provision":       "outside hook",
		"scion-token":                            "outside token file",
	} {
		p := filepath.Join(c.outside, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c.before = outsideSnapshot(t, c.outside)
	return c
}

// outsideSnapshot lists every entry under dir with its type and content.
func outsideSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		line := rel + " " + d.Type().String()
		if d.Type().IsRegular() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			line += " " + string(b)
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (c *confinedHome) checkOutside(t *testing.T) {
	t.Helper()
	if got := outsideSnapshot(t, c.outside); got != c.before {
		t.Fatalf("outside directory changed:\n%s\nwant:\n%s", got, c.before)
	}
}

func (c *confinedHome) mkdir(t *testing.T, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(c.home, rel), 0o755); err != nil {
		t.Fatal(err)
	}
}

// link replaces whatever is at rel in the home with a link to target.
func (c *confinedHome) link(t *testing.T, target, rel string) {
	t.Helper()
	p := filepath.Join(c.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func (c *confinedHome) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(c.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireRegular(t *testing.T, path, want string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("%s = %v, %v; want a regular file", path, fi, err)
	}
	if b, _ := os.ReadFile(path); string(b) != want {
		t.Fatalf("%s = %q, want %q", path, b, want)
	}
}

func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still present (err %v)", path, err)
	}
}

// --- resetStagedProvisioning (harness.ClearStagedProvisioning) ---

func TestResetStagedProvisioning_LinkLeavesRemovedThemselves(t *testing.T) {
	c := newConfinedHome(t)
	c.link(t, filepath.Join(c.outside, "20-harness-provision"), ".scion/hooks/pre-start.d/"+harness.HarnessProvisionHookFilename)
	c.link(t, filepath.Join(c.outside, "harness"), ".scion/harness")
	if err := resetStagedProvisioning(c.home); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, harness.HarnessProvisionHookPath(c.home))
	requireAbsent(t, filepath.Join(c.home, ".scion/harness"))
	c.checkOutside(t)
}

func TestResetStagedProvisioning_RefusesLinkedComponent(t *testing.T) {
	for _, rel := range []string{".scion", ".scion/hooks"} {
		t.Run(rel, func(t *testing.T) {
			c := newConfinedHome(t)
			c.link(t, c.outside, rel)
			if err := resetStagedProvisioning(c.home); !errors.Is(err, agenthome.ErrLinkComponent) {
				t.Fatalf("err = %v, want a linked directory component refused", err)
			}
			c.checkOutside(t)
		})
	}
}

func TestResetStagedProvisioning_RecursiveClearDoesNotFollowLinks(t *testing.T) {
	c := newConfinedHome(t)
	c.write(t, ".scion/harness/manifest.json", "{}")
	c.write(t, ".scion/harness/inputs/instructions.md", "x")
	c.link(t, c.outside, ".scion/harness/inputs/plugins")
	c.link(t, filepath.Join(c.outside, "sub"), ".scion/harness/secrets")
	c.link(t, filepath.Join(c.outside, "victim"), ".scion/harness/outputs/env.json")
	if err := resetStagedProvisioning(c.home); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, filepath.Join(c.home, ".scion/harness"))
	c.checkOutside(t)
}

func TestResetStagedProvisioning_LinkReplantedBetweenStartsIsRefused(t *testing.T) {
	c := newConfinedHome(t)
	c.write(t, ".scion/harness/manifest.json", "{}")
	if err := resetStagedProvisioning(c.home); err != nil {
		t.Fatal(err)
	}
	c.link(t, c.outside, ".scion/hooks")
	if err := resetStagedProvisioning(c.home); !errors.Is(err, agenthome.ErrLinkComponent) {
		t.Fatalf("err = %v, want the re-planted link refused", err)
	}
	c.checkOutside(t)
}

// --- restoreControlPlaneInputs ---

// writeInputsRecord writes a control-plane inputs record with a top-level
// file, a plugin index and one plugin.
func writeInputsRecord(t *testing.T, agentDir string) {
	t.Helper()
	record := filepath.Join(agentDir, controlPlaneInputsDirName)
	for rel, content := range map[string]string{
		"instructions.md":         "recorded instructions",
		agentplugin.IndexFileName: "recorded index",
		"plugins/p/plugin.json":   "recorded plugin",
	} {
		p := filepath.Join(record, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func setRestoreInputsHook(t *testing.T, hook func(stage string)) {
	t.Helper()
	restoreInputsTestHook = hook
	t.Cleanup(func() { restoreInputsTestHook = nil })
}

func (c *confinedHome) checkRestoredInputs(t *testing.T) {
	t.Helper()
	inputs := filepath.Join(c.home, agentplugin.InputsRel)
	requireRegular(t, filepath.Join(inputs, "instructions.md"), "recorded instructions")
	requireRegular(t, filepath.Join(inputs, agentplugin.IndexFileName), "recorded index")
	requireRegular(t, filepath.Join(inputs, "plugins/p/plugin.json"), "recorded plugin")
}

func TestRestoreControlPlaneInputs_LinkLeavesReplaced(t *testing.T) {
	c := newConfinedHome(t)
	writeInputsRecord(t, c.agentDir)
	// Links planted after the clear, at each leaf the restore writes.
	setRestoreInputsHook(t, func(stage string) {
		switch stage {
		case "cleared":
			c.link(t, filepath.Join(c.outside, "instructions.md"), agentplugin.InputsRel+"/instructions.md")
			c.link(t, filepath.Join(c.outside, "plugins.json"), agentplugin.InputsRel+"/"+agentplugin.IndexFileName)
		case "files":
			c.link(t, filepath.Join(c.outside, "sub"), agentplugin.InputsRel+"/plugins")
		}
	})
	if err := restoreControlPlaneInputs(c.agentDir, c.home, true); err != nil {
		t.Fatal(err)
	}
	c.checkRestoredInputs(t)
	c.checkOutside(t)
}

func TestRestoreControlPlaneInputs_PluginIndexLinkRemovedWithoutPlugins(t *testing.T) {
	c := newConfinedHome(t)
	writeInputsRecord(t, c.agentDir)
	setRestoreInputsHook(t, func(stage string) {
		if stage == "files" {
			c.link(t, filepath.Join(c.outside, "plugins.json"), agentplugin.InputsRel+"/"+agentplugin.IndexFileName)
		}
	})
	if err := restoreControlPlaneInputs(c.agentDir, c.home, false); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, filepath.Join(c.home, agentplugin.InputsRel, agentplugin.IndexFileName))
	c.checkOutside(t)
}

func TestRestoreControlPlaneInputs_RefusesLinkedComponent(t *testing.T) {
	// Planted before the start, at each directory component above inputs/.
	for _, rel := range []string{".scion", ".scion/harness"} {
		t.Run("before/"+rel, func(t *testing.T) {
			c := newConfinedHome(t)
			writeInputsRecord(t, c.agentDir)
			c.link(t, c.outside, rel)
			if err := restoreControlPlaneInputs(c.agentDir, c.home, true); !errors.Is(err, agenthome.ErrLinkComponent) {
				t.Fatalf("err = %v, want a linked directory component refused", err)
			}
			c.checkOutside(t)
		})
	}
	// Re-planted between the clear and the writes, and between the file
	// writes and the plugins tree.
	for _, tc := range []struct{ stage, rel string }{
		{"cleared", agentplugin.InputsRel},
		{"cleared", ".scion/harness"},
		{"files", agentplugin.InputsRel},
	} {
		t.Run(tc.stage+"/"+tc.rel, func(t *testing.T) {
			c := newConfinedHome(t)
			writeInputsRecord(t, c.agentDir)
			setRestoreInputsHook(t, func(stage string) {
				if stage == tc.stage {
					c.link(t, c.outside, tc.rel)
				}
			})
			if err := restoreControlPlaneInputs(c.agentDir, c.home, true); !errors.Is(err, agenthome.ErrLinkComponent) {
				t.Fatalf("err = %v, want the re-planted link refused", err)
			}
			c.checkOutside(t)
		})
	}
}

func TestRestoreControlPlaneInputs_ClearDoesNotFollowLinks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(c *confinedHome)
	}{
		{"inputs is a link", func(c *confinedHome) {
			c.mkdir(t, ".scion/harness")
			c.link(t, c.outside, agentplugin.InputsRel)
		}},
		{"links inside inputs", func(c *confinedHome) {
			c.write(t, agentplugin.InputsRel+"/old.md", "old")
			c.link(t, c.outside, agentplugin.InputsRel+"/plugins")
			c.link(t, filepath.Join(c.outside, "sub"), agentplugin.InputsRel+"/deep/dir")
			c.link(t, filepath.Join(c.outside, "victim"), agentplugin.InputsRel+"/instructions.md")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newConfinedHome(t)
			writeInputsRecord(t, c.agentDir)
			tc.plant(c)
			if err := restoreControlPlaneInputs(c.agentDir, c.home, true); err != nil {
				t.Fatal(err)
			}
			c.checkRestoredInputs(t)
			requireAbsent(t, filepath.Join(c.home, agentplugin.InputsRel, "old.md"))
			requireAbsent(t, filepath.Join(c.home, agentplugin.InputsRel, "deep"))
			c.checkOutside(t)
		})
	}
}

// --- restoreSecretsRecord ---

func writeSecretsRecord(t *testing.T, agentDir string, names ...string) *harnessConfigIdentity {
	t.Helper()
	record := filepath.Join(agentDir, config.HarnessSecretsRecordDirName)
	if err := os.MkdirAll(record, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(record, n), []byte("recorded "+n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id := &harnessConfigIdentity{Name: "hc", Source: "broker-local", Revision: "sha256:abc"}
	data, _ := json.Marshal(id)
	if err := os.WriteFile(filepath.Join(record, secretsIdentityFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRestoreSecretsRecord_LinkLeafReplaced(t *testing.T) {
	c := newConfinedHome(t)
	id := writeSecretsRecord(t, c.agentDir, "TOKEN_A")
	c.link(t, filepath.Join(c.outside, "TOKEN_A"), stagedSecretsRel+"/TOKEN_A")
	names, err := restoreSecretsRecord(c.agentDir, c.home, "agent-x", id)
	if err != nil || strings.Join(names, ",") != "TOKEN_A" {
		t.Fatalf("restore = %v, %v", names, err)
	}
	p := filepath.Join(c.home, stagedSecretsRel, "TOKEN_A")
	requireRegular(t, p, "recorded TOKEN_A")
	if fi, _ := os.Lstat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode())
	}
	c.checkOutside(t)
}

func TestRestoreSecretsRecord_RefusesLinkedComponent(t *testing.T) {
	for _, rel := range []string{".scion", ".scion/harness", stagedSecretsRel} {
		t.Run(rel, func(t *testing.T) {
			c := newConfinedHome(t)
			id := writeSecretsRecord(t, c.agentDir, "TOKEN_A")
			c.link(t, c.outside, rel)
			if _, err := restoreSecretsRecord(c.agentDir, c.home, "agent-x", id); !errors.Is(err, agenthome.ErrLinkComponent) {
				t.Fatalf("err = %v, want a linked directory component refused", err)
			}
			c.checkOutside(t)
		})
	}
}

func TestRestoreSecretsRecord_LinkReplantedBetweenStartsIsRefused(t *testing.T) {
	c := newConfinedHome(t)
	id := writeSecretsRecord(t, c.agentDir, "TOKEN_A", "TOKEN_B")
	if _, err := restoreSecretsRecord(c.agentDir, c.home, "agent-x", id); err != nil {
		t.Fatal(err)
	}
	c.link(t, c.outside, stagedSecretsRel)
	if _, err := restoreSecretsRecord(c.agentDir, c.home, "agent-x", id); !errors.Is(err, agenthome.ErrLinkComponent) {
		t.Fatalf("err = %v, want the re-planted link refused", err)
	}
	c.checkOutside(t)
}

// --- writeAgentTokenFile ---

func TestWriteAgentTokenFile_LinkLeavesReplaced(t *testing.T) {
	c := newConfinedHome(t)
	c.link(t, filepath.Join(c.outside, "victim"), ".scion/scion-token")
	c.link(t, filepath.Join(c.outside, "TOKEN_A"), ".scion/scion-token.tmp")
	if err := writeAgentTokenFile(c.home, tokenFileSentinel); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(c.home, ".scion/scion-token")
	requireRegular(t, p, tokenFileSentinel)
	if fi, _ := os.Lstat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode())
	}
	requireAbsent(t, filepath.Join(c.home, ".scion/scion-token.tmp"))
	c.checkOutside(t)
}

func TestWriteAgentTokenFile_RefusesLinkedComponent(t *testing.T) {
	c := newConfinedHome(t)
	c.link(t, c.outside, ".scion")
	if err := writeAgentTokenFile(c.home, tokenFileSentinel); !errors.Is(err, agenthome.ErrLinkComponent) {
		t.Fatalf("err = %v, want a linked .scion refused", err)
	}
	c.checkOutside(t)
}

func TestWriteAgentTokenFile_LinkReplantedBetweenStartsIsRefused(t *testing.T) {
	c := newConfinedHome(t)
	if err := writeAgentTokenFile(c.home, tokenFileSentinel); err != nil {
		t.Fatal(err)
	}
	c.link(t, c.outside, ".scion")
	if err := writeAgentTokenFile(c.home, tokenFileSentinel); !errors.Is(err, agenthome.ErrLinkComponent) {
		t.Fatalf("err = %v, want the re-planted link refused", err)
	}
	c.checkOutside(t)
}

// --- removeStaleAuthCandidates (Start with NoAuth) ---

func TestRemoveStaleAuthCandidates_LinkLeafAndComponent(t *testing.T) {
	c := newConfinedHome(t)
	rel := agentplugin.InputsRel + "/auth-candidates.json"
	c.link(t, filepath.Join(c.outside, "victim"), rel)
	if err := removeStaleAuthCandidates(c.home); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, filepath.Join(c.home, rel))
	c.checkOutside(t)

	for _, comp := range []string{".scion", ".scion/harness", agentplugin.InputsRel} {
		c := newConfinedHome(t)
		c.link(t, c.outside, comp)
		if err := removeStaleAuthCandidates(c.home); !errors.Is(err, agenthome.ErrLinkComponent) {
			t.Fatalf("%s: err = %v, want a linked directory component refused", comp, err)
		}
		c.checkOutside(t)
	}
	if err := removeStaleAuthCandidates(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("missing home: %v", err)
	}
}

// The restore direction reports a per-plugin limit exactly as the snapshot
// direction does: an *agentplugin.LimitError naming the plugin, with the
// same message.
func TestRestorePluginsTree_LimitErrorMatchesSnapshot(t *testing.T) {
	src := t.TempDir()
	for rel, content := range map[string]string{
		"plugins/ok/plugin.json": "{}",
		"plugins/big/a":          strings.Repeat("x", 1<<20),
		"plugins/big/b":          "y",
	} {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lim := agentplugin.Limits{MaxFiles: 100, MaxBytes: 1 << 20, MaxDepth: 4}
	snapErr := copyPluginsTreeWithLimits(src, "plugins", t.TempDir(), "rec/plugins", lim)

	c := newConfinedHome(t)
	h, err := agenthome.Open(c.home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	restoreErr := restorePluginsTreeWithLimits(src, "plugins", h, agentplugin.InputsRel+"/plugins", lim)
	for name, err := range map[string]error{"snapshot": snapErr, "restore": restoreErr} {
		if !agentplugin.IsLimitError(err) || !strings.Contains(err.Error(), `copy agent plugin "big"`) {
			t.Fatalf("%s: err = %v, want a limit error naming the plugin", name, err)
		}
	}
	if snapErr.Error() != restoreErr.Error() {
		t.Fatalf("restore error %q differs from snapshot error %q", restoreErr, snapErr)
	}
	if !strings.Contains(restoreErr.Error(), "1 MiB") {
		t.Fatalf("restore error %q does not name the limit in MiB", restoreErr)
	}
	c.checkOutside(t)
}
