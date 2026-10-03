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
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
)

func TestCappedWriter_UnderLimitNotTruncated(t *testing.T) {
	w := newCappedWriter(10)
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if w.truncated {
		t.Error("truncated = true, want false")
	}
	if w.String() != "hello" {
		t.Errorf("String() = %q, want %q", w.String(), "hello")
	}
}

func TestCappedWriter_ExactLimitNotTruncated(t *testing.T) {
	w := newCappedWriter(5)
	_, _ = w.Write([]byte("hello"))
	if w.truncated {
		t.Error("truncated = true at exactly the limit, want false")
	}
}

func TestCappedWriter_OverLimitTruncatesAndCaps(t *testing.T) {
	w := newCappedWriter(5)
	_, _ = w.Write([]byte("hello world"))
	if !w.truncated {
		t.Error("truncated = false, want true")
	}
	if len(w.String()) != 5 {
		t.Errorf("buffered length = %d, want capped at 5", len(w.String()))
	}
	if w.String() != "hello" {
		t.Errorf("String() = %q, want %q", w.String(), "hello")
	}
}

func TestCappedWriter_SplitAcrossWrites(t *testing.T) {
	w := newCappedWriter(5)
	_, _ = w.Write([]byte("he"))
	_, _ = w.Write([]byte("llo world"))
	if !w.truncated {
		t.Error("truncated = false, want true once combined writes exceed the cap")
	}
	if w.String() != "hello" {
		t.Errorf("String() = %q, want %q", w.String(), "hello")
	}
}

func TestShellQuote_EscapesSingleQuotes(t *testing.T) {
	got := shellQuote(`it's a "test"`)
	want := `'it'"'"'s a "test"'`
	if got != want {
		t.Errorf("shellQuote = %q, want %q", got, want)
	}
}

// TestRunExec_OutputCapsAndFlags is an end-to-end test (real subprocess) of
// the 4 MiB per-stream cap required by substrate-runtime.md §5.1. It generates
// more than maxOutputBytes on stdout and confirms the response is capped at
// exactly maxOutputBytes with truncated=true, and does the same for stderr
// independently.
func TestRunExec_OutputCapsAndFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real subprocesses producing several MB of output")
	}

	// head -c is fast and available on any Linux test runner; /dev/zero
	// bytes decode fine as a string for length-only assertions.
	over := maxOutputBytes + 1024
	resp := runExec(context.Background(), "scion",
		[]string{"sh", "-c", "head -c " + itoa(over) + " /dev/zero"}, nil, 10*time.Second)

	if resp.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if !resp.Truncated {
		t.Error("truncated = false, want true for output exceeding the cap")
	}
	if len(resp.Stdout) != maxOutputBytes {
		t.Errorf("stdout length = %d, want exactly %d", len(resp.Stdout), maxOutputBytes)
	}
}

func TestRunExec_StderrCappedIndependently(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real subprocesses producing several MB of output")
	}

	over := maxOutputBytes + 1024
	resp := runExec(context.Background(), "scion",
		[]string{"sh", "-c", "head -c " + itoa(over) + " /dev/zero 1>&2"}, nil, 10*time.Second)

	if !resp.Truncated {
		t.Error("truncated = false, want true when stderr alone exceeds the cap")
	}
	if len(resp.Stderr) != maxOutputBytes {
		t.Errorf("stderr length = %d, want exactly %d", len(resp.Stderr), maxOutputBytes)
	}
	if len(resp.Stdout) != 0 {
		t.Errorf("stdout length = %d, want 0", len(resp.Stdout))
	}
}

// TestRunExec_NeverConsultsPATHForSh is the required regression test for
// runExec's own "sh" resolution: with $PATH pointed at a directory
// containing a planted "sh" script (the attack shape a planted binary
// first on PATH would take) that leaves a marker file if ever run, the
// real system sh must still be what actually executes — rootexec.Resolve's
// fixed search list is what decides, never $PATH — so the command still
// runs normally and the marker is never created.
func TestRunExec_NeverConsultsPATHForSh(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "planted-ran")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	// Passing this process's own real user name takes the "already this
	// identity, no credential drop" branch (see execUserCredential) — the
	// same real user substrate's own broker-exec tests already rely on
	// running as.
	me := currentUsername(t)
	resp := runExec(context.Background(), me, []string{"true"}, nil, 5*time.Second)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("runExec executed a planted sh from $PATH")
	}
	if resp.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0 (the real, resolved sh must still have run the command)", resp.ExitCode)
	}
}

