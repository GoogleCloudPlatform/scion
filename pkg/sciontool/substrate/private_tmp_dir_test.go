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

package substrate

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestBootstrap_ClearsAndResetsModeOfStalePrivateTmpDirBeforeInit proves
// ensurePrivateTmpDir's create/clear actually runs as part of a real
// /bootstrap request, not just when called directly: a pre-planted file
// left over from an earlier bootstrap of a reused actor, and a bad (0777)
// mode on the directory itself, must both be gone by the time the response
// commits to 200 — the directory must exist at privateRootTmpDirMode
// (0700), owned by this process's own euid, and contain nothing.
func TestBootstrap_ClearsAndResetsModeOfStalePrivateTmpDirBeforeInit(t *testing.T) {
	base := realTempDir(t)
	dir := filepath.Join(base, "run", "scion", "tmp")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale-gitconfig-leftover")
	if err := os.WriteFile(stale, []byte("stale content from a previous bootstrap"), 0o644); err != nil {
		t.Fatal(err)
	}
	// os.MkdirAll's mode argument is masked by the process umask; force the
	// exact bad bits this test needs regardless of what umask happens to be.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	restore := SetPrivateRootTmpDirForTest(dir)
	t.Cleanup(restore)

	req := BootstrapRequest{
		StartCmd:     "true",
		ControlToken: "tok",
	}
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("lstat private tmp dir: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("private tmp dir is a symlink after bootstrap")
	}
	if perm := fi.Mode().Perm(); perm != privateRootTmpDirMode {
		t.Errorf("mode = %#o, want %#o", perm, privateRootTmpDirMode)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if st.Uid != uint32(os.Geteuid()) {
			t.Errorf("owner uid = %d, want %d (this process's own euid)", st.Uid, os.Geteuid())
		}
	} else {
		t.Fatal("could not read raw stat_t for owner check")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read private tmp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected private tmp dir to be empty after bootstrap, got %v", entries)
	}
}

// TestBootstrap_PrivateTmpDirSymlinkedAncestorRejectedWith422 proves
// ensurePrivateTmpDir's fail-closed contract reaches all the way out to the
// HTTP response: a symlinked ancestor in the private tmp dir's chain (the
// same shape a pre-built image or a prior compromised bootstrap could leave
// behind) must answer 422 with the same *bootstrapPathError code the
// enforced-hooks-dir clear uses, and must never start init.
func TestBootstrap_PrivateTmpDirSymlinkedAncestorRejectedWith422(t *testing.T) {
	base := realTempDir(t)
	real := filepath.Join(base, "real-private-tmp-parent")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "private-tmp-parent-symlink")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(link, "run", "scion", "tmp")

	restore := SetPrivateRootTmpDirForTest(dir)
	t.Cleanup(restore)

	initCalled := make(chan struct{}, 1)
	req := BootstrapRequest{
		StartCmd:     "true",
		ControlToken: "tok",
	}
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int {
			initCalled <- struct{}{}
			return 0
		}),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bootstrap status = %d, want 422 (a symlinked private-tmp-dir ancestor is a bootstrapPathError)", rec.Code)
	}
	if waitInitCalled(initCalled) {
		t.Error("init must never start when the private tmp dir chain contains a symlink")
	}
	if _, err := os.Stat(filepath.Join(real, "run")); !os.IsNotExist(err) {
		t.Errorf("expected nothing created through the symlinked ancestor, stat err=%v", err)
	}
}

// TestBootstrap_PrivateTmpDirNonDirComponentRejectedWithDistinctCode is the
// regression test for ensurePrivateTmpDir reporting a non-directory
// ancestor (a plain file sitting where a parent directory needs to be) with
// codeBootstrapPathInvalid, not the symlink code — the same code/case split
// clearEnforcedHooksDir and writeBootstrapFile both already make, which
// ensurePrivateTmpDir previously collapsed into one.
func TestBootstrap_PrivateTmpDirNonDirComponentRejectedWithDistinctCode(t *testing.T) {
	base := realTempDir(t)
	blocker := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("i am a file, not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "run", "scion", "tmp")

	restore := SetPrivateRootTmpDirForTest(dir)
	t.Cleanup(restore)

	initCalled := make(chan struct{}, 1)
	req := BootstrapRequest{
		StartCmd:     "true",
		ControlToken: "tok",
	}
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int {
			initCalled <- struct{}{}
			return 0
		}),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bootstrap status = %d, want 422 (a non-directory private-tmp-dir ancestor is a bootstrapPathError)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), codeBootstrapPathInvalid) {
		t.Errorf("body = %q, want it to carry code %q, not the symlink code", rec.Body.String(), codeBootstrapPathInvalid)
	}
	if strings.Contains(rec.Body.String(), codeBootstrapPathSymlink) {
		t.Errorf("body = %q, must not report a non-directory component as a symlink", rec.Body.String())
	}
	if waitInitCalled(initCalled) {
		t.Error("init must never start when the private tmp dir chain contains a non-directory component")
	}
}
