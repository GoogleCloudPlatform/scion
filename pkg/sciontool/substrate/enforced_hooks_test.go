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
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// withEnforcedHooksFixture points enforcedHooksHomePrefix and
// enforcedHooksDir at throwaway directories under a real temp dir for the
// duration of the test, restoring both afterward. Production never
// reassigns either var; this is the seam that lets a test drive the real
// redirect/clear logic without writing to "/home/scion" or the real,
// root-owned "/run/scion/hooks".
func withEnforcedHooksFixture(t *testing.T) (homePrefix, hooksDir string) {
	t.Helper()
	base := realTempDir(t)
	homePrefix = filepath.Join(base, "home", "scion", ".scion", "hooks")
	hooksDir = filepath.Join(base, "run", "scion", "hooks")

	origPrefix, origDir := enforcedHooksHomePrefix, enforcedHooksDir
	enforcedHooksHomePrefix = homePrefix
	enforcedHooksDir = hooksDir
	t.Cleanup(func() {
		enforcedHooksHomePrefix = origPrefix
		enforcedHooksDir = origDir
	})
	return homePrefix, hooksDir
}

func TestRedirectEnforcedHooksPath_MatchesUnderPrefix(t *testing.T) {
	homePrefix, hooksDir := withEnforcedHooksFixture(t)

	got, ok := redirectEnforcedHooksPath(filepath.Join(homePrefix, "pre-start.d", "20-harness-provision"))
	if !ok {
		t.Fatal("expected a path under the hooks home prefix to be redirected")
	}
	want := filepath.Join(hooksDir, "pre-start.d", "20-harness-provision")
	if got != want {
		t.Errorf("redirected path = %q, want %q", got, want)
	}
}

func TestRedirectEnforcedHooksPath_LeavesOtherPathsAlone(t *testing.T) {
	homePrefix, _ := withEnforcedHooksFixture(t)

	tests := []string{
		filepath.Join(filepath.Dir(homePrefix), "harness", "manifest.json"),
		filepath.Join(filepath.Dir(filepath.Dir(homePrefix)), ".gitconfig"),
		"/etc/scion/hooks/pre-start.d/system-hook",
		homePrefix, // the bare prefix itself, nothing under it
		homePrefix + "-sibling-that-happens-to-share-a-prefix/pre-start.d/x",
	}
	for _, path := range tests {
		if _, ok := redirectEnforcedHooksPath(path); ok {
			t.Errorf("redirectEnforcedHooksPath(%q) matched, want no match", path)
		}
	}
}

func TestRedirectEnforcedHooksPath_DotDotIsResolvedByCleanBeforeMatching(t *testing.T) {
	homePrefix, hooksDir := withEnforcedHooksFixture(t)

	// A ".." trick that lexically resolves back inside the prefix must still
	// redirect (matching the writeBootstrapFile contract: the caller always
	// calls this with an already-Cleaned path).
	dirty := filepath.Join(homePrefix, "pre-start.d", "..", "pre-start.d", "20-harness-provision")
	got, ok := redirectEnforcedHooksPath(filepath.Clean(dirty))
	if !ok {
		t.Fatal("expected the cleaned path to redirect")
	}
	want := filepath.Join(hooksDir, "pre-start.d", "20-harness-provision")
	if got != want {
		t.Errorf("redirected path = %q, want %q", got, want)
	}

	// A ".." trick that lexically escapes the prefix entirely must NOT
	// redirect — it names a different, unredirected location instead of
	// spoofing its way into the root-owned dir.
	escaped := filepath.Clean(filepath.Join(homePrefix, "..", "not-hooks", "evil"))
	if _, ok := redirectEnforcedHooksPath(escaped); ok {
		t.Errorf("redirectEnforcedHooksPath(%q) matched after escaping the prefix, want no match", escaped)
	}
}

// TestWriteBootstrapFile_EnforcedHooksPathNeverChowned proves the two
// properties enforced-mode redirection exists for: the file lands under the
// dedicated hooks dir (not the workload's home), and it is never chowned to
// the workload — even though this Server is configured with a real
// chownUID/chownGID, exactly like a production substrate-serve instance.
func TestWriteBootstrapFile_EnforcedHooksPathNeverChowned(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ownership check uses syscall.Stat_t (Linux only)")
	}
	homePrefix, hooksDir := withEnforcedHooksFixture(t)

	srv := NewServer(WithChownOwner(1000, 1000))
	target := filepath.Join(homePrefix, "pre-start.d", "20-harness-provision")
	content := []byte("#!/bin/sh\nexit 0\n")
	f := BootstrapFile{
		Path:       target,
		Mode:       0o755,
		ContentB64: base64.StdEncoding.EncodeToString(content),
	}
	if err := srv.writeBootstrapFile(f); err != nil {
		t.Fatalf("writeBootstrapFile: %v", err)
	}

	wantPath := filepath.Join(hooksDir, "pre-start.d", "20-harness-provision")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("expected nothing written at the original %q, err=%v", target, err)
	}
	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat redirected path %q: %v", wantPath, err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no raw stat available")
	}
	self := uint32(os.Getuid())
	if st.Uid != self {
		t.Errorf("owner uid = %d, want %d (the test process's own uid — never chowned to the configured workload uid 1000)", st.Uid, self)
	}
	if st.Uid == 1000 {
		t.Error("file was chowned to the workload uid; enforced-hooks content must never be chowned")
	}

	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}
}

