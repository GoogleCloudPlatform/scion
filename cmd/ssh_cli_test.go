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
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
)

// TestSSHCLIHelperProcess runs the scion CLI in a child process for the CLI
// tests below. It does nothing in a normal test run. The child receives the
// CLI arguments after "--"; SCION_TEST_HUB_TOKEN, when set, becomes the hub
// credential (the test binary clears other SCION_ variables at start).
func TestSSHCLIHelperProcess(t *testing.T) {
	if os.Getenv("SCION_TEST_EXEC_CLI") != "1" {
		t.Skip("child process for the scion CLI tests")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if tok := os.Getenv("SCION_TEST_HUB_TOKEN"); tok != "" {
		if err := os.Setenv("SCION_HUB_TOKEN", tok); err != nil {
			panic(err)
		}
	}
	os.Args = append([]string{"scion"}, args...)
	Execute()
	os.Exit(0)
}

// sshCLIResult is the outcome of one child CLI run.
type sshCLIResult struct {
	code    int
	stdout  string
	stderr  string
	elapsed time.Duration
}

// runSSHCLI runs scion with args in a child process: working directory dir
// (no .scion), HOME home, a PATH with nothing on it, and stdin a pipe that
// is never written or closed, so a read would hang the child.
func runSSHCLI(t *testing.T, dir, home string, extraEnv []string, args ...string) sshCLIResult {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestSSHCLIHelperProcess$", "--"}, args...)...)
	c.Dir = dir
	c.Env = append([]string{"SCION_TEST_EXEC_CLI=1", "PATH=/nonexistent", "HOME=" + home}, extraEnv...)
	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = stdinW.Close() }()
	defer func() { _ = stdinR.Close() }()
	c.Stdin = stdinR
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	start := time.Now()
	runErr := c.Run()
	res := sshCLIResult{stdout: out.String(), stderr: errb.String(), elapsed: time.Since(start)}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.code = 0
	case errors.As(runErr, &exitErr):
		res.code = exitErr.ExitCode()
	default:
		t.Fatalf("running the CLI: %v", runErr)
	}
	require.NoError(t, ctx.Err(), "the CLI did not finish; it may be reading stdin")
	return res
}

// assertOnePlainLine checks that s is exactly one line with no ANSI sequence
// and no usage block.
func assertOnePlainLine(t *testing.T, s string) {
	t.Helper()
	assert.Equal(t, 1, strings.Count(s, "\n"), "want one line, got %q", s)
	assert.NotContains(t, s, "\x1b")
	assert.NotContains(t, s, "Usage:")
}

// ssh and ssh-config need no project; another command named ssh below an
// unrelated parent still does.
func TestSSHCLI_ProjectGate(t *testing.T) {
	assert.False(t, commandRequiresProject(sshCmd))
	assert.False(t, commandRequiresProject(sshConfigCmd))

	root := &cobra.Command{Use: "scion"}
	parent := &cobra.Command{Use: "other"}
	nested := &cobra.Command{Use: "ssh"}
	parent.AddCommand(nested)
	root.AddCommand(parent)
	assert.True(t, commandRequiresProject(nested))
}

// Outside a project, with no credential and a minimal PATH, scion ssh exits
// 255 with one plain line and does not read stdin; usage and flag errors
// also give 255 with one line.
func TestSSHCLI_SSHFailuresExit255OneLine(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	dir, home := t.TempDir(), t.TempDir()

	res := runSSHCLI(t, dir, home, nil, "ssh", "--hub", hub.srv.URL, "a-b/c")
	assert.Equal(t, 255, res.code, res.stderr)
	assertOnePlainLine(t, res.stderr)
	assert.Contains(t, res.stderr, "scion ssh: no usable credential")
	assert.Empty(t, res.stdout)
	assert.Less(t, res.elapsed, 10*time.Second)

	res = runSSHCLI(t, dir, home, nil, "ssh")
	assert.Equal(t, 255, res.code, res.stderr)
	assertOnePlainLine(t, res.stderr)
	assert.Contains(t, res.stderr, "usage: scion ssh")

	res = runSSHCLI(t, dir, home, nil, "ssh", "--no-such-flag", "a-b/c")
	assert.Equal(t, 255, res.code, res.stderr)
	assertOnePlainLine(t, res.stderr)
	assert.Contains(t, res.stderr, "no-such-flag")
}

// scion ssh-config keeps its own exit codes: 3 with no credential, 2 for a
// usage or flag error, each with one plain line.
func TestSSHCLI_SSHConfigExitCodes(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	dir, home := t.TempDir(), t.TempDir()

	res := runSSHCLI(t, dir, home, nil, "ssh-config", "--hub", hub.srv.URL)
	assert.Equal(t, tunnelclient.ExitCredential, res.code, res.stderr)
	assertOnePlainLine(t, res.stderr)
	assert.Contains(t, res.stderr, "no usable credential")

	res = runSSHCLI(t, dir, home, nil, "ssh-config", "--hub", hub.srv.URL, "a", "b")
	assert.Equal(t, tunnelclient.ExitUsage, res.code, res.stderr)
	assertOnePlainLine(t, res.stderr)

	res = runSSHCLI(t, dir, home, nil, "ssh-config", "--no-such-flag")
	assert.Equal(t, tunnelclient.ExitUsage, res.code, res.stderr)
	assertOnePlainLine(t, res.stderr)

	_, err := os.Stat(filepath.Join(home, ".ssh"))
	assert.True(t, errors.Is(err, os.ErrNotExist), "nothing is written to ~/.ssh")
}

