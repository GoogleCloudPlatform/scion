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

package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sshSpecialSuffixes are endpoint path suffixes with characters that the
// ProxyCommand shell or the C runtime's command-line parser treats
// specially (and '%').
var sshSpecialSuffixes = []string{"/$(id)", "/`id`", `/x"y`, "/$HOME", "/a!b", "/a&b", "/100%25", "/a'b", "/a;b", `/a\b`}

// A hub endpoint is held to ASCII letters, digits and -._:/ (plus the
// brackets of an IPv6 host) on every OS. Controls: a path, a port and an
// IPv6 host are accepted.
func TestSSHEndpointURL_RefusesShellSpecialChars(t *testing.T) {
	for _, s := range sshSpecialSuffixes {
		ep := "https://hub.example.com" + s
		_, err := sshEndpointURL(ep)
		require.Error(t, err, ep)
		assert.Contains(t, err.Error(), "may contain only", ep)
	}
	for _, bad := range []string{"https://hub.example.com/a[b]", "https://hub.example.com/~me", "https://hub.example.com/a,b", "https://hub.example.com/a+b", "https://hüb.example.com"} {
		_, err := sshEndpointURL(bad)
		require.Error(t, err, bad)
	}
	for _, ok := range []string{"https://hub.example.com", "https://hub.example.com:8443/team-a/hub_1.v2/", "http://[::1]:9810", "http://127.0.0.1:8080"} {
		_, err := sshEndpointURL(ok)
		require.NoError(t, err, ok)
	}
}

// With the Windows ProxyCommand form, scion ssh-config (all agents and one
// agent), scion vscode and scion ssh refuse an endpoint with a character
// that form cannot carry, and nothing in the home directory changes.
// Control: the same commands with the hub's plain endpoint succeed.
func TestSSHWindowsForm_RefusesEndpointShellSpecialChars(t *testing.T) {
	setSSHQuoteForWindows(t, true)
	hub := twoProjectHub(t, "tok")
	for _, s := range sshSpecialSuffixes {
		env, _, _ := newSSHTestEnv(t, t.TempDir(), hub)
		env.exe = testAbsExe(t)
		fake := stubVSCode(t)
		env.endpoint = hub.srv.URL + s
		before := sshFilesSnapshot(t, env.home)

		assert.Error(t, sshConfigMain(context.Background(), env, nil, true), s)
		assert.Error(t, sshConfigMain(context.Background(), env, []string{"a/b-c"}, true), s)
		assert.Error(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false), s)
		_, cleanup, err := prepareSSH(context.Background(), env, "a/b-c", nil)
		if cleanup != nil {
			cleanup()
		}
		assert.Error(t, err, s)
		assert.Empty(t, fake.runs, s)
		assert.Equal(t, before, sshFilesSnapshot(t, env.home), "%s: nothing is written", s)
	}

	env, _, _ := newSSHTestEnv(t, t.TempDir(), hub)
	env.exe = testAbsExe(t)
	stubVSCode(t)
	require.NoError(t, sshConfigMain(context.Background(), env, nil, true))
	require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false))
	setTestTempDir(t, testPrivateTempDir(t))
	_, cleanup, err := prepareSSH(context.Background(), env, "a/b-c", nil)
	require.NoError(t, err)
	cleanup()
}

// setTestTempDir points TMPDIR, TMP and TEMP (os.TempDir on every OS) at
// dir for the rest of the test.
func setTestTempDir(t *testing.T, dir string) {
	t.Helper()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, dir)
	}
	require.Equal(t, filepath.Clean(dir), filepath.Clean(os.TempDir()))
}

// With the Windows form, a scion executable path with a character outside
// the allowlist is refused by ssh-config, vscode and ssh, and nothing in
// the home directory changes. Controls: paths with a space, with
// "(x86)" and with an 8.3 "~" are written double-quoted.
func TestSSHWindowsForm_RefusesExePathShellSpecialChars(t *testing.T) {
	setSSHQuoteForWindows(t, true)
	hub := twoProjectHub(t, "tok")
	base := t.TempDir()
	for _, dir := range []string{"a$(b)", "a`b`", "a!b", "a%b", "a&b", "a;b", "a'b", "a^b"} {
		exe := filepath.Join(base, dir, "scion")
		env, _, _ := newSSHTestEnv(t, t.TempDir(), hub)
		fake := stubVSCode(t)
		env.exe = exe
		before := sshFilesSnapshot(t, env.home)

		err := sshConfigMain(context.Background(), env, nil, true)
		require.Error(t, err, exe)
		assert.Contains(t, err.Error(), "scion executable path", exe)
		assert.Error(t, sshConfigMain(context.Background(), env, []string{"a/b-c"}, true), exe)
		assert.Error(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false), exe)
		_, cleanup, err := prepareSSH(context.Background(), env, "a/b-c", nil)
		if cleanup != nil {
			cleanup()
		}
		assert.Error(t, err, exe)
		assert.Empty(t, fake.runs, exe)
		assert.Equal(t, before, sshFilesSnapshot(t, env.home), "%s: nothing is written", exe)
	}

	for _, dir := range []string{"My Tools", "Program Files (x86)", "PROGRA~1"} {
		env, _, _ := newSSHTestEnv(t, t.TempDir(), hub)
		stubVSCode(t)
		env.exe = filepath.Join(base, dir, "scion")
		require.NoError(t, sshConfigMain(context.Background(), env, nil, true), dir)
		require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false), dir)
		cfg := readTestFile(t, vscodePaths{home: env.home}.sshConfig())
		assert.Contains(t, cfg, `ProxyCommand "`+filepath.ToSlash(env.exe)+`" tunnel --stdio --ssh`, dir)
	}
}

