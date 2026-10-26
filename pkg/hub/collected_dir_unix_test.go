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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// replaceWithLink replaces the file at p with a link to a file outside the
// collected tree holding the same bytes, so only the regular-file and in-root
// checks can tell them apart.
func replaceWithLink(t *testing.T, p string) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), filepath.Base(p))
	if err := os.WriteFile(outside, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, p); err != nil {
		t.Fatal(err)
	}
}

func TestCollectedReaderSites_ReplacedByLinkAfterCollectFails(t *testing.T) {
	for _, site := range collectedReaderSites {
		t.Run(site.name, func(t *testing.T) {
			dir := collectedReaderTree(t)
			cd := collectTestDir(t, dir)
			replaceWithLink(t, filepath.Join(dir, filepath.FromSlash(site.entry)))
			err := site.read(t, cd)
			if err == nil {
				t.Fatalf("%s: read of %s replaced by a link after collection succeeded", site.name, site.entry)
			}
			if !strings.Contains(err.Error(), "no longer a regular file") {
				t.Errorf("%s: error %q does not say the entry is no longer a regular file", site.name, err)
			}
		})
	}
}

func TestCollectedReaderSites_LinkAtCollectionIsNotRead(t *testing.T) {
	for _, site := range collectedReaderSites {
		t.Run(site.name, func(t *testing.T) {
			dir := collectedReaderTree(t)
			// The entry is a link before collection, so collection skips it.
			replaceWithLink(t, filepath.Join(dir, filepath.FromSlash(site.entry)))
			cd := collectTestDir(t, dir)
			if _, ok := cd.entry(site.entry); ok {
				t.Fatalf("collection recorded the link %s", site.entry)
			}
			if _, err := cd.read(site.entry); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("read of the uncollected link %s: err %v, want not exist", site.entry, err)
			}
		})
	}

	// The derived values show the link was not read: no agent config, no
	// config.yaml.
	t.Run("template index derives from the name", func(t *testing.T) {
		dir := collectedReaderTree(t)
		replaceWithLink(t, filepath.Join(dir, "scion-agent.yaml"))
		idx, err := deriveTemplateIndexFromCollected(collectTestDir(t, dir), "claude-probe")
		if err != nil {
			t.Fatal(err)
		}
		if idx.DefaultHarnessConfig != "" || idx.Harness != "claude" {
			t.Errorf("index = %+v, want only the name-derived harness", idx)
		}
	})
	t.Run("harness-config index derives from the name", func(t *testing.T) {
		dir := collectedReaderTree(t)
		replaceWithLink(t, filepath.Join(dir, "config.yaml"))
		cd := collectTestDir(t, dir)
		if _, present, err := cd.harnessConfigYAML(); present || err != nil {
			t.Fatalf("harnessConfigYAML = present %v, err %v; want absent", present, err)
		}
		if idx := deriveHarnessConfigIndexFromCollected(cd, "my-config"); idx.Harness != "" || idx.Config != nil {
			t.Errorf("index = %+v, want empty", idx)
		}
	})
	t.Run("bundled check skips the link", func(t *testing.T) {
		// The link leads to a config.yaml with an unusable provisioner: a
		// reader that followed it would fail the check.
		dir := collectedReaderTree(t)
		entry := filepath.Join(dir, filepath.FromSlash(collectedTestBundledEntry))
		outside := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(outside, []byte(collectedTestUnusableYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(entry); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, entry); err != nil {
			t.Fatal(err)
		}
		if err := (&templatePersistence{}).CheckDir(t.Context(), "t", collectTestDir(t, dir)); err != nil {
			t.Errorf("CheckDir with the bundled config.yaml a link at collection: %v, want nil (not read)", err)
		}
	})
	t.Run("commit blob fallback does not write the link", func(t *testing.T) {
		dir := collectedReaderTree(t)
		p := filepath.Join(dir, collectedTestBlobPath)
		replaceWithLink(t, p)
		cd := collectTestDir(t, dir)
		stor := newCollectedReadStorage(t)
		hash := transfer.HashBytes([]byte(collectedTestBlobContent))
		blobPath := templateBlobPath("hubs/h/templates/global/t", strings.TrimPrefix(hash, transfer.HashPrefix))
		r := &templateBlobResolver{stor: stor, opts: commitOpts{collected: cd}}
		e := &store.TemplateFile{Path: collectedTestBlobPath, Size: int64(len(collectedTestBlobContent)), Hash: hash}
		found, err := r.fromCollected(t.Context(), e, blobPath)
		if err != nil || found {
			t.Fatalf("fromCollected of a link at collection = (%v, %v), want not found", found, err)
		}
		if exists, err := stor.Exists(t.Context(), blobPath); err != nil || exists {
			t.Errorf("blob written for a link at collection (exists %v, err %v)", exists, err)
		}
	})
}

