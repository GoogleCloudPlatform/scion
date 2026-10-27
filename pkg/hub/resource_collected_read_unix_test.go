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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCollectedReadSites_ReplacedByLinkAfterCollectFails replaces a
// collected file, or its parent directory, with a symbolic link after
// collection. The link target has the collected content, so only the
// regular-file and in-root checks can refuse it.
func TestCollectedReadSites_ReplacedByLinkAfterCollectFails(t *testing.T) {
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
				t.Fatalf("%s: upload of an entry replaced by a link after collection succeeded", site.name)
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
				t.Fatalf("%s: upload of an entry replaced by an in-root link after collection succeeded", site.name)
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

	// uploadDirBlobs writes one blob per content hash and reads one file
	// per hash. When the replaced entry shares its hash with another
	// collected file, the entry is never opened: the upload succeeds and
	// stores exactly the collected bytes. The link target here has other
	// content, so opening it would fail the hash check. The case depends on
	// the order of the collected entries: transfer.CollectFiles returns them
	// sorted by Path, so "copy" comes before "home/.bashrc" and is the file
	// uploadDirBlobs reads for the shared hash.
	t.Run("uploadDirBlobs/replaced entry sharing a collected hash is not opened", func(t *testing.T) {
		site := collectedReadSites[0]
		if site.name != "uploadDirBlobs" {
			t.Fatalf("collectedReadSites[0] is %s, want uploadDirBlobs", site.name)
		}
		dir, root, files := collectForRead(t, map[string]string{"home/.bashrc": content, "copy": content})
		outside := filepath.Join(t.TempDir(), "bashrc")
		if err := os.WriteFile(outside, []byte("other content\n"), 0o644); err != nil {
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
		if err := site.upload(context.Background(), stor, root, files); err != nil {
			t.Fatalf("upload: %v", err)
		}
		fi := collectedEntry(t, files, "home/.bashrc")
		rc, _, err := stor.Download(context.Background(), site.object(fi))
		if err != nil {
			t.Fatalf("download the blob of home/.bashrc: %v", err)
		}
		defer func() { _ = rc.Close() }()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content {
			t.Errorf("stored blob = %q, want the collected content %q", got, content)
		}
	})
}
