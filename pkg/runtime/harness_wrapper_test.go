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

//go:build unix

package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// TestTmuxAgentWindowCmd_UsesFixedPaths pins the production wrapper to the
// shared state paths that sciontool init reads.
func TestTmuxAgentWindowCmd_UsesFixedPaths(t *testing.T) {
	got := tmuxAgentWindowCmd("/bin/sh", "'claude'")
	want := "/bin/sh -c " + shellQuote(harnessWrapperLine("'claude'", state.HarnessTailFile, state.HarnessExitCodeFile))
	if got != want {
		t.Fatalf("tmuxAgentWindowCmd() =\n%s\nwant\n%s", got, want)
	}
	// The tail is captured before the exit code is written.
	if strings.Index(got, state.HarnessTailFile) > strings.Index(got, state.HarnessExitCodeFile) {
		t.Errorf("the tail capture must precede the exit-code write: %s", got)
	}
}

// wrapperRun is the outcome of one run of the wrapper under a fake tmux.
type wrapperRun struct {
	rc       string // contents of the exit-code file, "" if missing
	tail     string // contents of the tail file, "" if missing
	tailMode os.FileMode
	tmuxArgs string // the fake tmux's argv, "" if it did not run
	output   string // combined stdout/stderr of the wrapper shell
	// rcBeforeTail is true if the exit-code file already existed when
	// the fake tmux captured the pane.
	rcBeforeTail bool
}

