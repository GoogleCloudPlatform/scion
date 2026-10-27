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

package cmd

// ensureSSHDir makes no location decision, so the follow-then-verify rule
// on the resolved handles carries the property alone. These tests
// pin that down for every caller of ensureSSHDir, for the name validation
// on its path, and for the link-count rule on every ssh/vscode write leaf.
// Each refusal has its positive control in the same test.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// foreignWritableDir returns a directory outside any home on which Users
// hold grant (an icacls grant: "(OI)(CI)(M)" for Modify, "(DC)" for
// DELETE_CHILD only), and a function that takes it away again.
func foreignWritableDir(t *testing.T, grant string) (string, func()) {
	t.Helper()
	d := privateHome(t)
	wintest.Icacls(t, d, "/grant", "*"+wintest.SIDUsers+":"+grant)
	return d, func() {
		wintest.Icacls(t, d, "/remove:g", "*"+wintest.SIDUsers)
		setUserOnlyDirDACL(t, d)
	}
}

// grantSubtestName and safeSubtestName keep subtest names (and so the
// temp directories passed to mklink) to letters, digits and underscores.
func grantSubtestName(grant string) string {
	switch {
	case grant == "":
		return "none"
	case strings.Contains(grant, "DC"):
		return "deletechild"
	default:
		return "modify"
	}
}

func safeSubtestName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, s)
}

// treeSnapshot records every entry below dir (links are recorded, not
// followed): directories, other entries by type, and regular files with
// their content. Two equal snapshots mean nothing was created, removed or
// changed.
func treeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.Type().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] = "file:" + string(b)
		case d.IsDir():
			out[path] = "dir"
		default:
			out[path] = "other:" + d.Type().String()
		}
		return nil
	}))
	return out
}

func vscodeUpdateFor(t *testing.T, agent string) vscodeUpdate {
	t.Helper()
	block, err := renderSSHHostBlockKnown(winSSHExe, "https://h.example", "h", "p", agent, vscodeKnownHostsRef)
	require.NoError(t, err)
	return vscodeUpdate{host: "scion.h.p." + agent, block: block,
		key: mustNormKey(t, testSSHPublicKey(t)), endpoint: tunnelclient.NormalizeEndpoint("https://h.example")}
}

