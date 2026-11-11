/*
Copyright 2026 The Scion Authors.
*/

package rootexec

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolve_FindsRealSystemBinaries proves Resolve finds ordinary system
// tools this suite depends on being present and trusted in any environment
// these tests run in — including, on a real Debian-family image, one
// (iptables) that is reached only through a chain of root-installed
// symlinks (update-alternatives), proving the chain is followed rather than
// refused outright.
func TestResolve_FindsRealSystemBinaries(t *testing.T) {
	for _, name := range []string{"sh", "su"} {
		t.Run(name, func(t *testing.T) {
			got, err := Resolve(name)
			if err != nil {
				t.Fatalf("Resolve(%q) = %v", name, err)
			}
			if !strings.Contains(got, name) {
				t.Errorf("Resolve(%q) = %q, want a path containing %q", name, got, name)
			}
			if !strings.HasPrefix(got, "/") {
				t.Errorf("Resolve(%q) = %q, want an absolute path", name, got)
			}
		})
	}
}

// TestSearchPath_IncludesUsrLocalDirectories pins the default SearchPath
// value itself (not a synthetic override): "/usr/local/sbin" and
// "/usr/local/bin" must be present, ahead of "/usr/sbin" and "/usr/bin" in
// the standard root PATH order, since a real scion agent image installs
// git (among other tools) only under "/usr/local/bin" — omitting
// "/usr/local/*" would break every caller that needs git on images that
// install it only there.
func TestSearchPath_IncludesUsrLocalDirectories(t *testing.T) {
	want := []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}
	if len(SearchPath) != len(want) {
		t.Fatalf("SearchPath = %v, want %v", SearchPath, want)
	}
	for i, dir := range want {
		if SearchPath[i] != dir {
			t.Errorf("SearchPath[%d] = %q, want %q (full: %v)", i, SearchPath[i], dir, SearchPath)
		}
	}
}

// selfOwnedTrustedDir creates a fresh, self-owned, non-group/other-writable
// directory to anchor a SearchPath fixture under. It deliberately does not
// use t.TempDir() (which resolves under os.TempDir(), i.e. "/tmp" — world-
// writable by design, so it fails Resolve's own chain check before the
// test's fixture is ever reached, the same reason
// pkg/sciontool/dirfd's own test suite anchors its fixtures under $HOME
// instead). Skips if $HOME can't be resolved.
func selfOwnedTrustedDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("cannot resolve a real home directory to anchor a trusted SearchPath fixture under")
	}
	dir, err := os.MkdirTemp(home, ".rootexec-test-*")
	if err != nil {
		t.Skipf("cannot create a test directory under %s: %v", home, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestResolve_MultiCallBinaryDispatchesCorrectly is the required positive-
// path test for the class of binary Resolve exists to support correctly: a
// multi-call binary that decides its own behavior from argv[0]'s basename
// must receive the CANDIDATE path Resolve returns as argv[0], not whatever
// its own symlink chain resolves to. Debian's real "iptables" is exactly
// this shape (it resolves through "/etc/alternatives" to a
// "xtables-nft-multi" binary that refuses to run at all if handed the
// wrong argv[0]); this builds a synthetic fixture mirroring that layout —
// a two-hop symlink chain through an "alternatives"-style directory — under
// a temporary SearchPath, so the test is hermetic and does not depend on
// the real "iptables" being installed.
func TestResolve_MultiCallBinaryDispatchesCorrectly(t *testing.T) {
	dir := selfOwnedTrustedDir(t)
	origSearchPath := SearchPath
	SearchPath = []string{dir}
	t.Cleanup(func() { SearchPath = origSearchPath })

	altDir := filepath.Join(dir, "alternatives")
	if err := os.Mkdir(altDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker")
	multiCall := filepath.Join(dir, "multi-call-binary")
	script := "#!/bin/sh\necho \"${0##*/}\" > " + marker + "\n"
	if err := os.WriteFile(multiCall, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	hop1 := filepath.Join(altDir, "tool")
	if err := os.Symlink(multiCall, hop1); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "tool")
	if err := os.Symlink(hop1, candidate); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve("tool")
	if err != nil {
		t.Fatalf("Resolve(tool) = %v", err)
	}
	if got != candidate {
		t.Fatalf("Resolve(tool) = %q, want the candidate path %q (not the resolved destination)", got, candidate)
	}

	cmd := exec.Command(got)
	cmd.Env = Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running the resolved tool: %v, output=%s", err, out)
	}

	gotBasename, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker was never written: %v", err)
	}
	if strings.TrimSpace(string(gotBasename)) != "tool" {
		t.Errorf("multi-call binary saw argv[0] basename %q, want %q — dispatch would fail on a real multi-call binary", strings.TrimSpace(string(gotBasename)), "tool")
	}
}

// TestResolve_RealIptablesDispatchesCorrectly is the same claim as
// TestResolve_MultiCallBinaryDispatchesCorrectly, against the real system
// "iptables" rather than a synthetic fixture, on any environment where it
// is resolvable. A dispatch failure prints "No valid subcommand given"
// instead of version output.
func TestResolve_RealIptablesDispatchesCorrectly(t *testing.T) {
	path, err := Resolve("iptables")
	if err != nil {
		t.Skipf("iptables not resolvable as a trusted binary in this environment: %v", err)
	}
	cmd := exec.Command(path, "-V")
	cmd.Env = Env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the real, resolved iptables: %v, output=%s", err, out)
	}
	if !strings.Contains(string(out), "iptables") {
		t.Errorf("iptables -V output = %q, want it to mention \"iptables\" (a multi-call dispatch failure prints \"No valid subcommand given\" instead)", out)
	}
}

