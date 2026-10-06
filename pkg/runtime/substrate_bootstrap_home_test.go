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
	"encoding/base64"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// decodeBootstrapFileContent is a small test helper: buildBootstrapFiles and
// homeBootstrapFiles only expose ContentB64 on the wire type; tests need the
// raw bytes back to assert on content.
func decodeBootstrapFileContent(t *testing.T, f bootstrapFile) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(f.ContentB64)
	if err != nil {
		t.Fatalf("content_b64 for %s did not decode: %v", f.Path, err)
	}
	return string(data)
}

func findBootstrapFile(files []bootstrapFile, path string) (bootstrapFile, bool) {
	for _, f := range files {
		if f.Path == path {
			return f, true
		}
	}
	return bootstrapFile{}, false
}

// -----------------------------------------------------------------------
// homeBootstrapFiles: walk, precedence via mode/paths, skip-and-count.
// -----------------------------------------------------------------------

func TestHomeBootstrapFiles_NestedFileKeepsRelPathAndMode(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(nested, []byte(`{"a":1}`), 0o640); err != nil {
		t.Fatal(err)
	}

	files, err := homeBootstrapFiles(home, "/home/scion")
	if err != nil {
		t.Fatalf("homeBootstrapFiles: %v", err)
	}

	f, ok := findBootstrapFile(files, "/home/scion/.claude/settings.json")
	if !ok {
		t.Fatalf("expected an entry for /home/scion/.claude/settings.json, got %+v", files)
	}
	if f.Mode != 0o640 {
		t.Errorf("mode = %o, want %o", f.Mode, 0o640)
	}
	if got := decodeBootstrapFileContent(t, f); got != `{"a":1}` {
		t.Errorf("content = %q, want %q", got, `{"a":1}`)
	}
}

