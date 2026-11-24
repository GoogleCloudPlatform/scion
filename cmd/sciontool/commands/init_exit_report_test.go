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

package commands

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// Process-exit failure reporting (ptone/scion#7938): the immediate-exit
// branch, the pre-start failure and the crash reason's pane tail.
//
// Isolation: every RunInit here runs embedded (DisableReExec), with HOME,
// agent home, the exit-code, pane-tail and limits paths in t.TempDir(),
// Hub env scrubbed by setupRunInitAsRootlessScion (testutil.ScrubHubEnv)
// and, where a Hub is configured, pointed at an in-process httptest server.
// The package TestMain points the token home and the readiness files at a
// sandbox. No real scion, sciontool or tmux binary runs.

func TestLastNonEmptyLines(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"empty", "", ""},
		{"blank lines only", "\n  \n\t\n", ""},
		{"fewer than five", "a\n\nb\n", "a | b"},
		{"keeps the last five", "1\n2\n3\n\n4\n5\n6\n7\n\n\n", "3 | 4 | 5 | 6 | 7"},
		{"trims each line", "  x  \r\n y\n", "x | y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastNonEmptyLines(tc.text, 5); got != tc.want {
				t.Errorf("lastNonEmptyLines() = %q, want %q", got, tc.want)
			}
		})
	}
}

// withHarnessExitFiles points the exit-code and pane-tail paths at fresh
// temp files for the test and returns them.
func withHarnessExitFiles(t *testing.T) (exitCodeFile, tailFile string) {
	t.Helper()
	dir := t.TempDir()
	origCode, origTail := harnessExitCodePath, harnessTailPath
	harnessExitCodePath = filepath.Join(dir, "scion-harness-exit-code")
	harnessTailPath = filepath.Join(dir, "scion-harness-tail")
	t.Cleanup(func() { harnessExitCodePath, harnessTailPath = origCode, origTail })
	return harnessExitCodePath, harnessTailPath
}

