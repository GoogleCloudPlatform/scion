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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// realTempDir returns t.TempDir() with any symlinks in its path resolved.
// t.TempDir() is not guaranteed to be symlink-free: on macOS it lives under
// /var/folders/..., and /var is itself a symlink to /private/var, and a
// symlinked TMPDIR reproduces the same thing on any platform (e.g.
// TMPDIR=/tmp/link pointing at a real directory). mkdirAllTracked's
// every-component symlink guard (see helpers.go) Lstats every existing
// ancestor of a bootstrap path, including ones above the test's own temp
// root, so a test that builds its bootstrap path directly on a symlinked
// t.TempDir() would spuriously trip that guard — not because the test's
// fixture contains a symlink, but because the *environment* does. Tests
// that build a bootstrap path from a temp directory must root it here
// instead, so only symlinks the fixture itself creates are under test.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	return dir
}

// withAgentHomeFixture points agentHomeDir at dir for the duration of the
// test, restoring the original value afterward — the same seam
// withEnforcedHooksFixture provides for enforcedHooksHomePrefix/
// enforcedHooksDir, needed here so a fixture built under a throwaway
// realTempDir() satisfies isWithinAgentHome's containment check without a
// test writing to a real "/home/scion". Production never reassigns
// agentHomeDir.
func withAgentHomeFixture(t *testing.T, dir string) {
	t.Helper()
	orig := agentHomeDir
	agentHomeDir = dir
	t.Cleanup(func() { agentHomeDir = orig })
}

func doJSON(t *testing.T, h http.Handler, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal response %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestHealthz_InitiallyAwaitingBootstrap(t *testing.T) {
	srv := NewServer(WithChownOwner(-1, -1))
	rec := doJSON(t, srv.Handler(), http.MethodGet, "/scion/v1/healthz", "", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON[HealthzResponse](t, rec)
	if got.State != StateAwaitingBootstrap {
		t.Errorf("state = %q, want %q", got.State, StateAwaitingBootstrap)
	}
}

// TestHealthz_NonZeroInitFlipsToInitFailedButServerKeepsServing proves that
// substrate-serve does not exit the process on a non-zero init (there is no
// os.Exit anywhere in this package to begin with — that decision lives in
// the cmd layer's InitRunner wrapper, which does not act on the exit code
// at all), so the control server keeps serving, and healthz flips to the
// distinct StateInitFailed rather than staying "running" — see
// StateInitFailed's doc comment for why that matters given Substrate
// doesn't observe a PID 1 exit as a failure signal either way.
func TestHealthz_NonZeroInitFlipsToInitFailedButServerKeepsServing(t *testing.T) {
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int {
			return 1
		}),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd:     "true",
		ControlToken: "tok",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// The server must still be serving (this test is still running — a real
	// os.Exit anywhere in this path would have killed the test binary
	// itself, not just failed an assertion), and healthz must reflect the
	// failed init rather than reporting "running". The init runner's exit
	// code is only applied to s.initFailed *after* it returns (see
	// handleBootstrap's goroutine), so poll rather than checking once
	// immediately.
	deadline := time.Now().Add(2 * time.Second)
	var got HealthzResponse
	for {
		healthz := doJSON(t, srv.Handler(), http.MethodGet, "/scion/v1/healthz", "", nil)
		if healthz.Code != http.StatusOK {
			t.Fatalf("healthz status = %d, want 200", healthz.Code)
		}
		got = decodeJSON[HealthzResponse](t, healthz)
		if got.State == StateInitFailed || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.State != StateInitFailed {
		t.Errorf("healthz state = %q, want %q", got.State, StateInitFailed)
	}
}

// TestHealthz_ZeroExitStaysRunning is the control for the test above: a
// clean (0) init exit must not flip healthz away from StateRunning.
func TestHealthz_ZeroExitStaysRunning(t *testing.T) {
	initDone := make(chan struct{})
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int {
			defer close(initDone)
			return 0
		}),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd:     "true",
		ControlToken: "tok",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	select {
	case <-initDone:
	case <-time.After(2 * time.Second):
		t.Fatal("init runner was never invoked")
	}

	healthz := doJSON(t, srv.Handler(), http.MethodGet, "/scion/v1/healthz", "", nil)
	got := decodeJSON[HealthzResponse](t, healthz)
	if got.State != StateRunning {
		t.Errorf("healthz state = %q, want %q", got.State, StateRunning)
	}
}

func TestBootstrap_BadNonceRejected(t *testing.T) {
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithNonceVerifier(StaticNonceVerifier{Expected: "correct-nonce"}),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "wrong-nonce", BootstrapRequest{
		StartCmd: "true",
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if srv.isBootstrapped() {
		t.Error("a rejected bootstrap must not consume the single-shot slot")
	}
}

func TestBootstrap_MissingBearerRejected(t *testing.T) {
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithNonceVerifier(StaticNonceVerifier{Expected: "correct-nonce"}),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "", BootstrapRequest{StartCmd: "true"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestBootstrap_SingleShot_SecondCallGets409(t *testing.T) {
	var runCount int
	var mu sync.Mutex
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int {
			mu.Lock()
			runCount++
			mu.Unlock()
			if forwardTermSignal {
				t.Error("bootstrap must call the init runner with forwardTermSignal=false")
			}
			return 0
		}),
	)

	req := BootstrapRequest{StartCmd: "true", ControlToken: "tok-1"}

	first := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", req)
	if first.Code != http.StatusOK {
		t.Fatalf("first bootstrap status = %d, want 200: %s", first.Code, first.Body.String())
	}

	second := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{StartCmd: "true", ControlToken: "tok-2"})
	if second.Code != http.StatusConflict {
		t.Fatalf("second bootstrap status = %d, want 409", second.Code)
	}

	// healthz should now report running.
	health := doJSON(t, srv.Handler(), http.MethodGet, "/scion/v1/healthz", "", nil)
	got := decodeJSON[HealthzResponse](t, health)
	if got.State != StateRunning {
		t.Errorf("state after bootstrap = %q, want %q", got.State, StateRunning)
	}

	// Give the async init-runner goroutine a moment to run, then confirm it
	// only ran once (the rejected second request must not re-trigger init).
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := runCount
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if runCount != 1 {
		t.Errorf("init runner invoked %d times, want exactly 1", runCount)
	}
}

func TestBootstrap_WritesFilesWithParentDirsAndEnv(t *testing.T) {
	dir := realTempDir(t)
	withAgentHomeFixture(t, dir)
	filePath := filepath.Join(dir, "nested", "deep", "config.json")

	content := []byte(`{"hello":"world"}`)
	req := BootstrapRequest{
		Env: map[string]string{
			"SCION_SUBSTRATE_TEST_VAR": "set-by-bootstrap",
		},
		Files: []BootstrapFile{
			{Path: filePath, Mode: 0o600, ContentB64: base64.StdEncoding.EncodeToString(content)},
		},
		StartCmd:     "true",
		ControlToken: "tok",
	}

	t.Setenv("SCION_SUBSTRATE_TEST_VAR", "")
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("expected bootstrap file to exist: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("file content = %q, want %q", got, content)
	}
	if info, err := os.Stat(filePath); err == nil {
		if info.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
		}
	}

	if v := os.Getenv("SCION_SUBSTRATE_TEST_VAR"); v != "set-by-bootstrap" {
		t.Errorf("SCION_SUBSTRATE_TEST_VAR = %q, want %q", v, "set-by-bootstrap")
	}
}

