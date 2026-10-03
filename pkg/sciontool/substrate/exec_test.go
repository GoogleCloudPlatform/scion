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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

	withExecUserAsCurrent(t)

	// head -c is fast and available on any Linux test runner; /dev/zero
	// bytes decode fine as a string for length-only assertions.
	over := maxOutputBytes + 1024
	resp := runExec(context.Background(), "scion",
		[]string{"sh", "-c", "head -c " + strconv.Itoa(over) + " /dev/zero"}, nil, 10*time.Second)

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

	withExecUserAsCurrent(t)

	over := maxOutputBytes + 1024
	resp := runExec(context.Background(), "scion",
		[]string{"sh", "-c", "head -c " + strconv.Itoa(over) + " /dev/zero 1>&2"}, nil, 10*time.Second)

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

	withExecUserAsCurrent(t)
	resp := runExec(context.Background(), "scion", []string{"true"}, nil, 5*time.Second)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("runExec executed a planted sh from $PATH")
	}
	if resp.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0 (the real, resolved sh must still have run the command)", resp.ExitCode)
	}
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

	withExecUserAsCurrent(t)
	resp := runExec(context.Background(), "scion", []string{"env"}, nil, 5*time.Second)

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
// from rootexec.Env, not from execUserCredential's own return value. The
// "scion" passwd entry is the host-independent one withExecUserAsCurrent
// installs, so this runs (rather than skips) on hosts with no real "scion"
// account, and HOME is checked against that entry's temporary home.
func TestRunExec_SetsHomeUserPathShellForScion(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess")
	}
	home := withExecUserAsCurrent(t)
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
		"HOME":    home,
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

// TestRunExec_TimeoutKillsProcess pins the timeout path end to end: the
// command forks a grandchild (a background sleep that also holds the
// output pipes open), and when the timeout fires runExec must kill the
// whole process group, so that grandchild is dead too, and return promptly
// rather than waiting out execWaitDelay for the grandchild to release the
// pipes. The grandchild records its own PID so the test can check it
// directly instead of inferring its death from timing alone.
func TestRunExec_TimeoutKillsProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on a real subprocess timeout")
	}
	withExecUserAsCurrent(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	script := "sleep 30 & echo $! > " + pidFile + "; echo started; wait"

	const timeout = 500 * time.Millisecond
	start := time.Now()
	resp := runExec(context.Background(), "scion", []string{"sh", "-c", script}, nil, timeout)
	elapsed := time.Since(start)

	// The marker confirms the command actually ran before the timeout
	// killed it, distinguishing a real timeout kill from the command never
	// having run at all.
	if !strings.Contains(resp.Stdout, "started") {
		t.Fatalf("subprocess never ran (stdout=%q stderr=%q) — timeout killed something other than the command", resp.Stdout, resp.Stderr)
	}
	if resp.ExitCode == 0 {
		t.Errorf("exit_code = 0, want non-zero for a timed-out command")
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read grandchild pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("grandchild pid file = %q, want a positive pid", raw)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if !waitProcessGone(pid, 2*time.Second) {
		t.Errorf("grandchild pid %d is still running after the timeout; the process-group kill did not reach it", pid)
	}
	if limit := timeout + execWaitDelay; elapsed >= limit {
		t.Errorf("runExec took %v, want under timeout+execWaitDelay (%v): it waited for a grandchild to release the output pipes instead of killing it", elapsed, limit)
	}
}

// waitProcessGone polls until pid no longer names a live process — either
// it is gone entirely (kill(pid, 0) reports ESRCH) or only its zombie entry
// remains, waiting for whichever ancestor it was reparented to to reap it —
// or until timeout. It reports whether the process was seen gone.
func waitProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil && procStatIsZombie(string(stat)) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// procStatIsZombie reports whether a /proc/<pid>/stat line shows state Z.
// The state is the first field after the parenthesised command name, which
// may itself contain spaces or parentheses, so it is located from the last
// ')'.
func procStatIsZombie(stat string) bool {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return false
	}
	return stat[i+2] == 'Z'
}

// reaperChildEnv marks the re-executed test binary that
// TestRunExec_SucceedsUnderActiveReaper runs its body in.
const reaperChildEnv = "SUBSTRATE_EXEC_REAPER_CHILD"

// TestRunExec_SucceedsUnderActiveReaper is the regression test for running
// the exec child through procreap.RunManaged rather than a bare cmd.Run():
// with the SIGCHLD reaper goroutine actually running (StartReaper), an
// unrelated reap pass racing a call's own cmd.Wait must never steal its
// exit status out from under it (see procreap's package doc for the
// "waitid: no child processes" failure this registration prevents). The
// race is timing-dependent, so the body repeats the exec many times to give
// it a real chance to show up; every run must still report exit code 0.
//
// procreap.StartReaper installs a process-wide SIGCHLD handler and a
// goroutine that cannot be stopped, and it reaps any unregistered child of
// the process. Started in this test binary, it would stay live for every
// later test and could reap a child some other test is waiting on outside
// procreap. The body therefore runs in a re-executed copy of this test
// binary, and the reaper dies with that process.
func TestRunExec_SucceedsUnderActiveReaper(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real subprocesses and a real signal-handling goroutine")
	}
	if os.Getenv(reaperChildEnv) == "1" {
		runExecUnderActiveReaper(t)
		return
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	cmd := exec.Command(self, "-test.run=^TestRunExec_SucceedsUnderActiveReaper$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), reaperChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reaper subprocess failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestRunExec_SucceedsUnderActiveReaper") {
		t.Fatalf("reaper subprocess did not run the test body:\n%s", out)
	}
}

func runExecUnderActiveReaper(t *testing.T) {
	withExecUserAsCurrent(t)
	procreap.StartReaper()

	const runs = 100
	for i := 0; i < runs; i++ {
		resp := runExec(context.Background(), "scion", []string{"true"}, nil, 5*time.Second)
		if resp.ExitCode != 0 {
			t.Fatalf("run %d/%d: exit_code = %d, want 0 (stderr=%q)", i+1, runs, resp.ExitCode, resp.Stderr)
		}
	}
}

// TestRunExec_RunsInUserHomeDir pins that the child's working directory is
// the exec user's home directory, never the control server's own.
func TestRunExec_RunsInUserHomeDir(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess")
	}
	home := withExecUserAsCurrent(t)

	resp := runExec(context.Background(), "scion", []string{"pwd", "-P"}, nil, 5*time.Second)
	if resp.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	want, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(resp.Stdout); got != want {
		t.Errorf("child working directory = %q, want the user's home %q", got, want)
	}
}

// TestRunExec_MissingHomeDirRefusesWithoutExec pins the failure path: when
// the exec user's home directory does not exist, runExec reports a clear
// error and starts no process at all.
func TestRunExec_MissingHomeDirRefusesWithoutExec(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-home")
	withExecUserHome(t, missing)

	called := false
	orig := execCommandContext
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		called = true
		return orig(ctx, name, arg...)
	}
	t.Cleanup(func() { execCommandContext = orig })

	resp := runExec(context.Background(), "scion", []string{"true"}, nil, 5*time.Second)
	if called {
		t.Error("runExec built a command although the user's home directory is missing")
	}
	if resp.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, missing) || !strings.Contains(resp.Stderr, "not available") {
		t.Errorf("stderr = %q, want it to name the missing home %q and say it is not available", resp.Stderr, missing)
	}
}