// runWrapper runs cmdLine through the same two quoting layers as the
// tmux agent window ("sh -c '<wrapper line>'", itself run by a shell),
// with the tail and exit-code files in a temporary directory. tmuxScript
// is the body of a fake tmux put first on PATH ("" for no tmux at all).
func runWrapper(t *testing.T, cmdLine, tmuxScript string) wrapperRun {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	tailFile := filepath.Join(dir, "tail")
	rcFile := filepath.Join(dir, "rc")
	argsFile := filepath.Join(dir, "tmux-args")
	orderFile := filepath.Join(dir, "rc-before-tail")
	if tmuxScript != "" {
		script := "#!/bin/sh\n" +
			"echo \"$@\" > " + shellQuote(argsFile) + "\n" +
			"[ -e " + shellQuote(rcFile) + " ] && touch " + shellQuote(orderFile) + "\n" +
			tmuxScript + "\n"
		if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// PATH holds only the fake tmux and the few tools the scripts use,
	// so a real tmux on the host is never reached.
	for _, tool := range []string{"sh", "touch", "date"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	window := "sh -c " + shellQuote(harnessWrapperLine(cmdLine, tailFile, rcFile))
	cmd := exec.Command(filepath.Join(bin, "sh"), "-c", window)
	cmd.Env = []string{"PATH=" + bin, "TMUX_PANE=%7", "HOME=" + dir}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper run failed: %v\n%s", err, out)
	}
	r := wrapperRun{output: string(out)}
	if b, err := os.ReadFile(rcFile); err == nil {
		r.rc = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(tailFile); err == nil {
		r.tail = string(b)
		if fi, err := os.Stat(tailFile); err == nil {
			r.tailMode = fi.Mode().Perm()
		}
	}
	if b, err := os.ReadFile(argsFile); err == nil {
		r.tmuxArgs = strings.TrimSpace(string(b))
	}
	_, statErr := os.Stat(orderFile)
	r.rcBeforeTail = statErr == nil
	return r
}

// A crashing harness gets its pane tail saved, owner-only, before its own
// exit code is recorded, and the capture targets the harness's own pane.
func TestHarnessWrapperLine_CapturesTailBeforeExitCode(t *testing.T) {
	r := runWrapper(t, `echo 'it'\''s "broken"'; sh -c 'exit 3'`, `printf 'line one\nfatal: boom\n'`)
	if r.rc != "3" {
		t.Errorf("exit code file = %q, want 3", r.rc)
	}
	if r.tail != "line one\nfatal: boom\n" {
		t.Errorf("tail file = %q", r.tail)
	}
	if r.rcBeforeTail {
		t.Error("the exit-code file was written before the pane tail was captured")
	}
	if r.tmuxArgs != "capture-pane -p -J -S -40 -t %7" {
		t.Errorf("tmux args = %q, want the capture of $TMUX_PANE", r.tmuxArgs)
	}
	if r.tailMode != 0o600 {
		t.Errorf("tail file mode = %o, want 600", r.tailMode)
	}
}

// The recorded code is the harness's, whatever the capture does: a tmux
// that fails, or no tmux at all, never replaces it, including a clean 0.
func TestHarnessWrapperLine_PreservesExitStatus(t *testing.T) {
	cases := []struct {
		name, cmdLine, tmux, want string
	}{
		{"failing tmux", "(exit 3)", "echo 'no server' >&2; exit 1", "3"},
		{"no tmux", "(exit 127)", "", "127"},
		{"clean exit", "true", "exit 1", "0"},
		{"signal-like code", "sh -c 'exit 137'", "printf 'x\\n'", "137"},
		// A resume-fallback line ends in an if/else; its status is
		// the last run's.
		{"compound line", `if false; then :; else (exit 5); fi`, "printf 'x\\n'", "5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runWrapper(t, tc.cmdLine, tc.tmux)
			if r.rc != tc.want {
				t.Errorf("exit code file = %q, want %q", r.rc, tc.want)
			}
		})
	}
}

// Defense in depth (ptone/scion#5082): an empty harness command or an empty
// cmdLine never produces an invalid shell line ("sh -c '; ...'"); instead it
// fails with an explained non-zero exit status recorded to HarnessExitCodeFile.
func TestHarnessWrapperLine_EmptyCommandFailsWithExplanation(t *testing.T) {
	cmdLine, ok := harnessCmdLine(RunConfig{Harness: &mockHarness{}}, harnessCmdLineOptions{})
	if !ok {
		t.Fatal("harnessCmdLine returned ok=false for non-nil Harness")
	}
	if strings.TrimSpace(cmdLine) == "" {
		t.Fatalf("harnessCmdLine returned empty command line %q", cmdLine)
	}
	if strings.HasPrefix(strings.TrimSpace(cmdLine), ";") {
		t.Fatalf("harnessCmdLine starts with ';': %q", cmdLine)
	}

	resumeEmptyFallback, ok := harnessCmdLine(RunConfig{
		Harness: &resumeStubHarness{resumeTokens: []string{"--resume"}},
		Resume:  true,
	}, harnessCmdLineOptions{FreshStartOnFailedResume: true})
	if !ok {
		t.Fatal("harnessCmdLine(resume empty freshArgs) returned ok=false")
	}

	for _, tc := range []struct {
		name string
		line string
	}{
		{"from empty GetCommand", cmdLine},
		{"from resume fallback with empty freshArgs", resumeEmptyFallback},
		{"direct empty cmdLine", ""},
		{"direct whitespace cmdLine", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			window := tmuxAgentWindowCmd("sh", tc.line)
			if strings.HasPrefix(window, "sh -c ';") {
				t.Fatalf("tmuxAgentWindowCmd generated leading ';' syntax error: %s", window)
			}
			if !strings.Contains(window, emptyHarnessCmdNotice) {
				t.Errorf("tmuxAgentWindowCmd missing explanation %q: %s", emptyHarnessCmdNotice, window)
			}
			r := runWrapper(t, tc.line, "")
			if r.rc != "1" {
				t.Fatalf("exit code file = %q, want \"1\" (output: %q)", r.rc, r.output)
			}
			if !strings.Contains(r.output, emptyHarnessCmdNotice) {
				t.Errorf("wrapper stderr/stdout = %q, want substring %q", r.output, emptyHarnessCmdNotice)
			}
		})
	}
}

type shellStubHarness struct{ resumeStubHarness }

func (*shellStubHarness) Name() string { return "shell" }

