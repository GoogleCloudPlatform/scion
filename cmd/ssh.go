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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
)

// sshExitFailure is the ssh convention for a local or connection failure.
const sshExitFailure = 255

// sshRemoteUser is the user name sent to the agent. The agent's SSH server
// forces the container user regardless.
const sshRemoteUser = "scion"

// sshUsageLine is printed, on one line, for a usage or flag error.
const sshUsageLine = "usage: scion ssh [flags] <agent> [<command>...]"

var sshCmd = &cobra.Command{
	Use:   "ssh <agent> [<command>...]",
	Short: "Open an SSH session to an agent through scion tunnel",
	Long: `Open an SSH session to an agent with the system ssh client.

The connection runs through scion tunnel as the ProxyCommand, and the agent's
host key is taken from the hub, so no keys or known_hosts entries need to be
set up first. The agent is <agent> in the current project, <project>/<agent>,
or the only agent with that name across all projects.

Flags for scion ssh go before the agent. Everything after the agent is the
remote command and is passed to ssh unchanged, flags included.

scion ssh never prompts for authentication, registration or sync. It returns
the remote command's exit status, or 255 for a local or connection failure,
including a usage or flag error, with the reason on one stderr line.

A hangup (SIGHUP) or termination request (SIGTERM) sent to scion ssh is
passed on to ssh, and scion ssh exits with ssh, with status 129 or 143.`,
	Args: cobra.ArbitraryArgs,
	RunE: runSSHCommand,
}

func init() {
	// Stop flag parsing at the agent, so the remote command's flags reach ssh.
	sshCmd.Flags().SetInterspersed(false)
	sshCmd.SetFlagErrorFunc(sshFlagError)
	rootCmd.AddCommand(sshCmd)
}

// runSSHCommand is the RunE of scion ssh.
func runSSHCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "scion ssh: %s\n", sshUsageLine)
		sshExit(sshExitFailure)
		return nil
	}
	env, err := defaultSSHCommandEnv()
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "scion ssh: %s\n", sshOneLine(err.Error()))
		sshExit(sshExitFailure)
		return nil
	}
	if code := runSSH(cmd.Context(), env, args); code != tunnelclient.ExitOK {
		sshExit(code)
	}
	return nil
}

// reportSSHError writes a failed scion ssh invocation's error as one plain
// line and returns 255.
func reportSSHError(w io.Writer, err error) int {
	_, _ = fmt.Fprintf(w, "scion ssh: %s\n", plainErrorLine(config.RedactURLsIn(err.Error())))
	return sshExitFailure
}

// sshFlagError reports a flag error on scion ssh as one line and exits 255,
// following the ssh convention instead of the CLI's usual error output.
func sshFlagError(cmd *cobra.Command, err error) error {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "scion ssh: %s; %s\n", sshOneLine(err.Error()), sshUsageLine)
	sshExit(sshExitFailure)
	return err
}

// runSSH runs scion ssh and returns the process exit code: the remote status
// when a session ran, otherwise 255 with one line on env.stderr.
func runSSH(ctx context.Context, env *sshCommandEnv, args []string) int {
	remote := args[1:]
	// Flag parsing stops at the agent, so a "--" after it is still present;
	// it separates the remote command and is not part of it.
	if len(remote) > 0 && remote[0] == "--" {
		remote = remote[1:]
	}
	sshArgs, cleanup, err := prepareSSH(ctx, env, args[0], remote)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "scion ssh: %s\n", sshOneLine(err.Error()))
		return sshExitFailure
	}
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		_, _ = fmt.Fprintln(env.stderr, "scion ssh: ssh client not found on PATH")
		return sshExitFailure
	}
	c := exec.CommandContext(ctx, sshBin, sshArgs...)
	c.Stdin = env.stdin
	c.Stdout = env.stdout
	c.Stderr = env.stderr
	// Bound the wait for output a leftover ProxyCommand still holds open
	// after ssh has exited.
	c.WaitDelay = time.Second

	// Signals are caught before ssh starts, so none can end scion while
	// ssh outlives it.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, append(append([]os.Signal{}, sshTerminalSignals...), sshEndSignals...)...)
	defer signal.Stop(sigs)

	if err := c.Start(); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "scion ssh: running ssh: %s\n", sshOneLine(err.Error()))
		return sshExitFailure
	}
	return waitSSH(c, sigs, env.stderr)
}

// sshSignalGrace is how long ssh has to exit after a passed-on signal before
// it is killed.
var sshSignalGrace = 5 * time.Second

// waitSSH waits for the started ssh process and returns scion ssh's exit
// code. A terminal signal is left to ssh, which forwards it to the remote
// session, and scion returns ssh's status. A hangup or termination request
// is passed on to ssh, which is killed if it has not exited within
// sshSignalGrace, and scion returns 128 plus the signal number of the first
// one (129 or 143).
func waitSSH(c *exec.Cmd, sigs <-chan os.Signal, stderr io.Writer) int {
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	var ended os.Signal
	var kill <-chan time.Time
	for {
		select {
		case err := <-done:
			if ended != nil {
				return tunnelclient.SignalExit(ended)
			}
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
					return exitErr.ExitCode()
				}
				_, _ = fmt.Fprintf(stderr, "scion ssh: running ssh: %s\n", sshOneLine(err.Error()))
				return sshExitFailure
			}
			return tunnelclient.ExitOK
		case sig := <-sigs:
			if !slices.Contains(sshEndSignals, sig) {
				continue
			}
			forwardSSHSignal(c.Process, sig)
			if ended == nil {
				ended = sig
				kill = time.After(sshSignalGrace)
			}
		case <-kill:
			_ = c.Process.Kill()
			kill = nil
		}
	}
}