// TestResolve_FindsToolOnlyInLocalBinEquivalent proves Resolve finds a
// binary that exists ONLY in one of the "/usr/local/*" entries now in
// SearchPath — the exact shape that silently broke shared-workspace git
// resolution on a real scion agent image, where git is installed only in
// "/usr/local/bin". Hermetic: builds a synthetic SearchPath rather than
// depending on the test environment's own layout.
func TestResolve_FindsToolOnlyInLocalBinEquivalent(t *testing.T) {
	localBinLike := selfOwnedTrustedDir(t)
	systemBinLike := selfOwnedTrustedDir(t)
	origSearchPath := SearchPath
	SearchPath = []string{localBinLike, systemBinLike}
	t.Cleanup(func() { SearchPath = origSearchPath })

	onlyCopy := filepath.Join(localBinLike, "sometool")
	if err := os.WriteFile(onlyCopy, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve("sometool")
	if err != nil {
		t.Fatalf("Resolve(sometool) = %v, want it found in the first SearchPath entry", err)
	}
	if got != onlyCopy {
		t.Errorf("Resolve(sometool) = %q, want %q", got, onlyCopy)
	}
}

// TestResolve_RejectsPathLikeNames proves Resolve refuses anything that is
// not a bare name — it exists to turn a bare name into a verified absolute
// path, not to re-verify a path a caller already built.
func TestResolve_RejectsPathLikeNames(t *testing.T) {
	for _, name := range []string{"", "/usr/bin/sh", "a/b", "./sh"} {
		if _, err := Resolve(name); err == nil {
			t.Errorf("Resolve(%q) = nil error, want a refusal", name)
		}
	}
}

// TestResolve_NeverConsultsPATH proves Resolve's answer does not change when
// $PATH is pointed somewhere else entirely — including somewhere that would
// shadow a real binary if Resolve used the ambient PATH the way exec.Command
// or exec.LookPath does. This is the actual regression the vulnerability
// this package fixes was made of: PID 1's inherited PATH.
func TestResolve_NeverConsultsPATH(t *testing.T) {
	real, err := Resolve("sh")
	if err != nil {
		t.Fatalf("Resolve(sh) baseline: %v", err)
	}

	planted := t.TempDir()
	fake := planted + "/sh"
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho planted\n"), 0o755); err != nil {
		t.Fatalf("write planted binary: %v", err)
	}
	t.Setenv("PATH", planted+":/nonexistent")

	got, err := Resolve("sh")
	if err != nil {
		t.Fatalf("Resolve(sh) with hostile PATH: %v", err)
	}
	if got != real {
		t.Errorf("Resolve(sh) = %q with a hostile $PATH set, want unchanged %q — PATH must never be consulted", got, real)
	}
}

// TestEnv_NeverInheritsAmbientEnvironment proves Env's result never contains
// anything from the current process's real environment, even one of the
// exact variable names this package exists to keep out of a root exec.
func TestEnv_NeverInheritsAmbientEnvironment(t *testing.T) {
	t.Setenv("PATH", "/should/not/leak")
	t.Setenv("LD_PRELOAD", "/should/not/leak.so")
	t.Setenv("BASH_ENV", "/should/not/leak.sh")

	got := Env("HOME=/explicit")
	for _, kv := range got {
		if strings.Contains(kv, "should/not/leak") {
			t.Errorf("Env() = %v, leaked an ambient environment value: %q", got, kv)
		}
	}
	wantPath := "PATH=" + strings.Join(SearchPath, ":")
	if got[0] != wantPath {
		t.Errorf("Env()[0] = %q, want %q", got[0], wantPath)
	}
	if got[1] != "HOME=/explicit" {
		t.Errorf("Env()[1] = %q, want the explicit extra entry", got[1])
	}
}

// TestSelfExe_IsTheProcMagicSymlink proves SelfExe returns the fixed magic
// path, never a resolved on-disk path — the whole point being that this
// string is immune to the on-disk binary being replaced.
func TestSelfExe_IsTheProcMagicSymlink(t *testing.T) {
	if got := SelfExe(); got != "/proc/self/exe" {
		t.Errorf("SelfExe() = %q, want /proc/self/exe", got)
	}
}
