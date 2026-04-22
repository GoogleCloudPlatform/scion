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
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"testing"
)

func TestExecAsUserCmd_Shape(t *testing.T) {
	user := "scion"
	cmd := "tmux has-session -t scion"
	got := ExecAsUserCmd(user, cmd)

	// The vector is [sh, -c, <script>, "exec-as-user", <user>, <cmd>].
	// Per `sh -c <script> <arg0> <arg1> ...`, arg0 becomes $0
	// (script name), arg1 becomes $1, arg2 becomes $2 — so user
	// is referenced inside the script as $1 and cmd as $2, not
	// interpolated into the script body.
	if len(got) != 6 {
		t.Fatalf("expected 6-element command vector, got %d: %v", len(got), got)
	}
	if got[0] != "sh" || got[1] != "-c" {
		t.Errorf("expected command to start with [sh -c], got: %v", got[:2])
	}
	if got[3] != "exec-as-user" {
		t.Errorf("expected got[3] to be the script-name label \"exec-as-user\", got %q", got[3])
	}
	if got[4] != user {
		t.Errorf("expected user at got[4] verbatim, got %q want %q", got[4], user)
	}
	if got[5] != cmd {
		t.Errorf("expected cmd at got[5] verbatim, got %q want %q", got[5], cmd)
	}

	// The script body itself is fixed and references $1/$2 — it
	// must not leak the user or cmd values into its text.
	script := got[2]
	if !strings.Contains(script, `[ "$(whoami)" = "$1" ]`) {
		t.Errorf("expected script to compare whoami against $1, got: %s", script)
	}
	if !strings.Contains(script, `exec sh -c "$2"`) {
		t.Errorf("expected whoami branch to exec sh -c \"$2\", got: %s", script)
	}
	if !strings.Contains(script, `exec su - "$1" -c "$2"`) {
		t.Errorf("expected else branch to invoke su - \"$1\" -c \"$2\", got: %s", script)
	}
	if strings.Contains(script, user) || strings.Contains(script, cmd) {
		t.Errorf("script body should not contain literal user or cmd values (use $1/$2), got: %s", script)
	}
}

func TestExecAsUserCmd_PreservesShellQuoting(t *testing.T) {
	// The tmux window-name query embeds single-quoted shell tokens
	// (`'#{window_name}'`) — they must survive the helper untouched
	// because cmd is passed verbatim as a positional shell argument
	// rather than interpolated through any quoting layer.
	cmd := `tmux display-message -t scion -p '#{window_name}'`
	got := ExecAsUserCmd("scion", cmd)
	if got[5] != cmd {
		t.Errorf("expected cmd to be passed verbatim as got[5], got %q want %q", got[5], cmd)
	}
}

func TestExecAsUserCmd_PassesUsernameVerbatim(t *testing.T) {
	// Usernames are passed as a positional shell argument ($1) and
	// referenced inside the script as "$1" (double-quoted), so any
	// character — including shell metacharacters — survives the
	// helper without word-splitting or expansion. Defense against
	// shell injection from untrusted metadata still belongs at the
	// caller (see runtimebroker.sanitizeExecUser); the helper just
	// guarantees it does not introduce a quoting hazard.
	weird := `weird"user;rm -rf /`
	got := ExecAsUserCmd(weird, "echo hi")
	if got[4] != weird {
		t.Errorf("expected user to be passed verbatim as got[4], got %q want %q", got[4], weird)
	}
	if strings.Contains(got[2], weird) {
		t.Errorf("script body should not contain the raw user value, got: %s", got[2])
	}
}

// TestExecAsUserCmd_RuntimeWhoamiBranch is an execution-based smoke
// test: it actually runs the wrapper through /bin/sh against the
// current process's username and asserts the whoami branch is taken
// (i.e., the cmd is exec'd directly without invoking su).
func TestExecAsUserCmd_RuntimeWhoamiBranch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX /bin/sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not found in PATH: %v", err)
	}

	u, err := user.Current()
	if err != nil {
		t.Skipf("could not determine current user: %v", err)
	}

	// Use a sentinel that will only be printed by the whoami branch.
	// The else branch would invoke `su -`, which (a) likely isn't
	// installed in CI environments and (b) would prompt for a
	// password. If the whoami branch is correctly taken, we should
	// see the sentinel on stdout.
	wrapped := ExecAsUserCmd(u.Username, "echo whoami-branch-ok")
	out, err := exec.Command(wrapped[0], wrapped[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v (output: %s)", err, out)
	}
	if !strings.Contains(string(out), "whoami-branch-ok") {
		t.Errorf("expected whoami branch to run echo, got output: %s", out)
	}
}

