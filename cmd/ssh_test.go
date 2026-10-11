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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSSHClient installs a stand-in ssh as the only program on PATH. It
// records its arguments and a copy of the known-hosts file it was given, and
// exits with FAKE_SSH_EXIT.
type fakeSSHClient struct {
	argsFile, knownHostsFile string
}

func installFakeSSHClient(t *testing.T, exit string) *fakeSSHClient {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	bin := t.TempDir()
	out := t.TempDir()
	f := &fakeSSHClient{argsFile: filepath.Join(out, "args"), knownHostsFile: filepath.Join(out, "known_hosts")}
	script := "#!/bin/sh\n" +
		": > '" + f.argsFile + "'\n" +
		"for a in \"$@\"; do\n" +
		"  printf '%s\\n' \"$a\" >> '" + f.argsFile + "'\n" +
		"  case \"$a\" in UserKnownHostsFile=*) while IFS= read -r l; do printf '%s\\n' \"$l\"; done < \"${a#UserKnownHostsFile=}\" > '" + f.knownHostsFile + "';; esac\n" +
		"done\n" +
		"exit " + exit + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755))
	t.Setenv("PATH", bin)
	return f
}

func (f *fakeSSHClient) args(t *testing.T) []string {
	t.Helper()
	return strings.Split(strings.TrimRight(readTestFile(t, f.argsFile), "\n"), "\n")
}

// failingReader fails the test if anything reads stdin.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read")
	return 0, os.ErrClosed
}

// scion ssh runs the system ssh with the tunnel ProxyCommand, the hub's host
// key pinned in a temporary known-hosts file, and no prompts.
func TestSSH_RunsSystemSSHWithProxyCommand(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	fake := installFakeSSHClient(t, "0")
	env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)

	require.Equal(t, ExitOK, runSSH(context.Background(), env, []string{"a-b/c", "uname", "-a"}), errb.String())

	l := hub.label(t)
	host := "scion." + l + ".a-b.c"
	args := fake.args(t)
	assert.Contains(t, args, "ProxyCommand="+sshTestExe+" tunnel --stdio --ssh --hub "+hub.endpoint(t)+" --project a-b c")
	assert.Contains(t, args, "HostKeyAlias="+host)
	assert.Contains(t, args, "StrictHostKeyChecking=yes")
	assert.Contains(t, args, "BatchMode=yes")
	require.GreaterOrEqual(t, len(args), 3)
	assert.Equal(t, []string{"-l", sshRemoteUser, host, "uname", "-a"}, args[len(args)-5:])
	assert.Equal(t, host+" "+mustNormKey(t, hub.agents[0].HostKey)+"\n", readTestFile(t, fake.knownHostsFile))
	assert.Empty(t, errb.String())
}

// The remote exit status is returned as is, including 255 from ssh.
func TestSSH_ReturnsRemoteStatus(t *testing.T) {
	for _, code := range []int{7, 255} {
		hub := twoProjectHub(t, "tok")
		installFakeSSHClient(t, map[int]string{7: "7", 255: "255"}[code])
		env, _, _ := newSSHTestEnv(t, t.TempDir(), hub)
		assert.Equal(t, code, runSSH(context.Background(), env, []string{"a/b-c"}))
	}
}

// Criterion 25(e): with no credential, scion ssh exits 255 with one plain
// line, never reads stdin and never runs ssh.
func TestSSH_NoCredential_Exit255OneLineNoStdin(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	fake := installFakeSSHClient(t, "0")
	env, out, errb := newSSHTestEnv(t, t.TempDir(), hub)
	env.token = ""
	env.stdin = failingReader{t}

	assert.Equal(t, 255, runSSH(context.Background(), env, []string{"a-b/c"}))
	assert.Equal(t, 1, strings.Count(errb.String(), "\n"), errb.String())
	assert.Contains(t, errb.String(), "no usable credential")
	assert.NotContains(t, errb.String(), "\x1b")
	assert.NotContains(t, errb.String(), "Usage")
	assert.Empty(t, out.String())
	assert.NoFileExists(t, fake.argsFile)
}

// A deleted agent, an unreachable hub and a missing ssh client each give
// 255 with one line.
func TestSSH_LocalFailuresExit255(t *testing.T) {
	t.Run("deleted agent", func(t *testing.T) {
		hub := twoProjectHub(t, "tok")
		fake := installFakeSSHClient(t, "0")
		hub.deleteAgent("ag-c")
		env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
		assert.Equal(t, 255, runSSH(context.Background(), env, []string{"a-b/c"}))
		assert.Equal(t, 1, strings.Count(errb.String(), "\n"), errb.String())
		assert.Contains(t, errb.String(), "agent not found or deleted")
		assert.NoFileExists(t, fake.argsFile)
	})
	t.Run("hub unreachable", func(t *testing.T) {
		hub := twoProjectHub(t, "tok")
		installFakeSSHClient(t, "0")
		env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
		hub.srv.Close()
		assert.Equal(t, 255, runSSH(context.Background(), env, []string{"a-b/c"}))
		assert.Contains(t, errb.String(), "hub unreachable or unavailable")
	})
	t.Run("no ssh client", func(t *testing.T) {
		hub := twoProjectHub(t, "tok")
		t.Setenv("PATH", t.TempDir())
		env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
		assert.Equal(t, 255, runSSH(context.Background(), env, []string{"a-b/c"}))
		assert.Equal(t, "scion ssh: ssh client not found on PATH\n", errb.String())
	})
}

