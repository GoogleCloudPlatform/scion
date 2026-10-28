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
		"project storage share is not visible on this broker (/mnt/nfs/ws1): make the share available in this broker's container at /mnt/nfs/ws1 (the same server and export as server.project_storage.nfs.shares[0])"; got != want {
		t.Errorf("platform-mounting broker:\n got %q\nwant %q", got, want)
	}
	if got, want := ProjectShareUnavailableText("/mnt/nfs/ws1", false),
		"project storage share is not mounted on this broker (/mnt/nfs/ws1): mount it, or set server.project_storage.nfs.auto_mount: true"; got != want {
		t.Errorf("host-mounting broker:\n got %q\nwant %q", got, want)
	}
	// A platform-mounting broker is never told to set auto_mount: it has no effect there.
	if strings.Contains(ProjectShareUnavailableText("/mnt/nfs/ws1", true), "auto_mount") {
		t.Error("the platform-mounting text must not advise auto_mount")
	}
	// The client-safe form carries the field name, never the path.
	for _, pm := range []bool{true, false} {
		got := ProjectShareUnavailableText(ProjectShareMountPathField, pm)
		if strings.Contains(got, "/mnt/") || !strings.Contains(got, ProjectShareMountPathField) {
			t.Errorf("client-safe form (platformMounts=%v) = %q", pm, got)
		}
	}
}
