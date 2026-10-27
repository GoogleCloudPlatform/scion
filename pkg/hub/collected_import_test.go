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

//go:build !no_sqlite && !windows

package hub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// harnessConfigsByName lists the stored harness-configs by name.
func harnessConfigsByName(t *testing.T, s store.Store) map[string]store.HarnessConfig {
	t.Helper()
	res, err := s.ListHarnessConfigs(context.Background(), store.HarnessConfigFilter{}, store.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.HarnessConfig{}
	for _, hc := range res.Items {
		out[hc.Name] = hc
	}
	return out
}

// bundledTemplateTree is a template directory bundling harness-config
// "probe".
func bundledTemplateTree(t *testing.T) string {
	return writeCollectedTestTree(t, map[string]string{
		"scion-agent.yaml":                  collectedTestAgentConfig,
		"harness-configs/probe/config.yaml": "harness: claude\n",
		"harness-configs/probe/home/.rc":    "# rc\n",
	})
}

func TestImportTemplateHarnessConfigs_UnchangedBundleImported(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	cd := collectTestDir(t, bundledTemplateTree(t))
	srv.importTemplateHarnessConfigs(context.Background(), cd, string(store.TemplateScopeGlobal), "")
	hc, ok := harnessConfigsByName(t, s)["probe"]
	if !ok {
		t.Fatal("the bundled harness-config was not imported")
	}
	if hc.Harness != "claude" {
		t.Errorf("harness = %q, want claude", hc.Harness)
	}
}

func TestImportTemplateHarnessConfigs_ChangedAfterCollectNotImported(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	dir := bundledTemplateTree(t)
	cd := collectTestDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "harness-configs", "probe", "config.yaml"), []byte("harness: gemini-cli\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.importTemplateHarnessConfigs(context.Background(), cd, string(store.TemplateScopeGlobal), "")
	if _, ok := harnessConfigsByName(t, s)["probe"]; ok {
		t.Error("a bundled harness-config changed after the template's collection was imported")
	}
}

func TestImportTemplateHarnessConfigs_ReplacedByLinkAfterCollectNotImported(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	dir := bundledTemplateTree(t)
	cd := collectTestDir(t, dir)
	sub := filepath.Join(dir, "harness-configs", "probe")
	outside := writeCollectedTestTree(t, map[string]string{
		"config.yaml": "harness: claude\n",
		"home/.rc":    "# rc\n",
	})
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}
	srv.importTemplateHarnessConfigs(context.Background(), cd, string(store.TemplateScopeGlobal), "")
	if _, ok := harnessConfigsByName(t, s)["probe"]; ok {
		t.Error("a bundled harness-config replaced by a link after the template's collection was imported")
	}
}

func TestImportTemplateHarnessConfigs_LinkAtCollectionNotImported(t *testing.T) {
	outside := writeCollectedTestTree(t, map[string]string{
		"probe/config.yaml": "harness: claude\n",
	})
	for name, link := range map[string]func(t *testing.T, dir string){
		"linked harness-configs directory": func(t *testing.T, dir string) {
			if err := os.Symlink(outside, filepath.Join(dir, "harness-configs")); err != nil {
				t.Fatal(err)
			}
		},
		"linked subdirectory": func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "harness-configs"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "probe"), filepath.Join(dir, "harness-configs", "probe")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv, s, _ := testTemplateBootstrapServer(t)
			dir := writeCollectedTestTree(t, map[string]string{"scion-agent.yaml": collectedTestAgentConfig})
			link(t, dir)
			cd := collectTestDir(t, dir)
			srv.importTemplateHarnessConfigs(context.Background(), cd, string(store.TemplateScopeGlobal), "")
			if _, ok := harnessConfigsByName(t, s)["probe"]; ok {
				t.Errorf("a harness-config reached through a link that collection skipped was imported (%s)", name)
			}
		})
	}
}

