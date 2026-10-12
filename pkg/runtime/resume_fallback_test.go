// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// resumeStubHarness builds its command like ContainerScriptHarness: base,
// then the resume tokens when resuming, then args, then the task. On a
// resume with no task it adds resumePrompt, as task_flag harnesses do.
type resumeStubHarness struct {
	MockHarness
	base         []string
	resumeTokens []string
	resumePrompt string
}

func (h *resumeStubHarness) GetCommand(task string, resume bool, args []string) []string {
	out := append([]string{}, h.base...)
	if resume {
		out = append(out, h.resumeTokens...)
		if task == "" {
			task = h.resumePrompt
		}
	}
	out = append(out, args...)
	if task != "" {
		out = append(out, task)
	}
	return out
}

// stubScript is the fake harness. Each run appends one line to $STUB_LOG:
// "resume" or "fresh", then its arguments separated by '|'. A resume run
// (first argument "resume") follows $STUB_RESUME: "fail" exits 1, "ok"
// exits 0, "term" kills itself with SIGTERM, "wait" writes $STUB_READY and
// sleeps. A fresh run exits 7.
const stubScript = `#!/bin/sh
mode=fresh
if [ "$1" = resume ]; then mode=resume; fi
line=$mode
for a in "$@"; do line="$line|$a"; done
printf '%s\n' "$line" >> "$STUB_LOG"
if [ "$mode" = fresh ]; then exit 7; fi
case "$STUB_RESUME" in
fail) exit 1 ;;
ok) exit 0 ;;
term) kill -TERM $$; sleep 5; exit 3 ;;
wait) : > "$STUB_READY"; sleep 30; exit 3 ;;
esac
exit 9
`

// dateStub replaces date(1). Without $DATE_STEP it prints a fixed time;
// with it, each call prints a time $DATE_STEP seconds after the last.
const dateStub = `#!/bin/sh
f="$DATE_STATE"
n=1000
if [ -f "$f" ]; then n=$(cat "$f"); n=$((n + ${DATE_STEP:-0})); fi
printf '%s\n' "$n" > "$f"
printf '%s\n' "$n"
`

type fallbackRun struct {
	log    []string
	rc     int
	stderr string
}

// fallbackEnv writes the stubs to a temp dir and returns the harness
// (multi-token resume flag, like codex) and the environment to run with.
func fallbackEnv(t *testing.T, resumeMode string, dateStep int) (*resumeStubHarness, []string, string) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "harness")
	if err := os.WriteFile(stub, []byte(stubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "date"), []byte(dateStub), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &resumeStubHarness{base: []string{stub}, resumeTokens: []string{"resume", "--last"}, resumePrompt: "Continue your previous task."}
	env := append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"STUB_LOG="+filepath.Join(dir, "log"),
		"STUB_READY="+filepath.Join(dir, "ready"),
		"STUB_RESUME="+resumeMode,
		"DATE_STATE="+filepath.Join(dir, "date-state"),
		"DATE_STEP="+strconv.Itoa(dateStep),
	)
	return h, env, dir
}

// runLine runs line under /bin/sh as tmuxAgentWindowCmd would, recording
// the line's exit status to a file instead of the fixed container path.
func runLine(t *testing.T, line string, env []string, dir string) fallbackRun {
	t.Helper()
	rcFile := filepath.Join(dir, "rc")
	cmd := exec.Command("/bin/sh", "-c", line+"; echo $? > "+shellQuote(rcFile))
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()
	return readRun(t, dir, rcFile, stderr.String())
}