// The temporary known-hosts file is removed after ssh exits.
func TestSSH_RemovesTemporaryKnownHosts(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	fake := installFakeSSHClient(t, "0")
	env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
	require.Equal(t, ExitOK, runSSH(context.Background(), env, []string{"a-b/c"}), errb.String())
	for _, a := range fake.args(t) {
		if p, ok := strings.CutPrefix(a, "UserKnownHostsFile="); ok {
			assert.NoFileExists(t, p)
			return
		}
	}
	t.Fatal("no UserKnownHostsFile argument")
}

// stubSSHExit records exit codes instead of exiting.
func stubSSHExit(t *testing.T) *[]int {
	t.Helper()
	var codes []int
	old := sshExit
	sshExit = func(code int) { codes = append(codes, code) }
	t.Cleanup(func() { sshExit = old })
	return &codes
}

// captureSSHCmdStderr sends scion ssh's command stderr to a buffer.
func captureSSHCmdStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	sshCmd.SetErr(&b)
	t.Cleanup(func() { sshCmd.SetErr(nil) })
	return &b
}

// A missing agent is a usage error: one line and exit 255.
func TestSSHCommand_NoAgentIsUsageError255(t *testing.T) {
	codes := stubSSHExit(t)
	errb := captureSSHCmdStderr(t)
	require.NoError(t, runSSHCommand(sshCmd, nil))
	assert.Equal(t, []int{255}, *codes)
	assert.Equal(t, "scion ssh: "+sshUsageLine+"\n", errb.String())
}

// A flag error before the agent is one line and exit 255.
func TestSSHCommand_FlagErrorIs255OneLine(t *testing.T) {
	codes := stubSSHExit(t)
	errb := captureSSHCmdStderr(t)
	err := sshCmd.ParseFlags([]string{"--no-such-flag", "a"})
	require.Error(t, err)
	assert.Error(t, sshCmd.FlagErrorFunc()(sshCmd, err))
	assert.Equal(t, []int{255}, *codes)
	assert.Equal(t, 1, strings.Count(errb.String(), "\n"), errb.String())
	assert.Contains(t, errb.String(), "no-such-flag")
	assert.NotContains(t, errb.String(), "\x1b")
}

// Everything after the agent, flags included, is left for ssh.
func TestSSHCommand_RemoteFlagsPassThrough(t *testing.T) {
	require.NoError(t, sshCmd.ParseFlags([]string{"a", "ls", "-la", "--hub", "x"}))
	assert.Equal(t, []string{"a", "ls", "-la", "--hub", "x"}, sshCmd.Flags().Args())
	assert.Empty(t, hubEndpoint, "--hub after the agent is not a scion flag")
}

// prepareSSH refuses a name part that is not a slug, an executable path
// that is not absolute, and a temporary directory that ssh would split or
// expand; each is 255 with one line and ssh is not run.
func TestSSH_RefusesUnsafeInput(t *testing.T) {
	t.Run("agent slug", func(t *testing.T) {
		hub := twoProjectHub(t, "tok")
		hub.addAgent(t, "ag-bad", "Bad_Slug", "p-ab")
		fake := installFakeSSHClient(t, "0")
		env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
		assert.Equal(t, 255, runSSH(context.Background(), env, []string{"a-b/Bad_Slug"}))
		assert.Equal(t, 1, strings.Count(errb.String(), "\n"), errb.String())
		assert.Contains(t, errb.String(), "not a slug")
		assert.NoFileExists(t, fake.argsFile)
	})
	t.Run("relative exe", func(t *testing.T) {
		hub := twoProjectHub(t, "tok")
		fake := installFakeSSHClient(t, "0")
		env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
		env.exe = "scion"
		assert.Equal(t, 255, runSSH(context.Background(), env, []string{"a-b/c"}))
		assert.Contains(t, errb.String(), "not a clean absolute path")
		assert.NoFileExists(t, fake.argsFile)
	})
	for name, sub := range map[string]string{"space": "tmp dir", "percent": "tmp%dir"} {
		t.Run("temp dir with "+name, func(t *testing.T) {
			hub := twoProjectHub(t, "tok")
			fake := installFakeSSHClient(t, "0")
			tmp := filepath.Join(t.TempDir(), sub)
			require.NoError(t, os.MkdirAll(tmp, 0o700))
			t.Setenv("TMPDIR", tmp)
			env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
			assert.Equal(t, 255, runSSH(context.Background(), env, []string{"a-b/c"}))
			assert.Equal(t, 1, strings.Count(errb.String(), "\n"), errb.String())
			assert.Contains(t, errb.String(), "TMPDIR")
			assert.NoFileExists(t, fake.argsFile)
			entries, err := os.ReadDir(tmp)
			require.NoError(t, err)
			assert.Empty(t, entries, "the temporary directory is removed")
		})
	}
}
