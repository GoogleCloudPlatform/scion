/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/substratecaps"
)

// -----------------------------------------------------------------------
// checkPrivilegeDropFeasible: the synchronous /bootstrap precondition.
// -----------------------------------------------------------------------

// fakeFileInfo is a minimal fs.FileInfo whose Sys() returns a
// *syscall.Stat_t, so canSearchDir/homeOwnedAndWritable's type assertion
// succeeds against a value that was never really stat'd.
type fakeFileInfo struct {
	mode     fs.FileMode
	uid, gid uint32
}

func (f fakeFileInfo) Name() string       { return "" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid, Gid: f.gid} }

// fakeStatAllTraversableAndOwned is checkPrivilegeDropFeasible's statPath
// default for tests: every path is a directory, mode 0755, owned by
// uid:gid 1000:1000 — traversable by everyone, owned and writable by the
// target uid used throughout these tests.
func fakeStatAllTraversableAndOwned(string) (fs.FileInfo, error) {
	return fakeFileInfo{mode: fs.ModeDir | 0o755, uid: 1000, gid: 1000}, nil
}

// fakePrivilegeDropDeps builds privilegeDropPreconditionDeps with every
// dependency controllable, defaulting to a fully-feasible environment (every
// capability present, every path traversable and correctly owned) so each
// test case only needs to override the one thing it's testing.
func fakePrivilegeDropDeps(t *testing.T) privilegeDropPreconditionDeps {
	t.Helper()
	env := map[string]string{"SCION_HOST_UID": "1000", "SCION_HOST_GID": "1000"}
	return privilegeDropPreconditionDeps{
		hasCapBit: func(uint) bool { return true },
		lookupUser: func(string) (*user.User, error) {
			return &user.User{Username: "scion", Uid: "1000", Gid: "1000", HomeDir: "/home/scion"}, nil
		},
		getenv:   func(k string) string { return env[k] },
		statPath: fakeStatAllTraversableAndOwned,
	}
}

// fakeHasCapBitMissing returns a hasCapBit fake that reports every bit
// present except the ones listed.
func fakeHasCapBitMissing(missing ...uint) func(uint) bool {
	missingSet := make(map[uint]bool, len(missing))
	for _, b := range missing {
		missingSet[b] = true
	}
	return func(bit uint) bool { return !missingSet[bit] }
}

func TestCheckPrivilegeDropFeasible_AllPresent_Passes(t *testing.T) {
	if err := checkPrivilegeDropFeasible(fakePrivilegeDropDeps(t)); err != nil {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want nil", err)
	}
}

// TestCheckPrivilegeDropFeasible_EveryRequiredCapabilityIsChecked is (A)'s
// direct proof that checkPrivilegeDropFeasible verifies the FULL committed
// capability set (substratecaps.Required), not a hardcoded SETUID/SETGID
// pair: for each capability in that shared list, simulating just that one
// missing must trip the precondition. Because both this loop and
// checkPrivilegeDropFeasible's own loop iterate the same substratecaps.
// Required slice, a capability added there is automatically covered here
// too — this is what "can never drift apart" means in practice.
//
// A regression that narrows checkPrivilegeDropFeasible's loop back to a
// hardcoded `!d.hasCapBit(7) || !d.hasCapBit(6)` SETUID/SETGID-only check
// fails this test's "CHOWN" subtest.
func TestCheckPrivilegeDropFeasible_EveryRequiredCapabilityIsChecked(t *testing.T) {
	for _, c := range substratecaps.Required {
		t.Run(c.Name, func(t *testing.T) {
			d := fakePrivilegeDropDeps(t)
			d.hasCapBit = fakeHasCapBitMissing(c.EffBit)
			if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
				t.Errorf("checkPrivilegeDropFeasible() = %v with only %s (bit %d) missing, want errPrivilegeDropPrecondition", err, c.Name, c.EffBit)
			}
		})
	}
}

