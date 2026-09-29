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

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
)

const (
	// maxOutputBytes caps stdout and stderr independently, per substrate-runtime.md
	// §5.1 ("Output is capped at 4 MiB per stream (truncated, with a flag on
	// the response)").
	maxOutputBytes = 4 * 1024 * 1024

	// defaultExecTimeout is used when the request omits timeout_s.
	defaultExecTimeout = 30 * time.Second

	// maxExecTimeout bounds an operator-supplied timeout_s so a single exec
	// call cannot pin the control server indefinitely.
	maxExecTimeout = 10 * time.Minute
)

// execCommandContext is overridden in tests so they don't depend on a real
// "scion"/"root" user or su being present in the test environment.
var execCommandContext = exec.CommandContext

// runExec runs argv as user, using the same su/exec-user semantics other
// scion runtimes use for exec (execAsUserCmd — see k8s_runtime.go's Exec for
// the reference call site pkg/runtime.ExecAsUserCmd this mirrors). Output is
// captured with a hard cap per stream; exceeding it sets Truncated rather
// than growing the response without bound.
//
// stdin, when non-empty, is piped to the command's standard input instead of
// being embedded in argv, so a caller delivering a secret (e.g. a
// reset-auth token) never puts it where it would be readable from this
// process's own argv via /proc/<pid>/cmdline. stdin is never logged, never
// echoed into the response, and never written to disk — it flows only from
// the caller's bytes into the child's stdin fd.
func runExec(ctx context.Context, user string, argv []string, stdin []byte, timeout time.Duration) ExecResponse {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	suCmd, err := execAsUserCmd(user, strings.Join(quoted, " "))
	if err != nil {
		// sh/su/whoami could not be resolved as trusted, root-owned
		// executables — this should never happen on a real image (they are
		// the same binaries every other root exec in this codebase depends
		// on existing), but fail closed rather than falling back to a bare
		// name. Reported the same way a process that could not even be
		// started is reported elsewhere in this function: no separate error
		// field exists on ExecResponse, and the underlying error carries no
		// operator-controlled content worth leaking into a log line.
		return ExecResponse{ExitCode: -1}
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := execCommandContext(runCtx, suCmd[0], suCmd[1:]...)
	// Built from scratch (rootexec.Env), not inherited from this process's
	// own environment: PID 1's PATH includes a workload-owned directory
	// (see the rootexec package doc comment), and this handler runs an
	// operator-supplied command as root before any privilege drop. The
	// CA-bundle vars are the one exception, passed through explicitly:
	// execAsUserCmd's generated script may ask su to copy them across the
	// login shell's own environment reset (its -w flag), and with a
	// from-scratch environment su has nothing to copy unless this process
	// hands the values over here itself.
	cmd.Env = rootexec.Env(trustBundleEnvPairs(execCandidateEnv())...)
	stdout := newCappedWriter(maxOutputBytes)
	stderr := newCappedWriter(maxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	// runtime.ExecAsUserCmd's wrapper script re-execs into a fresh `sh -c`
	// (see its doc comment), and that shell does not itself exec-replace
	// its way down to argv[0] — dash forks a real child for a simple
	// command like `sleep 30` rather than tail-call-execing it. Left alone,
	// exec.CommandContext's default Cancel only signals the top wrapper
	// process, orphaning that grandchild to run past the timeout. Put the
	// whole tree in its own process group and kill the group on cancel so
	// a timeout (or the request context ending) actually stops the work.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	runErr := cmd.Run()

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

// shellQuote single-quotes s for safe inclusion in a `sh -c` command line,
// escaping embedded single quotes. Mirrors the quoting KubernetesRuntime.Exec
// uses before handing a command to pkg/runtime.ExecAsUserCmd (execAsUserCmd
// here is the same wrapper; see its doc comment).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// cappedWriter accumulates up to max bytes and silently drops anything
// beyond that, recording that truncation occurred. It never allocates more
// than max bytes for buffered content, so a runaway command cannot exhaust
// the control server's memory.
type cappedWriter struct {
	max       int
	buf       []byte
	truncated bool
}

func newCappedWriter(max int) *cappedWriter {
	return &cappedWriter{max: max}
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if len(w.buf) >= w.max {
		if len(p) > 0 {
			w.truncated = true
		}
		return len(p), nil
	}
	remaining := w.max - len(w.buf)
	if len(p) > remaining {
		w.buf = append(w.buf, p[:remaining]...)
		w.truncated = true
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *cappedWriter) String() string {
	return string(w.buf)
}
