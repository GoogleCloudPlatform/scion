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
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// sshExitFailure is the ssh convention for a local or connection failure.
const sshExitFailure = 255

// sshRemoteUser is the user name sent to the agent. The agent's SSH server
// forces the container user regardless.
const sshRemoteUser = "scion"

var sshCmd = &cobra.Command{
	Use:   "ssh <agent> [-- <command>...]",
	Short: "Open an SSH session to an agent through scion tunnel",
	Long: `Open an SSH session to an agent with the system ssh client.

The connection runs through scion tunnel as the ProxyCommand, and the agent's
host key is taken from the hub, so no keys or known_hosts entries need to be
set up first. The agent is <agent> in the current project, <project>/<agent>,
or the only agent with that name across all projects.

scion ssh never prompts for authentication, registration or sync. It returns
the remote command's exit status, or 255 for a local or connection failure,
with the reason on one stderr line.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		env, err := defaultSSHCommandEnv()
		if err != nil {
			fmt.Fprintf(os.Stderr, "scion ssh: %s\n", sshOneLine(err.Error()))
			sshExit(sshExitFailure)
			return nil
		}
		if code := runSSH(cmd.Context(), env, args); code != ExitOK {
			sshExit(code)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(sshCmd)
}

// runSSH runs scion ssh and returns the process exit code: the remote status
// when a session ran, otherwise 255 with one line on env.stderr.
func runSSH(ctx context.Context, env *sshCommandEnv, args []string) int {
	sshArgs, cleanup, err := prepareSSH(ctx, env, args[0], args[1:])
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "scion ssh: %s\n", sshOneLine(err.Error()))
		return sshExitFailure
	}
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		fmt.Fprintln(env.stderr, "scion ssh: ssh client not found on PATH")
		return sshExitFailure
	}
	c := exec.CommandContext(ctx, sshBin, sshArgs...)
	c.Stdin = env.stdin
	c.Stdout = env.stdout
	c.Stderr = env.stderr

	// The terminal's interrupt goes to ssh, which forwards it to the remote
	// session; scion stays alive to return ssh's status.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(sigs)

	if err := c.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(env.stderr, "scion ssh: running ssh: %s\n", sshOneLine(err.Error()))
		return sshExitFailure
	}
	return ExitOK
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
	entry, err := hub.conn.resolveSSHTarget(rctx, ref, env.settingsProjectID)
	if err != nil {
		return nil, nil, err
	}
	key, err := hub.conn.agentSSHHostKey(rctx, "", entry.agentID)
	if err != nil {
		return nil, nil, err
	}
	if key == "" {
		return nil, nil, Errorf(ExitError, "agent %s/%s has no SSH host key yet", entry.projectSlug, entry.agentSlug)
	}
	norm, err := normalizeSSHHostKey(key)
	if err != nil {
		return nil, nil, Errorf(ExitError, "agent %s/%s has an invalid SSH host key: %v", entry.projectSlug, entry.agentSlug, err)
	}
	host := sshHostName(hub.label, entry.projectSlug, entry.agentSlug)

	dir, err := os.MkdirTemp("", "scion-ssh-")
	if err != nil {
		return nil, nil, Errorf(ExitError, "temporary directory: %v", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(host+" "+norm+"\n"), sshFileMode); err != nil {
		return nil, cleanup, Errorf(ExitError, "writing known hosts: %v", err)
	}

	proxy := strings.Join([]string{
		sshShellQuote(env.exe), "tunnel", "--stdio", "--ssh",
		"--hub", sshShellQuote(hub.endpoint),
		"--project", sshShellQuote(entry.projectSlug),
		sshShellQuote(entry.agentSlug),
	}, " ")
	sshArgs := []string{
		"-o", "ProxyCommand=" + proxy,
		"-o", "HostKeyAlias=" + host,
		"-o", "UserKnownHostsFile=" + knownHosts,
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
