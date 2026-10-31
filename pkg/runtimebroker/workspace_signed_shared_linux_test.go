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

//go:build linux

package runtimebroker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"golang.org/x/sys/unix"
)

// caseBFixture runs the signed bootstrap of the shared Layout fixture's
// project workspace leaf.
type caseBFixture struct {
	*sharedRootFixture
	h        *fakeSignedHub
	ws       string
	sentinel string
	req      CreateAgentRequest
}

func newCaseBFixture(t *testing.T, objects map[string]string) *caseBFixture {
	t.Helper()
	f := &caseBFixture{sharedRootFixture: newSharedRootFixture(t, true)}
	f.h = newFakeSignedHub(t, objects)
	f.srv.signedDownload = signedDownloadSettings{fetchPage: f.h.fetch, sleep: noSleep}
	forbidGCSDownload(t, f.srv)
	base, err := filepath.EvalSymlinks(f.layout.HostBase)
	if err != nil {
		t.Fatal(err)
	}
	f.ws = filepath.Join(base, "projects", sharedRootProjectID, "workspace")
	f.sentinel = filepath.Join(base, "projects", sharedRootProjectID, "provision", ".scion-provisioned")
	f.req = CreateAgentRequest{ID: "agent-uuid-a1", Name: "a1", ProjectID: sharedRootProjectID, ProjectSlug: "web",
		ProjectPath: f.root, WorkspaceStoragePath: "workspaces/x", WorkspaceDownloadMode: api.WorkspaceDownloadModeSignedURLs,
		workspaceAdmissionChecked: true, workspaceAbsentAtAdmission: true}
	return f
}

func (f *caseBFixture) run() (bool, error) {
	var out bootstrapOutcome
	_, _, _, err := f.srv.downloadWorkspaceFromGCS(context.WithValue(context.Background(), bootstrapOutcomeKey{}, &out), f.req, api.StartOptions{})
	return out.skipped, err
}

func (f *caseBFixture) sentinelPresent() bool {
	_, err := os.Stat(f.sentinel)
	return err == nil
}

func TestSignedCaseB_CommitThenSentinel(t *testing.T) {
	f := newCaseBFixture(t, map[string]string{"a.txt": "A", "d/b.txt": "B"})
	var sentinelAtRename []bool
	testHookSignedBootstrap = func(step string) error {
		if step == "before_rename" || step == "after_rename" {
			sentinelAtRename = append(sentinelAtRename, f.sentinelPresent())
		}
		return nil
	}
	t.Cleanup(func() { testHookSignedBootstrap = nil })
	if skipped, err := f.run(); err != nil || skipped {
		t.Fatalf("run = (skipped %v, err %v)", skipped, err)
	}
	wantTree(t, f.ws, map[string]string{"a.txt": "A", "d/b.txt": "B"})
	if !f.sentinelPresent() {
		t.Fatal("the sentinel is written after the commit")
	}
	if len(sentinelAtRename) != 2 || sentinelAtRename[0] || sentinelAtRename[1] {
		t.Fatalf("sentinel present at (before, after) rename = %v; it must come last", sentinelAtRename)
	}
	if s := stagesOf(t, f.ws); len(s) != 0 {
		t.Fatalf("stages left: %v", s)
	}
}

