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

//go:build !windows

package hub

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// localLinkedConfigYAMLTree returns a parent directory with two
// harness-config directories: claude-real, whose config.yaml is a regular
// file naming it real-cfg, and claude-linked, whose config.yaml is a link to
// a valid config.yaml outside the parent that names it outside-cfg and
// declares an unusable provisioner. A reader that follows the link sees a
// valid config, a name and the provisioner refusal.
func localLinkedConfigYAMLTree(t *testing.T) string {
	t.Helper()
	parent := writeCollectedTestTree(t, map[string]string{
		"claude-real/config.yaml":    "harness: claude\nname: real-cfg\n",
		"claude-linked/home/.rc":     "# rc\n",
		"claude-real/home/.rc":       "# rc\n",
		"claude-other/home/.rc":      "# rc\n",
		"claude-other/README.md":     "not a harness-config\n",
		"claude-other/config.yaml/x": "a directory named config.yaml\n",
	})
	outside := writeCollectedTestTree(t, map[string]string{
		"config.yaml": "harness: claude\nname: outside-cfg\nprovisioner:\n  type: builtin\n",
	})
	if err := os.Symlink(filepath.Join(outside, "config.yaml"), filepath.Join(parent, "claude-linked", "config.yaml")); err != nil {
		t.Fatal(err)
	}
	// The link target is valid: the unconfined loader accepts it.
	if _, err := config.LoadHarnessConfigDir(filepath.Join(parent, "claude-linked")); err != nil {
		t.Fatalf("fixture: the link target does not load: %v", err)
	}
	return parent
}

// localInRootLinkDir returns a harness-config directory whose config.yaml
// and agent-plugins.yaml are links to files INSIDE the directory: os.Root
// would resolve them, so only the regular-file check refuses them. The
// config.yaml target is valid and names the config inroot-cfg; the sidecar
// target is invalid, so a reader that followed it records it.
func localInRootLinkDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(writeCollectedTestTree(t, map[string]string{
		"claude-inroot/home/real.yaml": "harness: claude\nname: inroot-cfg\n",
		"claude-inroot/home/side.yaml": "{{ not yaml",
	}), "claude-inroot")
	if err := os.Symlink("home/real.yaml", filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("home/side.yaml", filepath.Join(dir, config.AgentPluginsSidecarFile)); err != nil {
		t.Fatal(err)
	}
	// The unconfined loader follows both links.
	if hc, err := config.LoadHarnessConfigDir(dir); err != nil || hc.Name != "inroot-cfg" || hc.Config.AgentPluginsSidecar == nil {
		t.Fatalf("fixture: the unconfined loader did not follow the in-root links (%+v, %v)", hc, err)
	}
	return dir
}

// mkfifoOrSkip makes a FIFO at p, or skips the test where FIFOs are not
// supported.
func mkfifoOrSkip(t *testing.T, p string) {
	t.Helper()
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
}

// returnsPromptly fails the test if fn does not return within 5 s (a read
// that blocked on a FIFO).
func returnsPromptly(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("call blocked on a FIFO")
	}
}

