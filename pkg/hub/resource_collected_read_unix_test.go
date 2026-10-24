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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCollectedReadSites_SymlinkSwapRefused replaces a collected file, or
// its parent directory, with a symbolic link after collection. The link
// target has the collected content, so only the regular-file and in-root
// checks can refuse it.
func TestCollectedReadSites_SymlinkSwapRefused(t *testing.T) {
	const content = "export A=1\n"
	for _, site := range collectedReadSites {
		t.Run(site.name+"/entry", func(t *testing.T) {
			dir, root, files := collectForRead(t, map[string]string{"home/.bashrc": content})
			outside := filepath.Join(t.TempDir(), "bashrc")
			if err := os.WriteFile(outside, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(dir, "home", ".bashrc")
			if err := os.Remove(entry); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, entry); err != nil {
				t.Fatal(err)
			}
			stor := newCollectedReadStorage(t)
			err := site.upload(context.Background(), stor, root, files)
			if err == nil {
				t.Fatalf("%s: upload through a link swapped in after collection succeeded", site.name)
			}
			if !strings.Contains(err.Error(), "no longer a regular file") {
				t.Errorf("%s: error %q does not say the entry is no longer a regular file", site.name, err)
			}
			assertNotStored(t, stor, site, collectedEntry(t, files, "home/.bashrc"))
		})

		t.Run(site.name+"/in-root entry", func(t *testing.T) {
			// The link target is written after collection, so no collected
			// file shares the entry's hash and the site has to read the
			// entry itself (uploadDirBlobs reads one file per hash).
			dir, root, files := collectForRead(t, map[string]string{"home/.bashrc": content, "other": "other\n"})
			if err := os.WriteFile(filepath.Join(dir, "copy"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(dir, "home", ".bashrc")
			if err := os.Remove(entry); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../copy", entry); err != nil {
				t.Fatal(err)
			}
			stor := newCollectedReadStorage(t)
			err := site.upload(context.Background(), stor, root, files)
			if err == nil {
				t.Fatalf("%s: upload through an in-root link swapped in after collection succeeded", site.name)
			}
			if !strings.Contains(err.Error(), "no longer a regular file") {
				t.Errorf("%s: error %q does not say the entry is no longer a regular file", site.name, err)
			}
			assertNotStored(t, stor, site, collectedEntry(t, files, "home/.bashrc"))
		})

		t.Run(site.name+"/parent", func(t *testing.T) {
			dir, root, files := collectForRead(t, map[string]string{"home/.bashrc": content})
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, ".bashrc"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(dir, "home")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, "home")); err != nil {
				t.Fatal(err)
			}
			stor := newCollectedReadStorage(t)
			if err := site.upload(context.Background(), stor, root, files); err == nil {
				t.Fatalf("%s: upload through a parent link leaving the collected dir succeeded", site.name)
			}
			assertNotStored(t, stor, site, collectedEntry(t, files, "home/.bashrc"))
		})
	}
}
