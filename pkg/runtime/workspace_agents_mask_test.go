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
// fail. The mock ignores backoffPastDeadline and shadowPath, which the
// test applies.
type appleGateOpts struct {
	version, statOut  string
	statRC, failFirst int
	hang, rmFail      bool
	// backoffPastDeadline sets a retry delay longer than the check's
	// timeout, so the deadline passes during the first backoff.
	backoffPastDeadline bool
	// shadowPath puts a stat and an env that record "shadow-stat" and
	// "shadow-env" in calls first on PATH. The mock runs the first word of
	// the exec command through PATH when it is not an absolute path, as the
	// container's PATH lookup would, so a check whose first word were plain
	// stat or plain env would record a shadow call. That is all the row
	// shows by behaviour: the mock never resolves the later words, so the
	// absolute /usr/bin/stat and /bin/cat that env runs, and the empty
	// environment (env -i), are pinned by the exact-argv test in
	// TestAppleContainerRuntime_Run_AgentsMaskArgs only.
	shadowPath bool
	// mountinfo is what the mock prints for the read-only check (cat of
	// /proc/self/mountinfo); empty means defaultAppleMountinfo.
	mountinfo string
}

// defaultAppleMountinfo has /workspace/.scion mounted read-only over a
// read-write /workspace.
const defaultAppleMountinfo = "100 90 0:50 / /workspace rw,relatime - virtiofs share rw\n101 100 0:50 /.scion /workspace/.scion ro,relatime - virtiofs share rw"

