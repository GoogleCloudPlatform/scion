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

package projectfs

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestAttachmentRelDir(t *testing.T) {
	got, err := AttachmentRelDir(AttachmentChannelWebChat, "0b9c")
	if err != nil {
		t.Fatalf("AttachmentRelDir: %v", err)
	}
	if want := ".attachments/_webchat/0b9c"; got != want {
		t.Fatalf("AttachmentRelDir = %q, want %q", got, want)
	}
	for _, tc := range []struct{ channel, id string }{
		{"", "id"}, {".", "id"}, {"..", "id"}, {"a/b", "id"}, {`a\b`, "id"}, {"a\x00", "id"},
		{"_webchat", ""}, {"_webchat", ".."}, {"_webchat", "x/y"}, {"_webchat", "."},
	} {
		if got, err := AttachmentRelDir(tc.channel, tc.id); err == nil {
			t.Errorf("AttachmentRelDir(%q, %q) = %q, want an error", tc.channel, tc.id, got)
		}
	}
}

func TestAttachmentContainerPath(t *testing.T) {
	cases := []struct {
		name string
		sd   api.SharedDir
		ws   string
		want string
	}{
		{"scion-volumes", api.SharedDir{Name: "scratchpad"}, "", "/scion-volumes/scratchpad/.attachments/_webchat/id1/a.png"},
		{"in-workspace default", api.SharedDir{Name: "scratchpad", InWorkspace: true}, "", "/workspace/.scion-volumes/scratchpad/.attachments/_webchat/id1/a.png"},
		{"in-workspace worktree", api.SharedDir{Name: "scratchpad", InWorkspace: true}, "/repo/wt", "/repo/wt/.scion-volumes/scratchpad/.attachments/_webchat/id1/a.png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AttachmentContainerPath(SharedDirMount(tc.sd, tc.ws), AttachmentChannelWebChat, "id1", "a.png")
			if err != nil {
				t.Fatalf("AttachmentContainerPath: %v", err)
			}
			if got != tc.want {
				t.Fatalf("AttachmentContainerPath = %q, want %q", got, tc.want)
			}
		})
	}

	m := SharedDirMount(api.SharedDir{Name: "scratchpad"}, "")
	for _, name := range []string{"", ".", "..", "a/b", "../x"} {
		if got, err := AttachmentContainerPath(m, AttachmentChannelWebChat, "id1", name); err == nil {
			t.Errorf("AttachmentContainerPath(name %q) = %q, want an error", name, got)
		}
	}
	if got, err := AttachmentContainerPath(Mount{Name: "x"}, AttachmentChannelWebChat, "id1", "a"); err == nil {
		t.Errorf("AttachmentContainerPath(no target) = %q, want an error", got)
	}
	if got, err := AttachmentContainerPath(Mount{Name: "x", Target: "rel/x"}, AttachmentChannelWebChat, "id1", "a"); err == nil {
		t.Errorf("AttachmentContainerPath(relative target) = %q, want an error", got)
	}
}

func TestSharedDirTargets(t *testing.T) {
	got := SharedDirTargets(ScratchpadName, "")
	want := []string{"/scion-volumes/scratchpad", "/workspace/.scion-volumes/scratchpad"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("SharedDirTargets = %q, want %q", got, want)
	}
}
