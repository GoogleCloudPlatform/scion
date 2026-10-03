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

//go:build !no_sqlite

package hub

// The symlink matrix in project_workspace_symlink_test.go
// (newOutsideTree, plantSymlinks, assertNotLeaked) already covers the
// workspace and local shared-dir bases via runMatrix/symlinkBases. This file
// reuses those same primitives for a third base -- an NFS-backed shared dir
// -- rather than duplicating the link shapes or the leak assertions.
//
// It is a separate, small driver rather than a third symlinkBases() entry:
// that table's setup closures run AFTER testServer(t) inside runMatrix, but
// an NFS-backed resolution needs HOME (and the global settings.yaml under
// it) set up BEFORE testServer(t) -- see setNFSSharedDirStorageGlobalSettings's
// own docstring. Each test below calls setupNFSSymlinkMatrix once, exactly
// like runMatrix does per base, and drives the same HTTP-level cases as the
// upstream matrix's Download/List/Archive/Upload/Write/Delete tests.

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// setupNFSSymlinkMatrix sets up NFS shared_dir_storage global settings,
// starts a test server, creates a project with a declared shared dir, and
// plants the symlink matrix inside the leaf -- created ahead of time the way
// agent provisioning would, since a read verb never creates the leaf.
func setupNFSSymlinkMatrix(t *testing.T) (srv *Server, outside *outsideTree, leaf, filesURL, archiveURL string) {
	t.Helper()
	hostBase := setNFSSharedDirStorageGlobalSettings(t)

	srv, _ = testServer(t)
	outside = newOutsideTree(t)
	project := createTestGitProject(t, srv, "Symlink SD NFS", "github.com/test/symlink-sd-nfs")

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/shared-dirs", project.ID),
		map[string]interface{}{"name": "scratch"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	leaf = filepath.Join(hostBase, "projects", project.ID, "shared-dirs", "scratch")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	plantSymlinks(t, leaf, outside)

	filesURL = fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratch/files", project.ID)
	archiveURL = fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratch/archive", project.ID)
	return srv, outside, leaf, filesURL, archiveURL
}

func TestSymlinkNFS_Download(t *testing.T) {
	srv, outside, _, filesURL, _ := setupNFSSymlinkMatrix(t)

	cases := []struct {
		name     string
		path     string
		wantCode int
		wantBody string // exact body expected on success
	}{
		{"escaping leaf", "esc_leaf", http.StatusBadRequest, ""},
		{"escaping intermediate dir", "esc_dir/secret.txt", http.StatusBadRequest, ""},
		{"inside link is followed", "in_link", http.StatusOK, "inside content"},
		{"inside dir link is followed", "in_dir/inside.txt", http.StatusOK, "inside content"},
		{"dangling absolute", "dangling", http.StatusBadRequest, ""},
		{"dangling relative", "dangling_rel", http.StatusNotFound, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := outside.snapshot(t)
			rec := doRequest(t, srv, http.MethodGet, filesURL+"/"+tc.path, nil)

			assert.Equal(t, tc.wantCode, rec.Code, "body: %s", rec.Body.String())
			assertNotLeaked(t, rec)
			outside.assertIntact(t, before)
			if tc.wantBody != "" {
				assert.Equal(t, tc.wantBody, rec.Body.String())
			}
		})
	}
}

