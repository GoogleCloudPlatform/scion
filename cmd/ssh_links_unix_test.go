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

//go:build unix

package cmd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// writeLinkedLayout runs the ssh-config and vscode writers in home and
// returns nothing; failures fail the test.
func writeLinkedLayout(t *testing.T, home string) {
	t.Helper()
	p := sshPaths{home: home}
	_, err := regenerateSSHHubFiles(p, regenTestEndpoint, "/bin/scion", regenListing(
		sshListedAgent{"proj", "agent", testSSHPublicKey(t)}))
	require.NoError(t, err)
	block, err := renderSSHHostBlockKnown(sshTestExe, "https://h.example", "h", "p", "a1", vscodeKnownHostsRef)
	require.NoError(t, err)
	_, err = updateVSCodeFiles(vscodePaths{home: home}, vscodeUpdate{host: "scion.h.p.a1", block: block,
		key: mustNormKey(t, testSSHPublicKey(t)), endpoint: tunnelclient.NormalizeEndpoint("https://h.example")})
	require.NoError(t, err)
}

// ~/.ssh and ~/.scion linked to directories OUTSIDE
// home (a dotfile manager, a ~/.scion moved to another disk) are followed:
// ssh-config and vscode write into the link targets, and the links are
// kept. Control: the same writers in a home without links.
func TestSSHAndVSCodeFollowLinksOutsideHome(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	for _, d := range []string{"dotfiles-ssh", "big-disk-scion"} {
		require.NoError(t, os.Mkdir(filepath.Join(outside, d), 0o700))
	}
	wintest.MustSymlink(t, filepath.Join(outside, "dotfiles-ssh"), filepath.Join(home, ".ssh"))
	wintest.MustSymlink(t, filepath.Join(outside, "big-disk-scion"), filepath.Join(home, ".scion"))

	writeLinkedLayout(t, home)
	for _, f := range []string{
		filepath.Join(outside, "dotfiles-ssh", "config"),
		filepath.Join(outside, "dotfiles-ssh", "config.d", "scion-hub"),
		filepath.Join(outside, "big-disk-scion", "ssh", "known_hosts-hub"),
		filepath.Join(outside, "big-disk-scion", "vscode", "ssh_config"),
		filepath.Join(outside, "big-disk-scion", "vscode", "user-data", "User", "settings.json"),
	} {
		assert.FileExists(t, f)
	}
	for _, l := range []string{".ssh", ".scion"} {
		fi, err := os.Lstat(filepath.Join(home, l))
		require.NoError(t, err)
		assert.NotZero(t, fi.Mode()&os.ModeSymlink, "%s is still a link", l)
	}

	plain := t.TempDir()
	writeLinkedLayout(t, plain)
	assert.FileExists(t, filepath.Join(plain, ".ssh", "config.d", "scion-hub"), "control: a home without links")
}

// On unix, a hard-linked ~/.ssh/config makes writeSSHHubPlan refuse
// (the one-name rule) BEFORE it writes the known-hosts or
// include file or creates their directories: home is unchanged. Control:
// with one name the same call succeeds.
func TestWriteSSHHubPlan_HardLinkedConfigNoSideEffect(t *testing.T) {
	home := t.TempDir()
	p := sshPaths{home: home}
	require.NoError(t, os.Mkdir(p.sshDir(), 0o700))
	require.NoError(t, os.WriteFile(p.userConfig(), []byte("Host old\n"), 0o600))
	second := filepath.Join(home, "second-name")
	require.NoError(t, os.Link(p.userConfig(), second))
	before := treeSnapshotUnix(t, home)
	res := &sshRegenResult{label: "hub", includeFile: p.includeFile("hub"), knownHostsFile: p.knownHostsFile("hub"),
		includeContent: "# include\n", knownContent: "# known hosts\n"}
	err := writeSSHHubPlan(p, res)
	require.ErrorIs(t, err, privatefs.ErrHardLinked)
	assert.Equal(t, before, treeSnapshotUnix(t, home), "the refusal changed nothing in home")

	require.NoError(t, os.Remove(second))
	require.NoError(t, writeSSHHubPlan(p, res), "control: one name")
	assert.FileExists(t, p.includeFile("hub"))
}

