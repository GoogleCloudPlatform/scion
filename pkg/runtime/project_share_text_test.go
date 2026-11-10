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

package runtime

import (
	"strings"
	"testing"
)

func TestProjectShareUnavailableText(t *testing.T) {
	if got, want := ProjectShareUnavailableText("/mnt/nfs/ws1", true),
		"project storage share is not visible on this broker at /mnt/nfs/ws1: make the share (the same storage the hub uses) available in this broker's container at that path"; got != want {
		t.Errorf("platform-mounting broker:\n got %q\nwant %q", got, want)
	}
	if got, want := ProjectShareUnavailableText("/mnt/nfs/ws1", false),
		"project storage share is not mounted on this broker at /mnt/nfs/ws1: mount it there, or set server.project_storage.nfs.auto_mount: true"; got != want {
		t.Errorf("host-mounting broker:\n got %q\nwant %q", got, want)
	}
	if got, want := ProjectShareMountFailedText("/mnt/nfs/ws1"),
		"project storage share could not be mounted on this broker at /mnt/nfs/ws1: check that the NFS server and export (server.project_storage.nfs.shares[0]) are reachable from the broker; the broker log has the details"; got != want {
		t.Errorf("mount failure:\n got %q\nwant %q", got, want)
	}
	if got, want := ProjectShareInlineUnavailableText("/mnt/nfs/ws1"),
		"project storage share is not visible on this broker at /mnt/nfs/ws1: make the share (the same storage the hub uses) available in this broker's container at that path, writable by the broker; without nfs.shares[0].pv_name, pods mount each directory by its server path, so the broker creates the directories before the pod starts"; got != want {
		t.Errorf("inline share:\n got %q\nwant %q", got, want)
	}
	if got, want := ProjectShareMountFlagsText("/mnt/nfs/ws1"),
		"project storage share at /mnt/nfs/ws1: nosuid,nodev could not be confirmed (a mount there or below it lacks them, or the mount table could not be read): remount the share with nosuid,nodev; the broker log has the details"; got != want {
		t.Errorf("mount flags:\n got %q\nwant %q", got, want)
	}
	// A platform-mounting broker is never told to set auto_mount: it has no effect there.
	// Nor is a broker whose auto_mount is already on and failed.
	// Nor is a broker refused over a share without nosuid,nodev: the fix is
	// a remount with those options.
	for _, s := range []string{ProjectShareUnavailableText("/mnt/nfs/ws1", true), ProjectShareInlineUnavailableText("/mnt/nfs/ws1"), ProjectShareMountFailedText("/mnt/nfs/ws1"), ProjectShareMountFlagsText("/mnt/nfs/ws1")} {
		if strings.Contains(s, "auto_mount") {
			t.Errorf("text must not advise auto_mount: %q", s)
		}
	}
	// The client-safe forms carry the neutral mount point name, never a path,
	// and are exactly the client texts.
	texts := ProjectShareClientTexts()
	if len(texts) != 5 {
		t.Fatalf("ProjectShareClientTexts = %d texts, want 5", len(texts))
	}
	for _, got := range texts {
		if strings.Contains(got, "/mnt/") || !strings.Contains(got, ProjectShareMountPathField) {
			t.Errorf("client text = %q", got)
		}
	}
}