// TestWriteBootstrapFile_EnforcesModeOnPreExistingFile asserts that writing
// to a file that already exists at a looser mode (as if baked into the
// image) still ends with exactly the requested mode and the new content —
// not the pre-existing file's mode or content. os.WriteFile's mode argument
// only applies to a newly created file's open(2) call and has no effect on
// a file that already exists; it only truncates and rewrites contents.
func TestWriteBootstrapFile_EnforcesModeOnPreExistingFile(t *testing.T) {
	dir := realTempDir(t)
	withAgentHomeFixture(t, dir)
	filePath := filepath.Join(dir, "credential.json")

	// Pre-create the file at a looser mode, as if baked into the image.
	if err := os.WriteFile(filePath, []byte("stale"), 0o644); err != nil {
		t.Fatalf("failed to pre-create file: %v", err)
	}

	srv := NewServer(WithChownOwner(-1, -1))
	f := BootstrapFile{
		Path:       filePath,
		Mode:       0o600,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("fresh")),
	}
	if err := srv.writeBootstrapFile(f); err != nil {
		t.Fatalf("writeBootstrapFile: %v", err)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 (pre-existing file's mode must not survive)", info.Mode().Perm())
	}
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("content = %q, want %q", got, "fresh")
	}
}

// TestWriteBootstrapFile_SetsModeAndOwnerAtomically asserts that
// writeFileAtomicMode's write-to-temp-then-rename result has exactly the
// requested mode, owner, and content, for both a fresh file and a
// pre-existing one. (The absence of a readable-at-wrong-mode window during
// the write isn't itself observable from a single-threaded test — what's
// verifiable, and what this pins, is that the function never produces a
// file with the wrong mode or owner once it returns.)
func TestWriteBootstrapFile_SetsModeAndOwnerAtomically(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ownership check uses syscall.Stat_t (Linux only)")
	}

	uid := os.Getuid()
	gid := os.Getgid()

	assertModeAndOwner := func(t *testing.T, path string, wantMode os.FileMode, wantContent string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != wantMode {
			t.Errorf("mode = %v, want %v", info.Mode().Perm(), wantMode)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("could not read platform-specific stat info")
		}
		if int(stat.Uid) != uid {
			t.Errorf("uid = %d, want %d", stat.Uid, uid)
		}
		if int(stat.Gid) != gid {
			t.Errorf("gid = %d, want %d", stat.Gid, gid)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != wantContent {
			t.Errorf("content = %q, want %q", got, wantContent)
		}
	}

	t.Run("fresh file", func(t *testing.T) {
		dir := realTempDir(t)
		withAgentHomeFixture(t, dir)
		filePath := filepath.Join(dir, "fresh.json")

		srv := NewServer(WithChownOwner(uid, gid))
		f := BootstrapFile{
			Path:       filePath,
			Mode:       0o600,
			ContentB64: base64.StdEncoding.EncodeToString([]byte("fresh-secret")),
		}
		if err := srv.writeBootstrapFile(f); err != nil {
			t.Fatalf("writeBootstrapFile: %v", err)
		}
		assertModeAndOwner(t, filePath, 0o600, "fresh-secret")
	})

	t.Run("pre-existing file at a different mode", func(t *testing.T) {
		dir := realTempDir(t)
		withAgentHomeFixture(t, dir)
		filePath := filepath.Join(dir, "existing.json")
		if err := os.WriteFile(filePath, []byte("stale"), 0o644); err != nil {
			t.Fatalf("failed to pre-create file: %v", err)
		}

		srv := NewServer(WithChownOwner(uid, gid))
		f := BootstrapFile{
			Path:       filePath,
			Mode:       0o640,
			ContentB64: base64.StdEncoding.EncodeToString([]byte("replaced-secret")),
		}
		if err := srv.writeBootstrapFile(f); err != nil {
			t.Fatalf("writeBootstrapFile: %v", err)
		}
		assertModeAndOwner(t, filePath, 0o640, "replaced-secret")
	})
}

// TestWriteBootstrapFile_OmittedModeDefaultsToOwnerOnly is the regression
// test for defaultFileMode: a bootstrap file entry that omits Mode (the
// zero value) must land at 0o600 (owner read/write only), not a more
// permissive default — a bootstrap file can carry secret content (a CA
// bundle, an env file with credentials), and a caller that wants it more
// widely readable must say so explicitly via a non-zero Mode.
func TestWriteBootstrapFile_OmittedModeDefaultsToOwnerOnly(t *testing.T) {
	dir := realTempDir(t)
	withAgentHomeFixture(t, dir)
	filePath := filepath.Join(dir, "omitted-mode.json")

	srv := NewServer(WithChownOwner(-1, -1))
	f := BootstrapFile{
		Path:       filePath,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("secret")),
	}
	if err := srv.writeBootstrapFile(f); err != nil {
		t.Fatalf("writeBootstrapFile: %v", err)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 for an omitted Mode", info.Mode().Perm())
	}
}

// TestWriteBootstrapFile_ModeMaskedToPermissionBits is the regression test
// for masking a caller-supplied Mode to the permission bits only: a Mode
// carrying the setuid bit (or any bit outside 0o777) must never reach the
// filesystem — this process runs as root and is about to chown the file to
// the workload uid, so an unmasked setuid bit would plant a
// privilege-escalation path that survives bootstrap.
func TestWriteBootstrapFile_ModeMaskedToPermissionBits(t *testing.T) {
	dir := realTempDir(t)
	withAgentHomeFixture(t, dir)
	filePath := filepath.Join(dir, "setuid-attempt.json")

	srv := NewServer(WithChownOwner(-1, -1))
	f := BootstrapFile{
		Path:       filePath,
		Mode:       0o4755, // setuid + rwxr-xr-x
		ContentB64: base64.StdEncoding.EncodeToString([]byte("secret")),
	}
	if err := srv.writeBootstrapFile(f); err != nil {
		t.Fatalf("writeBootstrapFile: %v", err)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Errorf("mode = %v, setuid bit must be masked out", info.Mode())
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 (the permission bits only) after masking", info.Mode().Perm())
	}
}

