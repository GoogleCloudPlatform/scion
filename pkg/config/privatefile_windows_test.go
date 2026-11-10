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

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/util/filelock"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// Windows site tests for the pkg/config private files. They
// run on the windows-latest job through the anchored -run line in
// .github/windows-test-packages.txt (every test here is named TestWindows*).

// usersInheritDir returns a new directory whose DACL hands an inheritable
// BUILTIN\Users read ACE to everything created inside it, so an Inherit
// object visibly differs from a Private one.
func usersInheritDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	wintest.Icacls(t, dir, "/grant", "*"+wintest.SIDUsers+":(OI)(CI)R")
	return dir
}

// assertInherited fails unless path carries the inherited Users ACE and an
// unprotected DACL: the Inherit protection.
func assertInherited(t *testing.T, path string) {
	t.Helper()
	if sd := wintest.ReadSD(t, path); sd.Protected || !sd.HasAllowACE(wintest.SIDUsers) {
		t.Fatalf("%s: SD %+v; want the inherited Users ACE (Inherit)", path, sd)
	}
}

func mustRead(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("read %s = %q, %v; want %q", path, got, err, want)
	}
}

// useWindowsHome points the global directory (USERPROFILE\.scion) at a new
// temporary home and returns that home.
func useWindowsHome(t *testing.T) string {
	t.Helper()
	home := usersInheritDir(t)
	t.Setenv("USERPROFILE", home)
	return home
}

