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
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// The shared command helpers capture standard output and standard error
// apart. A CLI's warning on stderr (podman prints "The input device is not
// a TTY" for "run -t" with no terminal) must not reach a value a caller
// parses, such as the container ID from "run -d"; a failure's stderr must
// still reach the caller, in the returned output and the error.

const noTTYWarning = "WARN[0000] The input device is not a TTY. The --tty and --interactive flags might not work properly"

func TestRunSimpleCommand_ParsesStdoutOnly(t *testing.T) {
	bin := writeMockBin(t, `echo "`+noTTYWarning+`" >&2; echo '  abc123  '; echo "later warning" >&2`)
	out, err := runSimpleCommand(context.Background(), bin, "run")
	if err != nil {
		t.Fatalf("runSimpleCommand: %v", err)
	}
	if out != "abc123" {
		t.Fatalf("output = %q, want only the trimmed stdout %q", out, "abc123")
	}
}

func TestRunSimpleCommandWithStdin_ParsesStdoutOnly(t *testing.T) {
	bin := writeMockBin(t, `cat; echo "`+noTTYWarning+`" >&2`)
	out, err := runSimpleCommandWithStdin(context.Background(), strings.NewReader("payload\n"), bin, "exec")
	if err != nil {
		t.Fatalf("runSimpleCommandWithStdin: %v", err)
	}
	if out != "payload" {
		t.Fatalf("output = %q, want only the trimmed stdout %q", out, "payload")
	}
}

// On failure the returned output is stdout then stderr (callers classify a
// failure by its text, e.g. isImageNotFoundOutput), and the error carries
// stderr and still unwraps to the exit error.
func TestRunSimpleCommand_FailureCarriesStderr(t *testing.T) {
	bin := writeMockBin(t, `echo partial; echo "Error: no such image" >&2; exit 3`)
	for _, tc := range []struct {
		name string
		run  func() (string, error)
	}{
		{"plain", func() (string, error) { return runSimpleCommand(context.Background(), bin, "image", "inspect", "x") }},
		{"stdin", func() (string, error) {
			return runSimpleCommandWithStdin(context.Background(), strings.NewReader(""), bin, "exec")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.run()
			if err == nil {
				t.Fatal("expected an error")
			}
			if out != "partial\nError: no such image\n" {
				t.Errorf("output = %q, want stdout then stderr", out)
			}
			if !isImageNotFoundOutput(out) {
				t.Errorf("isImageNotFoundOutput(%q) = false, want true", out)
			}
			if !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "Error: no such image") {
				t.Errorf("error = %q, want the exit status and stderr", err)
			}
			if strings.Contains(err.Error(), "partial") {
				t.Errorf("error = %q carries stdout; only stderr belongs there", err)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || !isExitError(err) {
				t.Errorf("error %v does not unwrap to *exec.ExitError", err)
			}
		})
	}
}

// A failure with nothing on stderr keeps the plain "<cmd> failed: %w" text.
func TestRunSimpleCommand_FailureWithoutStderr(t *testing.T) {
	bin := writeMockBin(t, `echo only-stdout; exit 1`)
	_, err := runSimpleCommand(context.Background(), bin, "stop")
	if err == nil {
		t.Fatal("expected an error")
	}
	if want := bin + " failed: exit status 1"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// Under WithSensitiveExec the stderr of a failure is not put in the error.
func TestRunSimpleCommand_SensitiveFailureOmitsStderr(t *testing.T) {
	const sentinel = "SENSITIVE-STDERR-SENTINEL-not-for-errors"
	bin := writeMockBin(t, `echo `+sentinel+` >&2; exit 1`)
	ctx := WithSensitiveExec(context.Background())
	for name, run := range map[string]func() (string, error){
		"plain": func() (string, error) { return runSimpleCommand(ctx, bin, "exec") },
		"stdin": func() (string, error) { return runSimpleCommandWithStdin(ctx, strings.NewReader(""), bin, "exec") },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run()
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("error %q carries the stderr of a sensitive exec", err)
			}
		})
	}
}

// A long stderr is cut to its tail in the error: the reason comes last.
func TestRunSimpleCommand_FailureStderrBounded(t *testing.T) {
	bin := writeMockBin(t, `i=0; while [ $i -lt 400 ]; do echo "Copying blob $i of a long pull" >&2; i=$((i+1)); done; echo "Error: final reason" >&2; exit 125`)
	_, err := runSimpleCommand(context.Background(), bin, "pull", "x")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.HasSuffix(msg, "Error: final reason") {
		t.Errorf("error does not end with the last stderr line: ...%q", msg[max(0, len(msg)-80):])
	}
	if len(msg) > commandStderrInErrorMax+len(bin)+64 {
		t.Errorf("error is %d bytes, want stderr bounded to %d", len(msg), commandStderrInErrorMax)
	}
}

// Logs and exec output are shown to a person, not parsed: both streams are
// kept, in the order written.
func TestContainerLogsAndExec_KeepStderr(t *testing.T) {
	bin := writeMockBin(t, `case "$1" in ps|list) echo '[]' ;; *) echo out-line; echo err-line >&2 ;; esac`)
	for _, rc := range containerRuntimeCases() {
		t.Run(rc.name, func(t *testing.T) {
			rt := rc.new(bin)
			logs, err := rt.GetLogs(context.Background(), "agent")
			if err != nil {
				t.Fatalf("GetLogs: %v", err)
			}
			if logs != "out-line\nerr-line" {
				t.Errorf("GetLogs = %q, want both streams in order", logs)
			}
			out, err := rt.Exec(context.Background(), "agent", []string{"true"})
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if out != "out-line\nerr-line" {
				t.Errorf("Exec = %q, want both streams in order", out)
			}
		})
	}
}