// A bootstrap commit stores the harness the collected config.yaml gives,
// never the store's placeholder; a collected config.yaml that gives none
// fails the bootstrap.
func TestHarnessConfigBootstrap_StoresCollectedHarnessOrFails(t *testing.T) {
	t.Run("derived harness replaces the placeholder", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		dir := writeCollectedTestTree(t, map[string]string{"config.yaml": "harness: gemini-cli\n"})
		if _, err := srv.harnessConfigStore("claude").Bootstrap(context.Background(), "my-config", dir, store.HarnessConfigScopeGlobal, "", "", false); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		hc := harnessConfigsByName(t, s)["my-config"]
		if hc.Harness != "gemini-cli" || hc.Status != store.HarnessConfigStatusActive {
			t.Errorf("stored harness %q status %q, want gemini-cli active", hc.Harness, hc.Status)
		}
	})
	t.Run("collected config.yaml without a harness fails", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		// Validation before collection (LoadHarnessConfigDir) would refuse
		// this file; it stands for a config.yaml changed after it.
		dir := writeCollectedTestTree(t, map[string]string{"config.yaml": "image: example:latest\n"})
		_, err := srv.harnessConfigStore("claude").Bootstrap(context.Background(), "my-config", dir, store.HarnessConfigScopeGlobal, "", "", false)
		if err == nil || !strings.Contains(err.Error(), "sets no harness") {
			t.Fatalf("bootstrap error = %v, want the no-harness refusal", err)
		}
		if hc, ok := harnessConfigsByName(t, s)["my-config"]; ok && hc.Status == store.HarnessConfigStatusActive {
			t.Errorf("an active row was stored with harness %q", hc.Harness)
		}
	})
	t.Run("collected config.yaml over the parse limit keeps the stored harness", func(t *testing.T) {
		// A config.yaml larger than maxHarnessConfigFileSize is not parsed on
		// the collected path (derived as unparsable), so its harness is not
		// derived; the commit keeps the harness the row was created with and
		// logs a warning, as before. LoadHarnessConfigDir has no size limit,
		// so such a file passes the validation before collection.
		srv, s, _ := testTemplateBootstrapServer(t)
		big := "harness: gemini-cli\n# " + strings.Repeat("x", maxHarnessConfigFileSize) + "\n"
		dir := writeCollectedTestTree(t, map[string]string{"config.yaml": big})
		if _, err := config.LoadHarnessConfigDir(dir); err != nil {
			t.Fatalf("validation before collection: %v", err)
		}
		if _, err := srv.harnessConfigStore("claude").Bootstrap(context.Background(), "my-config", dir, store.HarnessConfigScopeGlobal, "", "", false); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		hc := harnessConfigsByName(t, s)["my-config"]
		if hc.Harness != "claude" || hc.Status != store.HarnessConfigStatusActive {
			t.Errorf("stored harness %q status %q, want the kept harness claude, active", hc.Harness, hc.Status)
		}
	})
	t.Run("a config.yaml that passes validation gives a harness", func(t *testing.T) {
		dir := writeCollectedTestTree(t, map[string]string{"config.yaml": "harness: claude\nimage: example:latest\n"})
		if _, err := config.LoadHarnessConfigDir(dir); err != nil {
			t.Fatalf("validation: %v", err)
		}
		if idx := deriveHarnessConfigIndexFromCollected(collectTestDir(t, dir), "my-config"); idx.Harness == "" {
			t.Error("a validated config.yaml derived no harness")
		}
	})
}

// The import named the resource from config.yaml before collection: the
// collected config.yaml must still carry that name.
func TestHarnessConfigImport_NameFromConfigMustMatchCollected(t *testing.T) {
	t.Run("same name imports", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		dir := writeCollectedTestTree(t, map[string]string{"config.yaml": "harness: claude\nname: cfg-a\n"})
		rs := srv.harnessConfigStore("claude")
		rs.requireConfigName()
		if _, err := rs.Bootstrap(context.Background(), "cfg-a", dir, store.HarnessConfigScopeGlobal, "", "", true); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if _, ok := harnessConfigsByName(t, s)["cfg-a"]; !ok {
			t.Error("cfg-a was not imported")
		}
	})
	t.Run("over the parse limit with the same name imports", func(t *testing.T) {
		// The name of a config.yaml over the parse limit is re-checked by
		// reading it in full; the harness keeps exception (a).
		srv, s, _ := testTemplateBootstrapServer(t)
		big := "harness: gemini-cli\nname: cfg-big\n# " + strings.Repeat("x", maxHarnessConfigFileSize) + "\n"
		dir := writeCollectedTestTree(t, map[string]string{"config.yaml": big})
		rs := srv.harnessConfigStore("claude")
		rs.requireConfigName()
		if _, err := rs.Bootstrap(context.Background(), "cfg-big", dir, store.HarnessConfigScopeGlobal, "", "", true); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if hc, ok := harnessConfigsByName(t, s)["cfg-big"]; !ok || hc.Status != store.HarnessConfigStatusActive {
			t.Errorf("cfg-big stored %v, status %q; want active", ok, hc.Status)
		}
	})
	t.Run("over the parse limit with a changed name fails", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		big := "harness: gemini-cli\nname: cfg-other\n# " + strings.Repeat("x", maxHarnessConfigFileSize) + "\n"
		dir := writeCollectedTestTree(t, map[string]string{"config.yaml": big})
		rs := srv.harnessConfigStore("claude")
		rs.requireConfigName()
		_, err := rs.Bootstrap(context.Background(), "cfg-big", dir, store.HarnessConfigScopeGlobal, "", "", true)
		if err == nil || !strings.Contains(err.Error(), `names it "cfg-other"`) {
			t.Fatalf("bootstrap error = %v, want the name mismatch refusal naming cfg-other", err)
		}
		if _, ok := harnessConfigsByName(t, s)["cfg-big"]; ok {
			t.Error("cfg-big was stored although its collected config.yaml names it cfg-other")
		}
	})
	t.Run("unparseable config.yaml fails as a parse error", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		dir := writeCollectedTestTree(t, map[string]string{"config.yaml": "harness: [unclosed\n"})
		rs := srv.harnessConfigStore("claude")
		rs.requireConfigName()
		_, err := rs.Bootstrap(context.Background(), "cfg-a", dir, store.HarnessConfigScopeGlobal, "", "", true)
		if err == nil || !strings.Contains(err.Error(), "does not parse") {
			t.Fatalf("bootstrap error = %v, want the parse refusal", err)
		}
		if _, ok := harnessConfigsByName(t, s)["cfg-a"]; ok {
			t.Error("cfg-a was stored although its collected config.yaml does not parse")
		}
	})
	t.Run("changed name fails", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		dir := writeCollectedTestTree(t, map[string]string{"config.yaml": "harness: claude\nname: cfg-b\n"})
		rs := srv.harnessConfigStore("claude")
		rs.requireConfigName()
		_, err := rs.Bootstrap(context.Background(), "cfg-a", dir, store.HarnessConfigScopeGlobal, "", "", true)
		if err == nil || !strings.Contains(err.Error(), "names it") {
			t.Fatalf("bootstrap error = %v, want the name mismatch refusal", err)
		}
		if _, ok := harnessConfigsByName(t, s)["cfg-a"]; ok {
			t.Error("cfg-a was stored although its collected config.yaml names it cfg-b")
		}
	})
}