// prepareSSH resolves the agent and its host key and returns the ssh
// arguments. The returned cleanup removes the temporary known-hosts file.
func prepareSSH(ctx context.Context, env *sshCommandEnv, ref string, remote []string) ([]string, func(), error) {
	hub, err := resolveSSHHub(env)
	if err != nil {
		return nil, nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, sshHubRequestTimeout)
	defer cancel()
	entry, err := hub.conn.resolveSSHTarget(rctx, ref, env.project)
	if err != nil {
		return nil, nil, err
	}
	key, err := hub.conn.agentSSHHostKey(rctx, "", entry.agentID)
	if err != nil {
		return nil, nil, err
	}
	if key == "" {
		return nil, nil, tunnelclient.Errorf(tunnelclient.ExitError, "agent %s/%s has no SSH host key yet", entry.projectSlug, entry.agentSlug)
	}
	norm, err := normalizeSSHHostKey(key)
	if err != nil {
		return nil, nil, tunnelclient.Errorf(tunnelclient.ExitError, "agent %s/%s has an invalid SSH host key: %v", entry.projectSlug, entry.agentSlug, err)
	}
	if err := checkSSHNameParts(hub.label, entry.projectSlug, entry.agentSlug); err != nil {
		return nil, nil, tunnelclient.Errorf(tunnelclient.ExitError, "%v", err)
	}
	if !filepath.IsAbs(env.exe) || hasControl(env.exe) {
		return nil, nil, tunnelclient.Errorf(tunnelclient.ExitError, "scion executable path %q is not a clean absolute path", env.exe)
	}
	host := sshHostName(hub.label, entry.projectSlug, entry.agentSlug)
	// Every ProxyCommand word is checked before the temporary directory is
	// made.
	proxy, err := sshProxyCommand(env.exe, hub.hubArg, entry.projectSlug, entry.agentSlug, "", "")
	if err != nil {
		return nil, nil, tunnelclient.Errorf(tunnelclient.ExitError, "%v", err)
	}

	dir, err := os.MkdirTemp("", "scion-ssh-")
	if err != nil {
		return nil, nil, tunnelclient.Errorf(tunnelclient.ExitError, "temporary directory: %v", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	knownHosts := filepath.Join(dir, "known_hosts")
	knownHostsArg, err := sshKnownHostsOptionValue(knownHosts)
	if err != nil {
		return nil, cleanup, tunnelclient.Errorf(tunnelclient.ExitError, "temporary directory %s %v", dir, err)
	}
	// The shared atomic writer: a new private file in
	// the fresh temporary directory.
	if err := writeFileAtomic(knownHosts, []byte(host+" "+norm+"\n"), sshFileMode); err != nil {
		return nil, cleanup, tunnelclient.Errorf(tunnelclient.ExitError, "writing known hosts: %v", err)
	}

	sshArgs := []string{
		"-o", "ProxyCommand=" + proxy,
		"-o", "HostKeyAlias=" + host,
		"-o", "UserKnownHostsFile=" + knownHostsArg,
		// /dev/null on every OS: Win32-OpenSSH maps it to NUL when it opens
		// a file (contrib/win32/win32compat/misc.c w32_fopen_utf8 and
		// fileio.c fileio_open, NULL_DEVICE in misc_internal.h,
		// PowerShell/openssh-portable latestw_all 274893f8), and Git for
		// Windows' ssh (MSYS) has /dev/null natively.
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		"-l", sshRemoteUser,
		host,
	}
	// ssh stops option parsing at the host, so the rest is the remote command.
	sshArgs = append(sshArgs, remote...)
	return sshArgs, cleanup, nil
}

// sshKnownHostsOptionValue returns path as the value of ssh's
// UserKnownHostsFile option. ssh splits that value into words (it may list
// several files) and expands % tokens in it.
//
// On unix a path with whitespace, a control character or '%' is refused,
// as before, and the hint names TMPDIR (os.TempDir).
//
// On Windows (sshQuoteForWindows) the temp directory is under %TMP% or
// %TEMP%, typically C:\Users\<account>\AppData\Local\Temp, and account
// names with spaces are common. So the path is written with forward
// slashes (ssh's word splitter treats a backslash as an escape; both
// Win32-OpenSSH and Git for Windows' ssh accept C:/ paths) and wrapped in
// double quotes when it has a space, which ssh's option parser reads as
// one word. Only a control character, '%' or '"' is refused there, and the
// hint names TMP and TEMP.
func sshKnownHostsOptionValue(path string) (string, error) {
	if !sshQuoteForWindows {
		if hasSpaceOrControl(path) || strings.Contains(path, "%") {
			return "", errors.New("contains whitespace or %; set TMPDIR to a plain path")
		}
		return path, nil
	}
	if hasControl(path) || strings.ContainsAny(path, `%"`) {
		return "", errors.New(`contains a control character, % or "; set TMP and TEMP to a plain path`)
	}
	v := strings.ReplaceAll(path, `\`, "/")
	if strings.ContainsAny(v, " \t") {
		v = `"` + v + `"`
	}
	return v, nil
}