func TestIsHarnessConfigDir_DoesNotFollowLinkedConfigYAML(t *testing.T) {
	parent := localLinkedConfigYAMLTree(t)
	for name, want := range map[string]bool{
		"claude-real":   true,
		"claude-linked": false,
		"claude-other":  false,
	} {
		if got := isHarnessConfigDir(filepath.Join(parent, name)); got != want {
			t.Errorf("isHarnessConfigDir(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestLoadLocalHarnessConfigDir(t *testing.T) {
	parent := localLinkedConfigYAMLTree(t)

	t.Run("regular config.yaml loads like LoadHarnessConfigDir", func(t *testing.T) {
		dir := filepath.Join(parent, "claude-real")
		got, err := loadLocalHarnessConfigDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		want, err := config.LoadHarnessConfigDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != want.Name || got.Path != want.Path || got.Config.Harness != want.Config.Harness {
			t.Errorf("loaded %q %q %q, want %q %q %q", got.Name, got.Path, got.Config.Harness, want.Name, want.Path, want.Config.Harness)
		}
	})
	t.Run("linked config.yaml fails", func(t *testing.T) {
		hc, err := loadLocalHarnessConfigDir(filepath.Join(parent, "claude-linked"))
		if err == nil {
			t.Fatalf("loaded %+v through a linked config.yaml", hc)
		}
		if !errors.Is(err, errNotRegularLocalFile) {
			t.Errorf("error = %v, want the not-a-regular-file refusal", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "config.yaml is a link or not a regular file") || strings.Contains(msg, "does not exist") {
			t.Errorf("error message %q: want it to say the file is a link or not a regular file, not that it does not exist", msg)
		}
	})
	t.Run("config.yaml that is a directory fails", func(t *testing.T) {
		_, err := loadLocalHarnessConfigDir(filepath.Join(parent, "claude-other"))
		if !errors.Is(err, errNotRegularLocalFile) {
			t.Fatalf("error = %v, want the not-a-regular-file refusal", err)
		}
	})
	t.Run("in-root linked config.yaml fails", func(t *testing.T) {
		hc, err := loadLocalHarnessConfigDir(localInRootLinkDir(t))
		if !errors.Is(err, errNotRegularLocalFile) {
			t.Fatalf("loaded %+v, err %v; want the not-a-regular-file refusal", hc, err)
		}
	})
	t.Run("in-root linked sidecar reads as absent", func(t *testing.T) {
		dir := localInRootLinkDir(t)
		if err := os.Remove(filepath.Join(dir, "config.yaml")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("harness: claude\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := loadLocalHarnessConfigDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got.Config.AgentPluginsSidecar != nil {
			t.Errorf("the in-root linked sidecar was read: %+v", got.Config.AgentPluginsSidecar)
		}
	})
	t.Run("FIFO config.yaml fails without blocking", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "claude-fifo")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		mkfifoOrSkip(t, filepath.Join(dir, "config.yaml"))
		returnsPromptly(t, func() {
			if _, err := loadLocalHarnessConfigDir(dir); !errors.Is(err, errNotRegularLocalFile) {
				t.Errorf("error = %v, want the not-a-regular-file refusal", err)
			}
		})
	})
	t.Run("FIFO sidecar reads as absent without blocking", func(t *testing.T) {
		dir := filepath.Join(writeCollectedTestTree(t, map[string]string{"claude-fifo/config.yaml": "harness: claude\n"}), "claude-fifo")
		mkfifoOrSkip(t, filepath.Join(dir, config.AgentPluginsSidecarFile))
		returnsPromptly(t, func() {
			got, err := loadLocalHarnessConfigDir(dir)
			if err != nil {
				t.Errorf("load: %v", err)
				return
			}
			if got.Config.AgentPluginsSidecar != nil {
				t.Errorf("the FIFO sidecar was read: %+v", got.Config.AgentPluginsSidecar)
			}
		})
	})
	t.Run("linked directory fails", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "claude-dirlink")
		if err := os.Symlink(filepath.Join(parent, "claude-real"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := loadLocalHarnessConfigDir(link); err == nil {
			t.Fatal("loaded a harness-config through a linked directory")
		}
	})
	t.Run("linked sidecar reads as absent", func(t *testing.T) {
		dir := filepath.Join(parent, "claude-real")
		outside := filepath.Join(t.TempDir(), config.AgentPluginsSidecarFile)
		if err := os.WriteFile(outside, []byte("{{ not yaml"), 0o644); err != nil {
			t.Fatal(err)
		}
		sidecar := filepath.Join(dir, config.AgentPluginsSidecarFile)
		if err := os.Symlink(outside, sidecar); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(sidecar) })
		// The unconfined loader follows the link and records the invalid
		// sidecar; the confined one does not open it.
		if followed, err := config.LoadHarnessConfigDir(dir); err != nil || followed.Config.AgentPluginsSidecar == nil {
			t.Fatalf("fixture: the unconfined loader did not read the linked sidecar (%v)", err)
		}
		got, err := loadLocalHarnessConfigDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got.Config.AgentPluginsSidecar != nil {
			t.Errorf("the linked sidecar was read: %+v", got.Config.AgentPluginsSidecar)
		}
	})
}

func TestReadLocalHarnessConfigFile_LinkReadsAsAbsent(t *testing.T) {
	parent := localLinkedConfigYAMLTree(t)
	if _, err := readLocalHarnessConfigFile(filepath.Join(parent, "claude-real"), "config.yaml"); err != nil {
		t.Fatalf("regular config.yaml: %v", err)
	}
	_, err := readLocalHarnessConfigFile(filepath.Join(parent, "claude-linked"), "config.yaml")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("linked config.yaml: err = %v, want absent", err)
	}
}