// A harness-config with no collected config.yaml is refused, even when its
// harness could be inferred from its name: a missing config.yaml, and one
// that is a link (not collected).
func TestHarnessConfigBootstrap_MissingConfigYAMLRefused(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	dir := filepath.Join(writeCollectedTestTree(t, map[string]string{"claude-missing/home/.rc": "# rc\n"}), "claude-missing")
	_, err := srv.harnessConfigStore("claude").Bootstrap(context.Background(), "claude-missing", dir, store.HarnessConfigScopeGlobal, "", "", false)
	if err == nil || !strings.Contains(err.Error(), "no config.yaml was collected") {
		t.Fatalf("bootstrap error = %v, want the missing config.yaml refusal", err)
	}
	if _, ok := harnessConfigsByName(t, s)["claude-missing"]; ok {
		t.Error("a harness-config without config.yaml was stored")
	}
}

func TestHarnessConfigBootstrap_LinkedConfigYAMLRefused(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	outside := writeCollectedTestTree(t, map[string]string{"config.yaml": "harness: claude\n"})
	dir := filepath.Join(t.TempDir(), "claude-probe")
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "home", ".rc"), []byte("# rc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "config.yaml"), filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	_, err := srv.harnessConfigStore("claude").Bootstrap(context.Background(), "claude-probe", dir, store.HarnessConfigScopeGlobal, "", "", false)
	if err == nil || !strings.Contains(err.Error(), "no config.yaml was collected") {
		t.Fatalf("bootstrap error = %v, want the uncollected config.yaml refusal", err)
	}
	if _, ok := harnessConfigsByName(t, s)["claude-probe"]; ok {
		t.Error("a harness-config whose config.yaml is a link was stored")
	}
}

// A bundled harness-config whose harness-configs directory is replaced by a
// link before its bootstrap is refused before anything reads or rewrites the
// directory: the file the link leads to is not normalized.
func TestBundledHarnessConfigBootstrap_LinkBeforeNormalizeRefused(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	parent := bundledTemplateTree(t)
	pcd := collectTestDir(t, parent)
	const crlf = "harness: claude\r\n"
	outside := writeCollectedTestTree(t, map[string]string{"probe/config.yaml": crlf})
	if err := os.RemoveAll(filepath.Join(parent, "harness-configs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "harness-configs")); err != nil {
		t.Fatal(err)
	}
	rs := srv.harnessConfigStore("claude")
	rs.within, rs.withinPath = pcd, "harness-configs/probe"
	_, err := rs.Bootstrap(context.Background(), "probe", filepath.Join(parent, "harness-configs", "probe"), store.HarnessConfigScopeGlobal, "", "", false)
	if err == nil {
		t.Fatal("bootstrap through a linked harness-configs directory succeeded")
	}
	got, rerr := os.ReadFile(filepath.Join(outside, "probe", "config.yaml"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != crlf {
		t.Errorf("the file behind the link was rewritten: %q", got)
	}
	if _, ok := harnessConfigsByName(t, s)["probe"]; ok {
		t.Error("the bundled harness-config was stored")
	}
}