func TestSymlinkNFS_List(t *testing.T) {
	srv, outside, _, filesURL, _ := setupNFSSymlinkMatrix(t)

	before := outside.snapshot(t)
	rec := doRequest(t, srv, http.MethodGet, filesURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assertNotLeaked(t, rec)
	outside.assertIntact(t, before)

	var resp SharedDirListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	listed := map[string]bool{}
	for _, f := range resp.Files {
		listed[f.Path] = true
	}

	for path := range listed {
		assert.False(t, strings.HasPrefix(path, "esc_dir/"),
			"listing descended through a symlinked directory: %s", path)
		assert.False(t, strings.HasPrefix(path, "in_dir/"),
			"listing descended through a symlinked directory: %s", path)
	}
	assert.True(t, listed[filepath.Join("real", "inside.txt")],
		"the genuine file went missing from the listing: %v", listed)
	assert.True(t, listed["esc_leaf"], "the symlink itself should still be listed: %v", listed)
}

func TestSymlinkNFS_Archive(t *testing.T) {
	srv, outside, _, _, archiveURL := setupNFSSymlinkMatrix(t)

	before := outside.snapshot(t)
	rec := doRequest(t, srv, http.MethodGet, archiveURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	outside.assertIntact(t, before)

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)

	names := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		content, err := io.ReadAll(rc)
		require.NoError(t, rc.Close())
		require.NoError(t, err)
		names[f.Name] = string(content)
	}

	for name, content := range names {
		assert.NotContains(t, content, outsideSecret,
			"archive entry %q carries content from outside the served directory", name)
		assert.False(t, strings.HasPrefix(name, "esc_dir/"),
			"archive descended through a symlinked directory: %s", name)
	}
	assert.NotContains(t, names, "esc_leaf")
	assert.NotContains(t, names, "in_link")
	assert.NotContains(t, names, "dangling")
	assert.NotContains(t, names, "dangling_rel")
	assert.Equal(t, "inside content", names[filepath.Join("real", "inside.txt")])
}

func TestSymlinkNFS_Upload(t *testing.T) {
	srv, outside, leaf, filesURL, _ := setupNFSSymlinkMatrix(t)

	cases := []struct {
		name     string
		path     string
		wantCode int
	}{
		{"escaping leaf", "esc_leaf", http.StatusBadRequest},
		{"escaping intermediate dir", "esc_dir/landing/pwned.txt", http.StatusBadRequest},
		{"escaping dir, implicit mkdir", "esc_dir/landing/new/deep.txt", http.StatusBadRequest},
		{"dangling absolute", "dangling", http.StatusBadRequest},
		{"inside dir link", "in_dir/uploaded.txt", http.StatusOK},
		{"dangling relative", "dangling_rel", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := outside.snapshot(t)
			rec := doMultipartRequest(t, srv, http.MethodPost, filesURL,
				map[string][]byte{tc.path: []byte("written by the test")})

			assert.Equal(t, tc.wantCode, rec.Code, "body: %s", rec.Body.String())
			assertNotLeaked(t, rec)
			outside.assertIntact(t, before)
		})
	}

	got, err := os.ReadFile(filepath.Join(leaf, "real", "uploaded.txt"))
	require.NoError(t, err, "a write through an in-base directory link should have landed in the real directory")
	assert.Equal(t, "written by the test", string(got))

	got, err = os.ReadFile(filepath.Join(leaf, "no-such-file"))
	require.NoError(t, err, "a write through an in-base dangling link should have created the target inside the base")
	assert.Equal(t, "written by the test", string(got))
}

func TestSymlinkNFS_Write(t *testing.T) {
	srv, outside, leaf, filesURL, _ := setupNFSSymlinkMatrix(t)

	cases := []struct {
		name     string
		path     string
		wantCode int
	}{
		{"escaping leaf", "esc_leaf", http.StatusBadRequest},
		{"escaping intermediate dir", "esc_dir/landing/pwned.txt", http.StatusBadRequest},
		{"escaping dir, implicit mkdir", "esc_dir/landing/new/deep.txt", http.StatusBadRequest},
		{"dangling absolute", "dangling", http.StatusBadRequest},
		{"inside dir link", "in_dir/written.txt", http.StatusOK},
		{"inside link is followed", "in_link", http.StatusOK},
		{"dangling relative", "dangling_rel", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := outside.snapshot(t)
			rec := doRequest(t, srv, http.MethodPut, filesURL+"/"+tc.path,
				ProjectWorkspaceWriteRequest{Content: "written by the test"})

			assert.Equal(t, tc.wantCode, rec.Code, "body: %s", rec.Body.String())
			assertNotLeaked(t, rec)
			outside.assertIntact(t, before)
		})
	}

	fi, err := os.Lstat(filepath.Join(leaf, "in_link"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "in_link should still be a symlink")
	got, err := os.ReadFile(filepath.Join(leaf, "real", "inside.txt"))
	require.NoError(t, err)
	assert.Equal(t, "written by the test", string(got))
}

