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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// sharedRepoWithScionDir returns a repo root that contains a .scion
// directory with an agent dir under .scion/agents.
func sharedRepoWithScionDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(root, ".scion", "agents", "other-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSharedWorkspaceAgentsMaskTarget(t *testing.T) {
	shared := sharedRepoWithScionDir(t)

	worktree := filepath.Join(shared, "worktrees", "a")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}

	noScion := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(noScion, 0o755); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(marker, ".scion"), []byte("project-id: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(shared, link); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(t.TempDir(), "missing")

	scionOnly := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(scionOnly, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		repoRoot  string
		workspace string
		container string
		want      string
	}{
		{"shared workspace", shared, shared, "/workspace", "/workspace/.scion/agents"},
		{".scion without agents dir", scionOnly, scionOnly, "/workspace", "/workspace/.scion/agents"},
		{"custom container workspace", shared, shared, "/mnt/ws", "/mnt/ws/.scion/agents"},
		{"symlinked repo root", link, shared, "/workspace", "/workspace/.scion/agents"},
		{"symlinked workspace", shared, link, "/workspace", "/workspace/.scion/agents"},
		{"worktree under repo root", shared, worktree, "/workspace", ""},
		{"no .scion", noScion, noScion, "/workspace", ""},
		{".scion marker file", marker, marker, "/workspace", ""},
		{"no repo root", "", shared, "/workspace", ""},
		{"no workspace", shared, "", "/workspace", ""},
		{"repo root does not resolve", missing, shared, "/workspace", ""},
		{"workspace does not resolve", shared, missing, "/workspace", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sharedWorkspaceAgentsMaskTarget(RunConfig{RepoRoot: tt.repoRoot, Workspace: tt.workspace}, tt.container, false)
			if got != tt.want || ok != (tt.want != "") {
				t.Errorf("sharedWorkspaceAgentsMaskTarget = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.want != "")
			}
			// requireAgentsDir only adds the agents dir check.
			want := tt.want
			if tt.repoRoot == scionOnly {
				want = ""
			}
			got, ok = sharedWorkspaceAgentsMaskTarget(RunConfig{RepoRoot: tt.repoRoot, Workspace: tt.workspace}, tt.container, true)
			if got != want || ok != (want != "") {
				t.Errorf("requireAgentsDir: sharedWorkspaceAgentsMaskTarget = (%q, %v), want (%q, %v)", got, ok, want, want != "")
			}
		})
	}
}

// A path that does not resolve means no mask, and says why in the log.
func TestSharedWorkspaceAgentsMaskTarget_UnresolvedPathLogged(t *testing.T) {
	shared := sharedRepoWithScionDir(t)
	missing := filepath.Join(t.TempDir(), "missing")
	for _, tt := range []struct {
		name, repoRoot, workspace, msg string
	}{
		{"repo root", missing, shared, "Not masking the in-project agents dir: repo root does not resolve"},
		{"workspace", shared, missing, "Not masking the in-project agents dir: workspace does not resolve"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newRecordingHandler(slog.LevelDebug)
			setRuntimeLogHandler(t, h)
			if _, ok := sharedWorkspaceAgentsMaskTarget(RunConfig{Name: "a", RepoRoot: tt.repoRoot, Workspace: tt.workspace}, "/workspace", false); ok {
				t.Fatal("expected no mask")
			}
			rec, found := h.findRecord(tt.msg)
			if !found {
				t.Fatalf("no %q record in %+v", tt.msg, h.records())
			}
			if rec.level != slog.LevelWarn {
				t.Errorf("level = %v, want WARN", rec.level)
			}
		})
	}
}

// --- Apple ---

func TestStripUnsupportedAppleFlags_KeepsTmpfs(t *testing.T) {
	got := stripUnsupportedAppleFlags([]string{"--tmpfs", "/workspace/.scion/agents", "--mount", "type=tmpfs,destination=/x", "-e", "A=B"})
	want := []string{"--tmpfs", "/workspace/.scion/agents", "-e", "A=B"}
	if !slices.Equal(got, want) {
		t.Errorf("stripUnsupportedAppleFlags = %q, want %q", got, want)
	}
}