// TestExecAsUserCmd_EnvPrefixedCmd locks in support for cmds that
// begin with one or more "VAR=value" env-prefix assignments —
// valid POSIX shell syntax that the whoami branch must honor.
//
// Regression: an earlier draft of the helper used `exec %s` in
// the whoami branch, which fails because `exec` is a special
// builtin that does NOT parse leading env-prefix assignments.
// The current helper routes cmd through `exec sh -c "$1"` so
// the inner shell does the parsing.
//
// The two PTY attach call sites in pty_handlers.go pass
// "TERM=xterm-256color tmux attach-session -t scion", so this
// test prevents regressions there.
func TestExecAsUserCmd_EnvPrefixedCmd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX /bin/sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not found in PATH: %v", err)
	}
	u, err := user.Current()
	if err != nil {
		t.Skipf("could not determine current user: %v", err)
	}

	// Use `env` (which prints the env it actually receives) rather
	// than echoing $VAR — that directly tests whether the
	// env-prefix assignment was honored when the inner command was
	// invoked. Mirrors the real call sites' "TERM=xterm-256color
	// tmux ..." shape.
	wrapped := ExecAsUserCmd(u.Username, `MARKER=hello env`)
	out, err := exec.Command(wrapped[0], wrapped[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v (output: %s)", err, out)
	}
	if !strings.Contains(string(out), "MARKER=hello") {
		t.Errorf("expected env-prefixed cmd to set MARKER=hello in env, got: %s", out)
	}
}

// TestExecAsUserCmd_CallSiteShapes is a table-driven end-to-end test
// that exercises each cmd shape used by the production call sites in
// pkg/runtimebroker/pty_handlers.go and pkg/runtime/k8s_runtime.go.
//
// Real call sites invoke `tmux ...` which is not installed in CI;
// each test case substitutes a portable equivalent (`true`, `printf`,
// `env`) that preserves the relevant shell-shape feature being
// tested (env prefix, single-quoted token, plain command, etc.).
// This guards against future regressions in the helper's quoting
// logic that would only surface at one specific call site.
func TestExecAsUserCmd_CallSiteShapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX /bin/sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not found in PATH: %v", err)
	}
	u, err := user.Current()
	if err != nil {
		t.Skipf("could not determine current user: %v", err)
	}

	cases := []struct {
		name       string
		cmd        string
		wantStdout string // substring expected in CombinedOutput
		callSite   string // where this shape is used
	}{
		{
			name:       "tmux_has_session_shape",
			cmd:        `true && printf has-session-ok`,
			wantStdout: "has-session-ok",
			callSite:   "pty_handlers.go waitForTmuxSession (readiness probe)",
		},
		{
			name:       "tmux_attach_with_env_prefix_shape",
			cmd:        `TERM=xterm-256color env`,
			wantStdout: "TERM=xterm-256color",
			callSite:   "pty_handlers.go runK8sExec PTY attach paths",
		},
		{
			name:       "tmux_display_message_with_single_quotes_shape",
			cmd:        `printf '#{window_name}'`,
			wantStdout: "#{window_name}",
			callSite:   "pty_handlers.go queryTmuxActiveWindowK8s",
		},
		{
			name:       "k8s_exec_quoted_join_shape",
			cmd:        `'printf' 'hello world'`,
			wantStdout: "hello world",
			callSite:   "k8s_runtime.go Exec (joins shell-quoted argv)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := ExecAsUserCmd(u.Username, tc.cmd)
			out, err := exec.Command(wrapped[0], wrapped[1:]...).CombinedOutput()
			if err != nil {
				t.Fatalf("wrapper failed for %s shape (used at %s): %v\nwrapped: %v\noutput: %s",
					tc.name, tc.callSite, err, wrapped, out)
			}
			if !strings.Contains(string(out), tc.wantStdout) {
				t.Errorf("expected output to contain %q for %s shape, got: %s",
					tc.wantStdout, tc.name, out)
			}
		})
	}
}