func TestCheckPrivilegeDropFeasible_ScionUserUnresolvable_Fails(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	d.lookupUser = func(string) (*user.User, error) { return nil, errors.New("unknown user scion") }
	if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want errPrivilegeDropPrecondition", err)
	}
}

func TestCheckPrivilegeDropFeasible_HostUIDGIDMissing_Fails(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	d.getenv = func(string) string { return "" }
	if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want errPrivilegeDropPrecondition", err)
	}
}

func TestCheckPrivilegeDropFeasible_HostUIDGIDUnparseable_Fails(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	d.getenv = func(k string) string {
		if k == "SCION_HOST_UID" {
			return "not-a-number"
		}
		return "1000"
	}
	if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want errPrivilegeDropPrecondition", err)
	}
}

// -----------------------------------------------------------------------
// checkPrivilegeDropFeasible's rootfs traversability checks.
// -----------------------------------------------------------------------

// statPathOverride builds a statPath fake that returns override for the
// given path and fakeStatAllTraversableAndOwned's default for everything
// else.
func statPathOverride(path string, override fakeFileInfo) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		if p == path {
			return override, nil
		}
		return fakeStatAllTraversableAndOwned(p)
	}
}

func TestCheckPrivilegeDropFeasible_RootNotTraversable_Fails(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	// '/' owned by root (uid 0), mode 0700: the target uid (1000) is
	// neither owner nor group, and other has no x bit.
	d.statPath = statPathOverride("/", fakeFileInfo{mode: fs.ModeDir | 0o700, uid: 0, gid: 0})
	if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want errPrivilegeDropPrecondition (root not traversable)", err)
	}
}

func TestCheckPrivilegeDropFeasible_HomeParentNotTraversable_Fails(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	// /home (a parent of /home/scion, the fake user's HomeDir) not
	// traversable by the target uid/gid.
	d.statPath = statPathOverride("/home", fakeFileInfo{mode: fs.ModeDir | 0o700, uid: 0, gid: 0})
	if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want errPrivilegeDropPrecondition (home parent not traversable)", err)
	}
}

func TestCheckPrivilegeDropFeasible_WorkspaceParentNotTraversable_Fails(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	d.getenv = func(k string) string {
		switch k {
		case "SCION_HOST_UID", "SCION_HOST_GID":
			return "1000"
		case "SCION_WORKSPACE_PATH":
			return "/srv/workspace"
		}
		return ""
	}
	// /srv (a parent of the configured workspace path) not traversable.
	d.statPath = statPathOverride("/srv", fakeFileInfo{mode: fs.ModeDir | 0o700, uid: 0, gid: 0})
	if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want errPrivilegeDropPrecondition (workspace parent not traversable)", err)
	}
}

func TestCheckPrivilegeDropFeasible_HomeNotOwnedByTarget_Fails(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	// $HOME (/home/scion) owned by root, not the target uid.
	d.statPath = statPathOverride("/home/scion", fakeFileInfo{mode: fs.ModeDir | 0o755, uid: 0, gid: 0})
	if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want errPrivilegeDropPrecondition (home not owned by target)", err)
	}
}

func TestCheckPrivilegeDropFeasible_HomeNotWritable_Fails(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	// $HOME owned by the target uid, but with no write bit for anyone
	// (0555 — readable/traversable, never writable).
	d.statPath = statPathOverride("/home/scion", fakeFileInfo{mode: fs.ModeDir | 0o555, uid: 1000, gid: 1000})
	if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want errPrivilegeDropPrecondition (home not writable)", err)
	}
}

// TestCheckPrivilegeDropFeasible_HomeWritableButNotTraversable_Fails covers
// a $HOME with the owner write bit but not the owner execute bit (0600):
// writable in principle, but not reachable, so it must still fail closed.
func TestCheckPrivilegeDropFeasible_HomeWritableButNotTraversable_Fails(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	d.statPath = statPathOverride("/home/scion", fakeFileInfo{mode: fs.ModeDir | 0o600, uid: 1000, gid: 1000})
	if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want errPrivilegeDropPrecondition (home writable but not traversable)", err)
	}
}