// On unix, a hard-linked ~/.ssh/config that ALREADY has
// the scion Include line is not part of the operation (it is not written),
// so writeSSHHubPlan succeeds and leaves it and its other name
// byte-identical. The refusal side (Include line missing) is
// TestWriteSSHHubPlan_HardLinkedConfigNoSideEffect.
func TestWriteSSHHubPlan_HardLinkedConfigWithIncludeLineUntouched(t *testing.T) {
	home := t.TempDir()
	p := sshPaths{home: home}
	require.NoError(t, os.Mkdir(p.sshDir(), 0o700))
	content := sshIncludeLine + "\n\nHost old\n"
	require.NoError(t, os.WriteFile(p.userConfig(), []byte(content), 0o600))
	second := filepath.Join(home, "second-name")
	require.NoError(t, os.Link(p.userConfig(), second))
	res := &sshRegenResult{label: "hub", includeFile: p.includeFile("hub"), knownHostsFile: p.knownHostsFile("hub"),
		includeContent: "# include\n", knownContent: "# known hosts\n"}
	require.NoError(t, writeSSHHubPlan(p, res))
	assert.FileExists(t, p.includeFile("hub"))
	for _, f := range []string{p.userConfig(), second} {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.Equal(t, content, string(b), f)
	}
	fi, err := os.Stat(p.userConfig())
	require.NoError(t, err)
	assert.Equal(t, uint64(2), fi.Sys().(*syscall.Stat_t).Nlink, "still the same file with two names")
}

// On unix, vscode: a hard-linked settings.json that
// already holds the managed values is not rewritten, so updateVSCodeFiles
// succeeds and leaves it and its other name byte-identical. Refusal side:
// the same hard link on a settings.json that needs the managed keys is
// refused, and home is unchanged.
func TestUpdateVSCodeFiles_HardLinkedSettings(t *testing.T) {
	home := t.TempDir()
	p := vscodePaths{home: home}
	require.NoError(t, ensureVSCodeSettings(p))
	managed, err := os.ReadFile(p.settings())
	require.NoError(t, err)
	second := filepath.Join(home, "second-name")
	require.NoError(t, os.Link(p.settings(), second))
	block, err := renderSSHHostBlockKnown(sshTestExe, "https://h.example", "h", "p", "a1", vscodeKnownHostsRef)
	require.NoError(t, err)
	u := vscodeUpdate{host: "scion.h.p.a1", block: block, key: mustNormKey(t, testSSHPublicKey(t)), endpoint: tunnelclient.NormalizeEndpoint("https://h.example")}
	_, err = updateVSCodeFiles(p, u)
	require.NoError(t, err, "an unchanged hard-linked settings.json is not a leaf of the operation")
	for _, f := range []string{p.settings(), second} {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.Equal(t, string(managed), string(b), f)
	}

	home2 := t.TempDir()
	p2 := vscodePaths{home: home2}
	require.NoError(t, os.MkdirAll(filepath.Dir(p2.settings()), 0o700))
	require.NoError(t, os.WriteFile(p2.settings(), []byte("{}\n"), 0o600))
	require.NoError(t, os.Link(p2.settings(), filepath.Join(home2, "second-name")))
	before := treeSnapshotUnix(t, home2)
	_, err = updateVSCodeFiles(p2, u)
	require.ErrorIs(t, err, privatefs.ErrHardLinked)
	assert.Equal(t, before, treeSnapshotUnix(t, home2), "the refusal changed nothing in home")
}

// treeSnapshotUnix records every entry below dir (links recorded, not
// followed), with regular files' content.
func treeSnapshotUnix(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] = "file:" + string(b)
		} else {
			out[path] = d.Type().String()
		}
		return nil
	}))
	return out
}