// appleGateOpts describes the mock container CLI's answers: version for
// --version; for exec (the mask check), statOut with exit code statRC,
// after failFirst failing calls (exit 1, "exec failed"); hang makes every
// exec hang in a child that keeps the output pipe open; rmFail makes rm
// fail. The mock ignores backoffPastDeadline and shadowStat, which the
// test applies.
type appleGateOpts struct {
	version, statOut  string
	statRC, failFirst int
	hang, rmFail      bool
	// backoffPastDeadline sets a retry delay longer than the check's
	// timeout, so the deadline passes during the first backoff.
	backoffPastDeadline bool
	// shadowStat puts a stat that records "shadow-stat" in calls first on
	// PATH. PATH independence is tested by behaviour: the mock runs the
	// command given to exec through PATH when it is not an absolute path,
	// as the container's PATH lookup would, so a check that ran plain stat
	// would record shadow-stat. The empty environment (env -i) is pinned by
	// the argv only: no mock can show LD_* being cleared, so the exact-argv
	// test in TestAppleContainerRuntime_Run_AgentsMaskArgs is its proof.
	shadowStat bool
}

// appleGateMock writes a mock container CLI that records each subcommand
// in calls, prints cid for run and answers as opts says. For exec, the
// command given to exec, when not an absolute path, is first run through
// PATH.
func appleGateMock(t *testing.T, o appleGateOpts) (command, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	command = filepath.Join(dir, "mock-container")
	hang, rmRC := 0, 0
	if o.hang {
		hang = 1
	}
	if o.rmFail {
		rmRC = 1
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$1" in
--version) printf '%%s\n' %q ;;
run) echo cid ;;
exec)
	case "$3" in /*) ;; *) "$3" >/dev/null 2>&1 ;; esac
	[ %d -eq 1 ] && sleep 5
	n=$(grep -c '^exec' %q)
	if [ "$n" -le %d ]; then echo "exec failed"; exit 1; fi
	printf '%%b\n' %q; exit %d ;;
rm) exit %d ;;
esac
exit 0
`, calls, o.version, hang, calls, o.failFirst, o.statOut, o.statRC, rmRC)
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return command, calls
}

// readAppleGateCalls returns the subcommands the mock recorded, one per
// call.
func readAppleGateCalls(t *testing.T, calls string) []string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		out = append(out, strings.Fields(line)[0])
	}
	return out
}

// The Apple runtime masks the in-project agents root with --tmpfs only when
// that directory already exists. It then refuses a CLI older than 0.12.0
// before the run, and after the run checks that the mask is mounted
// (different device from /workspace/.scion), removing the container and
// failing the start otherwise. The check is retried only when it could not
// run; a result (equal devices, unparseable output) is never retried.
func TestAppleContainerRuntime_Run_AgentsMaskGate(t *testing.T) {
	const (
		v011  = "container CLI version 0.11.0 (build: release, commit: abc1234)"
		v012  = "container CLI version 0.12.0 (build: release, commit: abc1234)"
		v160  = "container CLI version 1.6.0 (build: release, commit: abc1234)"
		vBad  = "container CLI version unknown"
		mount = "41\n40"
	)
	prevDelay := appleMaskCheckRetryDelay
	appleMaskCheckRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { appleMaskCheckRetryDelay = prevDelay })
	withAgents := sharedRepoWithScionDir(t)
	noAgents := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(noAgents, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	removed := []string{"kill", "rm"}
	calls := func(c ...string) []string { return c }
	for _, tt := range []struct {
		name      string
		root      string
		opts      appleGateOpts
		wantErr   []string // substrings; nil means the start succeeds
		wantCalls []string
	}{
		{"below the floor refuses before run", withAgents, appleGateOpts{version: v011, statOut: mount},
			[]string{"0.11.0", "older than 0.12.0", "nested mount ordering", "0.12.0 or later"}, calls("--version")},
		{"at the floor, mask mounted", withAgents, appleGateOpts{version: v012, statOut: mount}, nil, calls("--version", "run", "exec")},
		{"above the floor, mask mounted", withAgents, appleGateOpts{version: v160, statOut: mount}, nil, calls("--version", "run", "exec")},
		{"unparseable version, mask mounted", withAgents, appleGateOpts{version: vBad, statOut: mount}, nil, calls("--version", "run", "exec")},
		{"exec fails once, then mask mounted", withAgents, appleGateOpts{version: v160, statOut: mount, failFirst: 1}, nil,
			calls("--version", "run", "exec", "exec")},
		{"exec fails every time", withAgents, appleGateOpts{version: v160, statOut: mount, failFirst: 99},
			[]string{"could not check that the mask is mounted", "exec failed", "version 1.6.0"},
			append(calls("--version", "run", "exec", "exec", "exec"), removed...)},
		{"stat exits non-zero every time", withAgents, appleGateOpts{version: v160, statOut: "stat: cannot stat", statRC: 1},
			[]string{"could not check that the mask is mounted", "cannot stat"},
			append(calls("--version", "run", "exec", "exec", "exec"), removed...)},
		{"same device fails at once", withAgents, appleGateOpts{version: v160, statOut: "40\n40"},
			[]string{"mask is not mounted", "version 1.6.0"}, append(calls("--version", "run", "exec"), removed...)},
		{"same device, unknown version", withAgents, appleGateOpts{version: vBad, statOut: "40\n40"},
			[]string{"mask is not mounted", "version unknown"}, append(calls("--version", "run", "exec"), removed...)},
		{"garbage output fails at once", withAgents, appleGateOpts{version: v160, statOut: "not a device"},
			[]string{"unexpected stat output"}, append(calls("--version", "run", "exec"), removed...)},
		{"two non-numeric values fail at once", withAgents, appleGateOpts{version: v160, statOut: "a\nb"},
			[]string{"unexpected stat output"}, append(calls("--version", "run", "exec"), removed...)},
		{"one value fails at once", withAgents, appleGateOpts{version: v160, statOut: "41"},
			[]string{"unexpected stat output"}, append(calls("--version", "run", "exec"), removed...)},
		{"deadline during backoff keeps the last attempt's output", withAgents, appleGateOpts{version: v160, statOut: mount, failFirst: 99, backoffPastDeadline: true},
			[]string{"could not check that the mask is mounted", "exec failed"}, append(calls("--version", "run", "exec"), removed...)},
		{"mask check hangs fails", withAgents, appleGateOpts{version: v160, statOut: mount, hang: true},
			[]string{"could not check that the mask is mounted"}, append(calls("--version", "run", "exec"), removed...)},
		{"removal failure is reported", withAgents, appleGateOpts{version: v160, statOut: "40\n40", rmFail: true},
			[]string{"mask is not mounted", "removing container cid", "the container may still exist"},
			calls("--version", "run", "exec", "kill", "rm", "rm", "rm", "rm", "rm")},
		{"a stat earlier on PATH is never invoked", withAgents, appleGateOpts{version: v160, statOut: mount, shadowStat: true}, nil,
			calls("--version", "run", "exec")},
		{"no mask: no version or exec call", noAgents, appleGateOpts{version: v011, statOut: "40\n40"}, nil, calls("run")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if tt.opts.hang {
				prev := appleMaskCheckTimeout
				appleMaskCheckTimeout = 500 * time.Millisecond
				t.Cleanup(func() { appleMaskCheckTimeout = prev })
			}
			if tt.opts.backoffPastDeadline {
				prevT, prevD := appleMaskCheckTimeout, appleMaskCheckRetryDelay
				appleMaskCheckTimeout, appleMaskCheckRetryDelay = 300*time.Millisecond, 5*time.Second
				t.Cleanup(func() { appleMaskCheckTimeout, appleMaskCheckRetryDelay = prevT, prevD })
			}
			command, callsFile := appleGateMock(t, tt.opts)
			if tt.opts.shadowStat {
				shadow := t.TempDir()
				script := fmt.Sprintf("#!/bin/sh\necho shadow-stat >> %q\n", callsFile)
				if err := os.WriteFile(filepath.Join(shadow, "stat"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", shadow+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			rt := &AppleContainerRuntime{Command: command}
			id, err := rt.Run(context.Background(), RunConfig{
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				RepoRoot:     tt.root,
				Workspace:    tt.root,
			})
			if tt.wantErr == nil {
				if err != nil || id != "cid" {
					t.Fatalf("Run = (%q, %v), want (cid, nil)", id, err)
				}
			} else {
				if err == nil {
					t.Fatalf("Run succeeded (%q), want an error", id)
				}
				for _, w := range tt.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not contain %q", err, w)
					}
				}
			}
			if got := readAppleGateCalls(t, callsFile); !slices.Equal(got, tt.wantCalls) {
				t.Errorf("CLI calls = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}

func TestTruncateCheckOutput(t *testing.T) {
	long := strings.Repeat("x", 300)
	if got := truncateCheckOutput("  " + long + "\n"); got != strings.Repeat("x", 200)+"..." {
		t.Errorf("truncateCheckOutput(300 bytes) = %d bytes %q", len(got), got[len(got)-5:])
	}
	if got := truncateCheckOutput(" short\n"); got != "short" {
		t.Errorf("truncateCheckOutput = %q, want short", got)
	}
}

// The run args carry the --tmpfs mask, not a --mount value, and the mask
// check stats the agents dir and its parent in the started container.
func TestAppleContainerRuntime_Run_AgentsMaskArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	command, calls := appleGateMock(t, appleGateOpts{version: "container CLI version 1.6.0", statOut: "41\n40"})
	rt := &AppleContainerRuntime{Command: command}
	root := sharedRepoWithScionDir(t)
	if _, err := rt.Run(context.Background(), RunConfig{
		Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
		Image: "scion-agent:latest", RepoRoot: root, Workspace: root,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if !strings.Contains(log, "--tmpfs /workspace/.scion/agents") || strings.Contains(log, "type=tmpfs") {
		t.Errorf("run args: %s", log)
	}
	var execs [][]string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "exec" {
			execs = append(execs, f)
		}
	}
	want := []string{"exec", "cid", "/usr/bin/env", "-i", "/usr/bin/stat", "-c", "%d", "/workspace/.scion/agents", "/workspace/.scion"}
	if len(execs) != 1 || !slices.Equal(execs[0], want) {
		t.Errorf("mask check argv = %q, want exactly %q", execs, want)
	}
}

func TestAppleCLIVersion(t *testing.T) {
	for _, tt := range []struct {
		name, output string
		fail, hang   bool
		version      string
		below, ok    bool
	}{
		{name: "below the floor", output: "container CLI version 0.11.0 (build: release, commit: abc1234)", version: "0.11.0", below: true, ok: true},
		{name: "dev build below the floor", output: "container CLI version 0.10.0-27-g9fd15f0 (build: debug, commit: 9fd15f0)", version: "0.10.0", below: true, ok: true},
		{name: "at the floor", output: "container CLI version 0.12.0 (build: release, commit: abc1234)", version: "0.12.0", ok: true},
		{name: "above the floor", output: "container CLI version 1.6.0 (build: release, commit: abc1234)", version: "1.6.0", ok: true},
		{name: "unparseable", output: "container CLI version unknown"},
		{name: "command fails", output: "container CLI version 0.1.0", fail: true},
		{name: "command hangs", output: "container CLI version 0.1.0", hang: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newRecordingHandler(slog.LevelDebug)
			setRuntimeLogHandler(t, h)
			script := "#!/bin/sh\necho '" + tt.output + "'\n"
			if tt.fail {
				script += "exit 1\n"
			}
			if tt.hang {
				// A child that keeps the output pipe open: WaitDelay must
				// still bound the call.
				script = "#!/bin/sh\nsleep 5\n"
				prev := appleVersionTimeout
				appleVersionTimeout = 50 * time.Millisecond
				t.Cleanup(func() { appleVersionTimeout = prev })
			}
			mock := filepath.Join(t.TempDir(), "mock-container")
			if err := os.WriteFile(mock, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			version, below, ok := appleCLIVersion(context.Background(), mock)
			if d := time.Since(start); d > 4*time.Second {
				t.Errorf("version check took %v, want it bounded", d)
			}
			if version != tt.version || below != tt.below || ok != tt.ok {
				t.Errorf("appleCLIVersion = (%q, %v, %v), want (%q, %v, %v)", version, below, ok, tt.version, tt.below, tt.ok)
			}
			if _, logged := h.findRecord("Could not read the container CLI version"); logged == tt.ok {
				t.Errorf("debug record present = %v, want %v", logged, !tt.ok)
			}
			for _, r := range h.records() {
				if r.level >= slog.LevelWarn {
					t.Errorf("unexpected %v record %q", r.level, r.msg)
				}
			}
		})
	}
}