// A mode-0600 atomic write replaces an existing too-open file with
// a private one (implicit repair) and leaves no temporary file. Control: a
// mode-0644 write in the same directory gets the inherited ACL, with no
// read-only attribute (no Chmod).
func TestWindowsAtomicWrite_PrivateReplacesTooOpenInheritInherits(t *testing.T) {
	dir := usersInheritDir(t)
	target := filepath.Join(dir, "secret.yaml")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertInherited(t, target)
	if err := writeFileAtomicMode(target, []byte("new"), 0o600); err != nil {
		t.Fatalf("private atomic write: %v", err)
	}
	wintest.AssertPrivateSD(t, target)
	mustRead(t, target, "new")

	plain := filepath.Join(dir, "plain.yaml")
	if err := writeFileAtomicMode(plain, []byte("p"), 0o644); err != nil {
		t.Fatalf("inherit atomic write: %v", err)
	}
	assertInherited(t, plain)
	if fi, err := os.Stat(plain); err != nil || fi.Mode().Perm()&0o200 == 0 {
		t.Fatalf("inherit write left %v, %v; want a writable file (no read-only attribute)", fi, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// An Inherit-mode atomic write
// keeps an existing owner-only file private, deciding from its real ACL on
// a no-follow handle, never from its meaningless Perm. Control: a file with
// the inherited ACL stays inherited, and a missing target gets Inherit.
// Both writers are covered: writeFileAtomic and writeFileAtomicMode(0644).
func TestWindowsAtomicWrite_InheritKeepsOwnerOnlyTarget(t *testing.T) {
	dir := usersInheritDir(t)
	tight := filepath.Join(dir, "tight.yaml")
	if err := writeFileAtomicMode(tight, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	wintest.AssertPrivateSD(t, tight)
	if err := writeFileAtomic(tight, []byte("new")); err != nil {
		t.Fatalf("writeFileAtomic over an owner-only file: %v", err)
	}
	wintest.AssertPrivateSD(t, tight)
	mustRead(t, tight, "new")
	if err := writeFileAtomicMode(tight, []byte("newer"), 0o644); err != nil {
		t.Fatalf("0644 write over an owner-only file: %v", err)
	}
	wintest.AssertPrivateSD(t, tight)
	mustRead(t, tight, "newer")

	plain := filepath.Join(dir, "plain.yaml")
	if err := writeFileAtomic(plain, []byte("first")); err != nil {
		t.Fatal(err)
	}
	assertInherited(t, plain)
	if err := writeFileAtomic(plain, []byte("second")); err != nil {
		t.Fatal(err)
	}
	assertInherited(t, plain)
	mustRead(t, plain, "second")
}

// The atomic write's protection decision never follows a link at the
// target. A symlink to an owner-only file is refused by OpenNoFollow, so
// the caller's protection (Inherit) is used, and the write replaces the
// link itself; the link's target is neither checked nor changed. The same
// holds for a junction. Control: TestWindowsAtomicWrite_InheritKeepsOwnerOnlyTarget
// keeps the same owner-only file private when it is the target itself.
func TestWindowsAtomicWrite_InheritDecisionDoesNotFollowLink(t *testing.T) {
	dir, outside := usersInheritDir(t), t.TempDir()
	victim := filepath.Join(outside, "victim.yaml")
	if err := writeFileAtomicMode(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	wintest.AssertPrivateSD(t, victim)

	link := filepath.Join(dir, "settings.yaml")
	wintest.MustSymlink(t, victim, link)
	if err := writeFileAtomic(link, []byte("new")); err != nil {
		t.Fatalf("writeFileAtomic over a symlink: %v", err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		t.Fatalf("after the write %s is %v, %v; want a regular file", link, fi, err)
	}
	assertInherited(t, link)
	mustRead(t, link, "new")
	mustRead(t, victim, "keep")
	wintest.AssertPrivateSD(t, victim)

	if got := atomicProtection(link, 0o644); got != privatefs.Inherit {
		t.Fatalf("control: atomicProtection of the replaced regular file = %v, want Inherit", got)
	}
	junction := filepath.Join(dir, "junction")
	wintest.MustJunction(t, outside, junction)
	if got := atomicProtection(junction, 0o644); got != privatefs.Inherit {
		t.Errorf("atomicProtection of a junction = %v, want Inherit (the link is not followed)", got)
	}
	if got := atomicProtection(filepath.Join(dir, "absent.yaml"), 0o644); got != privatefs.Inherit {
		t.Errorf("atomicProtection of a missing target = %v, want Inherit", got)
	}
}

// A private atomic write over a symlink replaces the link itself
// and never writes through it. Control: the link's target keeps its
// content.
func TestWindowsAtomicWrite_PrivateReplacesSymlinkNotTarget(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "context.yaml")
	wintest.MustSymlink(t, victim, link)
	if err := writeFileAtomicMode(link, []byte("new"), 0o600); err != nil {
		t.Fatalf("private atomic write over a symlink: %v", err)
	}
	mustRead(t, victim, "keep")
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		t.Fatalf("after the write %s is %v, %v; want a regular file", link, fi, err)
	}
	wintest.AssertPrivateSD(t, link)
	mustRead(t, link, "new")
}

// The first PersistentHubID creates a missing global directory
// private and persists the hub id private, through the hard-link path and,
// with hard links failing, through createFileExclusive.
func TestWindowsPersistHubID_GlobalDirAndFilePrivate(t *testing.T) {
	home := useWindowsHome(t)
	id := PersistentHubID()
	globalDir := filepath.Join(home, GlobalDir)
	wintest.AssertPrivateSD(t, globalDir)
	wintest.AssertPrivateSD(t, filepath.Join(globalDir, hubIDFileName))
	mustRead(t, filepath.Join(globalDir, hubIDFileName), id+"\n")

	dir := usersInheritDir(t)
	old := linkFile
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: errors.ErrUnsupported} }
	t.Cleanup(func() { linkFile = old })
	path := filepath.Join(dir, "hub_id")
	if got, err := persistHubIDOnce(path, "abc"); err != nil || got != "abc" {
		t.Fatalf("persistHubIDOnce without hard links = %q, %v", got, err)
	}
	wintest.AssertPrivateSD(t, path)
	mustRead(t, path, "abc\n")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// The settings lock refuses a junction planted at its lock file.
// Control: the same directory locks once the junction is gone.
func TestWindowsSettingsLock_RefusesJunctionLockFile(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	j := filepath.Join(dir, settingsLockFileName)
	wintest.MustJunction(t, outside, j)
	if l, err := LockSettingsDir(dir); !errors.Is(err, filelock.ErrNotRegular) {
		if l != nil {
			l.Unlock()
		}
		t.Fatalf("LockSettingsDir with a junction lock file = %v, want filelock.ErrNotRegular", err)
	}
	if err := os.Remove(j); err != nil {
		t.Fatal(err)
	}
	l, err := LockSettingsDir(dir)
	if err != nil {
		t.Fatalf("control: LockSettingsDir: %v", err)
	}
	l.Unlock()
}

// UpdateCLIContext creates the global directory, context.yaml and
// its lock file private, and replaces a too-open context.yaml with a
// private one.
func TestWindowsUpdateCLIContext_Private(t *testing.T) {
	home := useWindowsHome(t)
	globalDir := filepath.Join(home, GlobalDir)
	if err := UpdateCLIContext(func(*CLIContext) error { return nil }); err != nil {
		t.Fatalf("UpdateCLIContext: %v", err)
	}
	wintest.AssertPrivateSD(t, globalDir)
	ctxPath := filepath.Join(globalDir, cliContextFileName)
	wintest.AssertPrivateSD(t, ctxPath)
	wintest.AssertPrivateSD(t, filepath.Join(globalDir, cliContextLockFileName))

	wintest.MakeUsersReadable(t, ctxPath)
	if err := UpdateCLIContext(func(*CLIContext) error { return nil }); err != nil {
		t.Fatalf("UpdateCLIContext over a too-open file: %v", err)
	}
	wintest.AssertPrivateSD(t, ctxPath)
}

// The plugin config file and its directory are created private,
// and a too-open existing file is replaced with a private one.
func TestWindowsIntegrationConfig_Private(t *testing.T) {
	base := usersInheritDir(t)
	path := filepath.Join(base, "plugins", "slack.yaml")
	if err := CreatePluginConfigFile("slack", path); err != nil {
		t.Fatalf("CreatePluginConfigFile: %v", err)
	}
	wintest.AssertPrivateSD(t, filepath.Dir(path))
	wintest.AssertPrivateSD(t, path)

	wintest.MakeUsersReadable(t, path)
	p, err := NewYAMLConfigProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Save(t.Context(), map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Save over a too-open file: %v", err)
	}
	wintest.AssertPrivateSD(t, path)
	got, err := p.Load(t.Context())
	if err != nil || got["k"] != "v" {
		t.Fatalf("Load after Save = %v, %v", got, err)
	}
}

// The migration owner check reads the owner SID on a no-follow
// handle. A SYSTEM-owned file or directory is not the user's; a junction is
// refused (an error: the caller skips). Control: the user's own file and
// directory pass.
func TestWindowsCurrentUserOwns(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	mine, mineDir := filepath.Join(dir, "mine"), filepath.Join(dir, "minedir")
	sysFile, sysDir := filepath.Join(dir, "sys"), filepath.Join(dir, "sysdir")
	for _, f := range []string{mine, sysFile} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{mineDir, sysDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	wintest.SetOwner(t, sysFile, wintest.SIDSystem)
	wintest.SetOwner(t, sysDir, wintest.SIDSystem)
	j := filepath.Join(dir, "junction")
	wintest.MustJunction(t, outside, j)

	for _, p := range []string{mine, mineDir} {
		if owned, err := currentUserOwns(p, nil); err != nil || !owned {
			t.Errorf("control: currentUserOwns(%s) = %v, %v; want true", p, owned, err)
		}
	}
	for _, p := range []string{sysFile, sysDir} {
		if owned, err := currentUserOwns(p, nil); err != nil || owned {
			t.Errorf("currentUserOwns(SYSTEM-owned %s) = %v, %v; want false", p, owned, err)
		}
	}
	if owned, err := currentUserOwns(j, nil); !errors.Is(err, privatefs.ErrReparsePoint) || owned {
		t.Errorf("currentUserOwns(junction) = %v, %v; want ErrReparsePoint", owned, err)
	}
}

// The owner check through the migration: a legacy entry owned by SYSTEM is skipped
// as owned by another user, and left in place. Control: the user's own
// entry next to it is migrated.
func TestWindowsMigrateLegacyGlobalLayout_SkipsForeignOwnedEntry(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, legacyProjectsDirName)
	for _, d := range []string{"mine", "foreign"} {
		if err := os.MkdirAll(filepath.Join(legacy, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	wintest.SetOwner(t, filepath.Join(legacy, "foreign"), wintest.SIDSystem)
	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	if len(r.skipped) != 1 || r.skipped[0].old != filepath.Join(legacy, "foreign") || r.skipped[0].reason != "owned by another user" {
		t.Fatalf("skipped = %+v; want the foreign entry, owned by another user", r.skipped)
	}
	if fi, err := os.Lstat(filepath.Join(legacy, "foreign")); err != nil || !fi.IsDir() {
		t.Fatalf("the foreign entry was touched: %v, %v", fi, err)
	}
	if len(r.migrated) != 1 || r.migrated[0].old != filepath.Join(legacy, "mine") {
		t.Fatalf("control: migrated = %+v; want the user's own entry", r.migrated)
	}
}

// mkdirLikeLegacyRoot: the canonical root is created Private when the legacy
// root passes CheckPrivate, and Inherit when it does not: protection
// follows the source's real ACL, not its meaningless Perm.
func TestWindowsMkdirLikeLegacyRoot_FollowsSourceACL(t *testing.T) {
	home := usersInheritDir(t)
	tree, err := privatefs.OpenTree(home, privatefs.TreeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	if err := tree.MkdirAll("legacy-private", privatefs.Private); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "legacy-open"), 0o700); err != nil {
		t.Fatal(err)
	}
	assertInherited(t, filepath.Join(home, "legacy-open"))

	if err := mkdirLikeLegacyRoot(home, "legacy-private", "canon-private", nil); err != nil {
		t.Fatal(err)
	}
	wintest.AssertPrivateSD(t, filepath.Join(home, "canon-private"))
	if err := mkdirLikeLegacyRoot(home, "legacy-open", "canon-open", nil); err != nil {
		t.Fatal(err)
	}
	assertInherited(t, filepath.Join(home, "canon-open"))
}

// writeConflictBackup: the conflict backup is created private, next to the
// settings file, with a numeric suffix when the name is taken.
func TestWindowsWriteConflictBackup_Private(t *testing.T) {
	dir := usersInheritDir(t)
	path := filepath.Join(dir, "settings.yaml")
	// The backup name itself is the migration's compatibility literal; it
	// is checked by the unix suite. Here: next to path, and the second
	// backup takes the first name plus ".1".
	first, err := writeConflictBackup(path, []byte("one"))
	if err != nil || filepath.Dir(first) != dir || !strings.HasPrefix(first, path+".") || !strings.HasSuffix(first, ".bak") {
		t.Fatalf("writeConflictBackup = %q, %v", first, err)
	}
	second, err := writeConflictBackup(path, []byte("two"))
	if err != nil || second != first+".1" {
		t.Fatalf("second writeConflictBackup = %q, %v; want %q", second, err, first+".1")
	}
	for p, want := range map[string]string{first: "one", second: "two"} {
		wintest.AssertPrivateSD(t, p)
		mustRead(t, p, want)
	}
}

// copySettingsBackup: the copied settings backup (no hard links) is Private when the
// original passes CheckPrivate and Inherit when it does not.
func TestWindowsCopySettingsBackup_FollowsSourceACL(t *testing.T) {
	dir := usersInheritDir(t)
	tree, err := privatefs.OpenTree(dir, privatefs.TreeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	if err := tree.WriteFile("private.yaml", []byte("p"), privatefs.Private); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "open.yaml"), []byte("o"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		src, want string
		private   bool
	}{{"private.yaml", "p", true}, {"open.yaml", "o", false}} {
		src := filepath.Join(dir, c.src)
		backup := src + ".bak"
		if err := copySettingsBackup(src, backup, nil); err != nil {
			t.Fatalf("copySettingsBackup(%s): %v", c.src, err)
		}
		mustRead(t, backup, c.want)
		if c.private {
			wintest.AssertPrivateSD(t, backup)
		} else {
			assertInherited(t, backup)
		}
		if err := copySettingsBackup(src, backup, nil); err == nil {
			t.Errorf("copySettingsBackup over an existing backup succeeded; want an exclusive create")
		}
	}
}

// copySettingsBackup: the protection decision and the read are made on
// the same handle. A settings name swapped for a link to a private file
// (after backupSettingsFile's Lstat) yields a Private backup holding those
// private bytes, never an Inherit copy of them. Control: the same swap to
// a too-open file yields an Inherit backup of its bytes. And copyBackupFrom,
// which takes only the handle, decides from the object that handle reads.
func TestWindowsCopySettingsBackup_DecisionFromReadHandle(t *testing.T) {
	dir, other := usersInheritDir(t), t.TempDir()
	otherTree, err := privatefs.OpenTree(other, privatefs.TreeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = otherTree.Close() }()
	if err := otherTree.WriteFile("credentials.json", []byte("secret"), privatefs.Private); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(other, "open.yaml")
	if err := os.WriteFile(open, []byte("open"), 0o644); err != nil {
		t.Fatal(err)
	}
	wintest.MakeUsersReadable(t, open)

	swapped := filepath.Join(dir, "settings.yaml")
	wintest.MustSymlink(t, filepath.Join(other, "credentials.json"), swapped)
	if err := copySettingsBackup(swapped, swapped+".bak", nil); err != nil {
		t.Fatalf("copySettingsBackup of a name swapped to a private file: %v", err)
	}
	mustRead(t, swapped+".bak", "secret")
	wintest.AssertPrivateSD(t, swapped+".bak")

	control := filepath.Join(dir, "other.yaml")
	wintest.MustSymlink(t, open, control)
	if err := copySettingsBackup(control, control+".bak", nil); err != nil {
		t.Fatalf("control: copySettingsBackup of a name swapped to an open file: %v", err)
	}
	mustRead(t, control+".bak", "open")
	assertInherited(t, control+".bak")

	// The helper sees only the handle: a handle on the private file gives
	// a Private copy whatever name it is written under.
	src, err := os.Open(filepath.Join(other, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	tree, err := privatefs.OpenTree(dir, privatefs.TreeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	if err := copyBackupFrom(tree, src, "open.yaml.bak"); err != nil {
		t.Fatal(err)
	}
	mustRead(t, filepath.Join(dir, "open.yaml.bak"), "secret")
	wintest.AssertPrivateSD(t, filepath.Join(dir, "open.yaml.bak"))
}

// mkdirLikeLegacyRoot and the backups: only a CheckPrivate error wrapping
// ErrNotPrivate yields Inherit. That includes a descriptor or owner privatefs
// cannot read, which it classifies as ErrNotPrivate (the case is built in
// securityOf's shape). A pass and the errors privatefs does not wrap (DACL
// or ACE read errors) fail closed to Private.
func TestWindowsProtectionForCheck_FailsClosed(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want privatefs.Protection
	}{
		{"passes", nil, privatefs.Private},
		{"not private", fmt.Errorf("x grants access to S-1-5-32-545: %w", privatefs.ErrNotPrivate), privatefs.Inherit},
		{"descriptor unreadable (classified ErrNotPrivate by privatefs)", fmt.Errorf("%w: cannot read the security descriptor of x: %w", privatefs.ErrNotPrivate, windows.ERROR_ACCESS_DENIED), privatefs.Inherit},
		{"DACL read error", fmt.Errorf("read the DACL of x: %w", windows.ERROR_ACCESS_DENIED), privatefs.Private},
		{"other error", errors.New("boom"), privatefs.Private},
	} {
		if got := protectionForCheck(c.err); got != c.want {
			t.Errorf("%s: protectionForCheck(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