// TestWriteBootstrapFile_RejectsWriteThroughPreExistingSymlinkDir proves the
// serve-side symlink safety: an image that ships a directory component as a
// symlink (e.g. the real-world case this guards, /home/scion/.config ->
// /etc) must not have a bootstrap file written through it. A naive
// os.Stat-based existence check followed by os.MkdirAll would instead
// follow the link and happily create the remaining path components on the
// other side of it.
func TestWriteBootstrapFile_RejectsWriteThroughPreExistingSymlinkDir(t *testing.T) {
	root := realTempDir(t)
	fakeHome := filepath.Join(root, "home", "scion")
	withAgentHomeFixture(t, fakeHome)
	outsideTarget := filepath.Join(root, "etc") // stands in for a real /etc
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideTarget, 0o755); err != nil {
		t.Fatal(err)
	}

	// The image pre-ships fakeHome/.config as a symlink to outsideTarget.
	configLink := filepath.Join(fakeHome, ".config")
	if err := os.Symlink(outsideTarget, configLink); err != nil {
		t.Fatal(err)
	}

	// A bootstrap file targets a path *inside* the symlinked directory.
	targetPath := filepath.Join(configLink, "nested", "secret.json")
	srv := NewServer(WithChownOwner(-1, -1))
	err := srv.writeBootstrapFile(BootstrapFile{
		Path:       targetPath,
		Mode:       0o600,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("must-not-land-in-etc")),
	})
	if err == nil {
		t.Fatal("writeBootstrapFile through a symlinked directory: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), targetPath) {
		t.Errorf("error = %v, want it to name the rejected bootstrap file path %q", err, targetPath)
	}
	if strings.Contains(err.Error(), "must-not-land-in-etc") {
		t.Errorf("error leaked file content: %v", err)
	}

	// Nothing must have been created on the other side of the symlink.
	if _, statErr := os.Stat(filepath.Join(outsideTarget, "nested")); statErr == nil {
		t.Error("a directory was created through the symlink into outsideTarget; the write escaped confinement")
	}
	if _, statErr := os.Stat(filepath.Join(outsideTarget, "nested", "secret.json")); statErr == nil {
		t.Error("the bootstrap file was written through the symlink into outsideTarget")
	}

	// The symlink itself must be untouched (still a symlink, still pointing
	// at outsideTarget) — rejecting the file must not disturb the image.
	info, err := os.Lstat(configLink)
	if err != nil {
		t.Fatalf("lstat %s: %v", configLink, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is no longer a symlink after the rejected write", configLink)
	}
}

// TestWriteBootstrapFile_RejectsWriteThroughSymlinkWhenTargetSubpathAlreadyExists
// covers the gap a naive "Lstat only the deepest ancestor that exists, found
// by walking upward" search leaves open: Lstat only declines to follow its
// own final argument, so an upward walk that stops at the first existing
// ancestor never Lstats anything above that point. If a symlinked component
// higher up the path already has the remaining subpath pre-created on its
// far side, the upward walk lands on that real directory and the symlink is
// never noticed. This must be rejected on the *current* mkdirAllTracked
// (every existing component checked top-down), and would have been silently
// accepted by a deepest-existing-ancestor upward-walk search.
func TestWriteBootstrapFile_RejectsWriteThroughSymlinkWhenTargetSubpathAlreadyExists(t *testing.T) {
	root := realTempDir(t)
	fakeHome := filepath.Join(root, "home", "scion")
	withAgentHomeFixture(t, fakeHome)
	outsideTarget := filepath.Join(root, "etc") // stands in for a real /etc
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// The subpath the bootstrap file will target already exists on the far
	// side of the link *before* the symlink is ever consulted.
	if err := os.MkdirAll(filepath.Join(outsideTarget, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	configLink := filepath.Join(fakeHome, ".config")
	if err := os.Symlink(outsideTarget, configLink); err != nil {
		t.Fatal(err)
	}

	targetPath := filepath.Join(configLink, "sub", "file")
	srv := NewServer(WithChownOwner(-1, -1))
	err := srv.writeBootstrapFile(BootstrapFile{
		Path:       targetPath,
		Mode:       0o600,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("must-not-land-in-etc")),
	})
	if err == nil {
		t.Fatal("writeBootstrapFile through a symlink whose target already has the subpath: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), targetPath) {
		t.Errorf("error = %v, want it to name the rejected bootstrap file path %q", err, targetPath)
	}
	if strings.Contains(err.Error(), "must-not-land-in-etc") {
		t.Errorf("error leaked file content: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(outsideTarget, "sub", "file")); statErr == nil {
		t.Error("the bootstrap file was written through the symlink into outsideTarget/sub")
	}
	entries, err := os.ReadDir(filepath.Join(outsideTarget, "sub"))
	if err != nil {
		t.Fatalf("readdir outsideTarget/sub: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("outsideTarget/sub gained entries %v; nothing must be created on the far side of the link", entries)
	}
}

// TestWriteBootstrapFile_RejectsSymlinkAtFirstComponentUnderHome is a
// specific, realistic trigger for the same symlink rejection: an image
// where a direct child of the home directory (e.g. ~/.config) is itself the
// symlink, one component down from home, with the file only one level below
// that.
func TestWriteBootstrapFile_RejectsSymlinkAtFirstComponentUnderHome(t *testing.T) {
	root := realTempDir(t)
	fakeHome := filepath.Join(root, "home", "scion")
	outsideTarget := filepath.Join(root, "outside")
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	// Without this, agentHomeDir stays the real "/home/scion" (or whatever
	// util.GetHomeDir("scion") resolves to), fakeHome is NOT under it, and
	// targetPath below is rejected by the outside-home check before
	// mkdirAllTracked's symlink walk ever runs — passing this test for the
	// wrong reason (see TestIsWithinAgentHome's own doc comment for the
	// class of defect this would otherwise mask).
	withAgentHomeFixture(t, fakeHome)

	configLink := filepath.Join(fakeHome, ".config")
	if err := os.Symlink(outsideTarget, configLink); err != nil {
		t.Fatal(err)
	}

	targetPath := filepath.Join(configLink, "x")
	srv := NewServer(WithChownOwner(-1, -1))
	err := srv.writeBootstrapFile(BootstrapFile{
		Path:       targetPath,
		Mode:       0o600,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("must-not-land-outside")),
	})
	var pathErr *bootstrapPathError
	if !errors.As(err, &pathErr) || pathErr.code != codeBootstrapPathSymlink {
		t.Fatalf("writeBootstrapFile through a symlink at the first component under home: err = %v, want a *bootstrapPathError with code %q", err, codeBootstrapPathSymlink)
	}
	if _, statErr := os.Stat(filepath.Join(outsideTarget, "x")); statErr == nil {
		t.Error("the bootstrap file was written through the symlink into outsideTarget")
	}
}

// TestWriteBootstrapFile_DotDotCleansToLocationUnderHomeAndNowhereElse proves
// a ".." segment in the bootstrap file's Path lands exactly where
// filepath.Clean says it should, and never touches the lexical component the
// ".." walks back through — it only proves the lexical Clean plus the
// symlink walk agree with each other on where a dotdot-bearing path
// resolves. The path Cleans to a location under home, so this is unaffected
// by isWithinAgentHome's containment check; see
// TestWriteBootstrapFile_RefusesTargetOutsideAgentHome for that check's own
// dotdot case.
func TestWriteBootstrapFile_DotDotCleansToLocationUnderHomeAndNowhereElse(t *testing.T) {
	root := realTempDir(t)
	fakeHome := filepath.Join(root, "home", "scion")
	withAgentHomeFixture(t, fakeHome)
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// Deliberately not filepath.Join, which would Clean the ".." away before
	// the test ever exercises writeBootstrapFile's own handling of it.
	targetPath := fakeHome + "/a/../b/file"
	wantPath := filepath.Join(fakeHome, "b", "file")

	srv := NewServer(WithChownOwner(-1, -1))
	if err := srv.writeBootstrapFile(BootstrapFile{
		Path:       targetPath,
		Mode:       0o600,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("dotdot-content")),
	}); err != nil {
		t.Fatalf("writeBootstrapFile with a dotdot-bearing path that Cleans under home: %v", err)
	}

	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read %s: %v", wantPath, err)
	}
	if string(got) != "dotdot-content" {
		t.Errorf("content at %s = %q, want %q", wantPath, got, "dotdot-content")
	}

	// The lexical component the ".." walked back through must never have
	// been created — the file must land only at the Cleaned location.
	if _, statErr := os.Stat(filepath.Join(fakeHome, "a")); statErr == nil {
		t.Error("a directory was created for the dotdot-only path component \"a\"; the write should have used the Cleaned path only")
	}
}

// TestWriteBootstrapFile_LeafSymlinkIsReplacedNotWrittenThrough covers the
// final path component itself being a pre-existing symlink, as distinct
// from every test above which targets a symlinked *parent*.
// writeFileAtomicMode's os.Rename(tmp, path) call replaces whatever
// directory entry currently sits at path — including a symlink — rather
// than following it, so this must succeed by atomically replacing the link
// with a regular file, and the symlink's old target must be left untouched.
// This pins "replaced" as the one documented outcome (writeBootstrapFile's
// leaf-symlink comment and substrate-runtime.md §5.5 both claim it): a future change
// that instead rejects the leaf case must update those docs, which means it
// must also update this test.
func TestWriteBootstrapFile_LeafSymlinkIsReplacedNotWrittenThrough(t *testing.T) {
	root := realTempDir(t)
	fakeHome := filepath.Join(root, "home", "scion")
	withAgentHomeFixture(t, fakeHome)
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(root, "outside-secret.txt")
	if err := os.WriteFile(outsideFile, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}

	targetPath := filepath.Join(fakeHome, "leaf")
	if err := os.Symlink(outsideFile, targetPath); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(WithChownOwner(-1, -1))
	if err := srv.writeBootstrapFile(BootstrapFile{
		Path:       targetPath,
		Mode:       0o600,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("new-content")),
	}); err != nil {
		t.Fatalf("writeBootstrapFile with a pre-existing symlink at the leaf: want the link replaced, got an error: %v", err)
	}

	outsideContent, readErr := os.ReadFile(outsideFile)
	if readErr != nil {
		t.Fatalf("read outsideFile: %v", readErr)
	}
	if string(outsideContent) != "do-not-touch" {
		t.Fatalf("outsideFile content = %q, want unchanged %q (write-through the leaf symlink)", outsideContent, "do-not-touch")
	}

	info, statErr := os.Lstat(targetPath)
	if statErr != nil {
		t.Fatalf("lstat %s: %v", targetPath, statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is still a symlink after a successful write; want it replaced by a regular file", targetPath)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("%s mode = %v, want a regular file", targetPath, info.Mode())
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read %s: %v", targetPath, err)
	}
	if string(got) != "new-content" {
		t.Errorf("content at %s = %q, want %q", targetPath, got, "new-content")
	}
}

// TestWriteBootstrapFile_RefusesTargetOutsideAgentHome proves a file
// secret's target must resolve inside the agent home, or the bootstrap
// fails closed before anything is written — a root-written,
// workload-chowned file at an arbitrary absolute path (e.g.
// "/etc/ld.so.preload" paired with a workload-writable ".so" it names) is a
// root code-execution primitive, not a legitimate bootstrap target.
func TestWriteBootstrapFile_RefusesTargetOutsideAgentHome(t *testing.T) {
	root := realTempDir(t)
	fakeHome := filepath.Join(root, "home", "scion")
	withAgentHomeFixture(t, fakeHome)
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("outside-home target refused and untouched", func(t *testing.T) {
		targetPath := filepath.Join(root, "etc", "ld.so.preload")
		srv := NewServer(WithChownOwner(-1, -1))
		err := srv.writeBootstrapFile(BootstrapFile{
			Path:       targetPath,
			Mode:       0o644,
			ContentB64: base64.StdEncoding.EncodeToString([]byte("/home/scion/evil.so\n")),
		})
		var pathErr *bootstrapPathError
		if !errors.As(err, &pathErr) {
			t.Fatalf("err = %v (%T), want a *bootstrapPathError", err, err)
		}
		if pathErr.code != codeBootstrapPathOutsideHome {
			t.Errorf("code = %q, want %q", pathErr.code, codeBootstrapPathOutsideHome)
		}
		// server.go's own comment on this check promises the error names
		// only the target's own leaf component, never the full path —
		// pinned here directly, since nothing else in this test file
		// asserts pathErr.path's value for this code.
		if pathErr.path != filepath.Base(targetPath) {
			t.Errorf("pathErr.path = %q, want only the leaf component %q, not the full path", pathErr.path, filepath.Base(targetPath))
		}
		if strings.Contains(pathErr.Error(), root) {
			t.Errorf("err = %q, must not include the full outside-home path", pathErr.Error())
		}
		if _, statErr := os.Stat(targetPath); statErr == nil {
			t.Error("ld.so.preload was written despite being outside the agent home")
		}
	})

	t.Run("home-relative target still works", func(t *testing.T) {
		targetPath := filepath.Join(fakeHome, "app", "x")
		srv := NewServer(WithChownOwner(-1, -1))
		if err := srv.writeBootstrapFile(BootstrapFile{
			Path:       targetPath,
			Mode:       0o640,
			ContentB64: base64.StdEncoding.EncodeToString([]byte("config-value")),
		}); err != nil {
			t.Fatalf("writeBootstrapFile for a home-relative target: %v", err)
		}
		got, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatalf("read %s: %v", targetPath, err)
		}
		if string(got) != "config-value" {
			t.Errorf("content = %q, want %q", got, "config-value")
		}
	})

	t.Run("symlink escape from home refused", func(t *testing.T) {
		outsideTarget := filepath.Join(root, "outside-escape")
		if err := os.MkdirAll(outsideTarget, 0o755); err != nil {
			t.Fatal(err)
		}
		escapeLink := filepath.Join(fakeHome, "escape")
		if err := os.Symlink(outsideTarget, escapeLink); err != nil {
			t.Fatal(err)
		}
		targetPath := filepath.Join(escapeLink, "payload")
		srv := NewServer(WithChownOwner(-1, -1))
		err := srv.writeBootstrapFile(BootstrapFile{
			Path:       targetPath,
			Mode:       0o644,
			ContentB64: base64.StdEncoding.EncodeToString([]byte("payload")),
		})
		if err == nil {
			t.Fatal("writeBootstrapFile through a symlink escaping home: expected an error, got nil")
		}
		if _, statErr := os.Stat(filepath.Join(outsideTarget, "payload")); statErr == nil {
			t.Error("the bootstrap file was written through the symlink into outsideTarget")
		}
	})
}

// TestWriteBootstrapFile_UnixUsernameMismatchFailsSafe documents an
// intentional cross-package asymmetry: agentHomeDir here is always
// util.GetHomeDir("scion") — this package has no notion of
// RunConfig.UnixUsername at all — while pkg/runtime's broker-side bootstrap
// composition (substrate_bootstrap.go) resolves the agent's home via
// util.GetHomeDir(cfg.UnixUsername), whatever unix user a template
// configures. If those two ever disagree (a template sets a UnixUsername
// other than "scion"), every file bootstrap composes for that OTHER home
// directory is rejected here as outside-home — fail-safe (nothing is
// written somewhere this containment check didn't expect), not a silent
// misdirected write, but worth pinning explicitly rather than leaving as an
// unexercised interaction between the two packages.
func TestWriteBootstrapFile_UnixUsernameMismatchFailsSafe(t *testing.T) {
	root := realTempDir(t)
	fakeHome := filepath.Join(root, "home", "scion")
	withAgentHomeFixture(t, fakeHome)
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// The shape pkg/runtime's util.GetHomeDir("otheruser") would produce
	// for a template with UnixUsername: "otheruser" — a sibling of
	// agentHomeDir, not a descendant of it.
	otherUserHome := filepath.Join(root, "home", "otheruser")
	if err := os.MkdirAll(otherUserHome, 0o755); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(otherUserHome, "app", "config.json")

	srv := NewServer(WithChownOwner(-1, -1))
	err := srv.writeBootstrapFile(BootstrapFile{
		Path:       targetPath,
		Mode:       0o644,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("config-for-a-different-user")),
	})
	var pathErr *bootstrapPathError
	if !errors.As(err, &pathErr) || pathErr.code != codeBootstrapPathOutsideHome {
		t.Fatalf("err = %v, want a *bootstrapPathError with code %q (fail safe on a UnixUsername mismatch)", err, codeBootstrapPathOutsideHome)
	}
	if _, statErr := os.Stat(targetPath); statErr == nil {
		t.Error("the bootstrap file was written into the other user's home despite the mismatch")
	}
}

