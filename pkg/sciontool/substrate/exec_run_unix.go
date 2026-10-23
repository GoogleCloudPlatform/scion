//go:build unix

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

package substrate

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
)

// runExec runs argv as user via a direct privilege drop (SysProcAttr.
// Credential — see execUserCredential), never `su`. Output is captured with
// a hard cap per stream; exceeding it sets Truncated rather than growing the
// response without bound.
//
// stdin, when non-empty, is piped to the command's standard input instead of
// being embedded in argv, so a caller delivering a secret (e.g. a
// reset-auth token) never puts it where it would be readable from this
// process's own argv via /proc/<pid>/cmdline. stdin is never logged, never
// echoed into the response, and never written to disk — it flows only from
// the caller's bytes into the child's stdin fd.
func runExec(ctx context.Context, user string, argv []string, stdin []byte, timeout time.Duration) ExecResponse {
	// Resolved via execResolve (production: rootexec.Resolve) rather than
	// left as a bare name for the eventual shell to look up on its own
	// PATH: this handler runs as root before any privilege drop, and a
	// bare name here would otherwise be resolved against root's own
	// inherited PATH, which on substrate includes a directory the workload
	// owns outright (see the rootexec package doc comment).
	shPath, err := execResolve("sh")
	if err != nil {
		// This should never happen on a real image ("sh" is the same
		// binary every other root exec in this codebase depends on
		// existing), but fail closed rather than falling back to a bare
		// name. Reported the same way a process that could not even be
		// started is reported elsewhere in this function: no separate
		// error field exists on ExecResponse, and the underlying error
		// carries no operator-controlled content worth leaking into a log
		// line.
		return ExecResponse{ExitCode: -1}
	}
	envPairs, homeDir, cred, err := execUserCredential(user, shPath)
	if err != nil {
		return execSetupFailure(err)
	}
	if err := checkExecHomeDir(user, homeDir); err != nil {
		return execSetupFailure(err)
	}

	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	cmdString := strings.Join(quoted, " ")

	runCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := execCommandContext(runCtx, shPath, "-c", cmdString)
	// Run in the target user's home directory (checked above), never in
	// whatever directory this root control server happens to run from.
	cmd.Dir = homeDir
	// Built from scratch (rootexec.Env), not inherited from this process's
	// own environment: PID 1's PATH includes a workload-owned directory
	// (see the rootexec package doc comment), and this handler runs an
	// operator-supplied command as root before any privilege drop.
	// envPairs (HOME/USER/LOGNAME/SHELL, resolved for the target user —
	// see execUserCredential) and the CA-bundle vars are added explicitly
	// on top: a direct credential drop does not reset the environment the
	// way `su -` would have, but it also does not SET any of these on its
	// own, so both sets have to be supplied here regardless.
	cmd.Env = rootexec.Env(append(envPairs, trustBundleEnvPairs(execCandidateEnv())...)...)
	stdout := newCappedWriter(maxOutputBytes)
	stderr := newCappedWriter(maxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	// Put the whole tree in its own process group and kill the group on
	// cancel, so a timeout (or the request context ending) stops not just
	// the direct child but anything it forked too — a plain signal to the
	// top process alone would leave a grandchild running past the
	// timeout. WaitDelay bounds how long Wait() then keeps copying output
	// from a pipe a killed grandchild may still be holding open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}
	cmd.WaitDelay = execWaitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// Run under the SIGCHLD reaper's bookkeeping rather than a bare
	// cmd.Run(): without it, a concurrent reap elsewhere in this process
	// (procreap runs one process-wide) can race os/exec's own wait4 call
	// and report a successful command as if it had been reaped out from
	// under it. RunManaged registers this child's PID for the duration of
	// the call so the reaper skips it, then behaves exactly like
	// cmd.Run() otherwise.
	runErr := procreap.RunManaged(cmd)

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.As(runErr, &exitErr):
			exitCode = exitErr.ExitCode()
		case runCtx.Err() == context.DeadlineExceeded:
			// Killed by our own timeout, not a normal exit.
			exitCode = -1
		default:
			// The process could not even be started/waited on (e.g. the
			// shell binary is missing). Report it the same way a signaled
			// process would be reported rather than leaking the raw error
			// (which could echo back argv/paths) as a distinct field.
			exitCode = -1
		}
	}

	return ExecResponse{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ExitCode:  exitCode,
		Truncated: stdout.truncated || stderr.truncated,
	}
}