func TestReadHarnessTail(t *testing.T) {
	_, tailFile := withHarnessExitFiles(t)
	if got := readHarnessTail(); got != "" {
		t.Errorf("missing file: readHarnessTail() = %q, want empty", got)
	}

	if err := os.WriteFile(tailFile, []byte("$ claude\n\nl1\nl2\nl3\nl4\nError: quota exhausted\n\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := readHarnessTail(), "l1 | l2 | l3 | l4 | Error: quota exhausted"; got != want {
		t.Errorf("readHarnessTail() = %q, want %q", got, want)
	}

	// Too large: not read at all.
	if err := os.WriteFile(tailFile, []byte(strings.Repeat("x", harnessTailMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readHarnessTail(); got != "" {
		t.Errorf("oversized file: readHarnessTail() = %d bytes, want empty", len(got))
	}

	// A symlink planted by the workload is refused, its target unread.
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte("not the pane\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(tailFile)
	if err := os.Symlink(target, tailFile); err != nil {
		t.Fatal(err)
	}
	if got := readHarnessTail(); got != "" {
		t.Errorf("symlinked tail file: readHarnessTail() = %q, want empty", got)
	}
}

func TestReadHarnessTail_ShellHarnessReturnsEmpty(t *testing.T) {
	_, tailFile := withHarnessExitFiles(t)
	if err := os.WriteFile(tailFile, []byte("gh auth token\nghp_secret_value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Broker-written ReadyStrategyIdle suppresses tail even if SCION_HARNESS=claude.
	t.Setenv(state.HarnessReadyEnv, state.FormatReadySpecEnv(state.DefaultReadySpec(state.ReadyStrategyIdle)))
	t.Setenv("SCION_HARNESS", "claude")
	if got := readHarnessTail(); got != "" {
		t.Errorf("idle ready spec: readHarnessTail() = %q, want empty", got)
	}

	// Broker-written HarnessDeliveryDisabled suppresses tail even if SCION_HARNESS=claude.
	t.Setenv(state.HarnessReadyEnv, "")
	t.Setenv(state.HarnessDeliveryDisabledEnv, "1")
	if got := readHarnessTail(); got != "" {
		t.Errorf("delivery disabled: readHarnessTail() = %q, want empty", got)
	}

	t.Setenv(state.HarnessDeliveryDisabledEnv, "")
	t.Setenv("SCION_HARNESS", "claude")
	if got, want := readHarnessTail(), "gh auth token | ghp_secret_value"; got != want {
		t.Errorf("SCION_HARNESS=claude: readHarnessTail() = %q, want %q", got, want)
	}
}

func TestClassifyExit_CrashReasonCarriesRedactedTail(t *testing.T) {
	code := 1
	t.Run("tail appended", func(t *testing.T) {
		got := classifyExit(0, nil, &code, false, false, "loading | Error: invalid flag --frob")
		if want := "Agent crashed with exit code 1: loading | Error: invalid flag --frob"; got.message != want {
			t.Errorf("message = %q, want %q", got.message, want)
		}
		if !got.isCrash || got.exitCode != 1 {
			t.Errorf("outcome = %+v, want a crash with code 1", got)
		}
	})
	t.Run("credentials redacted and escapes stripped", func(t *testing.T) {
		got := classifyExit(0, nil, &code, false, false, "\x1b[31mAuthorization: Bearer sk-secret-token-value\x1b[0m | ANTHROPIC_API_KEY=sk-ant-abc123")
		if strings.Contains(got.message, "sk-secret-token-value") || strings.Contains(got.message, "sk-ant-abc123") {
			t.Errorf("message leaks a credential: %q", got.message)
		}
		if strings.Contains(got.message, "\x1b") {
			t.Errorf("message keeps a terminal escape: %q", got.message)
		}
		if !strings.HasPrefix(got.message, "Agent crashed with exit code 1: ") || !strings.Contains(got.message, "[REDACTED]") {
			t.Errorf("message = %q, want the crash prefix and a redaction marker", got.message)
		}
	})
	t.Run("long lines drop the oldest output, never the last line", func(t *testing.T) {
		// Five wide pane lines, as a traceback or a TUI frame gives:
		// the error is on the last one.
		var lines []string
		for i := 0; i < 4; i++ {
			lines = append(lines, "frame "+strings.Repeat("é", 120))
		}
		lines = append(lines, "ValueError: model returned 403 PERMISSION_DENIED")
		got := classifyExit(0, nil, &code, false, false, strings.Join(lines, " | "))
		if n := utf8.RuneCountInString(got.message); n > 300 {
			t.Errorf("message is %d runes, want at most 300", n)
		}
		if !strings.HasPrefix(got.message, "Agent crashed with exit code 1: …") {
			t.Errorf("message = %q, want the full prefix then a truncation marker", got.message)
		}
		if !strings.HasSuffix(got.message, "ValueError: model returned 403 PERMISSION_DENIED") {
			t.Errorf("message = %q, want it to end with the last pane line", got.message)
		}
	})
	t.Run("one overlong last line keeps its end", func(t *testing.T) {
		got := classifyExit(0, nil, &code, false, false, strings.Repeat("x ", 400)+"FATAL")
		if n := utf8.RuneCountInString(got.message); n > 300 {
			t.Errorf("message is %d runes, want at most 300", n)
		}
		if !strings.HasPrefix(got.message, "Agent crashed with exit code 1: …") || !strings.HasSuffix(got.message, "FATAL") {
			t.Errorf("message = %q, want the prefix, a marker and the line's end", got.message)
		}
	})
	t.Run("no tail keeps the plain reason", func(t *testing.T) {
		if got := classifyExit(0, nil, &code, false, false, " "); got.message != "Agent crashed with exit code 1" {
			t.Errorf("message = %q", got.message)
		}
	})
	t.Run("a tail without a harness code is not used", func(t *testing.T) {
		// The tail belongs to the harness run that recorded a code; a
		// crash classified from the child's own code has no such run.
		if got := classifyExit(2, nil, nil, false, false, "stale"); got.message != "Agent crashed with exit code 2" {
			t.Errorf("message = %q", got.message)
		}
	})
	t.Run("a clean exit ignores the tail", func(t *testing.T) {
		zero := 0
		if got := classifyExit(0, nil, &zero, false, false, "bye"); got.isCrash || got.message != "" {
			t.Errorf("outcome = %+v, want a clean stop", got)
		}
	})
}

func TestClassifyImmediateExit(t *testing.T) {
	code := func(n int) *int { return &n }
	cases := []struct {
		name        string
		childCode   int
		harnessCode *int
		wrapped     bool
		tail        string
		wantCrash   bool
		wantCode    int
		wantMessage string
	}{
		{
			name: "tmux exits 0 after the harness failed", childCode: 0, harnessCode: code(127), wrapped: true,
			tail:      "sh: 1: claude: not found",
			wantCrash: true, wantCode: 127, wantMessage: "Agent crashed with exit code 127: sh: 1: claude: not found",
		},
		{
			name: "no exit-code file from the wrapper, tmux exit 0", childCode: 0, wrapped: true,
			wantCrash: true, wantCode: 1, wantMessage: "Agent failed to start (exit code 1): " + errHarnessNotStarted.Error(),
		},
		{
			name: "no exit-code file from the wrapper, tmux exit 1", childCode: 1, wrapped: true,
			wantCrash: true, wantCode: 1, wantMessage: "Agent failed to start (exit code 1): " + errHarnessNotStarted.Error(),
		},
		{
			name: "plain child exits non-zero", childCode: 4,
			wantCrash: true, wantCode: 4, wantMessage: "Agent crashed with exit code 4",
		},
		{name: "plain child exits 0", childCode: 0},
		{name: "harness exits 0", childCode: 0, harnessCode: code(0), wrapped: true, tail: "done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, crashed := classifyImmediateExit(tc.childCode, tc.harnessCode, tc.wrapped, tc.tail, false)
			if crashed != tc.wantCrash {
				t.Fatalf("crashed = %v, want %v (outcome %+v)", crashed, tc.wantCrash, got)
			}
			if !crashed {
				return
			}
			if got.exitCode != tc.wantCode || got.message != tc.wantMessage {
				t.Errorf("outcome = (code %d, %q), want (code %d, %q)", got.exitCode, got.message, tc.wantCode, tc.wantMessage)
			}
		})
	}
}

func TestChildRecordsHarnessExitCode(t *testing.T) {
	withHarnessExitFiles(t)
	noEnv := func(string) string { return "" }
	wrapper := "tmux new-session -d -s scion -n agent sh -c 'claude; scion_hrc=$?; echo $scion_hrc > " + harnessExitCodePath + "'"
	if !childRecordsHarnessExitCode([]string{"sh", "-c", wrapper}, noEnv) {
		t.Error("the tmux wrapper in the child's arguments was not recognized")
	}
	fromEnv := func(k string) string {
		if k == "SCION_START_CMD" {
			return wrapper
		}
		return ""
	}
	if !childRecordsHarnessExitCode([]string{"sh", "-c", `mount-fuse && exec sh -c "$SCION_START_CMD"`}, fromEnv) {
		t.Error("the tmux wrapper in SCION_START_CMD was not recognized")
	}
	if childRecordsHarnessExitCode([]string{"sleep", "infinity"}, noEnv) {
		t.Error("a plain command was taken for the harness wrapper")
	}
}

// exitReportHub is an in-process Hub recording the agent status reports
// RunInit sends.
type exitReportHub struct {
	mu      sync.Mutex
	reports []exitStatusReport
}

type exitStatusReport struct {
	Phase    string `json:"phase"`
	Activity string `json:"activity"`
	Message  string `json:"message"`
	ExitCode *int   `json:"exitCode"`
}

// newExitReportHub starts the Hub and points RunInit's Hub client at it.
// Call it after setupRunInitAsRootlessScion, which scrubs the Hub env.
func newExitReportHub(t *testing.T) *exitReportHub {
	t.Helper()
	h := &exitReportHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/api/v1/agents/exit-test-agent/status" {
			var rep exitStatusReport
			if err := json.Unmarshal(body, &rep); err == nil {
				h.mu.Lock()
				h.reports = append(h.reports, rep)
				h.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_AUTH_TOKEN", "exit-test-token")
	t.Setenv("SCION_AGENT_ID", "exit-test-agent")
	return h
}

// errorReport returns the single phase=error report, failing the test if
// there is not exactly one.
func (h *exitReportHub) errorReport(t *testing.T) exitStatusReport {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var errs []exitStatusReport
	for _, r := range h.reports {
		if r.Phase == string(state.PhaseError) {
			errs = append(errs, r)
		}
	}
	if len(errs) != 1 {
		t.Fatalf("Hub received %d phase=error report(s), want 1; all reports: %+v", len(errs), h.reports)
	}
	return errs[0]
}

func (h *exitReportHub) errorReports() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.reports {
		if r.Phase == string(state.PhaseError) {
			n++
		}
	}
	return n
}

// localAgentInfo reads the phase and message RunInit left in agent-info.json.
func localAgentInfo(t *testing.T, agentHome string) (phase, message string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
	if err != nil {
		t.Fatalf("read agent-info.json: %v", err)
	}
	var info struct {
		Phase  string `json:"phase"`
		Detail struct {
			Message string `json:"message"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("unmarshal agent-info.json %q: %v", raw, err)
	}
	return info.Phase, info.Detail.Message
}

// exitRunInitFixture sets up an embedded RunInit with an in-process Hub and
// the harness exit files in a temp directory.
type exitRunInitFixture struct {
	agentHome, exitCodeFile, tailFile string
	hub                               *exitReportHub
}

func newExitRunInitFixture(t *testing.T) exitRunInitFixture {
	t.Helper()
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	t.Setenv("SCION_HOOKS_DIR", t.TempDir())
	withRunGitCloneWorkspace(t, func(uid, gid int, home string, requirePrivilegeDrop bool) error { return nil })
	origTrigger := limitsTriggerPath
	limitsTriggerPath = filepath.Join(t.TempDir(), "limits-exceeded")
	t.Cleanup(func() { limitsTriggerPath = origTrigger })
	codeFile, tailFile := withHarnessExitFiles(t)
	return exitRunInitFixture{agentHome: agentHome, exitCodeFile: codeFile, tailFile: tailFile, hub: newExitReportHub(t)}
}

// run runs RunInit with child `sh -c script sh <exit-code file> <tail file>`.
// Naming the exit-code file in the child's arguments makes it the harness
// wrapper as far as RunInit can tell, as the runtimes' tmux command is.
func (f exitRunInitFixture) run(script string) int {
	return RunInit([]string{"sh", "-c", script, "sh", f.exitCodeFile, f.tailFile},
		InitRunOptions{DisableTermSignalForwarding: true, DisableReExec: true, DisablePortForwarding: true})
}

// Criterion 16: a harness that fails inside the 100 ms startup window while
// tmux itself exits 0 is reported to the Hub as phase=error with the
// harness's code and reason, and the container exits with that code.
func TestRunInit_ImmediateHarnessFailure_ReportsHarnessCode(t *testing.T) {
	f := newExitRunInitFixture(t)
	paths := withChildStartupWindow(t, 5*time.Second)

	got := f.run(`printf 'starting\nsh: 1: claude: not found\n' > "$2"; echo 127 > "$1"; exit 0`)
	if got != 127 {
		t.Fatalf("RunInit() = %d, want the harness code 127, not tmux's 0", got)
	}
	if want := []bool{true}; !reflect.DeepEqual(*paths, want) {
		t.Fatalf("startup paths = %v, want %v", *paths, want)
	}
	rep := f.hub.errorReport(t)
	if want := "Agent crashed with exit code 127: starting | sh: 1: claude: not found"; rep.Message != want {
		t.Errorf("Hub message = %q, want %q", rep.Message, want)
	}
	if rep.ExitCode == nil || *rep.ExitCode != 127 {
		t.Errorf("Hub exitCode = %v, want 127", rep.ExitCode)
	}
	if phase, msg := localAgentInfo(t, f.agentHome); phase != string(state.PhaseError) || msg != rep.Message {
		t.Errorf("agent-info.json = (%q, %q), want (error, %q)", phase, msg, rep.Message)
	}
}

// Criterion 16: the wrapper never ran (no exit-code file) and tmux exited
// 0: still phase=error and a non-zero container exit, never a clean stop.
func TestRunInit_ImmediateExitWithoutHarnessCode_ReportsNotStarted(t *testing.T) {
	f := newExitRunInitFixture(t)
	paths := withChildStartupWindow(t, 5*time.Second)

	got := f.run(`exit 0`)
	if got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []bool{true}; !reflect.DeepEqual(*paths, want) {
		t.Fatalf("startup paths = %v, want %v", *paths, want)
	}
	rep := f.hub.errorReport(t)
	if !strings.HasPrefix(rep.Message, "Agent failed to start (exit code 1)") {
		t.Errorf("Hub message = %q, want the harness-did-not-start reason", rep.Message)
	}
	if rep.ExitCode == nil || *rep.ExitCode != 1 {
		t.Errorf("Hub exitCode = %v, want 1", rep.ExitCode)
	}
}

// Stale exit-code and tail files from a previous run in the same
// container are removed before launch, so neither is taken for this run's.
func TestRunInit_StaleHarnessExitFilesIgnored(t *testing.T) {
	f := newExitRunInitFixture(t)
	paths := withChildStartupWindow(t, 5*time.Second)
	if err := os.WriteFile(f.exitCodeFile, []byte("9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.tailFile, []byte("old run\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// This run's harness records its code but leaves no pane tail.
	got := f.run(`echo 3 > "$1"; exit 0`)
	if got != 3 {
		t.Fatalf("RunInit() = %d, want this run's code 3", got)
	}
	if want := []bool{true}; !reflect.DeepEqual(*paths, want) {
		t.Fatalf("startup paths = %v, want %v", *paths, want)
	}
	if rep := f.hub.errorReport(t); rep.Message != "Agent crashed with exit code 3" {
		t.Errorf("Hub message = %q, want no tail from the previous run", rep.Message)
	}
}

// A plain child that is not the harness wrapper and exits 0 at once keeps
// today's clean outcome.
func TestRunInit_ImmediateCleanExitOfPlainChild_Unchanged(t *testing.T) {
	f := newExitRunInitFixture(t)
	paths := withChildStartupWindow(t, 5*time.Second)
	got := RunInit([]string{"true"}, InitRunOptions{DisableTermSignalForwarding: true, DisableReExec: true, DisablePortForwarding: true})
	if got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if want := []bool{true}; !reflect.DeepEqual(*paths, want) {
		t.Fatalf("startup paths = %v, want %v", *paths, want)
	}
	if n := f.hub.errorReports(); n != 0 {
		t.Errorf("Hub received %d phase=error report(s) for a clean exit", n)
	}
}

// Criterion 17: a crash after startup records the harness's last output.
func TestRunInit_CrashAfterStartup_ReasonCarriesPaneTail(t *testing.T) {
	f := newExitRunInitFixture(t)
	paths := withChildStartupWindow(t, 20*time.Millisecond)

	// Outlive the 20 ms startup window so RunInit takes the wait loop.
	got := f.run(`sleep 0.5; printf '> fix the bug\n\nTypeError: x is undefined\n    at main.js:1\n\n' > "$2"; echo 3 > "$1"; exit 0`)
	if got != 3 {
		t.Fatalf("RunInit() = %d, want the harness code 3", got)
	}
	if want := []bool{false}; !reflect.DeepEqual(*paths, want) {
		t.Fatalf("startup paths = %v, want %v", *paths, want)
	}
	rep := f.hub.errorReport(t)
	if want := "Agent crashed with exit code 3: > fix the bug | TypeError: x is undefined | at main.js:1"; rep.Message != want {
		t.Errorf("Hub message = %q, want %q", rep.Message, want)
	}
	if phase, msg := localAgentInfo(t, f.agentHome); phase != string(state.PhaseError) || msg != rep.Message {
		t.Errorf("agent-info.json = (%q, %q), want (error, %q)", phase, msg, rep.Message)
	}
}

// Criterion 16: a required pre-start failure reaches the Hub, not only the
// local agent-info.json, so runtimes whose broker cannot read the
// container's home also see phase=error with the reason.
func TestRunInit_PreStartFailure_ReportedToHub(t *testing.T) {
	f := newExitRunInitFixture(t)
	hook := resolveProjectHookPath(f.agentHome, false)
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'provisioner: no valid auth method' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(t.TempDir(), "child-ran")

	got := RunInit([]string{"sh", "-c", `touch "$1"`, "sh", ran},
		InitRunOptions{DisableTermSignalForwarding: true, DisableReExec: true, DisablePortForwarding: true})
	if got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
		t.Error("the harness child ran despite the required pre-start failure")
	}
	rep := f.hub.errorReport(t)
	if !strings.HasPrefix(rep.Message, "pre-start hook failed: ") {
		t.Errorf("Hub message = %q, want the pre-start failure", rep.Message)
	}
	if phase, msg := localAgentInfo(t, f.agentHome); phase != string(state.PhaseError) || msg != rep.Message {
		t.Errorf("agent-info.json = (%q, %q), want (error, %q)", phase, msg, rep.Message)
	}
}
