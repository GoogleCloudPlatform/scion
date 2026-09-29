/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// TestEnforcedHooksDirAncestors_MatchesExpectedChain pins the exact ancestor
// chain fixupEnforcedHooksDirChain walks for the real hooks.EnforcedHooksDir
// constant — "/" -> "/run" -> "/run/scion" -> "/run/scion/hooks" — the same
// chain LifecycleManager's own DecideExecAsRoot check walks at hook-exec
// time (pkg/sciontool/hooks/exec_enforced.go's openChainNoFollow), so a
// change to EnforcedHooksDir's value is caught here rather than silently
// fixing up the wrong directories.
func TestEnforcedHooksDirAncestors_MatchesExpectedChain(t *testing.T) {
	got := enforcedHooksDirAncestors()
	want := []string{"/", "/run", "/run/scion", hooks.EnforcedHooksDir}
	if len(got) != len(want) {
		t.Fatalf("enforcedHooksDirAncestors() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("enforcedHooksDirAncestors()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestFixupEnforcedHooksDirEntry_StripsGroupAndOtherWrite proves the
// mode-narrowing half of fixupEnforcedHooksDirEntry: a pre-existing
// directory that is group- or world-writable gets those bits stripped,
// regardless of whether the chown half below also applies. This does not
// depend on running as root.
func TestFixupEnforcedHooksDirEntry_StripsGroupAndOtherWrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	fixupEnforcedHooksDirEntry(dir)

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("mode = %o, want 0755 (group/other write stripped)", got)
	}
}

// TestFixupEnforcedHooksDirEntry_AlreadyCorrectIsNoop proves a directory
// that is already root-owned (when running as root) with mode 0755 is left
// completely untouched — no error, no unexpected chmod/chown side effect.
// Skipped when not root since the fixture can't be made root-owned
// otherwise; TestFixupEnforcedHooksDirEntry_StripsGroupAndOtherWrite already
// covers the mode-only half without needing root.
func TestFixupEnforcedHooksDirEntry_AlreadyCorrectIsNoop(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create a root-owned fixture")
	}
	dir := t.TempDir()
	if err := os.Chown(dir, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	fixupEnforcedHooksDirEntry(dir)

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("mode = %o, want unchanged 0755", got)
	}
	st := info.Sys().(*syscall.Stat_t)
	if st.Uid != 0 || st.Gid != 0 {
		t.Errorf("owner = %d:%d, want unchanged 0:0", st.Uid, st.Gid)
	}
}

// TestFixupEnforcedHooksDirEntry_ChownsToRootWhenRoot proves the
// ownership-correcting half: a directory owned by a non-root uid gets
// chowned to root:root. Skipped when not root — chown(2) to an arbitrary
// uid/gid is root-only, so a non-root test can't set up a fixture whose
// "before" state this assertion needs, and can't perform the chown itself
// either.
func TestFixupEnforcedHooksDirEntry_ChownsToRootWhenRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create a non-root-owned fixture and to chown it")
	}
	dir := t.TempDir()
	if err := os.Chown(dir, 1000, 1000); err != nil {
		t.Fatal(err)
	}

	fixupEnforcedHooksDirEntry(dir)

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if st.Uid != 0 || st.Gid != 0 {
		t.Errorf("owner = %d:%d, want 0:0 after fixup", st.Uid, st.Gid)
	}
}

// TestFixupEnforcedHooksDirEntry_SymlinkRefused proves a symlinked ancestor
// is left completely alone — never chmod/chowned, and never followed to
// modify whatever it points at. Matches fixupRootfsForScion's own treatment
// of "/" (Stat only, never traverses through an unexpected symlink) and the
// broader "symlinked hooks/dirs must resolve to the real owner or be
// refused" requirement this fixup exists to support.
func TestFixupEnforcedHooksDirEntry_SymlinkRefused(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o777); err != nil {
		t.Fatal(err)
	}
	// os.Mkdir's mode argument is masked by the process umask; force the
	// exact bits this test needs regardless of the ambient umask.
	if err := os.Chmod(target, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	fixupEnforcedHooksDirEntry(link)

	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := targetInfo.Mode().Perm(); got != 0o777 {
		t.Errorf("target mode = %o, want unchanged 0777 (fixup must not follow the symlink)", got)
	}
}

// TestFixupEnforcedHooksDirEntry_MissingPathIsNoop proves a missing ancestor
// (the common case before any bootstrap has run) is silently accepted —
// bootstrap creates hooks.EnforcedHooksDir itself, fresh and root-owned;
// there is nothing to fix up until then.
func TestFixupEnforcedHooksDirEntry_MissingPathIsNoop(t *testing.T) {
	fixupEnforcedHooksDirEntry(filepath.Join(t.TempDir(), "does-not-exist"))
	// No panic and nothing to assert beyond that — success is "did nothing".
}
