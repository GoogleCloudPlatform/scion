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

package projectfs

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// removeFixture is a local scratchpad with .attachments/<channel>/<entry>
// files, and a directory outside it that must never change.
type removeFixture struct {
	dir     string // the scratchpad
	outside string // a tree outside the scratchpad
	res     Resolved
}

func newRemoveFixture(t *testing.T) *removeFixture {
	t.Helper()
	base := t.TempDir()
	f := &removeFixture{dir: filepath.Join(base, "scratchpad"), outside: filepath.Join(base, "outside")}
	f.res = Resolved{Backend: BackendLocal, HostPath: f.dir, Kind: KindSharedDir, Name: ScratchpadName}
	mustWrite(t, filepath.Join(f.outside, "keep.txt"))
	mustWrite(t, filepath.Join(f.outside, "id-1", "keep.txt"))
	return f
}

func mustWrite(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *removeFixture) entry(t *testing.T, channel, id string) string {
	t.Helper()
	p := filepath.Join(f.dir, AttachmentsDir, channel, id)
	mustWrite(t, filepath.Join(p, "file.txt"))
	return p
}

func (f *removeFixture) requireOutsideIntact(t *testing.T) {
	t.Helper()
	for _, p := range []string{filepath.Join(f.outside, "keep.txt"), filepath.Join(f.outside, "id-1", "keep.txt")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("outside file %s changed: %v", p, err)
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestRemoveAttachmentEntry_RemovesOnlyThatEntry(t *testing.T) {
	f := newRemoveFixture(t)
	gone := f.entry(t, AttachmentChannelWebChat, "id-1")
	kept := f.entry(t, AttachmentChannelWebChat, "id-2")
	other := f.entry(t, AttachmentChannelTelegram, "id-1")

	removed, err := RemoveAttachmentEntry(f.res, AttachmentChannelWebChat, "id-1")
	if err != nil || !removed {
		t.Fatalf("RemoveAttachmentEntry = %v, %v; want true, nil", removed, err)
	}
	if exists(gone) {
		t.Fatal("the entry is still there")
	}
	if !exists(kept) || !exists(other) {
		t.Fatal("another entry was removed")
	}

	// Idempotent: a second call removes nothing and is not an error.
	removed, err = RemoveAttachmentEntry(f.res, AttachmentChannelWebChat, "id-1")
	if err != nil || removed {
		t.Fatalf("second RemoveAttachmentEntry = %v, %v; want false, nil", removed, err)
	}
	f.requireOutsideIntact(t)
}

func TestRemoveAttachmentChannel_RemovesOnlyThatChannel(t *testing.T) {
	f := newRemoveFixture(t)
	f.entry(t, AttachmentChannelGChat, "spaces_X")
	kept := f.entry(t, AttachmentChannelWebChat, "id-1")

	removed, err := RemoveAttachmentChannel(f.res, AttachmentChannelGChat)
	if err != nil || !removed {
		t.Fatalf("RemoveAttachmentChannel = %v, %v; want true, nil", removed, err)
	}
	if exists(filepath.Join(f.dir, AttachmentsDir, AttachmentChannelGChat)) {
		t.Fatal("the channel is still there")
	}
	if !exists(kept) {
		t.Fatal("another channel was removed")
	}
	removed, err = RemoveAttachmentChannel(f.res, AttachmentChannelGChat)
	if err != nil || removed {
		t.Fatalf("second RemoveAttachmentChannel = %v, %v; want false, nil", removed, err)
	}
	f.requireOutsideIntact(t)
}

func TestRemoveAttachment_MissingTreesAreNotErrors(t *testing.T) {
	// No scratchpad at all.
	missing := Resolved{Backend: BackendLocal, HostPath: filepath.Join(t.TempDir(), "gone")}
	if removed, err := RemoveAttachmentEntry(missing, AttachmentChannelWebChat, "id-1"); err != nil || removed {
		t.Fatalf("missing scratchpad: %v, %v", removed, err)
	}
	if removed, err := RemoveAttachmentChannel(missing, AttachmentChannelWebChat); err != nil || removed {
		t.Fatalf("missing scratchpad: %v, %v", removed, err)
	}
	if names, err := ListAttachmentEntries(missing, AttachmentChannelWebChat); err != nil || len(names) != 0 {
		t.Fatalf("missing scratchpad: %v, %v", names, err)
	}
	// A scratchpad without .attachments, then without the channel.
	f := newRemoveFixture(t)
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveAttachmentEntry(f.res, AttachmentChannelWebChat, "id-1"); err != nil || removed {
		t.Fatalf("no .attachments: %v, %v", removed, err)
	}
	f.entry(t, AttachmentChannelTelegram, "tg_1")
	if removed, err := RemoveAttachmentChannel(f.res, AttachmentChannelWebChat); err != nil || removed {
		t.Fatalf("no channel: %v, %v", removed, err)
	}
	if names, err := ListAttachmentEntries(f.res, AttachmentChannelWebChat); err != nil || len(names) != 0 {
		t.Fatalf("no channel: %v, %v", names, err)
	}
}

func TestRemoveAttachment_SymlinkedAttachmentsDirRefused(t *testing.T) {
	for _, target := range []string{".", "OUTSIDE"} {
		t.Run(target, func(t *testing.T) {
			f := newRemoveFixture(t)
			// Other files in the scratchpad that a followed link would reach.
			mustWrite(t, filepath.Join(f.dir, AttachmentChannelWebChat, "id-1", "keep.txt"))
			link := target
			if target == "OUTSIDE" {
				link = f.outside
			}
			if err := os.Symlink(link, filepath.Join(f.dir, AttachmentsDir)); err != nil {
				t.Fatal(err)
			}
			if _, err := RemoveAttachmentEntry(f.res, AttachmentChannelWebChat, "id-1"); !errors.Is(err, ErrNotRealDir) {
				t.Fatalf("RemoveAttachmentEntry error = %v, want ErrNotRealDir", err)
			}
			if _, err := RemoveAttachmentChannel(f.res, AttachmentChannelWebChat); !errors.Is(err, ErrNotRealDir) {
				t.Fatalf("RemoveAttachmentChannel error = %v, want ErrNotRealDir", err)
			}
			if _, err := ListAttachmentEntries(f.res, AttachmentChannelWebChat); !errors.Is(err, ErrNotRealDir) {
				t.Fatalf("ListAttachmentEntries error = %v, want ErrNotRealDir", err)
			}
			if !exists(filepath.Join(f.dir, AttachmentChannelWebChat, "id-1", "keep.txt")) {
				t.Fatal("a file reached through the link was removed")
			}
			f.requireOutsideIntact(t)
		})
	}
}

func TestRemoveAttachment_SymlinkedChannelDirRefused(t *testing.T) {
	f := newRemoveFixture(t)
	if err := os.MkdirAll(filepath.Join(f.dir, AttachmentsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.outside, filepath.Join(f.dir, AttachmentsDir, AttachmentChannelWebChat)); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveAttachmentEntry(f.res, AttachmentChannelWebChat, "id-1"); !errors.Is(err, ErrNotRealDir) {
		t.Fatalf("RemoveAttachmentEntry error = %v, want ErrNotRealDir", err)
	}
	if _, err := RemoveAttachmentChannel(f.res, AttachmentChannelWebChat); !errors.Is(err, ErrNotRealDir) {
		t.Fatalf("RemoveAttachmentChannel error = %v, want ErrNotRealDir", err)
	}
	if !exists(filepath.Join(f.dir, AttachmentsDir, AttachmentChannelWebChat)) {
		t.Fatal("the link itself is left for the projection sweep")
	}
	f.requireOutsideIntact(t)
}

func TestRemoveAttachmentEntry_SymlinkedEntryRemovedAsLink(t *testing.T) {
	f := newRemoveFixture(t)
	ch := filepath.Join(f.dir, AttachmentsDir, AttachmentChannelWebChat)
	if err := os.MkdirAll(ch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.outside, "id-1"), filepath.Join(ch, "id-1")); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveAttachmentEntry(f.res, AttachmentChannelWebChat, "id-1")
	if err != nil || !removed {
		t.Fatalf("RemoveAttachmentEntry = %v, %v; want true, nil", removed, err)
	}
	if exists(filepath.Join(ch, "id-1")) {
		t.Fatal("the link is still there")
	}
	f.requireOutsideIntact(t)
}

func TestRemoveAttachment_InvalidComponentsRejected(t *testing.T) {
	f := newRemoveFixture(t)
	f.entry(t, AttachmentChannelWebChat, "id-1")
	for _, bad := range []string{"", ".", "..", "a/b", "../outside", "a\\b", "a\x00b"} {
		if _, err := RemoveAttachmentChannel(f.res, bad); err == nil {
			t.Errorf("RemoveAttachmentChannel(%q) accepted", bad)
		}
		if _, err := RemoveAttachmentEntry(f.res, bad, "id-1"); err == nil {
			t.Errorf("RemoveAttachmentEntry(channel %q) accepted", bad)
		}
		if _, err := RemoveAttachmentEntry(f.res, AttachmentChannelWebChat, bad); err == nil {
			t.Errorf("RemoveAttachmentEntry(entry %q) accepted", bad)
		}
		if _, err := ListAttachmentEntries(f.res, bad); err == nil {
			t.Errorf("ListAttachmentEntries(%q) accepted", bad)
		}
	}
	if !exists(filepath.Join(f.dir, AttachmentsDir, AttachmentChannelWebChat, "id-1")) {
		t.Fatal("a rejected call removed something")
	}
	f.requireOutsideIntact(t)
}

func TestListAttachmentEntries(t *testing.T) {
	f := newRemoveFixture(t)
	f.entry(t, AttachmentChannelWebChat, "id-2")
	f.entry(t, AttachmentChannelWebChat, "id-1")
	f.entry(t, AttachmentChannelTelegram, "tg_1")
	names, err := ListAttachmentEntries(f.res, AttachmentChannelWebChat)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "id-1" || names[1] != "id-2" {
		t.Fatalf("names = %v", names)
	}
}