// For EVERY caller of
// ensureSSHDir, a link (junction) at ~/.ssh, ~/.scion or a directory below
// them whose target another principal can write to (Modify) or delete
// children from (DELETE_CHILD only) is followed and then REFUSED by the
// FollowLinks component check, as a returned ErrNotPrivate. A refusal
// leaves no side effect: home and the target are
// unchanged, no file AND no directory created (not even the lock file or
// a directory for another path of the same call). Control: once the
// foreign right is removed, the same call succeeds and its leaves are
// private.
func TestSSHDirCallers_ForeignWritableLinkTargetRefused(t *testing.T) {
	includeLine := func(t *testing.T, home string) error {
		_, err := ensureSSHIncludeLine(sshPaths{home: home})
		return err
	}
	hubPlan := func(t *testing.T, home string) error {
		p := sshPaths{home: home}
		return writeSSHHubPlan(p, sshPlan(p))
	}
	vscodeFiles := func(t *testing.T, home string) error {
		_, err := updateVSCodeFiles(vscodePaths{home: home}, vscodeUpdateFor(t, "a1"))
		return err
	}
	vscodeSettings := func(t *testing.T, home string) error {
		return ensureVSCodeSettings(vscodePaths{home: home})
	}
	cases := []struct {
		name   string
		link   string   // relative to home; its parent is created first
		leaves []string // files the call writes, relative to the link target
		call   func(t *testing.T, home string) error
	}{
		{"ensureSSHIncludeLine ~/.ssh", ".ssh", []string{"config"}, includeLine},
		{"writeSSHHubPlan include ~/.ssh", ".ssh", []string{"config.d/scion-hub", "config"}, hubPlan},
		{"writeSSHHubPlan include ~/.ssh/config.d", ".ssh/config.d", []string{"scion-hub"}, hubPlan},
		{"writeSSHHubPlan known-hosts ~/.scion", ".scion", []string{"ssh/known_hosts-hub"}, hubPlan},
		{"writeSSHHubPlan known-hosts ~/.scion/ssh", ".scion/ssh", []string{"known_hosts-hub"}, hubPlan},
		{"updateVSCodeFiles ~/.scion", ".scion", []string{"vscode/ssh_config", "vscode/known_hosts", "vscode/user-data/User/settings.json"}, vscodeFiles},
		{"updateVSCodeFiles ~/.scion/vscode", ".scion/vscode", []string{"ssh_config", "known_hosts", "user-data/User/settings.json"}, vscodeFiles},
		{"updateVSCodeFiles ~/.scion/vscode/user-data/User", ".scion/vscode/user-data/User", []string{"settings.json"}, vscodeFiles},
		{"ensureVSCodeSettings ~/.scion", ".scion", []string{"vscode/user-data/User/settings.json"}, vscodeSettings},
		{"ensureVSCodeSettings ~/.scion/vscode/user-data/User", ".scion/vscode/user-data/User", []string{"settings.json"}, vscodeSettings},
	}
	for _, grant := range []string{"(OI)(CI)(M)", "(DC)"} {
		for _, tc := range cases {
			// Subtest names become temp paths given to mklink: no '=',
			// parentheses, '~' or spaces.
			t.Run(grantSubtestName(grant)+"_"+safeSubtestName(tc.name), func(t *testing.T) {
				home := privateHome(t)
				target, clear := foreignWritableDir(t, grant)
				link := filepath.Join(home, filepath.FromSlash(tc.link))
				require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
				wintest.MustJunction(t, target, link)
				homeBefore, targetBefore := treeSnapshot(t, home), treeSnapshot(t, target)

				err := tc.call(t, home)
				require.ErrorIs(t, err, privatefs.ErrNotPrivate, "the foreign-writable link target is refused")
				assert.Contains(t, err.Error(), wintest.SIDUsers)
				assert.Equal(t, homeBefore, treeSnapshot(t, home), "the refusal changed nothing in home")
				assert.Equal(t, targetBefore, treeSnapshot(t, target), "the refusal changed nothing in the target")

				clear()
				require.NoError(t, tc.call(t, home), "control: the same link once no one else can write there")
				for _, l := range tc.leaves {
					assertPrivateSD(t, filepath.Join(target, filepath.FromSlash(l)))
				}
			})
		}
	}
}

// Foreign-owned target: ~/.ssh/config is a link to a file
// owned by another principal (a group named like Administrators, not
// S-1-5-32-544) in a directory the user controls: refused, untouched, and
// with no side effect: home and the link's directory are unchanged, so
// writeSSHHubPlan wrote neither the known-hosts nor the include file
// first. Control: the same link to a user-owned file is followed and
// the file replaced by a private one.
func TestSSHWrite_LinkToForeignOwnedTargetRefused(t *testing.T) {
	calls := map[string]func(p sshPaths) error{
		"ensureSSHIncludeLine": func(p sshPaths) error { _, err := ensureSSHIncludeLine(p); return err },
		"writeSSHHubPlan":      func(p sshPaths) error { return writeSSHHubPlan(p, sshPlan(p)) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			home := privateHome(t)
			p := sshPaths{home: home}
			require.NoError(t, os.MkdirAll(p.sshDir(), 0o700))
			outside := privateHome(t)
			target := filepath.Join(outside, "ssh_config")
			require.NoError(t, os.WriteFile(target, []byte("Host old\n"), 0o600))
			foreign := fakeAdminsGroupSID(t)
			wintest.SetOwner(t, target, foreign)
			wintest.MustSymlink(t, target, p.userConfig())
			homeBefore, outsideBefore := treeSnapshot(t, home), treeSnapshot(t, outside)

			err := call(p)
			require.ErrorIs(t, err, privatefs.ErrNotPrivate)
			assert.Contains(t, err.Error(), foreign)
			assert.Equal(t, "Host old\n", readTestFile(t, target))
			assert.Equal(t, homeBefore, treeSnapshot(t, home), "the refusal changed nothing in home")
			assert.Equal(t, outsideBefore, treeSnapshot(t, outside), "the refusal changed nothing at the link target")

			wintest.SetOwner(t, target, wintest.UserSID(t))
			require.NoError(t, call(p), "control: a user-owned link target")
			assert.True(t, strings.HasPrefix(readTestFile(t, target), sshIncludeLine))
			assertPrivateSD(t, target)
		})
	}
}

