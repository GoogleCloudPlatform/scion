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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// ownerCalls makes the shared-tree walk see a root creator and records the
// uid:gid of every fchown it would make.
func ownerCalls(t *testing.T) *[][2]int {
	t.Helper()
	calls := &[][2]int{}
	t.Cleanup(shareddirs.SetOwnerHooksForTest(func() int { return 0 }, func(fd, uid, gid int) error {
		*calls = append(*calls, [2]int{uid, gid})
		return nil
	}))
	return calls
}

const ownerTestProjectID = "4f1d2c3b-5a69-4e70-8b91-a2b3c4d5e6f7"

// A root creator with the Layout's nfs owner set creates a project's
// configuration directory that is NOT nfs-owned: it holds the project's
// identity and only Scion components write it.
func TestEnsureProjectConfigDir_RootCreatorKeepsZeroOwner(t *testing.T) {
	l := Layout{Backend: BackendNFS, HostBase: t.TempDir(), UID: 1000, GID: 1001}
	calls := ownerCalls(t)
	dir, err := l.EnsureProjectConfigDir(ownerTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != ProjectConfigDirName {
		t.Fatalf("dir = %q", dir)
	}
	if len(*calls) != 0 {
		t.Errorf("the project configuration directory was chowned: %v", *calls)
	}
}

// A shared-dir leaf created through Open(true) by a root creator gets the
// Layout's nfs owner, and only the leaf does.
func TestOpenCreate_RootCreatorOwnsSharedDirLeaf(t *testing.T) {
	l := Layout{Backend: BackendNFS, HostBase: t.TempDir(), UID: 1000, GID: 1001}
	calls := ownerCalls(t)
	r, err := l.Resolve(Project{ID: ownerTestProjectID}, "", SharedDirMount(api.SharedDir{Name: "scratchpad"}, "/workspace"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := r.Open(true)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	if want := [][2]int{{1000, 1001}}; len(*calls) != 1 || (*calls)[0] != want[0] {
		t.Errorf("chown calls = %v, want %v (the leaf only)", *calls, want)
	}
}

// Open(true) of the project root or the project configuration directory by
// a root creator chowns nothing: those are trust directories, never
// agent-writable.
func TestOpenCreate_RootCreatorTrustDirsNotChowned(t *testing.T) {
	l := Layout{Backend: BackendNFS, HostBase: t.TempDir(), UID: 1000, GID: 1001}
	calls := ownerCalls(t)
	pr, err := l.ProjectRoot(ownerTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := l.ProjectConfig(ownerTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Resolved{pr, cfg} {
		root, err := r.Open(true)
		if err != nil {
			t.Fatal(err)
		}
		_ = root.Close()
	}
	if len(*calls) != 0 {
		t.Errorf("a trust directory was chowned: %v", *calls)
	}
}