// TestSymlinkNFS_Delete: NFS DELETE removes an escaping symlink itself --
// the link, not its target.
func TestSymlinkNFS_Delete(t *testing.T) {
	srv, outside, leaf, filesURL, _ := setupNFSSymlinkMatrix(t)

	t.Run("escaping intermediate dir", func(t *testing.T) {
		before := outside.snapshot(t)
		rec := doRequest(t, srv, http.MethodDelete, filesURL+"/esc_dir/victim.txt", nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		outside.assertIntact(t, before)
	})

	t.Run("escaping leaf removes the link, not the target", func(t *testing.T) {
		before := outside.snapshot(t)
		rec := doRequest(t, srv, http.MethodDelete, filesURL+"/esc_leaf", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

		_, err := os.Lstat(filepath.Join(leaf, "esc_leaf"))
		assert.True(t, os.IsNotExist(err), "the link should be gone from the leaf")
		outside.assertIntact(t, before)
	})

	t.Run("inside link removes the link, not the target", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodDelete, filesURL+"/in_link", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

		_, err := os.Lstat(filepath.Join(leaf, "in_link"))
		assert.True(t, os.IsNotExist(err), "the link should be gone")
		_, err = os.Stat(filepath.Join(leaf, "real", "inside.txt"))
		assert.NoError(t, err, "deleting a link must not delete what it points at")
	})

	t.Run("dangling absolute", func(t *testing.T) {
		before := outside.snapshot(t)
		rec := doRequest(t, srv, http.MethodDelete, filesURL+"/dangling", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

		_, err := os.Lstat(filepath.Join(leaf, "dangling"))
		assert.True(t, os.IsNotExist(err), "the dangling link should be gone")
		outside.assertIntact(t, before)
	})
}

// ============================================================================
// Genuine RENAME_EXCHANGE races, <pid> a REAL directory
// ============================================================================
//
// <pid> being a symlink in BOTH toggle states of a swap would let
// resolveNFSSharedDirPath's own EvalSymlinks equality check refuse every
// request before OpenAnchoredRoot's dev/ino comparison is ever reached, so a
// change disabling that comparison would pass untouched. The tests below
// avoid that gap.
//
// The tests below instead keep <pid> a REAL directory holding the actual
// leaf, and use unix.Renameat2(RENAME_EXCHANGE) to atomically swap it with a
// symlink to a victim tree that has a MATCHING shared-dirs/<name> subtree.
// Because the victim side is a fully-formed, real leaf, a successful open
// (a 200, or a removed victim file) is possible in principle -- only the
// intended protection (the dev/ino check, or DeleteSharedDir's fd walk)
// prevents it. Each test also proves the real leaf is actually reachable,
// so a protection that accidentally refused EVERYTHING (which would
// trivially "pass" by never touching the victim either) couldn't hide
// behind these assertions. That proof is taken deterministically, with no
// swap in flight, before and/or after the race (see each test's doc
// comment): how many racing requests land between two swaps depends on
// scheduler timing and can legitimately be zero, so racing success counts
// are logged only.

type nfsRenameExchangeRace struct {
	srv           *Server
	pid           string // hostBase/projects/<pid>: real directory, holds the actual leaf
	alt           string // hostBase/projects/alt: symlink to victim
	victim        string
	dirName       string // the shared dir declared on the project; its leaf is <pid>/shared-dirs/<dirName>
	filesURL      string
	sharedDirsURL string
}

// setupNFSRenameExchangeRace declares dirName on a fresh NFS-backed project,
// creates its real leaf under <pid>/shared-dirs/dirName with a "keep.txt" of
// known content, and builds an equally-shaped victim tree (a distinct,
// distinguishable "keep.txt") reachable only via the alt symlink.
func setupNFSRenameExchangeRace(t *testing.T, dirName string) *nfsRenameExchangeRace {
	t.Helper()
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "Rename Exchange Race", "github.com/test/rename-exchange-race")

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/shared-dirs", project.ID),
		map[string]interface{}{"name": dirName})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	projects := filepath.Join(hostBase, "projects")
	pid := filepath.Join(projects, project.ID)
	require.NoError(t, os.MkdirAll(filepath.Join(pid, "shared-dirs", dirName), 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(pid, "shared-dirs", dirName, "keep.txt"), []byte("real"), 0o644))

	victim := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(victim, "shared-dirs", dirName), 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(victim, "shared-dirs", dirName, "keep.txt"), []byte(outsideSecret), 0o600))

	alt := filepath.Join(projects, "alt")
	require.NoError(t, os.Symlink(victim, alt))

	return &nfsRenameExchangeRace{
		srv:           srv,
		pid:           pid,
		alt:           alt,
		victim:        victim,
		dirName:       dirName,
		filesURL:      fmt.Sprintf("/api/v1/projects/%s/shared-dirs/%s/files", project.ID, dirName),
		sharedDirsURL: fmt.Sprintf("/api/v1/projects/%s/shared-dirs", project.ID),
	}
}

