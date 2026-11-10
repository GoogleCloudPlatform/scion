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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// Both walks check existing upper directories against the Layout's owner,
// so an existing upper directory with the expected shared layout (setgid,
// the nfs gid, not other-writable) is not warned about on the provisioning
// state directory's walk either. The state directory is still not chowned
// (TestEnsureWorkspaceAndStateLeaves_StateDirKeepsZeroOwner).
func TestEnsureWorkspaceAndStateLeaves_ExpectedLayoutNotWarned(t *testing.T) {
	base := t.TempDir()
	projects := filepath.Join(base, "projects")
	if err := os.Mkdir(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	if unix.Geteuid() == 0 {
		if err := os.Chown(projects, 4243, 4242); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Chmod(projects, 0o2775); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(projects, &st); err != nil {
		t.Fatal(err)
	}
	if st.Uid == 0 || st.Gid == 0 || st.Mode&0o7777 != 0o2775 {
		t.Skip("needs a setgid directory with a non-zero owner and group")
	}
	shareddirs.ResetWalkWarningsForTest()
	t.Cleanup(shareddirs.ResetWalkWarningsForTest)
	h := &recordingHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })

	project := filepath.Join("projects", "4f1d2c3b-5a69-4e70-8b91-a2b3c4d5e6f7")
	owner := shareddirs.Owner{UID: int(st.Uid), GID: int(st.Gid)}
	if err := ensureWorkspaceAndStateLeaves(base, filepath.Join(project, "workspace"), filepath.Join(project, provision.ProvisionStateDirName), owner); err != nil {
		t.Fatal(err)
	}
	for _, r := range h.records {
		if r.Level == slog.LevelWarn && strings.HasPrefix(r.Message, "NFS directory walk:") {
			t.Errorf("unexpected walk warning: %s", r.Message)
		}
	}
}