func TestSignedCaseB_CrashPoints(t *testing.T) {
	objs := map[string]string{"a.txt": "A"}
	t.Run("before the rename: empty leaf, stale stage removed, applied", func(t *testing.T) {
		f := newCaseBFixture(t, objs)
		crashAt(t, "before_rename")
		if _, err := f.run(); !errors.Is(err, errSignedBootstrapCrash) {
			t.Fatalf("err = %v", err)
		}
		if entries, _ := os.ReadDir(f.ws); len(entries) != 0 || f.sentinelPresent() {
			t.Fatalf("leaf %v, sentinel %v; want an empty leaf and no sentinel", entries, f.sentinelPresent())
		}
		if len(stagesOf(t, f.ws)) != 1 {
			t.Fatal("the crash leaves one stale stage")
		}
		if skipped, err := f.run(); err != nil || skipped {
			t.Fatalf("next = (skipped %v, err %v)", skipped, err)
		}
		wantTree(t, f.ws, objs)
		if s := stagesOf(t, f.ws); len(s) != 0 {
			t.Fatalf("stages left: %v", s)
		}
	})
	t.Run("after the rename: complete tree without sentinel is in use", func(t *testing.T) {
		f := newCaseBFixture(t, objs)
		crashAt(t, "after_rename")
		if _, err := f.run(); !errors.Is(err, errSignedBootstrapCrash) {
			t.Fatalf("err = %v", err)
		}
		wantTree(t, f.ws, objs)
		if skipped, err := f.run(); err != nil || !skipped {
			t.Fatalf("next = (skipped %v, err %v); a complete tree is in use", skipped, err)
		}
	})
}

// TestSignedCaseB_LateContentSkips: a leaf that gains content before the
// rename is skipped, and the stage is removed.
func TestSignedCaseB_LateContentSkips(t *testing.T) {
	f := newCaseBFixture(t, map[string]string{"a.txt": "A"})
	testHookSignedBootstrap = func(step string) error {
		if step == "before_rename" {
			return os.WriteFile(filepath.Join(f.ws, "late.txt"), []byte("late"), 0o644)
		}
		return nil
	}
	t.Cleanup(func() { testHookSignedBootstrap = nil })
	if skipped, err := f.run(); err != nil || !skipped {
		t.Fatalf("run = (skipped %v, err %v), want a skip", skipped, err)
	}
	wantTree(t, f.ws, map[string]string{"late.txt": "late"})
	if s := stagesOf(t, f.ws); len(s) != 0 {
		t.Fatalf("stages left: %v", s)
	}
}

