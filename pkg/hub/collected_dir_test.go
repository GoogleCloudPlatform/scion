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

package hub

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// writeCollectedTestTree writes files (slash path to content) under a new
// directory and returns it.
func writeCollectedTestTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const (
	collectedTestAgentConfig  = "schema_version: \"1\"\nharness_config: probe-hc\n"
	collectedTestHarnessYAML  = "harness: claude\n"
	collectedTestBundledYAML  = "harness: claude\n"
	collectedTestBlobPath     = "notes.md"
	collectedTestBlobContent  = "notes\n"
	collectedTestBundledEntry = "harness-configs/probe/config.yaml"
)

// collectedReaderSite is one reader of collected content in the bootstrap and
// import flow: the entry it reads and a call that reads it.
type collectedReaderSite struct {
	name  string
	entry string
	read  func(t *testing.T, cd *collectedDir) error
}

// collectedReaderSites lists the collected-content readers, each driven
// through its production entry point.
var collectedReaderSites = []collectedReaderSite{
	{
		name:  "template bundled harness-config check",
		entry: collectedTestBundledEntry,
		read: func(t *testing.T, cd *collectedDir) error {
			return (&templatePersistence{}).CheckDir(context.Background(), "t", cd)
		},
	},
	{
		name:  "harness-config check",
		entry: "config.yaml",
		read: func(t *testing.T, cd *collectedDir) error {
			return (&harnessConfigPersistence{}).CheckDir(context.Background(), "probe", cd)
		},
	},
	{
		name:  "harness-config commit read",
		entry: "config.yaml",
		read: func(t *testing.T, cd *collectedDir) error {
			// The commit's config.yaml read, with the collected directory.
			next := []store.TemplateFile{{Path: "config.yaml"}}
			_, _, err := (&Server{}).readHarnessConfigYAML(context.Background(), nil, "", next, harnessConfigCommitOpts{collected: cd})
			return err
		},
	},
	{
		name:  "template index derivation",
		entry: "scion-agent.yaml",
		read: func(t *testing.T, cd *collectedDir) error {
			_, err := deriveTemplateIndexFromCollected(cd, "probe")
			return err
		},
	},
	{
		name:  "commit blob from the collected directory",
		entry: collectedTestBlobPath,
		read: func(t *testing.T, cd *collectedDir) error {
			fi, ok := cd.entry(collectedTestBlobPath)
			if !ok {
				t.Fatalf("%s was not collected", collectedTestBlobPath)
			}
			r := &templateBlobResolver{stor: newCollectedReadStorage(t), opts: commitOpts{collected: cd}}
			e := &store.TemplateFile{Path: fi.Path, Size: fi.Size, Hash: fi.Hash}
			found, err := r.fromCollected(context.Background(), e, templateBlobPath("hubs/h/templates/global/t", strings.TrimPrefix(fi.Hash, transfer.HashPrefix)))
			if err == nil && !found {
				return errors.New("blob not written")
			}
			return err
		},
	},
}

// collectedReaderTree is a directory holding every site's entry.
func collectedReaderTree(t *testing.T) string {
	return writeCollectedTestTree(t, map[string]string{
		"scion-agent.yaml":        collectedTestAgentConfig,
		"config.yaml":             collectedTestHarnessYAML,
		collectedTestBundledEntry: collectedTestBundledYAML,
		collectedTestBlobPath:     collectedTestBlobContent,
	})
}

func TestCollectedReaderSites_UnchangedEntryReads(t *testing.T) {
	for _, site := range collectedReaderSites {
		t.Run(site.name, func(t *testing.T) {
			cd := collectTestDir(t, collectedReaderTree(t))
			if err := site.read(t, cd); err != nil {
				t.Fatalf("%s: read of an unchanged entry failed: %v", site.name, err)
			}
		})
	}
}

func TestCollectedReaderSites_ChangedAfterCollectFails(t *testing.T) {
	for _, site := range collectedReaderSites {
		for change, mutate := range map[string]func(t *testing.T, p string){
			"size": func(t *testing.T, p string) {
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, append(data, "x: 1\n"...), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			"same size": func(t *testing.T, p string) {
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				data[0] ^= 0x20
				if err := os.WriteFile(p, data, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			"removed": func(t *testing.T, p string) {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			},
		} {
			t.Run(site.name+"/"+change, func(t *testing.T) {
				dir := collectedReaderTree(t)
				cd := collectTestDir(t, dir)
				mutate(t, filepath.Join(dir, filepath.FromSlash(site.entry)))
				if err := site.read(t, cd); err == nil {
					t.Fatalf("%s: read of %s changed after collection (%s) succeeded", site.name, site.entry, change)
				}
			})
		}
	}
}

func TestCollectedDir_PathNotCollectedReadsAsAbsent(t *testing.T) {
	dir := collectedReaderTree(t)
	cd := collectTestDir(t, dir)
	// Written after collection: present on disk, absent from the collection.
	if err := os.WriteFile(filepath.Join(dir, "later.md"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cd.read("later.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read of a path that was not collected: err %v, want fs.ErrNotExist", err)
	}
	if _, err := cd.fileReader()(context.Background(), "later.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("fileReader of a path that was not collected: err %v, want fs.ErrNotExist", err)
	}
}

func TestCollectedDir_HarnessConfigYAMLAbsentWhenNotCollected(t *testing.T) {
	dir := writeCollectedTestTree(t, map[string]string{"home/.bashrc": "# rc\n"})
	cd := collectTestDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(collectedTestHarnessYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	data, present, err := cd.harnessConfigYAML()
	if err != nil || present || data != nil {
		t.Errorf("harnessConfigYAML of a config.yaml written after collection = (%q, %v, %v), want absent", data, present, err)
	}
}

func TestCollectedDir_SubdirsListsCollectedSubdirectories(t *testing.T) {
	dir := writeCollectedTestTree(t, map[string]string{
		"harness-configs/b/config.yaml":       "harness: claude\n",
		"harness-configs/a/config.yaml":       "harness: claude\n",
		"harness-configs/a/home/.bashrc":      "# rc\n",
		"harness-configs/top.yaml":            "x: 1\n",
		"other/harness-configs/c/config.yaml": "harness: claude\n",
	})
	cd := collectTestDir(t, dir)
	got := strings.Join(cd.subdirs("harness-configs"), ",")
	if got != "a,b" {
		t.Errorf("subdirs = %q, want \"a,b\"", got)
	}
}