// The rendered Host block with the Windows form and an endpoint with a
// path (control): every word is unquoted except the executable path with a
// space.
func TestRenderSSHHostBlock_WindowsFormEndpointWithPath(t *testing.T) {
	setSSHQuoteForWindows(t, true)
	exe := filepath.Join(t.TempDir(), "Program Files", "scion.exe")
	pin := filepath.Join(t.TempDir(), "known_hosts")
	block, err := renderSSHHostBlockPinned(exe, "https://hub.example.com/team-a", "hub", "p", "a", vscodeKnownHostsRef, pin)
	require.NoError(t, err)
	assert.Contains(t, block, `  ProxyCommand "`+filepath.ToSlash(exe)+`" tunnel --stdio --ssh --hub https://hub.example.com/team-a --project p --known-hosts `+filepath.ToSlash(pin)+` --host-key-alias scion.hub.p.a a`+"\n")
}

// With the Windows form, an IPv6-literal endpoint is accepted and written
// double-quoted, and the vscode block parser reads it back.
func TestRenderSSHHostBlock_WindowsFormIPv6Endpoint(t *testing.T) {
	setSSHQuoteForWindows(t, true)
	exe := filepath.Join(t.TempDir(), "scion")
	block, err := renderSSHHostBlock(exe, "http://[::1]:9810", "hub", "p", "a")
	require.NoError(t, err)
	assert.Contains(t, block, ` --hub "http://[::1]:9810" --project p a`+"\n")
	assert.Equal(t, "http://[::1]:9810", vscodeBlockHub(block))
}

// useVSCodeLauncher makes the VS Code CLI a real file named name in a
// temporary directory and records the runs, with the Windows launcher
// check on.
func useVSCodeLauncher(t *testing.T, name string) (bin string, runs *[][]string) {
	t.Helper()
	orig, origLook, origRun := vscodeOnWindows, vscodeLookPath, vscodeRunCode
	t.Cleanup(func() { vscodeOnWindows, vscodeLookPath, vscodeRunCode = orig, origLook, origRun })
	vscodeOnWindows = true
	bin = filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(bin, []byte("@echo off\r\n"), 0o700))
	var got [][]string
	vscodeLookPath = func(string) (string, error) { return bin, nil }
	vscodeRunCode = func(_ context.Context, b string, args []string, _, _ io.Writer) error {
		got = append(got, append([]string{b}, args...))
		return nil
	}
	return bin, &got
}

// With the Windows launcher check, a .cmd or .bat VS Code CLI is not
// started when an argument (the user data directory under the home, or
// --path) has a character outside the allowlist; the error names the
// argument. Controls: a plain home starts the .cmd, and a home with '&'
// starts a CLI that is not a .cmd.
func TestVSCode_ValidatesArgumentsForCmdLauncher(t *testing.T) {
	// The unix ProxyCommand form, so only the launcher check refuses (the
	// Windows form would refuse these homes in the vscode block first).
	setSSHQuoteForWindows(t, false)
	hub := twoProjectHub(t, "tok")
	for _, launcher := range []string{"code.cmd", "CODE.BAT"} {
		for _, dir := range []string{"a&b", "a%b", "a^b", "a(b)", "a!b"} {
			_, runs := useVSCodeLauncher(t, launcher)
			env, _, _ := newSSHTestEnv(t, filepath.Join(t.TempDir(), dir), hub)
			env.exe = testAbsExe(t)
			require.NoError(t, os.MkdirAll(env.home, 0o700))
			stubVSCodePreflight(t)
			err := vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false)
			require.Error(t, err, dir)
			assert.Contains(t, err.Error(), vscodeUserDataFlag, dir)
			assert.Contains(t, err.Error(), "cannot be passed to the VS Code launcher", dir)
			assert.Empty(t, *runs, dir)
		}
		_, runs := useVSCodeLauncher(t, launcher)
		env, _, _ := newSSHTestEnv(t, t.TempDir(), hub)
		env.exe = testAbsExe(t)
		stubVSCodePreflight(t)
		err := vscodeMain(context.Background(), env, "a/b-c", "/work&x", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--path", launcher)
		assert.Empty(t, *runs, launcher)

		bin, runs := useVSCodeLauncher(t, launcher)
		env, _, _ = newSSHTestEnv(t, t.TempDir(), hub)
		env.exe = testAbsExe(t)
		stubVSCodePreflight(t)
		require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false), launcher)
		require.Len(t, *runs, 1)
		assert.Equal(t, bin, (*runs)[0][0])
	}

	_, runs := useVSCodeLauncher(t, "code.exe")
	env, _, _ := newSSHTestEnv(t, filepath.Join(t.TempDir(), "a&b"), hub)
	env.exe = testAbsExe(t)
	require.NoError(t, os.MkdirAll(env.home, 0o700))
	stubVSCodePreflight(t)
	require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false))
	assert.Len(t, *runs, 1, "a CLI that is not a .cmd is not checked")
}