// TestBootstrap_SymlinkedFileRejectionSurfacesAs422WithStableCode proves the
// end-to-end handler path for the binding decision on symlinked targets: a symlink-traversal
// rejection reaches the client as HTTP 422 with the stable
// codeBootstrapPathSymlink code and the rejected file's own path in the
// body, never any file content — and the single-shot bootstrap slot still
// behaves like any other failed bootstrap.
func TestBootstrap_SymlinkedFileRejectionSurfacesAs422WithStableCode(t *testing.T) {
	root := realTempDir(t)
	fakeHome := filepath.Join(root, "home", "scion")
	withAgentHomeFixture(t, fakeHome)
	outsideTarget := filepath.Join(root, "etc")
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	configLink := filepath.Join(fakeHome, ".config")
	if err := os.Symlink(outsideTarget, configLink); err != nil {
		t.Fatal(err)
	}

	targetPath := filepath.Join(configLink, "secret.json")
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		Files: []BootstrapFile{
			{
				Path:       targetPath,
				Mode:       0o600,
				ContentB64: base64.StdEncoding.EncodeToString([]byte("sentinel-secret-content")),
			},
		},
		StartCmd:     "true",
		ControlToken: "tok",
	})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	// Golden, byte-exact body: this is a wire contract with
	// pkg/runtime.parseBootstrapPathError (substrate_bootstrap.go), which
	// splits on the literal "bootstrap file "/" rejected: " substrings and
	// strconv.Unquote's the path in between. pkg/runtime's
	// TestSubstrateRun_BootstrapPathRejectedSurfacesCodeAndPathNoContent
	// feeds a byte-identical fixture back through that parser and
	// cross-references this test by name — a format change here must update
	// both. The trailing "\n" is http.Error's own Fprintln, not this
	// package's format.
	want := codeBootstrapPathSymlink + ": bootstrap file " + strconv.Quote(targetPath) + " rejected: path traverses a symlink\n"
	if body != want {
		t.Errorf("response body = %q, want exact golden body %q", body, want)
	}
	if strings.Contains(body, "sentinel-secret-content") {
		t.Errorf("response body leaked file content: %q", body)
	}
	if _, statErr := os.Stat(filepath.Join(outsideTarget, "secret.json")); statErr == nil {
		t.Error("the bootstrap file was written through the symlink into outsideTarget")
	}
}

