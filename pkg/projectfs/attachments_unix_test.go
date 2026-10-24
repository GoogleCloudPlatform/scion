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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestWriteAttachment_Local(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scratchpad")
	m := SharedDirMount(api.SharedDir{Name: ScratchpadName}, "")
	res := Resolved{Backend: BackendLocal, HostPath: dir, Target: m.Target, Kind: m.Kind, Name: m.Name}

	got, err := WriteAttachment(res, true, m, AttachmentChannelDiscord, "discord_1", "a.png", strings.NewReader("png"))
	if err != nil {
		t.Fatalf("WriteAttachment: %v", err)
	}
	if want := "/scion-volumes/scratchpad/.attachments/_discord/discord_1/a.png"; got != want {
		t.Fatalf("container path = %q, want %q", got, want)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".attachments", "_discord", "discord_1", "a.png"))
	if err != nil || string(b) != "png" {
		t.Fatalf("written file = %q, %v", b, err)
	}
	// O_EXCL: the same name again is refused.
	if _, err := WriteAttachment(res, true, m, AttachmentChannelDiscord, "discord_1", "a.png", strings.NewReader("x")); err == nil {
		t.Fatal("a second write to the same name must fail")
	}
	// Invalid components are refused before anything is opened.
	if _, err := WriteAttachment(res, true, m, AttachmentChannelDiscord, "../x", "a.png", strings.NewReader("x")); err == nil {
		t.Fatal("an id with a separator must be refused")
	}
}

func TestWriteAttachment_NFS(t *testing.T) {
	base := t.TempDir()
	l := Layout{Backend: BackendNFS, HostBase: base, SubPathRoot: "projects"}
	m := SharedDirMount(api.SharedDir{Name: ScratchpadName}, "")
	pid := "11111111-2222-3333-4444-555555555555"
	res, err := l.Resolve(Project{ID: pid}, "", m)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := WriteAttachment(res, true, m, AttachmentChannelGChat, "conv-1", "b.txt", strings.NewReader("nfs"))
	if err != nil {
		t.Fatalf("WriteAttachment: %v", err)
	}
	if want := "/scion-volumes/scratchpad/.attachments/_gchat/conv-1/b.txt"; got != want {
		t.Fatalf("container path = %q, want %q", got, want)
	}
	b, err := os.ReadFile(filepath.Join(base, "projects", pid, "shared-dirs", "scratchpad", ".attachments", "_gchat", "conv-1", "b.txt"))
	if err != nil || string(b) != "nfs" {
		t.Fatalf("written file = %q, %v", b, err)
	}
}

func TestWriteAttachment_RefusesSymlinkedAttachmentsDir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, AttachmentsDir)); err != nil {
		t.Fatal(err)
	}
	m := SharedDirMount(api.SharedDir{Name: ScratchpadName}, "")
	res := Resolved{Backend: BackendLocal, HostPath: dir, Target: m.Target, Kind: m.Kind, Name: m.Name}
	if _, err := WriteAttachment(res, false, m, AttachmentChannelTelegram, "tg_1", "a.txt", strings.NewReader("x")); err == nil {
		t.Fatal("writing through a symlinked .attachments must fail")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("nothing may be written outside the shared dir, found %d entries", len(entries))
	}
}