// trySwap atomically exchanges whatever currently sits at r.pid and r.alt.
func (r *nfsRenameExchangeRace) trySwap() error {
	return unix.Renameat2(unix.AT_FDCWD, r.pid, unix.AT_FDCWD, r.alt, unix.RENAME_EXCHANGE)
}

// skipIfRenameExchangeUnsupported performs one trial swap (immediately
// undone) and skips the test if the kernel/filesystem doesn't support
// RENAME_EXCHANGE at all (ENOSYS or EINVAL).
func (r *nfsRenameExchangeRace) skipIfRenameExchangeUnsupported(t *testing.T) {
	t.Helper()
	if err := r.trySwap(); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
			t.Skipf("RENAME_EXCHANGE unsupported on this filesystem/kernel: %v", err)
		}
		require.NoError(t, err)
	}
	require.NoError(t, r.trySwap(), "swap back to the original orientation")
}

// startSwapper launches a goroutine that repeatedly calls trySwap until
// stop is called (which also waits for the goroutine to exit), and reports
// how many swaps actually succeeded.
func (r *nfsRenameExchangeRace) startSwapper() (stop func(), swaps *atomic.Int64) {
	var stopFlag atomic.Bool
	swaps = &atomic.Int64{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stopFlag.Load() {
			if err := r.trySwap(); err == nil {
				swaps.Add(1)
			}
		}
	}()
	return func() { stopFlag.Store(true); <-done }, swaps
}

// TestRenameExchangeRaceNFS_Get_NeverLeaksVictim: the fraction of racing
// GETs that land between two swaps depends on scheduler timing and can
// legitimately be zero, so the proof that a GET reaches the real leaf is
// taken deterministically, with no swap in flight, both before the race and
// after it (once the real leaf is back at race.pid). The racing loop in
// between is what exercises the never-leaks-the-victim property; every
// racing response is checked for a leak, and its success count is logged
// only.
func TestRenameExchangeRaceNFS_Get_NeverLeaksVictim(t *testing.T) {
	race := setupNFSRenameExchangeRace(t, "scratch")
	race.skipIfRenameExchangeUnsupported(t)

	victimTree := &outsideTree{dir: race.victim}
	before := victimTree.snapshot(t)

	race.requireGetReachesRealLeaf(t)

	stop, swaps := race.startSwapper()
	const iterations = 3000
	var realOK, leaks int
	for i := 0; i < iterations; i++ {
		rec := doRequest(t, race.srv, http.MethodGet, race.filesURL+"/keep.txt", nil)
		if strings.Contains(rec.Body.String(), outsideSecret) {
			leaks++
		} else if rec.Code == http.StatusOK {
			realOK++
		}
	}
	stop()
	t.Logf("swaps=%d realOK=%d leaks=%d (racing GET successes are timing-dependent and not asserted)", swaps.Load(), realOK, leaks)
	assert.Zero(t, leaks, "the victim's content must never be served, regardless of race timing")

	race.restoreRealOrientation(t)
	race.requireGetReachesRealLeaf(t)

	victimTree.assertIntact(t, before)
}