// ValidName is load-bearing on the ensureSSHDir path. '..',
// absolute and drive forms, alternate data streams, trailing dot or space,
// mixed separators, device names and empty components are refused and
// create nothing. Control: the constant relative paths are accepted.
func TestEnsureSSHDir_RefusesInvalidComponents(t *testing.T) {
	root := privateHome(t)
	home := filepath.Join(root, "home")
	require.NoError(t, os.Mkdir(home, 0o700))
	for _, rel := range []string{
		"", "..", "../x", ".ssh/../../x", `.ssh\..\..\x`, `.ssh/..\x`,
		`C:\x`, `C:x`, `\x`, "/x", `\\server\share\x`, `\\?\C:\x`,
		".ssh:evil", ".ssh::$DATA", ".ssh/config.d:stream",
		".ssh.", ".ssh ", ".ssh/config.d.", ".ssh//config.d",
		"CON", "nul.txt", ".ssh/COM1", "LPT1.log",
	} {
		err := ensureSSHDir(home, rel)
		assert.Error(t, err, "%q must be refused", rel)
	}
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing was created next to home: %v", entries)
	entries, err = os.ReadDir(home)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing was created in home")

	for _, rel := range []string{sshDirRel, sshIncludeDirRel, sshKnownHostsDirRel, vscodeDirRel, vscodeSettingsDirRel} {
		require.NoError(t, ensureSSHDir(home, rel), "control: %s", rel)
		assertPrivateSD(t, filepath.Join(home, filepath.FromSlash(rel)))
	}
}

// 8.3 short names: ValidName accepts them by design (an 8.3
// name is a legal component; aliasing is pathcanon.Resolve's concern). On
// this path a component is opened relative to its parent's handle, so a
// short name can only name a CHILD of that same directory: it creates
// inside the tree, never outside it.
func TestEnsureSSHDir_ShortNameStaysInside(t *testing.T) {
	root := privateHome(t)
	home := filepath.Join(root, "home")
	long := filepath.Join(home, "longdirectoryname-8dot3")
	require.NoError(t, os.MkdirAll(long, 0o700))
	short := filepath.Base(wintest.ShortPath(t, long))
	require.Contains(t, short, "~", "the test volume must generate 8.3 names (the job enables them)")
	require.NoError(t, ensureSSHDir(home, short+"/sub"))
	assert.DirExists(t, filepath.Join(long, "sub"), "the short name named the same child")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "nothing outside home")
}

// Every ssh-config and vscode write leaf with a second name
// (a hard link) is REFUSED, never repaired, and its other name is left
// untouched. Control: once the second name is gone, the same writer
// succeeds and the leaf is private.
func TestSSHVSCodeWriteLeaves_RefuseHardLinks(t *testing.T) {
	// writeSSHHubPlan with a fixed plan (label "hub"): it writes exactly
	// these three ssh leaves. (regenerateSSHHubFiles would read the empty
	// pre-seeded scion-hub as another hub's file, pick the fallback label
	// and write scion-hub-example-com instead, so that leaf would not be
	// part of the operation; seen on the Windows job.)
	ssh := func(t *testing.T, home string) error {
		p := sshPaths{home: home}
		return writeSSHHubPlan(p, sshPlan(p))
	}
	vscode := func(t *testing.T, home string) error {
		_, err := updateVSCodeFiles(vscodePaths{home: home}, vscodeUpdateFor(t, "a1"))
		return err
	}
	for _, tc := range []struct {
		leaf  string // relative to home
		write func(t *testing.T, home string) error
	}{
		{".ssh/config", ssh},
		{".ssh/config.d/scion-hub", ssh},
		{".scion/ssh/known_hosts-hub", ssh},
		{".scion/vscode/ssh_config", vscode},
		{".scion/vscode/known_hosts", vscode},
		{".scion/vscode/user-data/User/settings.json", vscode},
	} {
		t.Run(strings.ReplaceAll(tc.leaf, "/", "_"), func(t *testing.T) {
			home := privateHome(t)
			leaf := filepath.Join(home, filepath.FromSlash(tc.leaf))
			require.NoError(t, os.MkdirAll(filepath.Dir(leaf), 0o700))
			require.NoError(t, os.WriteFile(leaf, nil, 0o600))
			second := filepath.Join(home, "second-name")
			require.NoError(t, os.Link(leaf, second))
			before := treeSnapshot(t, home)

			err := tc.write(t, home)
			require.ErrorIs(t, err, privatefs.ErrHardLinked)
			assert.Empty(t, readTestFile(t, second), "the other name is untouched")
			// No side effect: no other leaf, no
			// directory and no lock file was written before the refusal.
			assert.Equal(t, before, treeSnapshot(t, home), "the refusal changed nothing in home")

			require.NoError(t, os.Remove(second))
			require.NoError(t, tc.write(t, home), "control: one name")
			assertPrivateSD(t, leaf)
		})
	}
}

