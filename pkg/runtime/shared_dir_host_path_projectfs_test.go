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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// ResolveSharedDirHostPath carries the layout authority's answer, so chat
// plugins write below the shared dir through Resolved.Open
// (ptone/scion#5831).
func TestResolveSharedDirHostPath_CarriesResolved(t *testing.T) {
	home := t.TempDir()
	local, err := ResolveSharedDirHostPath(nil, home, "proj", testSharedDirProjectID, "scratchpad")
	if err != nil {
		t.Fatal(err)
	}
	if local.Resolved.Backend != projectfs.BackendLocal || local.Resolved.HostPath != local.Path ||
		local.Resolved.Target != "/scion-volumes/scratchpad" {
		t.Fatalf("local Resolved = %+v, path %q", local.Resolved, local.Path)
	}

	mountRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755); err != nil {
		t.Fatal(err)
	}
	nfs, err := ResolveSharedDirHostPath(nfsSharedDirSettings(mountRoot), home, "proj", testSharedDirProjectID, "scratchpad")
	if err != nil {
		t.Fatal(err)
	}
	wantRel := filepath.Join("projects", testSharedDirProjectID, "shared-dirs", "scratchpad")
	if nfs.Resolved.Backend != projectfs.BackendNFS || nfs.Resolved.RelPath != wantRel {
		t.Fatalf("nfs Resolved = %+v, want rel %q", nfs.Resolved, wantRel)
	}
	root, err := nfs.Resolved.Open(false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = root.Close()
}