func TestCheckPrivilegeDropFeasible_TraversableAndOwned_Passes(t *testing.T) {
	// The happy path: every default from fakePrivilegeDropDeps already
	// satisfies traversability and home ownership/writability, so this is
	// the same as TestCheckPrivilegeDropFeasible_AllPresent_Passes,
	// restated here to anchor it explicitly against the traversability
	// requirement rather than only the capability/user/env one.
	d := fakePrivilegeDropDeps(t)
	if err := checkPrivilegeDropFeasible(d); err != nil {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want nil", err)
	}
}

// TestCheckPrivilegeDropFeasible_SetuidRootSudo_Fails proves the fail-closed
// precondition added for the sudo hardening: a setuid-root "sudo" binary
// found under any one of the fixed system directories must refuse
// bootstrap, exactly like every other precondition in this function —
// catching a stale golden template that skipped the rootfs fixup's own
// setuid strip.
func TestCheckPrivilegeDropFeasible_SetuidRootSudo_Fails(t *testing.T) {
	for _, dir := range sudoCheckDirs {
		t.Run(dir, func(t *testing.T) {
			d := fakePrivilegeDropDeps(t)
			path := filepath.Join("/", dir, "sudo")
			d.statPath = statPathOverride(path, fakeFileInfo{mode: os.ModeSetuid | 0o755, uid: 0, gid: 0})
			if err := checkPrivilegeDropFeasible(d); !errors.Is(err, errPrivilegeDropPrecondition) {
				t.Errorf("checkPrivilegeDropFeasible() = %v, want errPrivilegeDropPrecondition (setuid-root sudo at %s)", err, path)
			}
		})
	}
}

// TestCheckPrivilegeDropFeasible_SudoSetuidButNotOwnedByRoot_Passes proves
// the check is specifically about a ROOT-owned setuid binary: a setuid
// binary owned by some other uid (never a real sudo installation, but
// worth pinning so the check doesn't over-fire on owner alone) does not
// trip the precondition.
func TestCheckPrivilegeDropFeasible_SudoSetuidButNotOwnedByRoot_Passes(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	d.statPath = statPathOverride("/usr/bin/sudo", fakeFileInfo{mode: os.ModeSetuid | 0o755, uid: 1000, gid: 1000})
	if err := checkPrivilegeDropFeasible(d); err != nil {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want nil (setuid binary not owned by root)", err)
	}
}

// TestCheckPrivilegeDropFeasible_NonSetuidSudo_Passes is the expected
// steady state after the rootfs fixup's setuid strip has run: a root-owned
// "sudo" binary with no setuid bit must never trip this precondition.
func TestCheckPrivilegeDropFeasible_NonSetuidSudo_Passes(t *testing.T) {
	d := fakePrivilegeDropDeps(t)
	d.statPath = statPathOverride("/usr/bin/sudo", fakeFileInfo{mode: 0o755, uid: 0, gid: 0})
	if err := checkPrivilegeDropFeasible(d); err != nil {
		t.Errorf("checkPrivilegeDropFeasible() = %v, want nil (non-setuid sudo)", err)
	}
}

// notStatTFileInfo is an fs.FileInfo whose Sys() deliberately does not
// return a *syscall.Stat_t, for TestFindSetuidRootSudo_FailsClosedOnNonNotExistStatError's
// "can't even determine the owner" case.
type notStatTFileInfo struct{}

func (notStatTFileInfo) Name() string       { return "sudo" }
func (notStatTFileInfo) Size() int64        { return 0 }
func (notStatTFileInfo) Mode() fs.FileMode  { return os.ModeSetuid | 0o755 }
func (notStatTFileInfo) ModTime() time.Time { return time.Time{} }
func (notStatTFileInfo) IsDir() bool        { return false }
func (notStatTFileInfo) Sys() any           { return "not a *syscall.Stat_t" }