// requireGetReachesRealLeaf issues one GET of keep.txt with no swap in
// flight and requires that it succeeds with the real leaf's content. This
// is the deterministic proof that the protection does not refuse every GET
// (which would trivially never leak the victim).
func (r *nfsRenameExchangeRace) requireGetReachesRealLeaf(t *testing.T) {
	t.Helper()
	rec := doRequest(t, r.srv, http.MethodGet, r.filesURL+"/keep.txt", nil)
	assertNotLeaked(t, rec)
	require.Equal(t, http.StatusOK, rec.Code,
		"a GET with no swap in flight must reach the real leaf, or this test proves nothing; body: %s", rec.Body.String())
	require.Equal(t, "real", rec.Body.String(), "the GET must serve the real leaf's keep.txt")
}

// TestRenameExchangeRaceNFS_Put_NeverWritesVictim: a racing PUT holds the
// path open across several resolution steps, so under a fast swapper the
// fraction of PUTs that land between two swaps depends heavily on scheduler
// timing and can legitimately be zero. The proof that a PUT reaches the real
// leaf is therefore taken deterministically, with no swap in flight, both
// before the race and after it (once the real leaf is back at race.pid), and
// checked on disk. The racing loop in between is what exercises the
// never-writes-the-victim property; its success count is logged only.
func TestRenameExchangeRaceNFS_Put_NeverWritesVictim(t *testing.T) {
	race := setupNFSRenameExchangeRace(t, "scratch")
	race.skipIfRenameExchangeUnsupported(t)

	victimTree := &outsideTree{dir: race.victim}
	before := victimTree.snapshot(t)

	race.requirePutReachesRealLeaf(t, "written before the race")

	stop, swaps := race.startSwapper()
	const iterations = 1000
	var puts int
	for i := 0; i < iterations; i++ {
		rec := doRequest(t, race.srv, http.MethodPut, race.filesURL+"/planted.txt",
			ProjectWorkspaceWriteRequest{Content: "written by the rename-exchange race test"})
		assertNotLeaked(t, rec)
		if rec.Code == http.StatusOK {
			puts++
		}
	}
	stop()
	t.Logf("swaps=%d putsOK=%d (racing PUT successes are timing-dependent and not asserted)", swaps.Load(), puts)

	race.restoreRealOrientation(t)
	race.requirePutReachesRealLeaf(t, "written after the race")

	victimTree.assertIntact(t, before)
	_, err := os.Lstat(filepath.Join(race.victim, "shared-dirs", race.dirName, "planted.txt"))
	assert.True(t, os.IsNotExist(err), "the victim must never receive the planted file")
}

// restoreRealOrientation puts the real leaf back at r.pid (and the victim
// symlink back at r.alt) after a swapper has stopped in either orientation.
func (r *nfsRenameExchangeRace) restoreRealOrientation(t *testing.T) {
	t.Helper()
	if !realLeafContentAt(r.pid, r.dirName) {
		require.NoError(t, r.trySwap(), "swap back to the original orientation")
	}
	require.True(t, realLeafContentAt(r.pid, r.dirName), "the real leaf must be at the project path")
	info, err := os.Lstat(r.alt)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink, "the victim symlink must be at the alt path")
}

// requirePutReachesRealLeaf issues one PUT of planted.txt with no swap in
// flight and requires that it succeeds and that the content lands in the
// real leaf under r.pid. This is the deterministic proof that the protection
// does not refuse every PUT (which would trivially never write the victim).
func (r *nfsRenameExchangeRace) requirePutReachesRealLeaf(t *testing.T, content string) {
	t.Helper()
	rec := doRequest(t, r.srv, http.MethodPut, r.filesURL+"/planted.txt",
		ProjectWorkspaceWriteRequest{Content: content})
	assertNotLeaked(t, rec)
	require.Equal(t, http.StatusOK, rec.Code,
		"a PUT with no swap in flight must reach the real leaf, or this test proves nothing; body: %s", rec.Body.String())
	got, err := os.ReadFile(filepath.Join(r.pid, "shared-dirs", r.dirName, "planted.txt"))
	require.NoError(t, err, "the PUT must have written into the real leaf")
	require.Equal(t, content, string(got))
}

// realLeafContentAt reports whether path is currently a REAL directory (not
// a symlink) whose shared-dirs/<dirName>/keep.txt holds the known-good "real"
// content -- checked with an Lstat first, so a path that has been swapped to
// a symlink is never followed.
func realLeafContentAt(path, dirName string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	content, err := os.ReadFile(filepath.Join(path, "shared-dirs", dirName, "keep.txt"))
	return err == nil && string(content) == "real"
}