// TestBootstrap_OutsideHomeTargetSurfacesAs422WithStableCode is the
// handler-level counterpart to TestWriteBootstrapFile_RefusesTargetOutsideAgentHome:
// every other 422 path-rejection code (symlink, non-directory) already has
// a test that drives it through a real /scion/v1/bootstrap request, but
// codeBootstrapPathOutsideHome did not — only writeBootstrapFile's own
// lower-level return value was ever checked for it.
func TestBootstrap_OutsideHomeTargetSurfacesAs422WithStableCode(t *testing.T) {
	root := realTempDir(t)
	fakeHome := filepath.Join(root, "home", "scion")
	withAgentHomeFixture(t, fakeHome)
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}

	targetPath := filepath.Join(root, "etc", "ld.so.preload")
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		Files: []BootstrapFile{
			{
				Path:       targetPath,
				Mode:       0o644,
				ContentB64: base64.StdEncoding.EncodeToString([]byte("/home/scion/evil.so\n")),
			},
		},
		StartCmd:     "true",
		ControlToken: "tok",
	})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	want := codeBootstrapPathOutsideHome + ": bootstrap file " + strconv.Quote(filepath.Base(targetPath)) + " rejected: target does not resolve inside the agent home\n"
	if body != want {
		t.Errorf("response body = %q, want exact golden body %q", body, want)
	}
	if strings.Contains(body, root) {
		t.Errorf("response body = %q, must not include the full outside-home path", body)
	}
	if _, statErr := os.Stat(targetPath); statErr == nil {
		t.Error("ld.so.preload was written despite being outside the agent home")
	}
}

// TestBootstrap_OversizedBodyRejectedWithoutOOM proves the http.MaxBytesReader
// switch: a body over maxBootstrapBodyBytes fails closed (a definite,
// bounded read error reported as 413) instead of being buffered without
// limit or silently truncated into a confusing 400.
func TestBootstrap_OversizedBodyRejectedWithoutOOM(t *testing.T) {
	srv := NewServer(WithChownOwner(-1, -1))

	oversized := bytes.Repeat([]byte("a"), maxBootstrapBodyBytes+1)
	body := `{"start_cmd":"true","control_token":"tok","env":{"PADDING":"` + string(oversized) + `"}}`

	req := httptest.NewRequest(http.MethodPost, "/scion/v1/bootstrap", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer any-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if srv.isBootstrapped() {
		t.Error("an oversized body must not consume the single-shot bootstrap slot")
	}
}

// TestBootstrap_RejectsRelativePath proves the second stable rejection code: a
// relative bootstrap file path is a distinct rejection from a symlink
// traversal (codeBootstrapPathInvalid, not codeBootstrapPathSymlink), also
// answered as 422 with the offending path in the body.
func TestBootstrap_RejectsRelativePath(t *testing.T) {
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	req := BootstrapRequest{
		Files:        []BootstrapFile{{Path: "relative/path.txt", ContentB64: base64.StdEncoding.EncodeToString([]byte("x"))}},
		StartCmd:     "true",
		ControlToken: "tok",
	}
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for an invalid bootstrap file path", rec.Code)
	}
	body := rec.Body.String()
	// Golden, byte-exact body — see the matching comment in
	// TestBootstrap_SymlinkedFileRejectionSurfacesAs422WithStableCode above
	// for why this must stay byte-identical to what
	// pkg/runtime.parseBootstrapPathError expects, and which broker test
	// mirrors it.
	want := codeBootstrapPathInvalid + ": bootstrap file " + strconv.Quote("relative/path.txt") + " rejected: " + errInvalidBootstrapPath.Error() + "\n"
	if body != want {
		t.Errorf("response body = %q, want exact golden body %q", body, want)
	}
}