func TestTmuxAgentWindowCmdForConfig_ShellSkipsPaneTail(t *testing.T) {
	shellWindow := tmuxAgentWindowCmdForConfig("/bin/sh", "'while :; do bash -l; done'", RunConfig{
		Harness: &shellStubHarness{},
	})
	if strings.Contains(shellWindow, "capture-pane") {
		t.Errorf("shell harness wrapper must not invoke tmux capture-pane: %s", shellWindow)
	}
	if strings.Contains(shellWindow, state.HarnessTailFile) {
		t.Errorf("shell harness wrapper must not reference tail file %s: %s", state.HarnessTailFile, shellWindow)
	}
	if !strings.Contains(shellWindow, state.HarnessExitCodeFile) {
		t.Errorf("shell harness wrapper must still record exit code to %s: %s", state.HarnessExitCodeFile, shellWindow)
	}

	nonShellWindow := tmuxAgentWindowCmdForConfig("/bin/sh", "'claude'", RunConfig{
		Harness: &resumeStubHarness{},
	})
	if !strings.Contains(nonShellWindow, "capture-pane") || !strings.Contains(nonShellWindow, state.HarnessTailFile) {
		t.Errorf("non-shell harness wrapper must keep pane-tail capture: %s", nonShellWindow)
	}

	// Execute harnessWrapperLine with empty tailFile under a fake tmux and
	// confirm exit code is recorded while tmux is never invoked.
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	tmuxCalled := filepath.Join(dir, "tmux-called")
	rcFile := filepath.Join(dir, "rc")
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\ntouch "+shellQuote(tmuxCalled)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sh", "touch"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	window := "sh -c " + shellQuote(harnessWrapperLine(`sh -c 'exit 7'`, "", rcFile))
	cmd := exec.Command(filepath.Join(bin, "sh"), "-c", window)
	cmd.Env = []string{"PATH=" + bin, "TMUX_PANE=%7", "HOME=" + dir}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper run failed: %v\n%s", err, out)
	}
	gotRC, err := os.ReadFile(rcFile)
	if err != nil {
		t.Fatalf("reading rcFile: %v", err)
	}
	if strings.TrimSpace(string(gotRC)) != "7" {
		t.Errorf("exit code = %q, want 7", strings.TrimSpace(string(gotRC)))
	}
	if _, err := os.Stat(tmuxCalled); err == nil {
		t.Error("fake tmux was invoked despite empty tailFile")
	}
}

