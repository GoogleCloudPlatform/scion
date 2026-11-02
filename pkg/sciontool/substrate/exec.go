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
	"os/exec"
	"strings"
	"time"
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

	// execWaitDelay bounds how long cmd.Wait() keeps copying output after
	// the child itself has exited or been killed: without it, a
	// grandchild that inherited the pipe's write end (the exact shape a
	// timeout's process-group kill is meant to clean up) could keep
	// Wait() blocked indefinitely waiting for that pipe to close, even
	// though the command we actually care about is long gone.
	execWaitDelay = 5 * time.Second
)

// execCommandContext is overridden in tests that need to observe or intercept
// the command runExec builds.
var execCommandContext = exec.CommandContext

// execSetupFailure reports an exec that was refused before any process was
// started (the target user could not be resolved, resolves to root, or has
// no usable home directory): exit code -1, like a process that could not be
// started, with the reason on stderr so the caller sees why. The reason
// names only the target user and its passwd fields, never argv or stdin.
func execSetupFailure(err error) ExecResponse {
	return ExecResponse{ExitCode: -1, Stderr: err.Error() + "\n"}
}

// shellQuote single-quotes s for safe inclusion in a `sh -c` command line,
// escaping embedded single quotes. Mirrors the quoting KubernetesRuntime.Exec
// uses before handing a command to pkg/runtime.ExecAsUserCmd, the equivalent
// mechanism other scion runtimes use for exec.
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