// TestSignedCaseB_ArtifactsOnlyLeafIsBusy: a leaf holding only provisioning
// artifacts (a legacy in-leaf lock and a satellite) fails closed with
// leaf_busy; the artifacts are untouched, nothing is downloaded and no
// stage is left.
func TestSignedCaseB_ArtifactsOnlyLeafIsBusy(t *testing.T) {
	f := newCaseBFixture(t, map[string]string{"a.txt": "A"})
	if err := os.MkdirAll(f.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(f.ws, ".scion-provision.lock")
	satellite := filepath.Join(f.ws, ".scion-provision.lock.holder")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(satellite, []byte("pid"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := f.run()
	var ce *signedCommitError
	if !errors.As(err, &ce) || ce.Class != signedCommitLeafBusy {
		t.Fatalf("err = %v, want leaf_busy", err)
	}
	if op := signedBootstrapOp(err); op != opApplySharedWorkspaceBusy {
		t.Fatalf("op = %q", op)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("the lock artifact must be untouched")
	}
	if b, err := os.ReadFile(satellite); err != nil || string(b) != "pid" {
		t.Fatal("the satellite must be untouched")
	}
	if f.h.pageCalls != 0 || f.sentinelPresent() {
		t.Fatalf("page calls %d, sentinel %v", f.h.pageCalls, f.sentinelPresent())
	}
	if s := stagesOf(t, f.ws); len(s) != 0 {
		t.Fatalf("stages left: %v", s)
	}
}

// TestSignedCaseB_RecoveryOnlyUnderProvisioningLocks: a stage another
// broker writes while it holds the provisioning locks is not removed by
// this broker's bootstrap, which waits for the locks; once they are
// released the stage is stale and is removed.
func TestSignedCaseB_RecoveryOnlyUnderProvisioningLocks(t *testing.T) {
	f := newCaseBFixture(t, map[string]string{"a.txt": "A"})
	base, _ := filepath.EvalSymlinks(f.layout.HostBase)
	rel := filepath.Join("projects", sharedRootProjectID, "workspace")
	owner := shareddirs.Owner{UID: f.layout.UID, GID: f.layout.GID}
	if err := ensureWorkspaceAndStateLeaves(base, rel, filepath.Join(filepath.Dir(rel), provision.ProvisionStateDirName), owner); err != nil {
		t.Fatal(err)
	}
	otherStage, _ := newBootstrapStageName("workspace")
	otherPath := filepath.Join(filepath.Dir(f.ws), otherStage)
	holding := make(chan struct{})
	release := make(chan struct{})
	otherDone := make(chan error, 1)
	go func() {
		otherDone <- provision.WithWorkspaceProvisionLocks(context.Background(), f.ws, func(context.Context) error {
			if err := os.Mkdir(otherPath, 0o755); err != nil {
				return err
			}
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	ours := make(chan error, 1)
	go func() {
		_, err := f.run()
		ours <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatal("another broker's live stage must not be removed")
	}
	if f.h.pageCalls != 0 {
		t.Fatal("the bootstrap must wait for the provisioning locks")
	}
	close(release)
	if err := <-otherDone; err != nil {
		t.Fatal(err)
	}
	if err := <-ours; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(otherPath); !os.IsNotExist(err) {
		t.Fatal("once the locks are free the stale stage is removed")
	}
	wantTree(t, f.ws, map[string]string{"a.txt": "A"})
}

// attrs are the attributes the shared Layout's leaf contract sets.
type attrs struct {
	mode       os.FileMode
	uid, gid   uint32
	aclAccess  []byte
	aclDefault []byte
}

func readAttrs(t *testing.T, p string) attrs {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return attrs{mode: fi.Mode(), uid: st.Uid, gid: st.Gid, aclAccess: getxattr(p, "system.posix_acl_access"), aclDefault: getxattr(p, "system.posix_acl_default")}
}

// getxattr returns the attribute's value, or nil when it is absent or the
// filesystem has no such attributes.
func getxattr(p, name string) []byte {
	buf := make([]byte, 4096)
	n, err := unix.Lgetxattr(p, name, buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

func sameAttrs(a, b attrs) bool {
	return a.mode == b.mode && a.uid == b.uid && a.gid == b.gid && bytes.Equal(a.aclAccess, b.aclAccess) && bytes.Equal(a.aclDefault, b.aclDefault)
}

// TestSignedCaseB_CommittedAttributesMatchDirectCreation: a file and a
// nested directory committed through the stage have the owner, mode and ACL
// entries of ones created directly in the leaf the way the bucket download
// creates them (0666 files, 0777 directories), and the leaf keeps its own.
func TestSignedCaseB_CommittedAttributesMatchDirectCreation(t *testing.T) {
	// Reference: a leaf created by the same helper, written directly.
	ref := newCaseBFixture(t, nil)
	base, _ := filepath.EvalSymlinks(ref.layout.HostBase)
	rel := filepath.Join("projects", sharedRootProjectID, "workspace")
	owner := shareddirs.Owner{UID: ref.layout.UID, GID: ref.layout.GID}
	if err := ensureWorkspaceAndStateLeaves(base, rel, filepath.Join(filepath.Dir(rel), provision.ProvisionStateDirName), owner); err != nil {
		t.Fatal(err)
	}
	leafBefore := readAttrs(t, ref.ws)
	if err := os.Mkdir(filepath.Join(ref.ws, "d"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ref.ws, "d", "f.txt"), []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}

	f := newCaseBFixture(t, map[string]string{"d/f.txt": "x"})
	if _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := readAttrs(t, f.ws); !sameAttrs(got, leafBefore) {
		t.Errorf("leaf attributes %+v, want %+v", got, leafBefore)
	}
	for _, p := range []string{"d", "d/f.txt"} {
		want := readAttrs(t, filepath.Join(ref.ws, p))
		got := readAttrs(t, filepath.Join(f.ws, p))
		if !sameAttrs(got, want) {
			t.Errorf("%s: committed attributes %+v, direct creation %+v", p, got, want)
		}
	}
}