// TestGenericHarness_ExplicitShellOptInAndExecForm verifies end-to-end
// execution through harnessCmdLine -> harnessWrapperLine -> POSIX sh:
//   - explicit opt-in (command.base: ["sh", "-c"] or command_args: ["sh", "-c"])
//     executes shell-form tasks with proper quoting, exit-code propagation, and
//     outer-wrapper isolation;
//   - default exec form (no opt-in) never shell-interprets task text, exits 127
//     on shell-form strings, and prints the opt-in hint to stderr;
//   - plain single-word missing commands and succeeding paths with spaces do
//     not print the opt-in hint.
func TestGenericHarness_ExplicitShellOptInAndExecForm(t *testing.T) {
	declShellOptIn := harness.NewDeclarativeGenericHarness(config.HarnessConfigEntry{
		Harness: "generic",
		Command: &config.HarnessCommandConfig{Base: []string{"sh", "-c"}},
	})
	declExecDefault := harness.NewDeclarativeGenericHarness(config.HarnessConfigEntry{
		Harness: "generic",
	})

	t.Run("explicit shell opt-in executes shell-form tasks", func(t *testing.T) {
		optIns := []struct {
			name        string
			h           api.Harness
			commandArgs []string
		}{
			{"declarative command.base sh -c", declShellOptIn, nil},
			{"generic command_args sh -c", &harness.Generic{}, []string{"sh", "-c"}},
		}
		cases := []struct {
			name    string
			task    string
			wantRC  string
			wantOut string
		}{
			{"&& chaining", "true && echo ok", "0", "ok"},
			{"single quotes", `printf '%s\n' 'hello world'`, "0", "hello world"},
			{"double quotes and semicolon", `echo "a;b"; echo "c'd"`, "0", "a;b\nc'd"},
			{"non-zero exit code", "echo failing && exit 42", "42", "failing"},
			{"quoting cannot break outer wrapper", `echo 'safe'\''quote'; exit 7`, "7", "safe'quote"},
		}
		for _, opt := range optIns {
			t.Run(opt.name, func(t *testing.T) {
				for _, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						cmdLine, ok := harnessCmdLine(RunConfig{
							Harness:     opt.h,
							Task:        tc.task,
							CommandArgs: opt.commandArgs,
						}, harnessCmdLineOptions{})
						if !ok {
							t.Fatal("harnessCmdLine returned ok=false")
						}
						r := runWrapper(t, cmdLine, "")
						if r.rc != tc.wantRC {
							t.Errorf("exit code = %q, want %q (output=%q)", r.rc, tc.wantRC, r.output)
						}
						if !strings.Contains(r.output, tc.wantOut) {
							t.Errorf("output = %q, want substring %q", r.output, tc.wantOut)
						}
						if strings.Contains(r.output, genericExecForm127Hint) {
							t.Errorf("opt-in shell task must not emit exec-form hint: %q", r.output)
						}
					})
				}
			})
		}
	})

	t.Run("default exec form never shell-interprets and hints on exit 127", func(t *testing.T) {
		harnesses := []struct {
			name string
			h    api.Harness
		}{
			{"Generic", &harness.Generic{}},
			{"DeclarativeGenericHarness", declExecDefault},
		}
		for _, hh := range harnesses {
			t.Run(hh.name, func(t *testing.T) {
				sentinel := filepath.Join(t.TempDir(), "pwned")
				cases := []struct {
					name     string
					task     string
					wantHint bool
				}{
					{"shell chaining", "true && echo ok", true},
					{"command with args", "sleep infinity", true},
					{"hostile semicolon", "; touch " + sentinel, true},
					{"hostile command substitution", "$(touch " + sentinel + ")", true},
					{"single-word missing program has no hint", "nonexistent_scion_cmd_6386", false},
				}
				for _, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						cmdLine, ok := harnessCmdLine(RunConfig{
							Harness: hh.h,
							Task:    tc.task,
						}, harnessCmdLineOptions{})
						if !ok {
							t.Fatal("harnessCmdLine returned ok=false")
						}
						r := runWrapper(t, cmdLine, "")
						if r.rc != "127" {
							t.Errorf("exit code = %q, want 127 (output=%q)", r.rc, r.output)
						}
						if gotHint := strings.Contains(r.output, genericExecForm127Hint); gotHint != tc.wantHint {
							t.Errorf("hint present = %v, want %v (output=%q)", gotHint, tc.wantHint, r.output)
						}
						if _, err := os.Stat(sentinel); err == nil {
							t.Fatalf("exec-form task executed shell and created %s", sentinel)
						}
					})
				}
			})
		}
	})

	t.Run("exec form succeeding executable path with spaces emits no hint", func(t *testing.T) {
		dir := t.TempDir()
		script := filepath.Join(dir, "run script.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\necho ran-ok\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmdLine, ok := harnessCmdLine(RunConfig{
			Harness: &harness.Generic{},
			Task:    script,
		}, harnessCmdLineOptions{})
		if !ok {
			t.Fatal("harnessCmdLine returned ok=false")
		}
		r := runWrapper(t, cmdLine, "")
		if r.rc != "0" {
			t.Errorf("exit code = %q, want 0 (output=%q)", r.rc, r.output)
		}
		if !strings.Contains(r.output, "ran-ok") {
			t.Errorf("output = %q, want ran-ok", r.output)
		}
		if strings.Contains(r.output, genericExecForm127Hint) {
			t.Errorf("succeeding script must not emit exit-127 hint: %q", r.output)
		}
	})

	t.Run("exec form with command_args passes task as literal data", func(t *testing.T) {
		cmdLine, ok := harnessCmdLine(RunConfig{
			Harness:     &harness.Generic{},
			Task:        "literal && echo INJECTED",
			CommandArgs: []string{"printf", "%s\n"},
		}, harnessCmdLineOptions{})
		if !ok {
			t.Fatal("harnessCmdLine returned ok=false")
		}
		r := runWrapper(t, cmdLine, "")
		if r.rc != "0" {
			t.Errorf("exit code = %q, want 0 (output=%q)", r.rc, r.output)
		}
		if !strings.Contains(r.output, "literal && echo INJECTED") || strings.Contains(r.output, "\nINJECTED") {
			t.Errorf("output = %q, want literal task without shell execution", r.output)
		}
	})
}