// TestWriteBootstrapFile_NonHooksPathStillChowned is
// TestWriteBootstrapFile_EnforcedHooksPathNeverChowned's control: a bootstrap
// file OUTSIDE the hooks prefix, on the exact same Server, still gets
// chowned exactly as before — the redirect must not accidentally widen to
// every bootstrap-written file.
func TestWriteBootstrapFile_NonHooksPathStillChowned(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ownership check uses syscall.Stat_t (Linux only)")
	}
	_, _ = withEnforcedHooksFixture(t)
	dir := realTempDir(t)

	self := os.Getuid()
	srv := NewServer(WithChownOwner(self, os.Getgid()))
	target := filepath.Join(dir, "harness", "manifest.json")
	f := BootstrapFile{
		Path:       target,
		Mode:       0o644,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("{}")),
	}
	if err := srv.writeBootstrapFile(f); err != nil {
		t.Fatalf("writeBootstrapFile: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if st.Uid != uint32(self) {
		t.Errorf("owner uid = %d, want %d (chown must still apply outside the hooks prefix)", st.Uid, self)
	}
}

func TestClearEnforcedHooksDir_RemovesStaleContentButNotTheDirItself(t *testing.T) {
	_, hooksDir := withEnforcedHooksFixture(t)
	if err := os.MkdirAll(filepath.Join(hooksDir, "pre-start.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(hooksDir, "pre-start.d", "30-project-custom")
	if err := os.WriteFile(stale, []byte("stale"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := clearEnforcedHooksDir(); err != nil {
		t.Fatalf("clearEnforcedHooksDir: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("expected stale hook to be removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, "pre-start.d")); !os.IsNotExist(err) {
		t.Errorf("expected the now-empty pre-start.d dir to be removed too, err=%v", err)
	}
}

func TestClearEnforcedHooksDir_MissingDirIsNotAnError(t *testing.T) {
	withEnforcedHooksFixture(t)
	if err := clearEnforcedHooksDir(); err != nil {
		t.Fatalf("clearEnforcedHooksDir on a never-created dir: %v", err)
	}
}

// TestBootstrap_ClearsStaleEnforcedHooksContentBeforeWriting drives the real
// HTTP handler (not writeBootstrapFile directly) to prove handleBootstrap's
// call site clears any pre-existing content under the enforced hooks dir
// before this bootstrap's own files land — "re-bootstrap: clear and rewrite
// on each bootstrap, so stale hooks never run".
func TestBootstrap_ClearsStaleEnforcedHooksContentBeforeWriting(t *testing.T) {
	homePrefix, hooksDir := withEnforcedHooksFixture(t)

	stalePath := filepath.Join(hooksDir, "pre-start.d", "99-stale-from-a-previous-bootstrap")
	if err := os.MkdirAll(filepath.Dir(stalePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}

	freshContent := []byte("#!/bin/sh\nexit 0\n")
	req := BootstrapRequest{
		Files: []BootstrapFile{
			{
				Path:       filepath.Join(homePrefix, "pre-start.d", "20-harness-provision"),
				Mode:       0o755,
				ContentB64: base64.StdEncoding.EncodeToString(freshContent),
			},
		},
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

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("expected stale hook from a previous bootstrap to be gone, err=%v", err)
	}
	freshPath := filepath.Join(hooksDir, "pre-start.d", "20-harness-provision")
	got, err := os.ReadFile(freshPath)
	if err != nil {
		t.Fatalf("expected this bootstrap's own file to exist: %v", err)
	}
	if string(got) != string(freshContent) {
		t.Errorf("content = %q, want %q", got, freshContent)
	}
}

// waitInitCalled polls a channel-backed init-started signal for a bounded
// time and reports whether it fired. handleBootstrap starts runInit in a
// goroutine, so reading a plain bool immediately after the HTTP response
// returns cannot reliably observe whether it ran: this polls briefly instead
// of racing that goroutine outright. The status-code and no-file-written
// assertions alongside this one carry the real weight of each test; this is
// a best-effort strengthening, not the only signal.
func waitInitCalled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

// TestBootstrap_ClearFailureAbortsBootstrapBeforeAnyWriteOrInit proves the
// fail-closed contract: when clearEnforcedHooksDir fails (here, because
// enforcedHooksDir is a symlink — a stand-in for any clear failure, e.g.
// EIO/EROFS/EPERM on a real deployment), handleBootstrap must abort BEFORE
// writing any bootstrap file and BEFORE starting init, not log-and-continue.
// A stale, still root-owned hook a failed clear left behind must never get
// the chance to run. A symlinked root is specifically a *bootstrapPathError,
// so this covers the 422 branch; TestBootstrap_ClearFailureGenericErrorAborts
// BootstrapBeforeAnyWriteOrInit below covers the generic-error 500 branch.
func TestBootstrap_ClearFailureAbortsBootstrapBeforeAnyWriteOrInit(t *testing.T) {
	homePrefix, _ := withEnforcedHooksFixture(t)

	// Make the clear itself fail: point enforcedHooksDir at a symlink
	// (clearDirContents refuses to operate through a symlinked root, per
	// TestClearEnforcedHooksDir_RefusesSymlinkedRoot below) rather than a
	// real directory. withEnforcedHooksFixture's own cleanup still restores
	// the true original enforcedHooksDir afterward, since it captured that
	// value before this reassignment ever ran.
	base := realTempDir(t)
	real := filepath.Join(base, "real-hooks-dir")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "hooks-dir-symlink")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	enforcedHooksDir = link

	initCalled := make(chan struct{}, 1)
	req := BootstrapRequest{
		Files: []BootstrapFile{
			{
				Path:       filepath.Join(homePrefix, "pre-start.d", "20-harness-provision"),
				Mode:       0o755,
				ContentB64: base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\nexit 0\n")),
			},
		},
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
		t.Fatalf("bootstrap status = %d, want 422 (a symlinked enforced-hooks root is a bootstrapPathError)", rec.Code)
	}
	if waitInitCalled(initCalled) {
		t.Error("init must never start when the enforced-hooks clear failed")
	}
	// No file should have been written under the real (non-symlinked)
	// target either — the clear failure must abort before the file loop.
	if _, err := os.Stat(filepath.Join(real, "pre-start.d", "20-harness-provision")); !os.IsNotExist(err) {
		t.Errorf("expected no file written past a failed clear, stat err=%v", err)
	}
}

// TestBootstrap_ClearFailureGenericErrorAbortsBootstrapBeforeAnyWriteOrInit
// is the 500-branch sibling of the 422 test above: a clear failure that is
// NOT a *bootstrapPathError (here, a permission error removing a stale entry
// — the failure mode that actually motivated the fail-closed fix: EIO/EROFS/
// EPERM/EACCES on a real deployment) must still abort the bootstrap before
// any file is written or init starts, answering 500, not 422 and not 200.
// Skipped as root: root bypasses the DAC permission check this fixture
// depends on to make the removal fail in the first place.
func TestBootstrap_ClearFailureGenericErrorAbortsBootstrapBeforeAnyWriteOrInit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory-permission check this fixture uses to make the clear fail")
	}
	homePrefix, hooksDir := withEnforcedHooksFixture(t)

	// Stage a stale entry, then remove write permission on its parent so
	// clearDirContents' os.Remove of the entry fails with EACCES — a
	// generic error, not a *bootstrapPathError — once the clear tries to
	// remove it.
	staleDir := filepath.Join(hooksDir, "pre-start.d")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(staleDir, "30-project-custom")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(staleDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(staleDir, 0o755) })

	initCalled := make(chan struct{}, 1)
	req := BootstrapRequest{
		Files: []BootstrapFile{
			{
				Path:       filepath.Join(homePrefix, "pre-start.d", "20-harness-provision"),
				Mode:       0o755,
				ContentB64: base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\nexit 0\n")),
			},
		},
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
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("bootstrap status = %d, want 500 (a permission error clearing a stale entry is not a bootstrapPathError)", rec.Code)
	}
	if waitInitCalled(initCalled) {
		t.Error("init must never start when the enforced-hooks clear failed")
	}
	if _, err := os.Stat(filepath.Join(hooksDir, "pre-start.d", "20-harness-provision")); !os.IsNotExist(err) {
		t.Errorf("expected no file written past a failed clear, stat err=%v", err)
	}
	// The stale entry must still be there — the clear failed, it did not
	// silently succeed by skipping the unremovable entry.
	if _, err := os.Stat(stalePath); err != nil {
		t.Errorf("expected the stale entry to remain after a failed clear: %v", err)
	}
}

func TestClearEnforcedHooksDir_RefusesSymlinkedRoot(t *testing.T) {
	base := realTempDir(t)
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "hooks-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	orig := enforcedHooksDir
	enforcedHooksDir = link
	t.Cleanup(func() { enforcedHooksDir = orig })

	if err := clearEnforcedHooksDir(); err == nil {
		t.Fatal("expected clearEnforcedHooksDir to refuse a symlinked hooks dir")
	}
	if _, err := os.Stat(real); err != nil {
		t.Errorf("the symlink target must be left untouched: %v", err)
	}
}