// writeSCIONWrapper writes an executable "scion" that runs this test binary
// as the CLI, so a generated ProxyCommand line can be run as is.
func writeSCIONWrapper(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "scion")
	script := "#!/bin/sh\nSCION_TEST_EXEC_CLI=1 exec " + sshShellQuote(exe) + " '-test.run=^TestSSHCLIHelperProcess$' -- \"$@\"\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// The generated ProxyCommand for a deleted agent exits 4 within 5s with
// one line, from an unrelated working directory with a minimal PATH, and
// the system ssh client reports 255 with that line.
func TestSSHCLI_ProxyCommandForDeletedAgent(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	env.exe = writeSCIONWrapper(t)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	l := hub.label(t)
	host := "scion." + l + ".a-b.c"
	include := sshPaths{home: home}.includeFile(l)
	var proxy string
	for _, line := range strings.Split(parseSSHHostBlocks(readTestFile(t, include))[host], "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ProxyCommand "); ok {
			proxy = strings.ReplaceAll(v, "%%", "%")
		}
	}
	require.NotEmpty(t, proxy)
	hub.deleteAgent("ag-c")
	childEnv := []string{"PATH=/nonexistent", "HOME=" + home, "SCION_TEST_HUB_TOKEN=tok"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "/bin/sh", "-c", "exec "+proxy)
	c.Dir = "/"
	c.Env = childEnv
	c.Stdin = strings.NewReader("")
	var perr bytes.Buffer
	c.Stderr = &perr
	c.Stdout = io.Discard
	start := time.Now()
	err := c.Run()
	elapsed := time.Since(start)
	var exitErr *exec.ExitError
	require.True(t, errors.As(err, &exitErr), "ProxyCommand: %v, stderr %q", err, perr.String())
	assert.Equal(t, tunnelclient.ExitAgentNotFound, exitErr.ExitCode(), perr.String())
	assert.Less(t, elapsed, 5*time.Second)
	assertOnePlainLine(t, perr.String())
	assert.Contains(t, perr.String(), "not found")

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh client")
	}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	s := exec.CommandContext(ctx, sshBin, "-F", include,
		"-o", "UserKnownHostsFile="+kh, "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes", "-o", "IdentityFile=none", "-o", "BatchMode=yes",
		host, "true")
	s.Dir = "/"
	s.Env = childEnv
	s.Stdin = strings.NewReader("")
	var serr bytes.Buffer
	s.Stderr = &serr
	err = s.Run()
	require.True(t, errors.As(err, &exitErr), "ssh: %v, stderr %q", err, serr.String())
	assert.Equal(t, 255, exitErr.ExitCode(), serr.String())
	assert.Contains(t, serr.String(), "not found")
}

// topLevelName handles the nil command ExecuteC can return.
func TestSSHCLI_TopLevelNameNil(t *testing.T) {
	assert.Equal(t, "", topLevelName(nil))
	assert.Equal(t, "ssh", topLevelName(sshCmd))
}

// hintTestCommand returns a command whose stderr is captured, as scion
// tunnel passes its own.
func hintTestCommand() (*cobra.Command, *bytes.Buffer) {
	var b bytes.Buffer
	c := &cobra.Command{Use: "tunnel"}
	c.SetErr(&b)
	return c, &b
}

// The host-key hint for tunnel --ssh prints the recreated line on a
// mismatch, stays silent when the key matches or a fetch fails, and always
// returns so the tunnel proceeds.
func TestSSHCLI_TunnelHostKeyHint(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	t.Setenv("HOME", home)
	t.Setenv("SCION_HUB_TOKEN", "tok")

	c, out := hintTestCommand()
	tunnelSSHHostKeyHintIn(c, true, hub.srv.URL, "a-b", "c", sshPinSource{})
	assert.Empty(t, out.String(), "matching key: no hint")

	hub.rotateKey(t, "ag-c")
	c, out = hintTestCommand()
	tunnelSSHHostKeyHintIn(c, true, hub.srv.URL, "a-b", "c", sshPinSource{})
	assert.Equal(t, sshRecreatedHint+"\n", out.String())

	c, out = hintTestCommand()
	tunnelSSHHostKeyHintIn(c, false, hub.srv.URL, "a-b", "c", sshPinSource{})
	assert.Empty(t, out.String(), "without --ssh: nothing")

	hub.deleteAgent("ag-c")
	c, out = hintTestCommand()
	tunnelSSHHostKeyHintIn(c, true, hub.srv.URL, "a-b", "c", sshPinSource{})
	assert.Empty(t, out.String(), "a failed fetch is silent")

	hub.srv.Close()
	c, out = hintTestCommand()
	start := time.Now()
	tunnelSSHHostKeyHintIn(c, true, hub.srv.URL, "a-b", "c", sshPinSource{})
	assert.Empty(t, out.String(), "an unreachable hub is silent")
	assert.Less(t, time.Since(start), sshHostKeyHintTimeout+time.Second)
}