// Import discovery does not follow a linked config.yaml: the directory is
// reported as skipped, and its name is not bound from the link target.
func TestDiscoverResourceDirs_LinkedHarnessConfigYAMLNotFollowed(t *testing.T) {
	parent := localLinkedConfigYAMLTree(t)
	kind := (&Server{}).harnessConfigImportKind()

	dirs, skipped, err := discoverResourceDirs(parent, "", kind)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0].name != "real-cfg" || !dirs[0].nameFromConfig {
		t.Errorf("dirs = %+v, want only real-cfg named from its config.yaml", dirs)
	}
	linkedSkipped := false
	for _, sd := range skipped {
		if sd.name == "claude-linked" {
			linkedSkipped = true
		}
	}
	if !linkedSkipped {
		t.Errorf("skipped = %+v, want claude-linked reported as skipped", skipped)
	}

	t.Run("name is not bound from the link target", func(t *testing.T) {
		// Admit every directory, so the name binding is tested on its own.
		admitAll := kind
		admitAll.isResourceDir = func(string) bool { return true }
		leaf := filepath.Join(parent, "claude-linked")
		dirs, _, err := discoverResourceDirs(leaf, "", admitAll)
		if err != nil {
			t.Fatal(err)
		}
		if len(dirs) != 1 || dirs[0].name != "claude-linked" || dirs[0].nameFromConfig {
			t.Errorf("dirs = %+v, want claude-linked not named from the link target", dirs)
		}
	})
	t.Run("child name is not bound from the link target", func(t *testing.T) {
		// Admit every child but not the parent, so the children's name
		// binding is tested (the parent stays a parent, not a leaf).
		admitChildren := kind
		admitChildren.isResourceDir = func(d string) bool { return d != parent }
		dirs, _, err := discoverResourceDirs(parent, "", admitChildren)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, rd := range dirs {
			if rd.path != filepath.Join(parent, "claude-linked") {
				continue
			}
			found = true
			if rd.name != "claude-linked" || rd.nameFromConfig {
				t.Errorf("child %+v: want claude-linked, not named from the link target", rd)
			}
		}
		if !found {
			t.Errorf("dirs = %+v, want the claude-linked child admitted", dirs)
		}
	})
}

// The import's store selection and provisioner check do not read a linked
// config.yaml.
func TestHarnessConfigImportKind_LinkedConfigYAMLNotRead(t *testing.T) {
	parent := localLinkedConfigYAMLTree(t)
	kind := (&Server{}).harnessConfigImportKind()
	linked := resourceDir{name: "claude-linked", path: filepath.Join(parent, "claude-linked")}

	if _, err := kind.newStore(linked.path); err == nil {
		t.Error("newStore accepted a directory whose config.yaml is a link")
	}
	if err := kind.checkContent([]resourceDir{linked}); err != nil {
		t.Errorf("checkContent read the linked config.yaml: %v", err)
	}
}

// The reimport's store selection loads config.yaml confined to the
// directory: a linked config.yaml is refused, a regular one is accepted.
func TestReimportHarnessConfigNewStore_LinkedConfigYAMLRefused(t *testing.T) {
	parent := localLinkedConfigYAMLTree(t)
	newStore := (&Server{}).reimportHarnessConfigNewStore("hc-target")
	if _, err := newStore(filepath.Join(parent, "claude-linked")); !errors.Is(err, errNotRegularLocalFile) {
		t.Errorf("linked config.yaml: err = %v, want the not-a-regular-file refusal", err)
	}
	if rs, err := newStore(filepath.Join(parent, "claude-real")); err != nil || rs == nil {
		t.Errorf("regular config.yaml: store %v, err %v; want a store", rs, err)
	}
}

