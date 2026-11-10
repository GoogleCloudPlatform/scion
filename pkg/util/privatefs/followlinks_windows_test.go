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

//go:build windows

package privatefs

// TreeOptions.FollowLinks, the one-name rule's link-count refusal and
// ReplaceFollowing (the follow-then-verify rule). Each refusal has its positive
// control in the same test.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

func followTree(t *testing.T, dir string) *Tree {
	t.Helper()
	tr, err := OpenTree(dir, TreeOptions{FollowLinks: true})
	if err != nil {
		t.Fatalf("OpenTree(%s, FollowLinks): %v", dir, err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// FollowLinks follows a junction, an absolute symlink and a relative
// symlink at a directory component (the dotfile-manager layouts), and the
// directory it creates through them is private. Control (the refusal side):
// the same walk in a tree without FollowLinks is refused.
func TestTree_FollowLinksDirectoryComponents(t *testing.T) {
	for _, kind := range []string{"junction", "symlink", "relative-symlink"} {
		t.Run(kind, func(t *testing.T) {
			// A directory tree no other principal can write to, like a
			// profile; FollowLinks checks every existing component.
			root := replaceDir(t)
			anchor := filepath.Join(root, "home")
			real := filepath.Join(root, "dotfiles", "ssh")
			for _, d := range []string{anchor, real} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(anchor, ".ssh")
			switch kind {
			case "junction":
				wintest.MustJunction(t, real, link)
			case "symlink":
				wintest.MustSymlink(t, real, link)
			default:
				wintest.MustSymlink(t, filepath.Join("..", "dotfiles", "ssh"), link)
			}

			if err := treeAt(t, anchor).MkdirAll(".ssh/config.d", Private); !errors.Is(err, ErrReparsePoint) {
				t.Fatalf("without FollowLinks: %v, want ErrReparsePoint", err)
			}
			tr := followTree(t, anchor)
			if err := tr.MkdirAll(".ssh/config.d", Private); err != nil {
				t.Fatalf("FollowLinks MkdirAll through a %s: %v", kind, err)
			}
			if fi, err := os.Stat(filepath.Join(real, "config.d")); err != nil || !fi.IsDir() {
				t.Fatalf("config.d not created in the link's target: %v", err)
			}
			assertPrivateSD(t, filepath.Join(real, "config.d"))
			// The link's own leaf (".ssh" as the last component) is
			// followed too, and existing directories are left as they are.
			if err := tr.MkdirAll(".ssh", Private); err != nil {
				t.Fatalf("FollowLinks MkdirAll of the link itself: %v", err)
			}
		})
	}
}

// grantName names an icacls grant for a subtest (and so for the temp
// directory that cmd's mklink is given: no '=' or parentheses).
func grantName(grant string) string {
	switch {
	case grant == "":
		return "none"
	case strings.Contains(grant, "DC"):
		return "deletechild"
	default:
		return "modify"
	}
}

// FollowLinks, refusal side: an
// EXISTING directory component another principal can write to (Modify) or
// delete children from (DELETE_CHILD only) is refused with ErrNotPrivate,
// whether it is reached through a junction or is a plain directory, and
// nothing is created there. Controls: the same layout once the grant is
// removed, and a component owned by Administrators (the follow-then-verify
// owner set).
func TestTree_FollowLinksRefusesForeignWritableComponent(t *testing.T) {
	for _, grant := range []string{"(OI)(CI)(M)", "(DC)"} {
		for _, viaLink := range []bool{true, false} {
			// Subtest names become temp paths given to mklink: letters only.
			t.Run(fmt.Sprintf("%s_link_%v", grantName(grant), viaLink), func(t *testing.T) {
				root := replaceDir(t)
				anchor := filepath.Join(root, "home")
				target := filepath.Join(root, "shared")
				for _, d := range []string{anchor, target} {
					if err := os.MkdirAll(d, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				comp := filepath.Join(anchor, ".scion")
				if viaLink {
					wintest.MustJunction(t, target, comp)
				} else {
					target = comp
					if err := os.Mkdir(comp, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				wintest.Icacls(t, target, "/grant", "*"+wintest.SIDUsers+":"+grant)
				err := followTree(t, anchor).MkdirAll(".scion/vscode/user-data", Private)
				if !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), wintest.SIDUsers) {
					t.Fatalf("foreign-writable component (%s): %v, want ErrNotPrivate naming Users", grant, err)
				}
				if entries, _ := os.ReadDir(target); len(entries) != 0 {
					t.Fatalf("created under the refused component: %v", entries)
				}
				wintest.Icacls(t, target, "/remove:g", "*"+wintest.SIDUsers)
				if err := followTree(t, anchor).MkdirAll(".scion/vscode/user-data", Private); err != nil {
					t.Fatalf("control: without the grant: %v", err)
				}
				wintest.SetOwner(t, target, wintest.SIDAdministrators)
				if err := followTree(t, anchor).MkdirAll(".scion/vscode/user-data", Private); err != nil {
					t.Fatalf("control: an Administrators-owned component: %v", err)
				}
			})
		}
	}
}

// Accepted residual, warned: when a
// followed link's target sits in a parent directory another principal can
// write to (Modify) or delete children from (DELETE_CHILD), Warn gets ONE
// message naming that directory and the SID, and the operation still
// succeeds. Control: a private parent gives no warning.
func TestTree_FollowLinksWarnsOnMutableTargetParent(t *testing.T) {
	for _, grant := range []string{"(M)", "(DC)", ""} {
		t.Run(grantName(grant), func(t *testing.T) {
			root := replaceDir(t)
			anchor := filepath.Join(root, "home")
			parent := filepath.Join(root, "shared")
			target := filepath.Join(parent, "ssh")
			for _, d := range []string{anchor, target} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if grant != "" {
				// Not inheritable: only the parent is foreign-writable;
				// the target itself passes the component check.
				wintest.Icacls(t, parent, "/grant", "*"+wintest.SIDUsers+":"+grant)
			}
			wintest.MustJunction(t, target, filepath.Join(anchor, ".ssh"))
			var warnings []string
			tr, err := OpenTree(anchor, TreeOptions{FollowLinks: true, Warn: func(m string) { warnings = append(warnings, m) }})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tr.Close() }()
			for _, rel := range []string{".ssh/config.d", ".ssh/other"} {
				if err := tr.MkdirAll(rel, Private); err != nil {
					t.Fatalf("MkdirAll(%s) must still succeed: %v", rel, err)
				}
			}
			if grant == "" {
				if len(warnings) != 0 {
					t.Fatalf("control: private parent warned: %v", warnings)
				}
				return
			}
			if len(warnings) != 1 {
				t.Fatalf("warnings %v, want exactly one", warnings)
			}
			if !strings.Contains(warnings[0], "shared") || !strings.Contains(warnings[0], wintest.SIDUsers) {
				t.Fatalf("warning %q must name the parent directory and the SID", warnings[0])
			}
		})
	}
}

// FollowLinks never follows a leaf FILE: a symlink at the leaf is still
// refused by Create. Control: a regular leaf in the same followed
// directory is created.
func TestTree_FollowLinksLeafFileStillRefused(t *testing.T) {
	root := replaceDir(t)
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	anchor := filepath.Join(root, "a")
	if err := os.MkdirAll(anchor, 0o700); err != nil {
		t.Fatal(err)
	}
	wintest.MustSymlink(t, outside, filepath.Join(anchor, "leaf"))
	tr := followTree(t, anchor)
	if _, err := tr.Create("leaf", false, Private); !errors.Is(err, ErrReparsePoint) {
		t.Fatalf("Create of a symlinked leaf with FollowLinks: %v, want ErrReparsePoint", err)
	}
	f, err := tr.Create("ok", true, Private)
	if err != nil {
		t.Fatalf("control: regular leaf: %v", err)
	}
	_ = f.Close()
}

// The one-name rule: an existing leaf opened for writing with more than one name
// is refused, never repaired: OpenPrivate for writing and a non-exclusive
// Create. Control: an ordinary leaf with the same DACL passes both, and a
// read-only OpenPrivate of the linked file (no write) is not affected.
func TestWriteLeaf_RefusesHardLink(t *testing.T) {
	root := t.TempDir()
	tr := treeAt(t, root)
	linked := mustCreate(t, tr, "linked", Private, "x")
	other := filepath.Join(t.TempDir(), "second-name")
	if err := os.Link(linked, other); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	ordinary := mustCreate(t, tr, "ordinary", Private, "x")
	dacl := privateDACL(wintest.UserSID(t))
	wintest.SetDACL(t, linked, dacl)
	wintest.SetDACL(t, ordinary, dacl)

	if _, err := tr.OpenPrivate("linked", os.O_WRONLY); !errors.Is(err, ErrHardLinked) {
		t.Fatalf("OpenPrivate(write) of a hard-linked file: %v, want ErrHardLinked", err)
	}
	if _, err := tr.OpenPrivate("linked", os.O_RDWR|os.O_TRUNC); !errors.Is(err, ErrHardLinked) {
		t.Fatalf("OpenPrivate(rdwr) of a hard-linked file: %v, want ErrHardLinked", err)
	}
	if _, err := tr.Create("linked", false, Private); !errors.Is(err, ErrHardLinked) {
		t.Fatalf("Create of a hard-linked file: %v, want ErrHardLinked", err)
	}
	if _, err := tr.Create("linked", false, Inherit); !errors.Is(err, ErrHardLinked) {
		t.Fatalf("Create(Inherit) of a hard-linked file: %v, want ErrHardLinked", err)
	}
	if b, err := os.ReadFile(other); err != nil || string(b) != "x" {
		t.Fatalf("the second name's content changed: %q, %v", b, err)
	}

	for _, flag := range []int{os.O_WRONLY, os.O_RDWR | os.O_TRUNC} {
		f, err := tr.OpenPrivate("ordinary", flag)
		if err != nil {
			t.Fatalf("control: OpenPrivate(%#x) of an ordinary file with the same DACL: %v", flag, err)
		}
		_ = f.Close()
	}
	f, err := tr.Create("ordinary", false, Private)
	if err != nil {
		t.Fatalf("control: Create of an ordinary file: %v", err)
	}
	_ = f.Close()
	if _, err := tr.ReadPrivate("linked", 0); err != nil {
		t.Fatalf("read-only access to the linked file: %v", err)
	}
}

// replaceDir returns a directory whose DACL is the user's alone (no
// foreign writer), like a default profile directory.
func replaceDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	u := wintest.UserSID(t)
	wintest.SetDACL(t, d, fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;%s)(A;OICI;FA;;;%s)", u, wintest.SIDSystem, wintest.SIDAdministrators))
	return d
}

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The follow-then-verify rule: a new file is private; a user-owned too-open file is replaced by
// a private one (the old descriptor is not copied).
func TestReplaceFollowing_NewPrivateAndRepairsOwned(t *testing.T) {
	dir := replaceDir(t)
	p := filepath.Join(dir, "config")
	if err := ReplaceFollowing(p, []byte("one"), 0o644, ".tmp-"); err != nil {
		t.Fatalf("new file: %v", err)
	}
	assertPrivateSD(t, p)
	u := wintest.UserSID(t)
	wintest.SetDACL(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;%s)", u, wintest.SIDUsers))
	if err := ReplaceFollowing(p, []byte("two"), 0o644, ".tmp-"); err != nil {
		t.Fatalf("user-owned too-open file: %v", err)
	}
	assertPrivateSD(t, p)
	if got := readString(t, p); got != "two" {
		t.Fatalf("content %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

// The follow-then-verify rule: a symlinked leaf is followed; the target is replaced (private)
// and the link is kept.
func TestReplaceFollowing_FollowsSymlinkLeaf(t *testing.T) {
	dir := replaceDir(t)
	real := filepath.Join(dir, "real-config")
	if err := os.WriteFile(real, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config")
	wintest.MustSymlink(t, real, link)
	if err := ReplaceFollowing(link, []byte("new"), 0o600, ".tmp-"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was not kept: %v %v", fi, err)
	}
	if got := readString(t, real); got != "new" {
		t.Fatalf("target content %q", got)
	}
	assertPrivateSD(t, real)
}

// Follow-then-verify refuse + control: a parent directory another principal can write
// to is refused (nothing written); without the foreign ACE it works.
func TestReplaceFollowing_RefusesForeignWritableParent(t *testing.T) {
	dir := replaceDir(t)
	p := filepath.Join(dir, "config")
	wintest.Icacls(t, dir, "/grant", "*"+wintest.SIDUsers+":(M)")
	err := ReplaceFollowing(p, []byte("x"), 0o600, ".tmp-")
	if !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("foreign-writable parent: %v, want ErrNotPrivate", err)
	}
	if _, serr := os.Lstat(p); !os.IsNotExist(serr) {
		t.Fatalf("a file was written: %v", serr)
	}
	wintest.Icacls(t, dir, "/remove:g", "*"+wintest.SIDUsers)
	if err := ReplaceFollowing(p, []byte("x"), 0o600, ".tmp-"); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// fakeAdminsGroupSID creates a local group named like Administrators
// ("scion-test-administrators", a new, non-well-known SID; the builtin
// name itself belongs to S-1-5-32-544) and returns its SID. The SID is
// looked up here, in the test, only to set it as an owner: the code under
// test compares SIDs and never names.
func fakeAdminsGroupSID(t *testing.T) string {
	t.Helper()
	const group = "scion-test-administrators"
	_ = exec.Command("net", "localgroup", group, "/delete").Run()
	if out, err := exec.Command("net", "localgroup", group, "/add").CombinedOutput(); err != nil {
		t.Fatalf("create local group %s (the Windows job runs as an administrator): %v: %s", group, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("net", "localgroup", group, "/delete").Run() })
	sid, _, _, err := windows.LookupSID("", group)
	if err != nil {
		t.Fatalf("look up the test group's SID: %v", err)
	}
	if sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) || sid.IsWellKnown(windows.WinLocalSystemSid) {
		t.Fatal("the test group resolved to a well-known SID")
	}
	return sid.String()
}

// Follow-then-verify refuse + control: a target owned by another principal (a local
// group named like Administrators, but not S-1-5-32-544) is refused and
// left untouched, naming its SID; a user-owned target with the same DACL
// is replaced.
func TestReplaceFollowing_RefusesForeignOwnedTarget(t *testing.T) {
	dir := replaceDir(t)
	u := wintest.UserSID(t)
	foreignSID := fakeAdminsGroupSID(t)
	open := fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FR;;;%s)", u, wintest.SIDUsers)
	foreign := filepath.Join(dir, "foreign")
	if err := os.WriteFile(foreign, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	wintest.SetDACL(t, foreign, open)
	wintest.SetOwner(t, foreign, foreignSID)
	err := ReplaceFollowing(foreign, []byte("new"), 0o600, ".tmp-")
	if !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), foreignSID) {
		t.Fatalf("target owned by %s: %v, want ErrNotPrivate naming the SID", foreignSID, err)
	}
	if got := readString(t, foreign); got != "old" {
		t.Fatalf("the foreign file changed: %q", got)
	}
	owned := filepath.Join(dir, "owned")
	if err := os.WriteFile(owned, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	wintest.SetDACL(t, owned, open)
	if err := ReplaceFollowing(owned, []byte("new"), 0o600, ".tmp-"); err != nil {
		t.Fatalf("control: user-owned target: %v", err)
	}
	assertPrivateSD(t, owned)
}

// For the integrity-only rule,
// SYSTEM (S-1-5-18) and Administrators (S-1-5-32-544) are accepted as
// owners of the target and of its parent directory, by SID. A too-open
// target they own is replaced by a private one. Refusal side: a parent
// owned by a local group merely NAMED like Administrators is refused.
// CheckPrivate is unchanged: it still refuses those owners.
func TestReplaceFollowing_AcceptsSystemAndAdminsOwners(t *testing.T) {
	u := wintest.UserSID(t)
	for _, owner := range []string{wintest.SIDSystem, wintest.SIDAdministrators} {
		t.Run(owner, func(t *testing.T) {
			dir := replaceDir(t)
			target := filepath.Join(dir, "config")
			if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			wintest.SetDACL(t, target, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;%s)(A;;FR;;;%s)", u, wintest.SIDSystem, wintest.SIDUsers))
			wintest.SetOwner(t, target, owner)
			if err := checkPath(t, target); !errors.Is(err, ErrNotPrivate) {
				t.Fatalf("CheckPrivate of a %s-owned file: %v, want ErrNotPrivate (unchanged)", owner, err)
			}
			if err := ReplaceFollowing(target, []byte("new"), 0o600, ".tmp-"); err != nil {
				t.Fatalf("target owned by %s: %v", owner, err)
			}
			assertPrivateSD(t, target)

			wintest.SetOwner(t, dir, owner)
			if err := ReplaceFollowing(filepath.Join(dir, "other"), []byte("x"), 0o600, ".tmp-"); err != nil {
				t.Fatalf("parent owned by %s: %v", owner, err)
			}
		})
	}
	dir := replaceDir(t)
	fake := fakeAdminsGroupSID(t)
	wintest.SetOwner(t, dir, fake)
	if err := ReplaceFollowing(filepath.Join(dir, "x"), []byte("x"), 0o600, ".tmp-"); !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), fake) {
		t.Fatalf("parent owned by the look-alike group %s: %v, want ErrNotPrivate naming it", fake, err)
	}
	wintest.SetOwner(t, dir, u)
	if err := ReplaceFollowing(filepath.Join(dir, "x"), []byte("x"), 0o600, ".tmp-"); err != nil {
		t.Fatalf("control: user-owned parent: %v", err)
	}
}

// The one-name rule on the relaxed path: a hard-linked target is refused (not
// replaced); an ordinary target with the same DACL is replaced.
func TestReplaceFollowing_RefusesHardLinkedTarget(t *testing.T) {
	dir := replaceDir(t)
	linked := filepath.Join(dir, "linked")
	ordinary := filepath.Join(dir, "ordinary")
	for _, p := range []string{linked, ordinary} {
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		wintest.SetDACL(t, p, privateDACL(wintest.UserSID(t)))
	}
	if err := os.Link(linked, filepath.Join(t.TempDir(), "second")); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	if err := ReplaceFollowing(linked, []byte("new"), 0o600, ".tmp-"); !errors.Is(err, ErrHardLinked) {
		t.Fatalf("hard-linked target: %v, want ErrHardLinked", err)
	}
	if got := readString(t, linked); got != "old" {
		t.Fatalf("the hard-linked file changed: %q", got)
	}
	if err := ReplaceFollowing(ordinary, []byte("new"), 0o600, ".tmp-"); err != nil {
		t.Fatalf("control: ordinary target: %v", err)
	}
}

// dirListing returns name -> content for the files in dir (one level),
// to show that a call wrote nothing.
func dirListing(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		out[e.Name()] = string(b)
	}
	return out
}

// CheckReplaceTarget is ReplaceFollowing's checks
// with no write: a hard-linked target, a foreign-owned target and a
// foreign-writable parent are refused and nothing in the directory
// changes; controls: an ordinary target and a missing target pass, still
// writing nothing.
func TestCheckReplaceTarget_RefusesAndWritesNothing(t *testing.T) {
	dir := replaceDir(t)
	linked := filepath.Join(dir, "linked")
	foreign := filepath.Join(dir, "foreign")
	ordinary := filepath.Join(dir, "ordinary")
	for _, p := range []string{linked, foreign, ordinary} {
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(linked, filepath.Join(t.TempDir(), "second")); err != nil {
		t.Fatal(err)
	}
	wintest.SetOwner(t, foreign, fakeAdminsGroupSID(t))
	before := dirListing(t, dir)
	if err := CheckReplaceTarget(linked); !errors.Is(err, ErrHardLinked) {
		t.Fatalf("hard-linked target: %v, want ErrHardLinked", err)
	}
	if err := CheckReplaceTarget(foreign); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("foreign-owned target: %v, want ErrNotPrivate", err)
	}
	for _, p := range []string{ordinary, filepath.Join(dir, "missing"), filepath.Join(dir, "no-dir", "missing")} {
		if err := CheckReplaceTarget(p); err != nil {
			t.Fatalf("control %s: %v", p, err)
		}
	}
	wintest.Icacls(t, dir, "/grant", "*"+wintest.SIDUsers+":(DC)")
	if err := CheckReplaceTarget(ordinary); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("foreign-writable parent: %v, want ErrNotPrivate", err)
	}
	wintest.Icacls(t, dir, "/remove:g", "*"+wintest.SIDUsers)
	if got := dirListing(t, dir); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("CheckReplaceTarget changed the directory:\nbefore %v\nafter  %v", before, got)
	}
}