// TestBootstrap_RejectedPathLogLineIsSingleLineEvenWithEmbeddedNewline proves
// that handleBootstrap's log line for a rejected bootstrap path logs the
// error's own text exactly once (via redactErr(err), which is
// pathErr.Error() — always quoted via strconv.Quote), never the raw
// pathErr.path a second time. Also logging the raw path unquoted alongside
// the quoted one would let a newline embedded in the path split the log
// line into two, forging a second entry.
func TestBootstrap_RejectedPathLogLineIsSingleLineEvenWithEmbeddedNewline(t *testing.T) {
	tmpLog := filepath.Join(t.TempDir(), "agent.log")
	log.SetLogPath(tmpLog)
	log.SetQuiet(true)
	t.Cleanup(func() {
		log.SetQuiet(false)
		log.SetDebug(false)
		// Restore the log path to TestMain's sandbox (testmain_test.go),
		// not a guessed default. Once t.TempDir() above is removed,
		// log.write's own fallback-on-OpenFile-failure path would
		// otherwise silently rewrite the package-global log path to
		// /tmp/agent.log and force debug mode on for every later test in
		// this binary, defeating TestMain's log sandbox.
		log.SetLogPath(filepath.Join(os.Getenv("HOME"), "agent.log"))
	})

	const forgedPath = "relative/path\nFAKE LOG LINE INJECTED\nmore.txt"
	srv := NewServer(WithChownOwner(-1, -1))
	req := BootstrapRequest{
		Files:        []BootstrapFile{{Path: forgedPath, ContentB64: base64.StdEncoding.EncodeToString([]byte("x"))}},
		StartCmd:     "true",
		ControlToken: "tok",
	}
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}

	data, err := os.ReadFile(tmpLog)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("log file had %d lines, want exactly 1 (the embedded newline split it):\n%s", len(lines), data)
	}
	if !strings.Contains(lines[0], codeBootstrapPathInvalid) {
		t.Errorf("log line = %q, want it to contain the stable code %q", lines[0], codeBootstrapPathInvalid)
	}
	if strings.Contains(string(data), "FAKE LOG LINE INJECTED\n") {
		t.Errorf("log file contains a forged line from the embedded newline: %q", data)
	}
}

// TestBootstrap_WriteFailureLogLineIsSingleLineEvenWithEmbeddedNewline is the
// 500-path sibling of TestBootstrap_RejectedPathLogLineIsSingleLineEvenWithEmbeddedNewline
// above: it proves the generic write-failure log line at server.go ("bootstrap:
// failed to write file %q: %v") also quotes f.Path, so an embedded newline in
// the path cannot split the log line or forge a second one. Invalid base64
// content routes writeBootstrapFile's error through this generic 500 branch
// rather than the *bootstrapPathError 422 branch the other test covers.
func TestBootstrap_WriteFailureLogLineIsSingleLineEvenWithEmbeddedNewline(t *testing.T) {
	tmpLog := filepath.Join(t.TempDir(), "agent.log")
	log.SetLogPath(tmpLog)
	log.SetQuiet(true)
	t.Cleanup(func() {
		log.SetQuiet(false)
		log.SetDebug(false)
		// See TestBootstrap_RejectedPathLogLineIsSingleLineEvenWithEmbeddedNewline
		// above for why this restores TestMain's sandbox path rather than a
		// guessed default.
		log.SetLogPath(filepath.Join(os.Getenv("HOME"), "agent.log"))
	})

	const forgedPath = "/bootstrap\nFAKE LOG LINE INJECTED\nmore.txt"
	srv := NewServer(WithChownOwner(-1, -1))
	req := BootstrapRequest{
		Files:        []BootstrapFile{{Path: forgedPath, ContentB64: "not-valid-base64!!!"}},
		StartCmd:     "true",
		ControlToken: "tok",
	}
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for invalid base64 content", rec.Code)
	}

	data, err := os.ReadFile(tmpLog)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("log file had %d lines, want exactly 1 (the embedded newline split it):\n%s", len(lines), data)
	}
	if !strings.Contains(lines[0], strconv.Quote(forgedPath)) {
		t.Errorf("log line = %q, want it to contain the quoted path %q", lines[0], strconv.Quote(forgedPath))
	}
	if strings.Contains(string(data), "FAKE LOG LINE INJECTED\n") {
		t.Errorf("log file contains a forged line from the embedded newline: %q", data)
	}
}

// TestBootstrap_RejectsNonDirComponentWith422 covers the third path-shape
// rejection: an existing path component that is a plain file, not a
// directory, where a bootstrap file's parent needs to be. Distinct from the
// symlink case, but the same stable "invalid" code as the relative-path
// case above, and also a distinct code from codeBootstrapPathSymlink.
func TestBootstrap_RejectsNonDirComponentWith422(t *testing.T) {
	root := realTempDir(t)
	withAgentHomeFixture(t, root)
	// "plain" is a regular file; a bootstrap file targeting a path below it
	// needs to be created there, but it can't become a directory.
	plain := filepath.Join(root, "plain")
	if err := os.WriteFile(plain, []byte("i-am-a-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(plain, "sub", "f")

	srv := NewServer(WithChownOwner(-1, -1))
	err := srv.writeBootstrapFile(BootstrapFile{
		Path:       targetPath,
		Mode:       0o600,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("x")),
	})
	if err == nil {
		t.Fatal("writeBootstrapFile with a non-directory path component: expected an error, got nil")
	}
	var pathErr *bootstrapPathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("err = %v (%T), want a *bootstrapPathError", err, err)
	}
	if pathErr.code != codeBootstrapPathInvalid {
		t.Errorf("code = %q, want %q", pathErr.code, codeBootstrapPathInvalid)
	}
	if pathErr.path != targetPath {
		t.Errorf("path = %q, want %q", pathErr.path, targetPath)
	}
}

// TestBootstrap_PrivilegeDropCheckerSeesReqEnv proves the ordering
// handleBootstrap's own comment claims but nothing else exercises:
// PrivilegeDropChecker must run *after* req.Env has been applied to the
// process environment, not before — checkPrivilegeDropFeasible's real
// SCION_HOST_UID/GID checks depend on this. If the checker call were placed
// earlier (before the req.Env loop) it would still "fail safe" against
// every other test here (nothing would be configured yet, so a real
// checker would just reject), so only a test that makes the ordering
// itself the assertion — rather than relying on some other check happening
// to fail either way — catches the misordering.
func TestBootstrap_PrivilegeDropCheckerSeesReqEnv(t *testing.T) {
	const testVar = "SCION_SUBSTRATE_CHECKER_ORDERING_TEST_VAR"
	t.Setenv(testVar, "")

	var sawValue string
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithPrivilegeDropChecker(func() error {
			sawValue = os.Getenv(testVar)
			return nil
		}),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		Env:          map[string]string{testVar: "from-req-env"},
		StartCmd:     "true",
		ControlToken: "tok",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if sawValue != "from-req-env" {
		t.Errorf("PrivilegeDropChecker saw %s=%q, want %q — it must run after req.Env is applied to the process environment", testVar, sawValue, "from-req-env")
	}
}