// stubVSCodePreflight replaces only the vscode preflight tunnel.
func stubVSCodePreflight(t *testing.T) {
	t.Helper()
	orig := vscodePreflight
	t.Cleanup(func() { vscodePreflight = orig })
	vscodePreflight = func(context.Context, string, string, string, io.Writer) error { return nil }
}

// checkVSCodeBatchArgs is off on other OSes and for a CLI that is not a
// .cmd or .bat; on the Windows form it accepts paths with backslashes,
// spaces, '~', non-ASCII letters and other combining marks, and refuses
// cmd.exe metacharacters, modifier letters, and the letters and combining
// marks (runes) that best-fit to ASCII punctuation.
func TestCheckVSCodeBatchArgs(t *testing.T) {
	orig := vscodeOnWindows
	t.Cleanup(func() { vscodeOnWindows = orig })
	bad := []vscodeArg{{"--path", "/a&b"}}
	vscodeOnWindows = false
	require.NoError(t, checkVSCodeBatchArgs(`C:\VS Code\bin\code.cmd`, bad))
	vscodeOnWindows = true
	require.NoError(t, checkVSCodeBatchArgs(`C:\VS Code\Code.exe`, bad))
	require.Error(t, checkVSCodeBatchArgs(`C:\VS Code\bin\code.cmd`, bad))
	require.NoError(t, checkVSCodeBatchArgs(`C:\VS Code\bin\code.cmd`, []vscodeArg{
		{vscodeUserDataFlag, `C:\Users\José Núñez\.scion\vscode\user-data`},
		{"--remote", "ssh-remote+scion.hub.p.a"}, {"--path", "/workspace/sub_dir-1"},
	}))
	// An 8.3 short name: '~' is literal for cmd.exe.
	require.NoError(t, checkVSCodeBatchArgs(`C:\VS Code\bin\code.cmd`, []vscodeArg{
		{vscodeUserDataFlag, `C:\Users\FIRSTN~1\.scion\vscode\user-data`},
	}))
	for _, c := range []string{`"`, "%", "^", "&", "|", "<", ">", "(", ")", "!", "\n", "'", ";", "$", "`", "\u02b9", "\u02ba", "\u02bc", "\u02c6"} {
		require.Error(t, checkVSCodeBatchArgs(`C:\VS Code\bin\code.cmd`, []vscodeArg{{"--path", "/a" + c + "b"}}), "%q", c)
	}
	// Letters and combining marks that best-fit to ASCII punctuation (marks
	// after a base letter); other combining marks stay accepted.
	for _, c := range []string{"\u01c0", "\u01c3", "\u0300", "\u0302", "\u0303", "\u030e"} {
		require.Error(t, checkVSCodeBatchArgs(`C:\VS Code\bin\code.cmd`, []vscodeArg{{vscodeUserDataFlag, `C:\Users\a` + c + `b`}}), "%q", c)
	}
	require.NoError(t, checkVSCodeBatchArgs(`C:\VS Code\bin\code.cmd`, []vscodeArg{{vscodeUserDataFlag, "C:\\Users\\Jose\u0301"}}))
}

// testAbsExe returns an absolute scion path on the host OS (sshTestExe is a
// unix path, which is not absolute on Windows).
func testAbsExe(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "scion")
}

// scion ssh checks every ProxyCommand word before it makes its temporary
// directory: on a refusal the temporary directory stays empty. Control: a
// successful prepare makes one entry there (removed by its cleanup).
func TestPrepareSSH_RefusalMakesNoTempDir(t *testing.T) {
	setSSHQuoteForWindows(t, true)
	hub := twoProjectHub(t, "tok")
	// Everything t.TempDir makes is made before the temp dir is redirected.
	env, _, _ := newSSHTestEnv(t, t.TempDir(), hub)
	badExe := filepath.Join(t.TempDir(), "a&b", "scion")
	goodExe := testAbsExe(t)
	tmp := testPrivateTempDir(t)
	setTestTempDir(t, tmp)
	entries := func() int {
		des, err := os.ReadDir(tmp)
		require.NoError(t, err)
		return len(des)
	}

	env.exe = badExe
	_, cleanup, err := prepareSSH(context.Background(), env, "a/b-c", nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "&")
	assert.Nil(t, cleanup, "no cleanup: nothing was made")
	assert.Equal(t, 0, entries(), "no temporary directory on refusal")

	env.exe = goodExe
	_, cleanup, err = prepareSSH(context.Background(), env, "a/b-c", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, entries(), "control: one temporary directory")
	cleanup()
	assert.Equal(t, 0, entries())
}