// The directory itself (or its parent) replaced between openRealDirRoot's
// Lstat and its open: a FIFO does not block the open and a link is refused;
// all fail.
// Checked through openRealDirRoot, collectFiles and the pre-collection loader.
func TestOpenRealDirRoot_DirectorySwappedAfterLstat(t *testing.T) {
	swaps := map[string]func(t *testing.T, dir string){
		"FIFO": func(t *testing.T, dir string) {
			if err := os.RemoveAll(dir); err != nil {
				t.Error(err)
			}
			if err := syscall.Mkfifo(dir, 0o600); err != nil {
				t.Errorf("mkfifo: %v", err)
			}
		},
		"parent FIFO": func(t *testing.T, dir string) {
			parent := filepath.Dir(dir)
			moved := parent + ".moved"
			if err := os.Rename(parent, moved); err != nil {
				t.Error(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(moved) })
			if err := syscall.Mkfifo(parent, 0o600); err != nil {
				t.Errorf("mkfifo: %v", err)
			}
		},
		"link": func(t *testing.T, dir string) {
			other := writeCollectedTestTree(t, map[string]string{"config.yaml": "harness: claude\n"})
			if err := os.RemoveAll(dir); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(other, dir); err != nil {
				t.Error(err)
			}
		},
	}
	calls := map[string]func(dir string) error{
		"openRealDirRoot": func(dir string) error {
			root, _, err := openRealDirRoot(dir)
			if root != nil {
				_ = root.Close()
			}
			return err
		},
		"collectFiles": func(dir string) error {
			cd, err := (&ResourceStore{}).collectFiles(dir)
			if cd != nil {
				_ = cd.root.Close()
			}
			return err
		},
		"loadLocalHarnessConfigDir": func(dir string) error {
			_, err := loadLocalHarnessConfigDir(dir)
			return err
		},
	}
	for sname, swap := range swaps {
		for cname, call := range calls {
			t.Run(sname+" "+cname, func(t *testing.T) {
				dir := filepath.Join(writeCollectedTestTree(t, map[string]string{"claude-x/config.yaml": "harness: claude\n"}), "claude-x")
				if sname != "link" {
					// mkfifo support is checked once, outside the hook.
					mkfifoOrSkip(t, filepath.Join(t.TempDir(), "probe"))
				}
				hook := func(string) { swap(t, dir) }
				afterDirLstatHook.Store(&hook)
				t.Cleanup(func() { afterDirLstatHook.Store(nil) })
				returnsPromptly(t, func() {
					if err := call(dir); err == nil {
						t.Errorf("%s succeeded on a directory replaced by a %s after the Lstat", cname, sname)
					}
				})
			})
		}
	}
	t.Run("control", func(t *testing.T) {
		dir := filepath.Join(writeCollectedTestTree(t, map[string]string{"claude-x/config.yaml": "harness: claude\n"}), "claude-x")
		for cname, call := range calls {
			if err := call(dir); err != nil {
				t.Errorf("%s on an unchanged directory: %v", cname, err)
			}
		}
	})
}

// The reads before collection are capped at maxHarnessConfigFileSize, as the
// hub's other harness-config file reads are: an oversized config.yaml fails
// the load, and an oversized sidecar is recorded as invalid for its size
// (Agent Plugins unsupported) instead of being read in full.
func TestLoadLocalHarnessConfigDir_OversizedFilesRefused(t *testing.T) {
	pad := strings.Repeat("# padding line to exceed the harness-config size cap\n", maxHarnessConfigFileSize/50+1)
	t.Run("config.yaml", func(t *testing.T) {
		dir := filepath.Join(writeCollectedTestTree(t, map[string]string{
			"claude-big/config.yaml": "harness: claude\n" + pad,
		}), "claude-big")
		hc, err := loadLocalHarnessConfigDir(dir)
		if !errors.Is(err, errLocalHarnessConfigTooLarge) {
			t.Fatalf("loaded %+v, err %v; want the size refusal", hc, err)
		}
	})
	t.Run("sidecar", func(t *testing.T) {
		dir := filepath.Join(writeCollectedTestTree(t, map[string]string{
			"claude-big/config.yaml":                       "harness: claude\n",
			"claude-big/" + config.AgentPluginsSidecarFile: pad,
		}), "claude-big")
		hc, err := loadLocalHarnessConfigDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		sc := hc.Config.AgentPluginsSidecar
		if sc == nil || !strings.Contains(sc.Reason, errLocalHarnessConfigTooLarge.Error()) {
			t.Errorf("sidecar = %+v, want it recorded as invalid for its size", sc)
		}
	})
}