// currentUsername resolves this test process's own username, the same way
// execUserCredential's own user.Lookup call will see it.
func currentUsername(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("could not resolve current username: %v", err)
	}
	return u.Username
}

// TestRunExec_ChildEnvNeverContainsScionAgentVars pins that runExec's child
// environment is genuinely built from scratch (rootexec.Env plus only the
// HOME/USER/LOGNAME/SHELL and CA-bundle pairs execUserCredential/
// trustBundleEnvPairs add), never from this process's own os.Environ():
// nothing under a "SCION_" prefix ever reaches the child, checked by asking
// the real child to print its own environment.
func TestRunExec_ChildEnvNeverContainsScionAgentVars(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess")
	}
	t.Setenv("SCION_AGENT_NAME", "should-not-leak")
	t.Setenv("SCION_AGENT_SLUG", "should-not-leak-slug")
	t.Setenv("SCION_UNRELATED_VAR", "should-not-leak-either")

	me := currentUsername(t)
	resp := runExec(context.Background(), me, []string{"env"}, nil, 5*time.Second)

	if resp.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if strings.Contains(resp.Stdout, "SCION_") {
		t.Errorf("runExec's child environment leaked a SCION_* variable:\n%s", resp.Stdout)
	}
}

// TestRunExec_SetsHomeUserPathShellForScion is the end-to-end regression
// test for A2: a real exec'd child for user "scion" must see HOME, USER,
// PATH, and SHELL all set explicitly — the four a direct credential drop
// does not set on its own the way `su -`'s login-shell semantics used to
// (see execUserCredential's own doc comment) — asked for directly rather
// than only at execUserCredential's own unit-test level, since PATH comes
// from rootexec.Env, not from execUserCredential's own return value.
func TestRunExec_SetsHomeUserPathShellForScion(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess")
	}
	u, err := user.Lookup("scion")
	if err != nil {
		t.Skipf("no real \"scion\" user on this machine: %v", err)
	}
	shPath, err := rootexec.Resolve("sh")
	if err != nil {
		t.Fatalf("resolve sh: %v", err)
	}

	resp := runExec(context.Background(), "scion", []string{"env"}, nil, 5*time.Second)
	if resp.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}

	env := make(map[string]string)
	for _, line := range strings.Split(resp.Stdout, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	want := map[string]string{
		"HOME":    u.HomeDir,
		"USER":    "scion",
		"LOGNAME": "scion",
		"SHELL":   shPath,
		"PATH":    strings.Join(rootexec.SearchPath, ":"),
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("child env %s = %q, want %q (full env:\n%s)", k, env[k], v, resp.Stdout)
		}
	}
}

func TestRunExec_TimeoutKillsProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on a real subprocess timeout")
	}
	start := time.Now()
	resp := runExec(context.Background(), "scion", []string{"sh", "-c", "echo started; sleep 30"}, nil, 300*time.Millisecond)
	elapsed := time.Since(start)

	// The marker confirms the command actually ran before the timeout
	// killed it ("started" must reach stdout), distinguishing a real
	// timeout kill from the command never having run at all — the same
	// elapsed time and exit code would otherwise make the two
	// indistinguishable.
	if !strings.Contains(resp.Stdout, "started") {
		t.Errorf("subprocess never ran (stdout=%q stderr=%q) — timeout killed something other than the command", resp.Stdout, resp.Stderr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runExec took %v, want it to be killed near the 300ms timeout", elapsed)
	}
	if resp.ExitCode == 0 {
		t.Errorf("exit_code = 0, want non-zero for a timed-out command")
	}
}

// TestRunExec_SucceedsUnderActiveReaper is the regression test for running
// the exec child through procreap.RunManaged rather than a bare cmd.Run():
// with the PID 1 SIGCHLD reaper goroutine actually running (StartReaper),
// an unrelated reap pass racing this call's own cmd.Wait must never steal
// its exit status out from under it (see procreap's package doc for the
// "waitid: no child processes" failure this registration prevents). A
// command that runs to completion must still report exit code 0 while that
// reaper is live.
func TestRunExec_SucceedsUnderActiveReaper(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess and a real signal-handling goroutine")
	}
	procreap.StartReaper()

	resp := runExec(context.Background(), "scion", []string{"true"}, nil, 5*time.Second)

	if resp.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
}

// itoa avoids pulling in strconv just for a couple of formatted numbers in
// shell commands built above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