// Accepted residual, warned: ~/.ssh is a link to a directory whose PARENT
// another user can write to or delete children from. ssh-config still
// succeeds (the target itself passes the follow-then-verify rule) and
// prints ONE warning naming that parent and the SID. Control: a
// private parent, no warning.
func TestSSHWrite_WarnsOnMutableLinkTargetParent(t *testing.T) {
	for _, grant := range []string{"(M)", "(DC)", ""} {
		t.Run(grantSubtestName(grant), func(t *testing.T) {
			var out strings.Builder
			prev := sshWarnOut
			sshWarnOut = &out
			t.Cleanup(func() { sshWarnOut = prev })

			home := privateHome(t)
			parent := filepath.Join(privateHome(t), "shared-dotfiles")
			target := filepath.Join(parent, "ssh")
			require.NoError(t, os.MkdirAll(target, 0o700))
			if grant != "" {
				wintest.Icacls(t, parent, "/grant", "*"+wintest.SIDUsers+":"+grant)
			}
			wintest.MustJunction(t, target, filepath.Join(home, ".ssh"))
			p := sshPaths{home: home}
			require.NoError(t, writeSSHHubPlan(p, sshPlan(p)), "the operation still succeeds")
			assertPrivateSD(t, filepath.Join(target, "config"))
			if grant == "" {
				assert.Empty(t, out.String(), "control: a private parent gives no warning")
				return
			}
			assert.Equal(t, 1, strings.Count(out.String(), "scion: warning:"), out.String())
			assert.Contains(t, out.String(), "shared-dotfiles")
			assert.Contains(t, out.String(), wintest.SIDUsers)
		})
	}
}

// On Windows, a hard-linked ~/.ssh/config that ALREADY
// has the scion Include line is not written, so writeSSHHubPlan succeeds
// and the config and its other name stay byte-identical. Refusal side
// (Include line missing): TestSSHVSCodeWriteLeaves_RefuseHardLinks/.ssh_config
// (home snapshot unchanged).
func TestWriteSSHHubPlan_HardLinkedConfigWithIncludeLineUntouched(t *testing.T) {
	home := privateHome(t)
	p := sshPaths{home: home}
	require.NoError(t, os.MkdirAll(p.sshDir(), 0o700))
	content := sshIncludeLine + "\n\nHost old\n"
	require.NoError(t, os.WriteFile(p.userConfig(), []byte(content), 0o600))
	second := filepath.Join(home, "second-name")
	require.NoError(t, os.Link(p.userConfig(), second))
	require.NoError(t, writeSSHHubPlan(p, sshPlan(p)))
	assertPrivateSD(t, p.includeFile("hub"))
	for _, f := range []string{p.userConfig(), second} {
		assert.Equal(t, content, readTestFile(t, f), f)
	}
}

// On Windows, vscode: a hard-linked settings.json that
// already holds the managed values is not rewritten: updateVSCodeFiles
// succeeds and both names stay byte-identical. Refusal side: a hard-linked
// settings.json that needs the managed keys is refused with home unchanged
// (TestSSHVSCodeWriteLeaves_RefuseHardLinks/..._settings.json).
func TestUpdateVSCodeFiles_HardLinkedSettingsUnchangedUntouched(t *testing.T) {
	home := privateHome(t)
	p := vscodePaths{home: home}
	require.NoError(t, ensureVSCodeSettings(p))
	managed := readTestFile(t, p.settings())
	second := filepath.Join(home, "second-name")
	require.NoError(t, os.Link(p.settings(), second))
	_, err := updateVSCodeFiles(p, vscodeUpdateFor(t, "a1"))
	require.NoError(t, err, "an unchanged hard-linked settings.json is not a leaf of the operation")
	for _, f := range []string{p.settings(), second} {
		assert.Equal(t, managed, readTestFile(t, f), f)
	}
	assertPrivateSD(t, p.sshConfig())
}