// TestBootstrap_ReqEnvNeverSetsPathOrDangerousVarsOnPID1 proves the req.Env
// loop refuses to apply PATH, LD_*, BASH_ENV, ENV, or IFS onto
// substrate-serve's own PID 1 environment, regardless of what a bootstrap
// request supplies — those specifically influence how this still-root
// process (and anything it execs while inheriting its environment) resolves
// and runs code, unlike an ordinary workload variable, which req.Env must
// still be able to set (see TestBootstrap_WritesFilesWithParentDirsAndEnv).
func TestBootstrap_ReqEnvNeverSetsPathOrDangerousVarsOnPID1(t *testing.T) {
	dangerous := map[string]string{
		"PATH":            "/workload-owned/bin:/usr/bin",
		"LD_PRELOAD":      "/workload-owned/evil.so",
		"LD_LIBRARY_PATH": "/workload-owned",
		"BASH_ENV":        "/workload-owned/evil.sh",
		"ENV":             "/workload-owned/evil.sh",
		"IFS":             ":",
	}
	const benignVar = "SCION_SUBSTRATE_REQENV_BENIGN_TEST_VAR"

	before := make(map[string]string, len(dangerous))
	for k := range dangerous {
		before[k] = os.Getenv(k)
		t.Cleanup(func(k, v string) func() { return func() { _ = os.Setenv(k, v) } }(k, before[k]))
	}
	t.Setenv(benignVar, "")

	reqEnv := map[string]string{benignVar: "from-req-env"}
	for k, v := range dangerous {
		reqEnv[k] = v
	}

	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		Env:          reqEnv,
		StartCmd:     "true",
		ControlToken: "tok",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	for k, v := range dangerous {
		if got := os.Getenv(k); got == v {
			t.Errorf("%s = %q after bootstrap; req.Env must never set this on PID 1's own environment", k, got)
		}
	}
	if got := os.Getenv(benignVar); got != "from-req-env" {
		t.Errorf("%s = %q, want %q — an ordinary workload var must still pass through", benignVar, got, "from-req-env")
	}
}

// TestBootstrap_RootfsFixupRunsBeforePrivilegeDropChecker proves call site 2
// (see RootfsFixup's doc comment): handleBootstrap must run it before the
// privilege-drop precondition, which depends on the rootfs it corrects
// (traversability, home ownership).
func TestBootstrap_RootfsFixupRunsBeforePrivilegeDropChecker(t *testing.T) {
	var order []string
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithRootfsFixup(func() { order = append(order, "rootfsFixup") }),
		WithPrivilegeDropChecker(func() error {
			order = append(order, "privilegeDropChecker")
			return nil
		}),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd:     "true",
		ControlToken: "tok",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	want := []string{"rootfsFixup", "privilegeDropChecker"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Errorf("call order = %v, want %v", order, want)
	}
}

// TestBootstrap_PrivilegeDropPreconditionFails_RejectsWithoutStartingInit
// proves the serve side of PrivilegeDropChecker's contract: when it rejects
// the bootstrap, the response must be non-2xx
// (so the broker's postBootstrap treats it as a failure and Run's existing
// cleanup deletes the actor — see PrivilegeDropChecker's doc comment) and
// the init runner must never be invoked, since the harness must not start.
func TestBootstrap_PrivilegeDropPreconditionFails_RejectsWithoutStartingInit(t *testing.T) {
	var initCalled bool
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithPrivilegeDropChecker(func() error {
			return errors.New("CAP_SETUID absent and this message must never reach the client")
		}),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int {
			initCalled = true
			return 0
		}),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd:     "true",
		ControlToken: "tok",
	})

	if rec.Code == http.StatusOK || rec.Code < 400 {
		t.Fatalf("status = %d, want a non-2xx failure", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "CAP_SETUID") {
		t.Errorf("response body leaked the checker's underlying error: %q", rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != privilegeDropPreconditionFailedMsg {
		t.Errorf("response body = %q, want the fixed message %q", got, privilegeDropPreconditionFailedMsg)
	}

	// Give any wrongly-started goroutine a moment to flip the flag before
	// asserting it never did.
	time.Sleep(20 * time.Millisecond)
	if initCalled {
		t.Error("init runner was invoked despite the privilege-drop precondition failing; the harness must never start")
	}
}

// TestBootstrap_PrivilegeDropPreconditionPasses_StartsInit is the control
// for the test above: a nil-returning checker must not change today's
// behaviour (200, init runner invoked).
func TestBootstrap_PrivilegeDropPreconditionPasses_StartsInit(t *testing.T) {
	initCh := make(chan struct{}, 1)
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithPrivilegeDropChecker(func() error { return nil }),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int {
			initCh <- struct{}{}
			return 0
		}),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd:     "true",
		ControlToken: "tok",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	select {
	case <-initCh:
	case <-time.After(2 * time.Second):
		t.Error("init runner was never invoked despite the precondition passing")
	}
}

// TestBootstrap_FailedBootstrapNeverPublishesControlToken is the regression
// test for publishing the control token only after every step that can
// still fail a bootstrap request has succeeded: a request that fails the
// privilege-drop precondition must leave /exec unauthenticatable with the
// ControlToken it carried, even though the single-use bootstrap slot is
// still claimed (a second bootstrap attempt still gets 409, not a retry).
// Without this, the token from a bootstrap that never actually completed —
// no init runner ever started, the workload home never finished staging —
// would already work against /exec during the window before the broker
// acts on the non-2xx response and deletes the actor.
func TestBootstrap_FailedBootstrapNeverPublishesControlToken(t *testing.T) {
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithPrivilegeDropChecker(func() error {
			return errors.New("CAP_SETUID absent")
		}),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd:     "true",
		ControlToken: "the-token-from-the-failed-request",
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a non-2xx failure", rec.Code)
	}

	execRec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "the-token-from-the-failed-request", ExecRequest{
		Argv: []string{"echo", "hi"},
	})
	if execRec.Code != http.StatusUnauthorized {
		t.Fatalf("exec status = %d, want 401: the failed bootstrap's ControlToken must never authenticate /exec (body=%s)", execRec.Code, execRec.Body.String())
	}

	// The single-use slot is still claimed: a second bootstrap attempt
	// must not be allowed to retry with a new token either.
	retryRec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd:     "true",
		ControlToken: "a-different-token",
	})
	if retryRec.Code != http.StatusConflict {
		t.Fatalf("retry status = %d, want 409 (no bootstrap retry after a failure)", retryRec.Code)
	}
}

func TestExec_RequiresControlToken(t *testing.T) {
	srv := NewServer(WithChownOwner(-1, -1))
	// Not bootstrapped yet: no control token exists, so exec must always 401.
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "anything", ExecRequest{Argv: []string{"echo", "hi"}})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 before bootstrap", rec.Code)
	}
}