func readRun(t *testing.T, dir, rcFile, stderr string) fallbackRun {
	t.Helper()
	run := fallbackRun{rc: -1, stderr: stderr}
	if data, err := os.ReadFile(filepath.Join(dir, "log")); err == nil {
		// Trim only the final newline: an argument may end in a space.
		run.log = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	if data, err := os.ReadFile(rcFile); err == nil {
		run.rc, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	return run
}

func fallbackLine(t *testing.T, h *resumeStubHarness, task string) string {
	t.Helper()
	line, ok := harnessCmdLine(RunConfig{Harness: h, Task: task, Resume: true, CommandArgs: []string{"--yolo"}},
		harnessCmdLineOptions{FreshStartOnFailedResume: true})
	if !ok {
		t.Fatal("harnessCmdLine: no command line")
	}
	return line
}

// TestResumeFallback_NoConversation: a resume that exits 1 at once (no
// session to continue) is followed by a fresh run without the harness's
// resume tokens, carrying only the task, whose exit code is recorded.
func TestResumeFallback_NoConversation(t *testing.T) {
	const task = `new message: it's "$HOME" & more`
	h, env, dir := fallbackEnv(t, "fail", 0)
	run := runLine(t, fallbackLine(t, h, task), env, dir)
	want := []string{
		"resume|resume|--last|--yolo|" + task,
		"fresh|--yolo|" + task,
	}
	if strings.Join(run.log, "\n") != strings.Join(want, "\n") {
		t.Fatalf("runs = %q, want %q", run.log, want)
	}
	if run.rc != 7 {
		t.Errorf("recorded exit code = %d, want 7 (the fresh run's)", run.rc)
	}
	if !strings.Contains(run.stderr, resumeFallbackNotice) {
		t.Errorf("stderr = %q, want the notice %q", run.stderr, resumeFallbackNotice)
	}
}

// TestResumeFallback_WakeStartsWithNoTask: on a wake (resume with no task)
// the fresh run carries no task at all; the resume-only prompt and any
// earlier task are not replayed.
func TestResumeFallback_WakeStartsWithNoTask(t *testing.T) {
	h, env, dir := fallbackEnv(t, "fail", 0)
	run := runLine(t, fallbackLine(t, h, ""), env, dir)
	want := []string{
		"resume|resume|--last|--yolo|Continue your previous task.",
		"fresh|--yolo",
	}
	if strings.Join(run.log, "\n") != strings.Join(want, "\n") {
		t.Fatalf("runs = %q, want %q", run.log, want)
	}
}

// TestResumeFallback_NormalResume: a resume that succeeds is not followed
// by a fresh run.
func TestResumeFallback_NormalResume(t *testing.T) {
	h, env, dir := fallbackEnv(t, "ok", 0)
	run := runLine(t, fallbackLine(t, h, ""), env, dir)
	if len(run.log) != 1 || !strings.HasPrefix(run.log[0], "resume|") {
		t.Fatalf("runs = %q, want only the resume run", run.log)
	}
	if run.rc != 0 {
		t.Errorf("recorded exit code = %d, want 0", run.rc)
	}
}

// TestResumeFallback_LateFailureKeepsExit: a resume that fails after the
// window (a real session that later crashed) is not restarted fresh, and
// its own exit code is recorded.
func TestResumeFallback_LateFailureKeepsExit(t *testing.T) {
	h, env, dir := fallbackEnv(t, "fail", resumeFallbackWindowSeconds+1)
	run := runLine(t, fallbackLine(t, h, ""), env, dir)
	if len(run.log) != 1 || !strings.HasPrefix(run.log[0], "resume|") {
		t.Fatalf("runs = %q, want only the resume run", run.log)
	}
	if run.rc != 1 {
		t.Errorf("recorded exit code = %d, want 1", run.rc)
	}
}

// TestResumeFallback_SignalledHarnessNoFresh: a resume killed by SIGTERM
// within the window (a stop or suspend) does not start the fresh run.
func TestResumeFallback_SignalledHarnessNoFresh(t *testing.T) {
	h, env, dir := fallbackEnv(t, "term", 0)
	run := runLine(t, fallbackLine(t, h, ""), env, dir)
	if len(run.log) != 1 || !strings.HasPrefix(run.log[0], "resume|") {
		t.Fatalf("runs = %q, want only the resume run", run.log)
	}
	if run.rc != 128+int(syscall.SIGTERM) {
		t.Errorf("recorded exit code = %d, want %d", run.rc, 128+int(syscall.SIGTERM))
	}
}

// TestResumeFallback_StopDuringWindowNoFresh: SIGTERM to the whole process
// group (shell and harness, as on pod delete) during the window does not
// start the fresh run.
func TestResumeFallback_StopDuringWindowNoFresh(t *testing.T) {
	h, env, dir := fallbackEnv(t, "wait", 0)
	line := fallbackLine(t, h, "")
	cmd := exec.Command("/bin/sh", "-c", line)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if ctx.Err() != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			t.Fatal("the resume run did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	// Give an (incorrect) fresh run time to log before checking.
	time.Sleep(200 * time.Millisecond)
	run := readRun(t, dir, filepath.Join(dir, "rc"), "")
	if len(run.log) != 1 || !strings.HasPrefix(run.log[0], "resume|") {
		t.Fatalf("runs = %q, want only the resume run", run.log)
	}
}

// TestHarnessCmdLine_OptionOffUnchanged pins the plain line: without the
// option, and with it when not resuming or when the harness has no resume
// flag, the line is the quoted harness command and nothing more.
func TestHarnessCmdLine_OptionOffUnchanged(t *testing.T) {
	h := &resumeStubHarness{base: []string{"claude"}, resumeTokens: []string{"--continue"}}
	noFlag := &resumeStubHarness{base: []string{"claude"}}
	on := harnessCmdLineOptions{FreshStartOnFailedResume: true}
	for _, tc := range []struct {
		name string
		cfg  RunConfig
		opts harnessCmdLineOptions
		want string
	}{
		{"resume, option off", RunConfig{Harness: h, Resume: true, Task: "hi", CommandArgs: []string{"--x"}}, harnessCmdLineOptions{}, `'claude' '--continue' '--x' 'hi'`},
		{"no resume, option on", RunConfig{Harness: h, Task: "hi"}, on, `'claude' 'hi'`},
		{"no resume flag, option on", RunConfig{Harness: noFlag, Resume: true}, on, `'claude'`},
	} {
		got, ok := harnessCmdLine(tc.cfg, tc.opts)
		if !ok || got != tc.want {
			t.Errorf("%s: harnessCmdLine = %q, %v; want %q", tc.name, got, ok, tc.want)
		}
	}
}

// TestResumeFallback_RuntimeSelection: the Kubernetes runtime adds the
// fallback only for a pod-local home; an NFS home and Docker/Podman keep
// the plain resume line.
func TestResumeFallback_RuntimeSelection(t *testing.T) {
	h := &resumeStubHarness{base: []string{"claude"}, resumeTokens: []string{"--continue"}}
	podCmd := func(nfs bool) string {
		t.Helper()
		rt, _, _ := newTestK8sRuntime()
		cfg := nfsHomeTestConfig(nfs)
		cfg.Harness, cfg.Resume = h, true
		pod, err := rt.buildPod("default", cfg)
		if err != nil {
			t.Fatalf("buildPod(nfs=%v): %v", nfs, err)
		}
		// The pod's command is a start gate; the harness line is in the
		// SCION_START_CMD env var it runs.
		start := podEnv(pod)["SCION_START_CMD"]
		if start == "" {
			t.Fatalf("buildPod(nfs=%v): no SCION_START_CMD", nfs)
		}
		return start
	}
	if got := podCmd(false); !strings.Contains(got, "scion_rc") {
		t.Errorf("pod-local home pod command has no resume fallback: %q", got)
	}
	plainWindow := tmuxAgentWindowCmd("sh", `'claude' '--continue'`)
	if got, want := podCmd(true), buildTmuxStartCmd(plainWindow, tmuxAttachSession); got != want {
		t.Errorf("NFS-home pod command = %q, want the plain resume line %q", got, want)
	}
	args, err := buildCommonRunArgs(RunConfig{Name: "a", Image: "i", Harness: h, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); strings.Contains(got, "scion_rc") || !strings.Contains(got, plainWindow) {
		t.Errorf("docker run args = %q, want the plain resume line %q", got, plainWindow)
	}
}

// longSharedArgs are a system-prompt-style argument of about 8 KB and a
// task of about 3 KB, both with characters the shell must not interpret.
func longSharedArgs() (prompt, task string) {
	prompt = strings.Repeat(`You are an agent; don't stop. "$HOME" * $(id) `, 180)
	task = strings.Repeat(`it's a task & a 'quote' `, 125)
	return prompt, task
}

// TestResumeFallback_LineSizeBounded: arguments common to the resume and
// fresh commands are written once, so a long inline system prompt and task
// add well under 1 KB to the tmux start command, which must stay under
// tmux's command length limit whenever the plain line does.
func TestResumeFallback_LineSizeBounded(t *testing.T) {
	prompt, task := longSharedArgs()
	h := &resumeStubHarness{
		base:         []string{"claude", "--no-chrome", "--dangerously-skip-permissions"},
		resumeTokens: []string{"--continue"},
	}
	cfg := RunConfig{Harness: h, Resume: true, Task: task, CommandArgs: []string{"--system-prompt", prompt}}
	startCmd := func(opts harnessCmdLineOptions) string {
		line, ok := harnessCmdLine(cfg, opts)
		if !ok {
			t.Fatal("harnessCmdLine: no command line")
		}
		return buildTmuxStartCmd(tmuxAgentWindowCmd("sh", line), tmuxAttachSession)
	}
	plain := startCmd(harnessCmdLineOptions{})
	fallback := startCmd(harnessCmdLineOptions{FreshStartOnFailedResume: true})
	if len(plain) < 8*1024 {
		t.Fatalf("plain start command is %d bytes; the test needs a long one", len(plain))
	}
	if added := len(fallback) - len(plain); added >= 1024 {
		t.Errorf("the fallback adds %d bytes to a %d-byte start command; want under 1024", added, len(plain))
	}
}

// TestResumeFallback_LongSharedArgsIntact: long arguments stored in shell
// variables reach both runs as single, unaltered words.
func TestResumeFallback_LongSharedArgsIntact(t *testing.T) {
	prompt, task := longSharedArgs()
	h, env, dir := fallbackEnv(t, "fail", 0)
	line, ok := harnessCmdLine(RunConfig{Harness: h, Task: task, Resume: true, CommandArgs: []string{"--system-prompt", prompt}},
		harnessCmdLineOptions{FreshStartOnFailedResume: true})
	if !ok {
		t.Fatal("harnessCmdLine: no command line")
	}
	if n := strings.Count(line, "You are an agent"); n != 180 {
		t.Errorf("the prompt text appears %d times in the line, want once", n/180)
	}
	run := runLine(t, line, env, dir)
	want := []string{
		"resume|resume|--last|--system-prompt|" + prompt + "|" + task,
		"fresh|--system-prompt|" + prompt + "|" + task,
	}
	if strings.Join(run.log, "\n") != strings.Join(want, "\n") {
		t.Fatalf("runs = %q, want %q", run.log, want)
	}
}
