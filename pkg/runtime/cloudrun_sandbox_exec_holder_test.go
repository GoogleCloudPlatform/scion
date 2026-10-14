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

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Log retrieval and exec output holders (ptone/scion#5040): a process the
// sandbox CLI leaves behind may inherit and hold the CLI's stdout and
// stderr, or its stdin. GetLogs, Attach's window-size step, Exec and
// ExecWithStdin capture the output in a file (runSandboxExec), so they
// return when the CLI exits or is killed, not when the holder exits. The
// holders below run 15 s; the 5 s bound leaves wide margins on both sides.

// sandboxExecHolderBound is the elapsed-time bound for a call that must
// not wait for a 15 s holder.
const sandboxExecHolderBound = 5 * time.Second

// sandboxExecHolderBin writes a mock sandbox CLI that starts a 15 s holder
// with holderRedir ("<&3": the CLI's stdin, kept on fd 3 since sh gives a
// background job /dev/null as stdin), records its PID, then runs tail.
func sandboxExecHolderBin(t *testing.T, holderRedir, tail string) string {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Cleanup(func() { killPIDFile(pidFile) })
	return writeMockBin(t, fmt.Sprintf("exec 3<&0; sleep 15 %s 3<&- & echo $! > %q\n%s", holderRedir, pidFile, tail))
}

// sandboxExecHolderCall runs call, failing the test if it does not return
// within 30 s, and returns how long it took.
func sandboxExecHolderCall(t *testing.T, call func()) time.Duration {
	t.Helper()
	done := make(chan struct{})
	begin := time.Now()
	go func() {
		call()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("call did not return within 30 s")
	}
	return time.Since(begin)
}

// Fault check X1: GetLogs captures `tmux capture-pane` through a pipe
// (runSimpleCommand) again. Each case then waits for the 15 s holder and
// exceeds the bound.
func TestSandboxExecHolder_GetLogs(t *testing.T) {
	cases := []struct {
		name    string
		tail    string
		timeout time.Duration
		want    string
		wantErr error
	}{
		{"pane text with a holder", `echo "pane line"; exit 0`, 10 * time.Second, "pane line", nil},
		{"capture hangs past the deadline with a holder", "exec sleep 60", 300 * time.Millisecond, "", context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newWorkaroundTestRuntime(t, sandboxExecHolderBin(t, "", tc.tail))
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			var logs string
			var err error
			d := sandboxExecHolderCall(t, func() { logs, err = rt.GetLogs(ctx, "sb-live") })
			if d > sandboxExecHolderBound {
				t.Errorf("GetLogs took %v, want well under the holder's 15 s: it waited on the held output", d)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("GetLogs error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || logs != tc.want {
				t.Errorf("GetLogs = (%q, %v), want (%q, nil)", logs, err, tc.want)
			}
		})
	}
}

// Fault check X2: Exec captures its output through a pipe
// (runSimpleCommand) again. Each case then waits for the 15 s holder and
// exceeds the bound.
func TestSandboxExecHolder_Exec(t *testing.T) {
	cases := []struct {
		name    string
		tail    string
		timeout time.Duration
		want    string
		wantErr string
	}{
		{"exit 0 with a holder", `echo "  out  "; exit 0`, 10 * time.Second, "out", ""},
		{"exit 3 with a holder", `echo bad; exit 3`, 10 * time.Second, "bad\n", "failed: exit status 3"},
		{"exec hangs past the deadline with a holder", "exec sleep 60", 300 * time.Millisecond, "", "failed: signal: killed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newWorkaroundTestRuntime(t, sandboxExecHolderBin(t, "", tc.tail))
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			var out string
			var err error
			d := sandboxExecHolderCall(t, func() { out, err = rt.Exec(ctx, "sb-live", []string{"/bin/true"}) })
			if d > sandboxExecHolderBound {
				t.Errorf("Exec took %v, want well under the holder's 15 s: it waited on the held output", d)
			}
			if out != tc.want {
				t.Errorf("Exec output = %q, want %q", out, tc.want)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Exec error = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Exec error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// Fault check X3: ExecWithStdin captures its output through a pipe
// (runSimpleCommandWithStdin) again. The CLI copies stdin to its output
// and exits 0, leaving a holder of the output; the call then waits for
// the 15 s holder and exceeds the bound.
func TestSandboxExecHolder_ExecWithStdin(t *testing.T) {
	rt := newWorkaroundTestRuntime(t, sandboxExecHolderBin(t, "", "cat; exit 0"))
	var out string
	var err error
	d := sandboxExecHolderCall(t, func() {
		out, err = rt.ExecWithStdin(context.Background(), "sb-live", []string{"/bin/true"}, strings.NewReader("payload-value\n"))
	})
	if d > sandboxExecHolderBound {
		t.Errorf("ExecWithStdin took %v, want well under the holder's 15 s: it waited on the held output", d)
	}
	if err != nil || out != "payload-value" {
		t.Errorf("ExecWithStdin = (%q, %v), want (%q, nil)", out, err, "payload-value")
	}
}

// Fault check X4: runSandboxExec sets no WaitDelay for stdin. The CLI exits
// 0 without reading a 1 MiB stdin, leaving a holder of the stdin pipe's
// read end that never reads; the copy into the pipe then blocks until the
// 15 s holder exits, and the call exceeds the bound. With the WaitDelay,
// the copy is abandoned after it and Wait reports exec.ErrWaitDelay.
func TestSandboxExecHolder_StdinHeld(t *testing.T) {
	rt := newWorkaroundTestRuntime(t, sandboxExecHolderBin(t, "<&3", "exit 0"))
	payload := strings.Repeat("x", 1<<20)
	var err error
	d := sandboxExecHolderCall(t, func() {
		_, err = rt.ExecWithStdin(context.Background(), "sb-live", []string{"/bin/true"}, strings.NewReader(payload))
	})
	if d > sandboxExecHolderBound {
		t.Errorf("ExecWithStdin took %v, want well under the holder's 15 s: it waited on the held stdin", d)
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("ExecWithStdin error = %v, want exec.ErrWaitDelay (stdin not fully delivered)", err)
	}
}

// Fault check X5: Attach's window-size step captures its output through a
// pipe (runSimpleCommand) again. The step exits 0 leaving a holder; the
// attach itself exits 0 at once. Attach then waits for the 15 s holder
// and exceeds the bound.
func TestSandboxExecHolder_AttachPreflight(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Cleanup(func() { killPIDFile(pidFile) })
	bin := writeMockBin(t, fmt.Sprintf(`case "$*" in
*set-option*) sleep 15 & echo $! > %q; exit 0 ;;
*) exit 0 ;;
esac`, pidFile))
	rt := newWorkaroundTestRuntime(t, bin)
	rt.state.add(&sandboxStateEntry{SandboxName: "sb-live", AgentID: "dev"})
	var err error
	d := sandboxExecHolderCall(t, func() { err = rt.Attach(context.Background(), "sb-live") })
	if d > sandboxExecHolderBound {
		t.Errorf("Attach took %v, want well under the holder's 15 s: the window-size step waited on the held output", d)
	}
	if err != nil {
		t.Errorf("Attach = %v, want nil", err)
	}
}

// TestSandboxExecHolder_NoHolderParity checks that, with no holder,
// runSandboxExec returns exactly what the shared helpers return
// (runSimpleCommand without stdin, runSimpleCommandWithStdin with it):
// trimmed output on success, raw output and the same error text on
// failure, stderr merged into the output in order, stdin delivered.
func TestSandboxExecHolder_NoHolderParity(t *testing.T) {
	cases := []struct {
		name   string
		script string
		stdin  string
		piped  bool
	}{
		{"success trimmed, stderr merged", "echo '  out'; echo err >&2; echo ' tail  '", "", false},
		{"failure raw and wrapped", "echo ' bad '; echo why >&2; exit 2", "", false},
		{"empty output", "exit 0", "", false},
		{"stdin delivered", "cat; echo done >&2", "  payload \n", true},
		{"stdin with failure", "cat; exit 4", "line\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := writeMockBin(t, tc.script)
			ctx := context.Background()
			args := []string{"exec", "sb-live", "--", "/bin/true"}
			var stdin io.Reader
			var wantOut string
			var wantErr error
			if tc.piped {
				stdin = strings.NewReader(tc.stdin)
				wantOut, wantErr = runSimpleCommandWithStdin(ctx, strings.NewReader(tc.stdin), bin, args...)
			} else {
				wantOut, wantErr = runSimpleCommand(ctx, bin, args...)
			}
			gotOut, gotErr := runSandboxExec(ctx, bin, stdin, args...)
			if gotOut != wantOut {
				t.Errorf("output = %q, want %q (shared helper)", gotOut, wantOut)
			}
			if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Errorf("error = %v, want %v (shared helper)", gotErr, wantErr)
			}
		})
	}
}

// Fault check X6: runSandboxExec logs a failed command's output directly
// instead of through logCommandFailure. The output of a failed call marked
// WithSensitiveExec then reaches the debug log.
func TestSandboxExecHolder_SensitiveExecLog(t *testing.T) {
	const sentinel = "SANDBOX-EXEC-SENTINEL-not-for-logs"
	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})
	origLevel := slog.SetLogLoggerLevel(slog.LevelDebug)
	t.Cleanup(func() { slog.SetLogLoggerLevel(origLevel) })

	bin := writeMockBin(t, "echo "+sentinel+"; exit 1")
	if _, err := runSandboxExec(context.Background(), bin, nil, "exec", "sb-live"); err == nil {
		t.Fatal("expected the command to fail")
	}
	if !strings.Contains(buf.String(), sentinel) {
		t.Errorf("a failed command's output should be logged when not marked sensitive; log: %s", buf.String())
	}
	buf.Reset()
	_, err := runSandboxExec(WithSensitiveExec(context.Background()), bin, strings.NewReader("in\n"), "exec", "sb-live")
	if err == nil {
		t.Fatal("expected the command to fail")
	}
	if strings.Contains(buf.String(), sentinel) {
		t.Errorf("the debug log of a call marked sensitive carries its output; log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "Command failed") {
		t.Errorf("the failure should still be logged without its output; log: %s", buf.String())
	}
}