func TestHomeBootstrapFiles_SkipsSymlinkFifoSocketAndCountsThem(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("fifo/unix-socket fixtures require Linux (syscall.Mkfifo, AF_UNIX bind)")
	}
	home := t.TempDir()

	// A regular file, which must be shipped.
	regularPath := filepath.Join(home, "regular.txt")
	if err := os.WriteFile(regularPath, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink to that regular file — must be skipped, never followed.
	symlinkPath := filepath.Join(home, "link.txt")
	if err := os.Symlink(regularPath, symlinkPath); err != nil {
		t.Fatal(err)
	}

	// A fifo — must be skipped (reading one could block).
	fifoPath := filepath.Join(home, "a.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}

	// A unix domain socket — must be skipped.
	sockPath := filepath.Join(home, "a.sock")
	if err := syscall.Bind(mustSocketFD(t), &syscall.SockaddrUnix{Name: sockPath}); err != nil {
		t.Fatalf("bind unix socket: %v", err)
	}

	files, err := homeBootstrapFiles(home, "/home/scion")
	if err != nil {
		t.Fatalf("homeBootstrapFiles: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("files = %+v, want exactly the one regular file", files)
	}
	if files[0].Path != "/home/scion/regular.txt" {
		t.Errorf("shipped path = %q, want /home/scion/regular.txt", files[0].Path)
	}
	if got := decodeBootstrapFileContent(t, files[0]); got != "kept" {
		t.Errorf("content = %q, want %q", got, "kept")
	}

	// Nothing outside homeDir was read: the symlink's target (also inside
	// homeDir here) is irrelevant, but prove the symlink entry itself never
	// produced a second files entry under any path.
	for _, f := range files {
		if strings.Contains(f.Path, "link.txt") {
			t.Errorf("symlink entry was shipped: %+v", f)
		}
	}
}

// mustSocketFD creates a unix domain socket fd for the bind() call in the
// test above and registers cleanup; syscall.Bind needs a raw fd, not the
// higher-level net.Listen("unix", ...) which also creates the file itself
// (equally valid, but this keeps the fixture explicit about the socket type
// being created).
func mustSocketFD(t *testing.T) int {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	return fd
}

func TestHomeBootstrapFiles_EmptyHomeDirIsNoop(t *testing.T) {
	files, err := homeBootstrapFiles("", "/home/scion")
	if err != nil {
		t.Fatalf("homeBootstrapFiles(\"\", ...) = %v, want nil error", err)
	}
	if files != nil {
		t.Errorf("homeBootstrapFiles(\"\", ...) = %+v, want nil", files)
	}
}

// TestHomeBootstrapFiles_ExistingEmptyHomeDirIsNoop covers a HomeDir that
// exists (unlike TestHomeBootstrapFiles_EmptyHomeDirIsNoop, which tests
// homeDir == "") but has nothing in it — a legitimate case (e.g. a template
// home that composed to nothing), distinct from "no home was composed" and
// from "home is missing."
func TestHomeBootstrapFiles_ExistingEmptyHomeDirIsNoop(t *testing.T) {
	home := t.TempDir()

	files, err := homeBootstrapFiles(home, "/home/scion")
	if err != nil {
		t.Fatalf("homeBootstrapFiles on an existing, empty HomeDir: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("files = %+v, want none", files)
	}
}

// TestHomeBootstrapFiles_SymlinkedDirectoryOutsideHomeIsNotDescendedInto
// proves the design's "nothing outside HomeDir is read" and "no descent"
// guarantees for a symlink that points at a *directory* (as opposed to the
// existing regular-file-symlink coverage), especially one pointing outside
// HomeDir entirely: the walk must skip the symlink entry itself and must
// never produce a bootstrap entry sourced from the directory it points to.
func TestHomeBootstrapFiles_SymlinkedDirectoryOutsideHomeIsNotDescendedInto(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secretOutside := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretOutside, []byte("outside-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink inside home pointing at the outside directory.
	linkDir := filepath.Join(home, "linkdir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatal(err)
	}

	files, err := homeBootstrapFiles(home, "/home/scion")
	if err != nil {
		t.Fatalf("homeBootstrapFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("files = %+v, want none (the only home entry is a symlinked directory, which must be skipped, not descended into)", files)
	}
	for _, f := range files {
		if got := decodeBootstrapFileContent(t, f); got == "outside-content" {
			t.Errorf("a file was shipped from outside HomeDir through the symlinked directory: %+v", f)
		}
	}
}

func TestHomeBootstrapFiles_MissingHomeDirIsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := homeBootstrapFiles(missing, "/home/scion")
	if err == nil {
		t.Fatal("homeBootstrapFiles with a missing HomeDir: expected an error, got nil")
	}
}

// -----------------------------------------------------------------------
// buildBootstrapFiles: precedence, dedup, cap, empty-HomeDir golden.
// -----------------------------------------------------------------------

func TestBuildBootstrapFiles_EmptyHomeDirGolden(t *testing.T) {
	authFilePath := filepath.Join(t.TempDir(), "auth-token")
	if err := os.WriteFile(authFilePath, []byte("auth-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := RunConfig{
		UnixUsername: "scion",
		ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{
				{SourcePath: authFilePath, ContainerPath: "~/.creds/auth-file"},
			},
		},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "GITHUB_TOKEN", Type: "file", Target: "~/.git-credentials", Value: "secret-content"},
		},
	}

	files, err := buildBootstrapFiles(cfg)
	if err != nil {
		t.Fatalf("buildBootstrapFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v, want exactly 2 (unchanged from pre-home-delivery behavior)", files)
	}

	auth, ok := findBootstrapFile(files, "/home/scion/.creds/auth-file")
	if !ok {
		t.Fatalf("missing auth file entry: %+v", files)
	}
	if auth.Mode != defaultFileMode {
		t.Errorf("auth file mode = %o, want %o", auth.Mode, defaultFileMode)
	}
	if got := decodeBootstrapFileContent(t, auth); got != "auth-content" {
		t.Errorf("auth file content = %q, want %q", got, "auth-content")
	}

	secret, ok := findBootstrapFile(files, "/home/scion/.git-credentials")
	if !ok {
		t.Fatalf("missing secret file entry: %+v", files)
	}
	if got := decodeBootstrapFileContent(t, secret); got != "secret-content" {
		t.Errorf("secret file content = %q, want %q", got, "secret-content")
	}
}

func TestBuildBootstrapFiles_PrecedenceHomeAuthSecret(t *testing.T) {
	home := t.TempDir()
	// Home ships a file at the same relative path as the auth file below —
	// the auth file must win.
	if err := os.WriteFile(filepath.Join(home, "shadowed-by-auth"), []byte("from-home"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Home also ships a file that the secret below will override.
	if err := os.WriteFile(filepath.Join(home, "shadowed-by-secret"), []byte("from-home-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Home ships a file nothing else touches — must survive untouched.
	if err := os.WriteFile(filepath.Join(home, "home-only"), []byte("home-only-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	authFilePath := filepath.Join(t.TempDir(), "auth-src")
	if err := os.WriteFile(authFilePath, []byte("from-auth"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := RunConfig{
		UnixUsername: "scion",
		HomeDir:      home,
		ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{
				{SourcePath: authFilePath, ContainerPath: "~/shadowed-by-auth"},
			},
		},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "S", Type: "file", Target: "~/shadowed-by-secret", Value: "from-secret"},
		},
	}

	files, err := buildBootstrapFiles(cfg)
	if err != nil {
		t.Fatalf("buildBootstrapFiles: %v", err)
	}

	// Exactly one entry per path — the dedup contract.
	seen := map[string]int{}
	for _, f := range files {
		seen[f.Path]++
	}
	for path, n := range seen {
		if n != 1 {
			t.Errorf("path %s appears %d times, want exactly 1", path, n)
		}
	}

	byAuth, ok := findBootstrapFile(files, "/home/scion/shadowed-by-auth")
	if !ok {
		t.Fatal("missing /home/scion/shadowed-by-auth")
	}
	if got := decodeBootstrapFileContent(t, byAuth); got != "from-auth" {
		t.Errorf("shadowed-by-auth content = %q, want %q (auth must win over home)", got, "from-auth")
	}

	bySecret, ok := findBootstrapFile(files, "/home/scion/shadowed-by-secret")
	if !ok {
		t.Fatal("missing /home/scion/shadowed-by-secret")
	}
	if got := decodeBootstrapFileContent(t, bySecret); got != "from-secret" {
		t.Errorf("shadowed-by-secret content = %q, want %q (secret must win over home)", got, "from-secret")
	}

	homeOnly, ok := findBootstrapFile(files, "/home/scion/home-only")
	if !ok {
		t.Fatal("missing /home/scion/home-only")
	}
	if got := decodeBootstrapFileContent(t, homeOnly); got != "home-only-content" {
		t.Errorf("home-only content = %q, want %q", got, "home-only-content")
	}
}

// TestBuildBootstrapFiles_CleanBeforeDedupeCollapsesDoubledSeparator proves
// the claim that every entry's Path is filepath.Clean-ed before dedup,
// not just home's (which is already effectively clean via filepath.Join): a
// home file at "x" and an auth file whose ContainerPath is the differently
// spelled but equivalent "/home/scion//x" must collapse to exactly one wire
// entry, with the auth content winning (auth overrides home, per
// precedence). Without the Clean at the auth/secret call sites, these would
// survive dedup as two distinct paths and the actor would see a stray
// "/home/scion//x" file rather than one clean override.
func TestBuildBootstrapFiles_CleanBeforeDedupeCollapsesDoubledSeparator(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "x"), []byte("from-home"), 0o600); err != nil {
		t.Fatal(err)
	}

	authFilePath := filepath.Join(t.TempDir(), "auth-src")
	if err := os.WriteFile(authFilePath, []byte("from-auth"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := RunConfig{
		UnixUsername: "scion",
		HomeDir:      home,
		ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{
				// Absolute and already equal to containerHome+"/x" once
				// Cleaned, but spelled with a doubled separator so it would
				// NOT string-equal "/home/scion/x" without Clean.
				{SourcePath: authFilePath, ContainerPath: "/home/scion//x"},
			},
		},
	}

	files, err := buildBootstrapFiles(cfg)
	if err != nil {
		t.Fatalf("buildBootstrapFiles: %v", err)
	}

	var matches []bootstrapFile
	for _, f := range files {
		if f.Path == "/home/scion/x" || f.Path == "/home/scion//x" {
			matches = append(matches, f)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("entries for x = %+v, want exactly 1 (Clean must collapse the doubled separator before dedup)", matches)
	}
	if matches[0].Path != "/home/scion/x" {
		t.Errorf("path = %q, want the Cleaned form %q", matches[0].Path, "/home/scion/x")
	}
	if got := decodeBootstrapFileContent(t, matches[0]); got != "from-auth" {
		t.Errorf("content = %q, want %q (the auth entry must win over home)", got, "from-auth")
	}
}

func TestBuildBootstrapFiles_CapAtCapPasses(t *testing.T) {
	home := t.TempDir()
	data := make([]byte, maxBootstrapFilesTotalBytes)
	if err := os.WriteFile(filepath.Join(home, "big"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := RunConfig{UnixUsername: "scion", HomeDir: home}
	files, err := buildBootstrapFiles(cfg)
	if err != nil {
		t.Fatalf("buildBootstrapFiles at exactly the cap: unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d entries, want 1", len(files))
	}
}

func TestBuildBootstrapFiles_CapPlusOneFails(t *testing.T) {
	home := t.TempDir()
	data := make([]byte, maxBootstrapFilesTotalBytes+1)
	if err := os.WriteFile(filepath.Join(home, "big"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := RunConfig{UnixUsername: "scion", HomeDir: home}
	_, err := buildBootstrapFiles(cfg)
	if err == nil {
		t.Fatal("buildBootstrapFiles at cap+1: expected an error, got nil")
	}
	// Error hygiene: names the cap and the total, never a path.
	if strings.Contains(err.Error(), home) {
		t.Errorf("error leaked the home directory path: %v", err)
	}
	if !strings.Contains(err.Error(), "16777217") {
		t.Errorf("error = %v, want it to name the total size (16777217)", err)
	}
	if !strings.Contains(err.Error(), "16777216") {
		t.Errorf("error = %v, want it to name the cap (16777216)", err)
	}
}

// TestBuildBootstrapFiles_CapSumsAcrossHomeAuthAndSecret proves the cap is
// enforced against the combined home+auth+secret total, not against any one
// source's own total: individually, the home file and the auth file below
// are each under the cap, but their sum is over it.
func TestBuildBootstrapFiles_CapSumsAcrossHomeAuthAndSecret(t *testing.T) {
	home := t.TempDir()
	homeData := make([]byte, maxBootstrapFilesTotalBytes/2)
	if err := os.WriteFile(filepath.Join(home, "home-file"), homeData, 0o600); err != nil {
		t.Fatal(err)
	}

	authFilePath := filepath.Join(t.TempDir(), "auth-src")
	// Individually under the cap; combined with the home file above, over it.
	authData := make([]byte, maxBootstrapFilesTotalBytes/2+2)
	if err := os.WriteFile(authFilePath, authData, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := RunConfig{
		UnixUsername: "scion",
		HomeDir:      home,
		ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{{SourcePath: authFilePath, ContainerPath: "~/auth-file"}},
		},
	}
	if _, err := buildBootstrapFiles(cfg); err == nil {
		t.Fatal("buildBootstrapFiles: home+auth combined over the cap: expected an error, got nil")
	}
}

// TestBuildBootstrapFiles_CapComputedAfterDedupe proves the cap is checked
// against the deduped total, not the raw pre-dedupe sum: a home file that
// gets overridden by a same-path auth file must not count toward the cap,
// even though its own size alone (added to the tiny auth override) would
// put the pre-dedupe sum over the cap.
func TestBuildBootstrapFiles_CapComputedAfterDedupe(t *testing.T) {
	home := t.TempDir()
	// Comfortably under the cap by itself (so homeBootstrapFiles' own
	// running-total early-exit never trips), but pre-dedupe sum with the
	// auth override below would be just over the cap.
	shadowed := make([]byte, maxBootstrapFilesTotalBytes-10)
	if err := os.WriteFile(filepath.Join(home, "shadowed"), shadowed, 0o600); err != nil {
		t.Fatal(err)
	}

	authFilePath := filepath.Join(t.TempDir(), "auth-src")
	if err := os.WriteFile(authFilePath, []byte("tiny-override"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := RunConfig{
		UnixUsername: "scion",
		HomeDir:      home,
		ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{{SourcePath: authFilePath, ContainerPath: "~/shadowed"}},
		},
	}
	files, err := buildBootstrapFiles(cfg)
	if err != nil {
		t.Fatalf("buildBootstrapFiles: cap must be computed after dedupe, not before: %v", err)
	}
	f, ok := findBootstrapFile(files, "/home/scion/shadowed")
	if !ok || decodeBootstrapFileContent(t, f) != "tiny-override" {
		t.Errorf("shadowed = %+v, want the auth override (%q) to have won", f, "tiny-override")
	}
}

// TestHomeBootstrapFiles_CapErrorReturnedDuringWalk proves the running-total
// early exit: a single home file whose size alone exceeds the cap must make
// homeBootstrapFiles itself return the cap error, rather than reading the
// whole file and deferring the check to buildBootstrapFiles' post-walk sum.
func TestHomeBootstrapFiles_CapErrorReturnedDuringWalk(t *testing.T) {
	home := t.TempDir()
	data := make([]byte, maxBootstrapFilesTotalBytes+1)
	if err := os.WriteFile(filepath.Join(home, "big"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := homeBootstrapFiles(home, "/home/scion")
	if err == nil {
		t.Fatal("homeBootstrapFiles with an over-cap file: expected an error directly from the walk, got nil")
	}
	if strings.Contains(err.Error(), home) {
		t.Errorf("error leaked the home directory path: %v", err)
	}
}

// TestBuildBootstrapFiles_ErrorHygiene_SentinelSecretInHomeFile proves that a
// home file's own content — settings.json can carry env, which is
// secret-grade per the design — never appears in an error string, matching
// the existing redaction contract for auth/secret files.
func TestBuildBootstrapFiles_ErrorHygiene_SentinelSecretInHomeFile(t *testing.T) {
	const sentinel = "FAKE-SENTINEL-home-file-secret-content-not-a-real-credential"
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	// Force an over-cap error by also shipping one huge file, so
	// buildBootstrapFiles' error path actually runs with the sentinel file
	// present in the same batch.
	big := make([]byte, maxBootstrapFilesTotalBytes+1)
	if err := os.WriteFile(filepath.Join(home, "big"), big, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := buildBootstrapFiles(RunConfig{UnixUsername: "scion", HomeDir: home})
	if err == nil {
		t.Fatal("expected a cap error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("error leaked home file content: %v", err)
	}
}

// TestDedupeBootstrapFilesByPath_LaterWinsStablePosition proves both halves
// of dedupeBootstrapFilesByPath's contract: the later group's content wins
// on a collision, AND the winning entry keeps the *position* of its first
// occurrence rather than moving to wherever the later group happened to put
// it. Paths are deliberately non-alphabetical (/b, /a, /c) so that sorting
// before comparing — which the previous version of this test did — cannot
// mask a broken position: a dedupe that instead re-appended overridden
// entries at the end would produce [/a, /c, /b] here, which sorts the same
// as the correct [/b, /a, /c] but is a different order.
func TestDedupeBootstrapFilesByPath_LaterWinsStablePosition(t *testing.T) {
	a := []bootstrapFile{{Path: "/b", ContentB64: "b1"}, {Path: "/a", ContentB64: "a1"}}
	b := []bootstrapFile{{Path: "/c", ContentB64: "c1"}}
	c := []bootstrapFile{{Path: "/b", ContentB64: "b2"}} // overrides a's /b in place

	got := dedupeBootstrapFilesByPath(a, b, c)

	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	want := []string{"/b", "/a", "/c"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("paths = %v, want %v (the first-occurrence position must be kept, not just the set of paths)", paths, want)
		}
	}

	b0, ok := findBootstrapFile(got, "/b")
	if !ok || b0.ContentB64 != "b2" {
		t.Errorf("/b = %+v, want content_b64 %q (later entry must win)", b0, "b2")
	}
}