// appleGateMock writes a mock container CLI that records each subcommand
// in calls, prints cid for run and answers as opts says. For exec, the
// first word of the command given to exec, when not an absolute path, is
// first run through PATH, without its arguments.
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
	mountinfo := o.mountinfo
	if mountinfo == "" {
		mountinfo = defaultAppleMountinfo
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
	if [ "$5" = /bin/cat ]; then printf '%%b\n' %q; exit 0; fi
	printf '%%b\n' %q; exit %d ;;
rm) exit %d ;;
esac
exit 0
`, calls, o.version, hang, calls, o.failFirst, mountinfo, o.statOut, o.statRC, rmRC)
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
	noScion := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(noScion, 0o755); err != nil {
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
		{"at the floor, mask mounted", withAgents, appleGateOpts{version: v012, statOut: mount}, nil, calls("--version", "run", "exec", "exec")},
		{"above the floor, mask mounted", withAgents, appleGateOpts{version: v160, statOut: mount}, nil, calls("--version", "run", "exec", "exec")},
		{"unparseable version, mask mounted", withAgents, appleGateOpts{version: vBad, statOut: mount}, nil, calls("--version", "run", "exec", "exec")},
		{"exec fails once, then mask mounted", withAgents, appleGateOpts{version: v160, statOut: mount, failFirst: 1}, nil,
			calls("--version", "run", "exec", "exec", "exec")},
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
		{"a stat or env earlier on PATH is never invoked", withAgents, appleGateOpts{version: v160, statOut: mount, shadowPath: true}, nil,
			calls("--version", "run", "exec", "exec")},
		// A read-only .scion bind alone (no agents dir, so no tmpfs) still
		// gets the version floor and the read-only check.
		{"read-only bind only, below the floor", noAgents, appleGateOpts{version: v011, statOut: "40\n40"},
			[]string{"older than 0.12.0", "/workspace/.scion"}, calls("--version")},
		{"read-only bind only, verified", noAgents, appleGateOpts{version: v160, statOut: "40\n40"}, nil, calls("--version", "run", "exec")},
		{"read-only bind not read-only fails", noAgents, appleGateOpts{version: v160, statOut: "40\n40", mountinfo: "101 100 0:50 /.scion /workspace/.scion rw,relatime - virtiofs share rw"},
			[]string{"/workspace/.scion is not mounted read-only"}, append(calls("--version", "run", "exec"), removed...)},
		{"read-only bind missing from the mount table fails", noAgents, appleGateOpts{version: v160, statOut: "40\n40", mountinfo: "100 90 0:50 / /workspace rw - virtiofs share rw"},
			[]string{"no mount table entry"}, append(calls("--version", "run", "exec"), removed...)},
		{"read-only bind shadowed by a later workspace mount fails", noAgents, appleGateOpts{version: v160, statOut: "40\n40",
			mountinfo: "101 100 0:50 /.scion /workspace/.scion ro - virtiofs share rw\n102 90 0:50 / /workspace rw - virtiofs share rw"},
			[]string{"/workspace/.scion is not mounted read-only"}, append(calls("--version", "run", "exec"), removed...)},
		{"mask and read-only bind: a later rw entry wins", withAgents, appleGateOpts{version: v160, statOut: mount, mountinfo: defaultAppleMountinfo + "\n102 101 0:51 / /workspace/.scion rw - tmpfs tmpfs rw"},
			[]string{"is not mounted read-only"}, append(calls("--version", "run", "exec", "exec"), removed...)},
		{"no .scion: no version or exec call", noScion, appleGateOpts{version: v011, statOut: "40\n40"}, nil, calls("run")},
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
			if tt.opts.shadowPath {
				shadow := t.TempDir()
				for _, name := range []string{"stat", "env"} {
					script := fmt.Sprintf("#!/bin/sh\necho shadow-%s >> %q\n", name, callsFile)
					if err := os.WriteFile(filepath.Join(shadow, name), []byte(script), 0o755); err != nil {
						t.Fatal(err)
					}
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
	// The project configuration directory is mounted read-only under the
	// agents mask; the agent home is not.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log, "-v "+filepath.Join(resolvedRoot, ".scion")+":/workspace/.scion:ro") {
		t.Errorf("run args lack the read-only .scion mount: %s", log)
	}
	if strings.Count(log, ":ro") != 1 {
		t.Errorf("want exactly one read-only mount: %s", log)
	}
	var execs [][]string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "exec" {
			execs = append(execs, f)
		}
	}
	want := [][]string{
		{"exec", "cid", "/usr/bin/env", "-i", "/usr/bin/stat", "-c", "%d", "/workspace/.scion/agents", "/workspace/.scion"},
		{"exec", "cid", "/usr/bin/env", "-i", "/bin/cat", "/proc/self/mountinfo"},
	}
	if len(execs) != 2 || !slices.Equal(execs[0], want[0]) || !slices.Equal(execs[1], want[1]) {
		t.Errorf("check argv = %q, want exactly %q", execs, want)
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

// An unregistered workspace outside the repo root is refused on the Apple runtime
// before any mount or run: the start fails fail-closed and no container is created
// (the removed full-repo-root fallback no longer masks /repo-root/.scion).
func TestAppleContainerRuntime_Run_FallbackScionMask(t *testing.T) {
	// #5865 removed the full-repo-root fallback: an unregistered workspace outside
	// the repo root is refused, so the Apple run never reaches a /repo-root/.scion
	// mask. (A registered outside-root worktree is covered by the
	// registered-outside-worktree project-config test.)
	t.Setenv("HOME", t.TempDir())
	command, calls := appleGateMock(t, appleGateOpts{version: "container CLI version 1.6.0", statOut: "41\n40"})
	rt := &AppleContainerRuntime{Command: command}
	root := sharedRepoWithScionDir(t)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	before := fsSnapshot(t, root, ws)
	id, err := rt.Run(context.Background(), RunConfig{
		Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
		Image: "scion-agent:latest", RepoRoot: root, Workspace: ws,
	})
	// Refusal triple: typed error + message + fail-closed.
	if !errors.Is(err, errOutsideRootNotRegistered) {
		t.Fatalf("Run error = %v, want errOutsideRootNotRegistered", err)
	}
	if !strings.Contains(err.Error(), "is not a registered worktree") {
		t.Errorf("error message lacks the registered-worktree reason: %v", err)
	}
	if id != "" {
		t.Errorf("fail-closed: expected empty container id, got %q", id)
	}
	// No container was created and nothing was written to the checkout.
	if data, rerr := os.ReadFile(calls); rerr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "run ") || line == "run" {
				t.Errorf("refused start still issued a container run: %q", line)
			}
		}
	}
	assertNothingCreated(t, before, root, ws)
}

func TestMountinfoReadOnly(t *testing.T) {
	const info = "100 90 0:50 / /workspace rw,relatime - virtiofs share rw\n" +
		"101 100 0:50 /.scion /workspace/.scion ro,relatime - virtiofs share rw\n" +
		"102 90 0:51 / /workspace/.scionx rw - tmpfs tmpfs rw\n"
	for _, tt := range []struct {
		target          string
		readOnly, found bool
	}{
		{"/workspace/.scion", true, true},
		{"/workspace", false, true},
		{"/workspace/.scionx", false, true},
		{"/workspace/missing", false, false},
	} {
		ro, found := mountinfoReadOnly(info, tt.target)
		if ro != tt.readOnly || found != tt.found {
			t.Errorf("%s: (ro=%v, found=%v), want (%v, %v)", tt.target, ro, found, tt.readOnly, tt.found)
		}
	}
	// A read-only child listed before the mount that covers it (a parent
	// mounted later) is shadowed: not read-only. A mount below the target
	// does not cover it.
	shadowed := "101 100 0:50 /.scion /workspace/.scion ro,relatime - virtiofs share rw\n" +
		"102 90 0:50 / /workspace rw,relatime - virtiofs share rw\n"
	if ro, found := mountinfoReadOnly(shadowed, "/workspace/.scion"); ro || !found {
		t.Errorf("shadowed child: (ro=%v, found=%v), want (false, true)", ro, found)
	}
	below := "100 90 0:50 / /workspace rw - virtiofs share rw\n" +
		"101 100 0:50 /.scion /workspace/.scion ro - virtiofs share rw\n" +
		"102 101 0:51 / /workspace/.scion/agents rw - tmpfs tmpfs rw\n"
	if ro, _ := mountinfoReadOnly(below, "/workspace/.scion"); !ro {
		t.Error("a mount below the target must not cover it")
	}
	// Mount points are octal-escaped: a space is \040.
	spaced := "101 100 0:50 /x /repo-root/my\\040proj/.scion ro - virtiofs share rw\n"
	if ro, found := mountinfoReadOnly(spaced, "/repo-root/my proj/.scion"); !ro || !found {
		t.Errorf("escaped mount point: (ro=%v, found=%v)", ro, found)
	}
	// Options are matched as whole words: "rorelatime" is not "ro", and the
	// super options after "-" do not count.
	if ro, _ := mountinfoReadOnly("1 0 0:1 / /x rw,rorelatime - ext4 /dev/x ro", "/x"); ro {
		t.Error("ro taken from a partial option or the super options")
	}
	if got := appleNestedReadOnlyTargets([]string{"-v", "/h/.scion:/workspace/.scion:ro", "-v", "/h:/workspace", "-v", "/g:/home/scion/.config/gcloud:ro", "--tmpfs", "/x"}); !slices.Equal(got, []string{"/workspace/.scion"}) {
		t.Errorf("appleNestedReadOnlyTargets = %q", got)
	}
}

// The plain layout (no repo root) on Apple: the read-only .scion bind alone
// is version-gated and checked after the start.
func TestAppleContainerRuntime_Run_PlainReadOnlyBindGate(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(ws, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name      string
		opts      appleGateOpts
		wantErr   string
		wantCalls []string
	}{
		{"below the floor", appleGateOpts{version: "container CLI version 0.11.0", statOut: "x"}, "older than 0.12.0", []string{"--version"}},
		{"verified", appleGateOpts{version: "container CLI version 1.6.0", statOut: "x"}, "", []string{"--version", "run", "exec"}},
		{"writable fails", appleGateOpts{version: "container CLI version 1.6.0", statOut: "x", mountinfo: "101 100 0:50 /.scion /workspace/.scion rw - virtiofs share rw"},
			"is not mounted read-only", []string{"--version", "run", "exec", "kill", "rm"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			command, calls := appleGateMock(t, tt.opts)
			rt := &AppleContainerRuntime{Command: command}
			_, err := rt.Run(context.Background(), RunConfig{
				Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
				Image: "scion-agent:latest", Workspace: ws,
			})
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Run error = %v, want %q", err, tt.wantErr)
			}
			if got := readAppleGateCalls(t, calls); !slices.Equal(got, tt.wantCalls) {
				t.Errorf("CLI calls = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}