// statErrorAt returns a statPath fake that returns err for exactly path,
// and a plain ENOENT *fs.PathError for anything else — so a table test can
// drive findSetuidRootSudo's loop past every OTHER sudoCheckDirs entry
// (genuinely absent) and stop precisely at the one candidate under test.
func statErrorAt(path string, err error) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		if p == path {
			return nil, err
		}
		return nil, &fs.PathError{Op: "stat", Path: p, Err: syscall.ENOENT}
	}
}

// TestFindSetuidRootSudo_FailsClosedOnNonNotExistStatError proves the
// never-fail-open branch: a stat error other than "not there at all" is
// treated the same as finding a setuid-root binary (report the candidate),
// not silently skipped like a genuinely missing candidate. Table over the
// shapes that distinguish it from ENOENT — EACCES, ELOOP, and EIO, plus a
// FileInfo whose Sys() isn't a *syscall.Stat_t at all — paired with the
// positive case (ENOENT everywhere) returning "".
func TestFindSetuidRootSudo_FailsClosedOnNonNotExistStatError(t *testing.T) {
	firstCandidate := filepath.Join("/", sudoCheckDirs[0], "sudo")

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"EACCES", &fs.PathError{Op: "stat", Path: firstCandidate, Err: syscall.EACCES}},
		{"ELOOP", &fs.PathError{Op: "stat", Path: firstCandidate, Err: syscall.ELOOP}},
		{"EIO", &fs.PathError{Op: "stat", Path: firstCandidate, Err: syscall.EIO}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := findSetuidRootSudo(statErrorAt(firstCandidate, tc.err)); got != firstCandidate {
				t.Errorf("findSetuidRootSudo() = %q, want %q (fail closed on a non-ENOENT stat error)", got, firstCandidate)
			}
		})
	}

	t.Run("Sys_not_Stat_t", func(t *testing.T) {
		stat := func(p string) (fs.FileInfo, error) {
			if p == firstCandidate {
				return notStatTFileInfo{}, nil
			}
			return nil, &fs.PathError{Op: "stat", Path: p, Err: syscall.ENOENT}
		}
		if got := findSetuidRootSudo(stat); got != firstCandidate {
			t.Errorf("findSetuidRootSudo() = %q, want %q (fail closed when Sys() isn't *syscall.Stat_t)", got, firstCandidate)
		}
	})

	t.Run("genuinely_absent_everywhere", func(t *testing.T) {
		stat := func(p string) (fs.FileInfo, error) {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: syscall.ENOENT}
		}
		if got := findSetuidRootSudo(stat); got != "" {
			t.Errorf("findSetuidRootSudo() = %q, want \"\" when every candidate is genuinely absent", got)
		}
	})
}

// TestDefaultPrivilegeDropPreconditionDeps_LookupUserGoesThroughScionUserLookup
// pins defaultPrivilegeDropPreconditionDeps.lookupUser's routing through the
// scionUserLookup var (init.go; see its own doc comment for why this must be
// a closure, not the var's value bound at package-init time): reverting to a
// direct user.Lookup call would pass every other test in this file, so
// nothing else catches that regression. withScionUserLookup is defined in
// init_privilege_drop_test.go, alongside the other scionUserLookup-dependent
// tests.
func TestDefaultPrivilegeDropPreconditionDeps_LookupUserGoesThroughScionUserLookup(t *testing.T) {
	var called bool
	withScionUserLookup(t, func(username string) (*user.User, error) {
		called = true
		return &user.User{Username: username, Uid: "1000", Gid: "1000", HomeDir: "/home/scion"}, nil
	})
	if _, err := defaultPrivilegeDropPreconditionDeps.lookupUser("scion"); err != nil {
		t.Fatalf("lookupUser(%q) = %v, want nil", "scion", err)
	}
	if !called {
		t.Error("defaultPrivilegeDropPreconditionDeps.lookupUser did not go through the scionUserLookup var")
	}
}