func TestExec_WrongTokenRejected(t *testing.T) {
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	bootstrap := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd: "true", ControlToken: "the-real-token",
	})
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200", bootstrap.Code)
	}

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "wrong-token", ExecRequest{Argv: []string{"echo", "hi"}})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for wrong control token", rec.Code)
	}
}

func TestExec_SucceedsWithCorrectToken(t *testing.T) {
	withExecUserAsCurrent(t)
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd: "true", ControlToken: "the-real-token",
	})

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "the-real-token", ExecRequest{
		Argv: []string{"echo", "hello-substrate"},
		User: "scion",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON[ExecResponse](t, rec)
	if got.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0 (stderr=%q)", got.ExitCode, got.Stderr)
	}
	if !bytes.Contains([]byte(got.Stdout), []byte("hello-substrate")) {
		t.Errorf("stdout = %q, want it to contain %q", got.Stdout, "hello-substrate")
	}
	if got.Truncated {
		t.Error("truncated = true, want false for small output")
	}
}

func TestExec_RejectsUnknownUser(t *testing.T) {
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd: "true", ControlToken: "tok",
	})

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "tok", ExecRequest{
		Argv: []string{"echo", "hi"},
		User: "nobody",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unsupported user", rec.Code)
	}
}

// TestExec_RejectsRootUser is the regression test for refusing "root" as an
// exec user server-side: /exec exists to run the agent workload, which
// always runs as the unprivileged scion user, so a caller asking for root
// must be refused the same way an unknown user already is, not silently
// honored.
func TestExec_RejectsRootUser(t *testing.T) {
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd: "true", ControlToken: "tok",
	})

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "tok", ExecRequest{
		Argv: []string{"echo", "hi"},
		User: "root",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for user \"root\"", rec.Code)
	}
}

// TestClampExecTimeout_OverflowClampsRatherThanRemovingCap is the regression
// test for bounding ExecRequest.TimeoutS before multiplying it by
// time.Second: a caller-supplied TimeoutS large enough to overflow
// time.Duration's int64 range, if multiplied first, wraps into a small or
// negative duration that would slip past runExec's "timeout > 0" check and
// run with no timeout at all. clampExecTimeout must instead clamp any such
// value to maxExecTimeout, the same as any other too-large value — proven
// here by comparing against math.MaxInt (the overflow case) directly,
// rather than only through the HTTP handler.
func TestClampExecTimeout_OverflowClampsRatherThanRemovingCap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timeoutS int
		want     time.Duration
	}{
		{"unspecified", 0, defaultExecTimeout},
		{"negative", -1, defaultExecTimeout},
		{"normal", 5, 5 * time.Second},
		{"exactly at cap", maxExecTimeoutSeconds, maxExecTimeout},
		{"over cap but no overflow", maxExecTimeoutSeconds + 1, maxExecTimeout},
		{"overflow (MaxInt)", math.MaxInt, maxExecTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := clampExecTimeout(tc.timeoutS)
			if got != tc.want {
				t.Errorf("clampExecTimeout(%d) = %v, want %v", tc.timeoutS, got, tc.want)
			}
			if got <= 0 {
				t.Errorf("clampExecTimeout(%d) = %v, want a positive duration (timeout removed entirely)", tc.timeoutS, got)
			}
		})
	}
}

func TestExec_NonZeroExitCodePropagated(t *testing.T) {
	withExecUserAsCurrent(t)
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd: "true", ControlToken: "tok",
	})

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "tok", ExecRequest{
		Argv: []string{"sh", "-c", "exit 7"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON[ExecResponse](t, rec)
	if got.ExitCode != 7 {
		t.Errorf("exit_code = %d, want 7", got.ExitCode)
	}
}

// -----------------------------------------------------------------------
// Exec stdin: a secret travels via ExecRequest.Stdin, never
// via Argv, through the real handler and a real exec'd command.
// -----------------------------------------------------------------------

func TestExec_StdinRoundTripsThroughRealCommand(t *testing.T) {
	const secret = "S3CR3T-1894-EXEC-STDIN"
	withExecUserAsCurrent(t)

	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd: "true", ControlToken: "tok",
	})

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "tok", ExecRequest{
		Argv:  []string{"cat"},
		User:  "scion",
		Stdin: []byte(secret),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON[ExecResponse](t, rec)
	if got.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0 (stderr=%q)", got.ExitCode, got.Stderr)
	}
	if got.Stdout != secret {
		t.Errorf("stdout = %q, want %q (cat should echo stdin verbatim)", got.Stdout, secret)
	}
	if !got.StdinSupported {
		t.Error("stdin_supported = false, want true: the server handled this request")
	}
}

func TestExec_StdinSupportedSetEvenWithoutStdin(t *testing.T) {
	withExecUserAsCurrent(t)
	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd: "true", ControlToken: "tok",
	})

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "tok", ExecRequest{
		Argv: []string{"echo", "hi"},
		User: "scion",
	})
	got := decodeJSON[ExecResponse](t, rec)
	if !got.StdinSupported {
		t.Error("stdin_supported = false, want true on every handled response, including plain Exec with no Stdin field")
	}
}

// TestExec_StdinNeverReachesSpawnedArgv spies on the real
// execCommandContext seam to inspect exactly what argv the server hands to
// the OS exec call, while still exercising the real handleExec -> runExec
// path end to end. The secret is sent only via ExecRequest.Stdin; Argv is
// fixed and secret-free by construction, and this pins that nothing in
// runExec's command construction ever folds Stdin into the command line.
func TestExec_StdinNeverReachesSpawnedArgv(t *testing.T) {
	const secret = "S3CR3T-MUST-NOT-BE-IN-ARGV"
	withExecUserAsCurrent(t)

	var captured [][]string
	orig := execCommandContext
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		all := append([]string{name}, arg...)
		captured = append(captured, all)
		return orig(ctx, name, arg...)
	}
	t.Cleanup(func() { execCommandContext = orig })

	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd: "true", ControlToken: "tok",
	})

	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "tok", ExecRequest{
		Argv:  []string{"cat"},
		User:  "scion",
		Stdin: []byte(secret),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON[ExecResponse](t, rec)
	if got.Stdout != secret {
		t.Fatalf("stdout = %q, want %q (sanity check that stdin was actually delivered)", got.Stdout, secret)
	}
	if len(captured) == 0 {
		t.Fatal("execCommandContext was never called")
	}
	for _, args := range captured {
		for _, a := range args {
			if strings.Contains(a, secret) {
				t.Errorf("secret leaked into the spawned command's argv: %q", args)
			}
		}
	}
}

func TestExec_OversizeStdinRejectedWithoutEchoing(t *testing.T) {
	const secret = "S3CR3T-OVERSIZE-MARKER"

	srv := NewServer(
		WithChownOwner(-1, -1),
		WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/bootstrap", "any-token", BootstrapRequest{
		StartCmd: "true", ControlToken: "tok",
	})

	// Comfortably over MaxExecBodyBytes once the JSON envelope and stdin's
	// base64 (4/3) expansion are accounted for.
	oversize := secret + strings.Repeat("A", MaxExecBodyBytes)
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/scion/v1/exec", "tok", ExecRequest{
		Argv:  []string{"cat"},
		Stdin: []byte(oversize),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an oversize exec request body", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("oversize-stdin rejection echoed the payload back: %q", rec.Body.String())
	}
}