// collectedTestUnusableYAML declares a provisioner that could never provision
// an agent (the bundled harness-config check refuses it).
const collectedTestUnusableYAML = "harness: claude\nprovisioner:\n  type: builtin\n"

func TestCollectFiles_DirectoryLinkRefused(t *testing.T) {
	target := writeCollectedTestTree(t, map[string]string{"config.yaml": collectedTestHarnessYAML})
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	rs := &ResourceStore{}
	cd, err := rs.collectFiles(link)
	if err == nil {
		_ = cd.root.Close()
		t.Fatal("collectFiles of a link to a directory succeeded")
	}
	if !strings.Contains(err.Error(), "is a link, not a directory") {
		t.Errorf("error %q does not say the directory is a link", err)
	}
}

func TestCollectFiles_DirectoryReplacedDuringCollectionFails(t *testing.T) {
	dir := writeCollectedTestTree(t, map[string]string{"config.yaml": collectedTestHarnessYAML})
	moved := dir + ".moved"
	hook := func(string) {
		// Replace the collected directory with another one at the same path
		// before the root is opened.
		if err := os.Rename(dir, moved); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Error(err)
		}
	}
	afterCollectHook.Store(&hook)
	t.Cleanup(func() {
		afterCollectHook.Store(nil)
		_ = os.RemoveAll(moved)
	})
	rs := &ResourceStore{}
	cd, err := rs.collectFiles(dir)
	if err == nil {
		_ = cd.root.Close()
		t.Fatal("collectFiles of a directory replaced during collection succeeded")
	}
	if !errors.Is(err, errCollectedDirChanged) {
		t.Errorf("error %v, want errCollectedDirChanged", err)
	}
}

func TestCollectFiles_WithinRequiresTheSameSubdirectory(t *testing.T) {
	parent := writeCollectedTestTree(t, map[string]string{
		"harness-configs/probe/config.yaml": collectedTestHarnessYAML,
	})
	pcd := collectTestDir(t, parent)
	sub := filepath.Join(parent, "harness-configs", "probe")

	rs := &ResourceStore{within: pcd, withinPath: "harness-configs/probe"}
	cd, err := rs.collectFiles(sub)
	if err != nil {
		t.Fatalf("collectFiles of the bundled subdirectory: %v", err)
	}
	_ = cd.root.Close()

	other := writeCollectedTestTree(t, map[string]string{"config.yaml": collectedTestHarnessYAML})
	if cd, err := rs.collectFiles(other); err == nil {
		_ = cd.root.Close()
		t.Fatal("collectFiles of a directory that is not within's subdirectory succeeded")
	}

	// The subdirectory replaced by a link inside the parent after the
	// parent's collection.
	outside := writeCollectedTestTree(t, map[string]string{"config.yaml": collectedTestHarnessYAML})
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}
	if cd, err := rs.collectFiles(outside); err == nil {
		_ = cd.root.Close()
		t.Fatal("collectFiles through a linked bundled subdirectory succeeded")
	}
}