// TestRenameExchangeRaceNFS_ConfigDelete_NeverRemovesVictim: unlike GET/PUT,
// a config DELETE always returns 204 regardless of whether the host-side
// cleanup actually reached anything (it is best-effort), so the response
// code alone can't distinguish "reached the real leaf" from "raced a
// symlink and did nothing". Each round below resets a fresh real leaf
// and checks the ACTUAL filesystem state after the DELETE.
//
// The proof that a DELETE reaches the real leaf is taken deterministically:
// one round before the race and one after it run with NO swapper, and the
// DELETE must remove the real leaf's content at race.pid while the victim's
// file stays intact. The racing rounds in between each race a single DELETE
// against the swapper and exercise the never-removes-the-victim property;
// how many of them land between two swaps depends on scheduler timing and
// can legitimately be zero, so their removal count (taken at whichever of
// race.pid/race.alt now holds the real content) is logged only.
func TestRenameExchangeRaceNFS_ConfigDelete_NeverRemovesVictim(t *testing.T) {
	race := setupNFSRenameExchangeRace(t, "scratch")
	race.skipIfRenameExchangeUnsupported(t)

	realLeaf := filepath.Join(race.pid, "shared-dirs", race.dirName)
	victimKeep := filepath.Join(race.victim, "shared-dirs", race.dirName, "keep.txt")
	deleteURL := race.sharedDirsURL + "/" + race.dirName

	// reset puts the real leaf at race.pid and a symlink to the victim at
	// race.alt -- both known-good before a round starts -- and re-declares
	// the shared dir. RemoveAll handles either path being a symlink
	// (unlinked directly, never followed) or a real directory (recursively
	// removed) left over from a previous round's swap.
	reset := func() {
		t.Helper()
		require.NoError(t, os.RemoveAll(race.pid))
		require.NoError(t, os.RemoveAll(race.alt))
		require.NoError(t, os.MkdirAll(realLeaf, 0o2775))
		require.NoError(t, os.WriteFile(filepath.Join(realLeaf, "keep.txt"), []byte("real"), 0o644))
		require.NoError(t, os.Symlink(race.victim, race.alt))
		doRequest(t, race.srv, http.MethodPost, race.sharedDirsURL, map[string]interface{}{"name": race.dirName})
	}

	requireVictimIntact := func() {
		t.Helper()
		content, err := os.ReadFile(victimKeep)
		require.NoError(t, err, "the victim's own tree must never lose its file")
		require.Equal(t, outsideSecret, string(content), "the victim's file content must never be touched")
	}

	// requireDeleteReachesRealLeaf runs one round with no swapper: the
	// DELETE must remove the real leaf's content at race.pid. This is the
	// deterministic proof that the protection does not refuse every DELETE
	// (which would trivially never remove the victim).
	requireDeleteReachesRealLeaf := func(when string) {
		t.Helper()
		reset()
		require.True(t, realLeafContentAt(race.pid, race.dirName), "the real leaf must be at the project path before the DELETE")
		doRequest(t, race.srv, http.MethodDelete, deleteURL, nil)
		require.False(t, realLeafContentAt(race.pid, race.dirName),
			"a DELETE with no swap in flight (%s) must remove the real leaf, or this test proves nothing about reaching the real delete path", when)
		requireVictimIntact()
	}

	requireDeleteReachesRealLeaf("before the race")

	const rounds = 300
	var realRemovals int
	for i := 0; i < rounds; i++ {
		reset()

		stop, _ := race.startSwapper()
		doRequest(t, race.srv, http.MethodDelete, deleteURL, nil)
		stop()

		if !realLeafContentAt(race.pid, race.dirName) && !realLeafContentAt(race.alt, race.dirName) {
			realRemovals++
		}

		requireVictimIntact()
	}
	t.Logf("rounds=%d realRemovals=%d (racing removals are timing-dependent and not asserted)", rounds, realRemovals)

	requireDeleteReachesRealLeaf("after the race")
}
