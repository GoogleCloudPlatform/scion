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

package runtimebroker

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// A root broker gives the shared workspace leaf it creates the Layout's
// nfs owner, and keeps the provisioning state directory (locks, sentinel)
// at the zero owner: it is never agent-writable.
func TestEnsureWorkspaceAndStateLeaves_StateDirKeepsZeroOwner(t *testing.T) {
	base := t.TempDir()
	type call struct {
		ino      uint64
		uid, gid int
	}
	var calls []call
	t.Cleanup(shareddirs.SetOwnerHooksForTest(func() int { return 0 }, func(fd, uid, gid int) error {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		calls = append(calls, call{ino: uint64(st.Ino), uid: uid, gid: gid})
		return nil
	}))
	project := filepath.Join("projects", "4f1d2c3b-5a69-4e70-8b91-a2b3c4d5e6f7")
	wsRel := filepath.Join(project, "workspace")
	stateRel := filepath.Join(project, provision.ProvisionStateDirName)

	if err := ensureWorkspaceAndStateLeaves(base, wsRel, stateRel, shareddirs.Owner{UID: 1000, GID: 1001}); err != nil {
		t.Fatal(err)
	}
	var ws unix.Stat_t
	if err := unix.Lstat(filepath.Join(base, wsRel), &ws); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != (call{ino: uint64(ws.Ino), uid: 1000, gid: 1001}) {
		t.Errorf("chown calls = %+v, want only the workspace leaf with 1000:1001", calls)
	}
	if _, err := os.Stat(filepath.Join(base, stateRel)); err != nil {
		t.Errorf("the provisioning state directory was not created: %v", err)
	}
}
