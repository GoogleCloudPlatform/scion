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

// appleGateMock writes a mock container CLI that records each subcommand
// in calls, prints version for --version, prints cid for run, and answers
// exec (the mask check) with statOut and exit code statRC. A statRC of -1
// makes exec hang in a child that keeps the output pipe open.
func appleGateMock(t *testing.T, version, statOut string, statRC int) (command, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	command = filepath.Join(dir, "mock-container")
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$1" in
--version) printf '%%s\n' %q ;;
run) echo cid ;;
exec) [ %d -lt 0 ] && sleep 5; printf '%%b\n' %q; exit %d ;;
esac
exit 0
`, calls, version, statRC, statOut, max(statRC, 0))
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return command, calls
}

// readAppleGateCalls returns the subcommands the mock recorded, one per call.
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
// failing the start otherwise.
func TestAppleContainerRuntime_Run_AgentsMaskGate(t *testing.T) {
	const (
		v011  = "container CLI version 0.11.0 (build: release, commit: abc1234)"
		v012  = "container CLI version 0.12.0 (build: release, commit: abc1234)"
		v160  = "container CLI version 1.6.0 (build: release, commit: abc1234)"
		vBad  = "container CLI version unknown"
		mount = "41\n40"
	)
	withAgents := sharedRepoWithScionDir(t)
	noAgents := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(noAgents, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, root, version, statOut string
		statRC                       int
		wantErr                      []string // substrings; nil means the start succeeds
		wantCalls                    []string
	}{
		{"below the floor refuses before run", withAgents, v011, mount, 0,
			[]string{"0.11.0", "older than 0.12.0", "nested mount ordering"}, []string{"--version"}},
		{"at the floor, mask mounted", withAgents, v012, mount, 0, nil, []string{"--version", "run", "exec"}},
		{"above the floor, mask mounted", withAgents, v160, mount, 0, nil, []string{"--version", "run", "exec"}},
		{"unparseable version, mask mounted", withAgents, vBad, mount, 0, nil, []string{"--version", "run", "exec"}},
		{"same device fails", withAgents, v160, "40\n40", 0,
			[]string{"mask is not mounted", "version 1.6.0"}, []string{"--version", "run", "exec", "kill", "rm"}},
		{"same device, unknown version", withAgents, vBad, "40\n40", 0,
			[]string{"mask is not mounted", "version unknown"}, []string{"--version", "run", "exec", "kill", "rm"}},
		{"stat failure fails", withAgents, v160, "stat: cannot stat", 1,
			[]string{"could not check that the mask is mounted"}, []string{"--version", "run", "exec", "kill", "rm"}},
		{"garbage output fails", withAgents, v160, "not a device", 0,
			[]string{"unexpected stat output"}, []string{"--version", "run", "exec", "kill", "rm"}},
		{"mask check hangs fails", withAgents, v160, mount, -1,
			[]string{"could not check that the mask is mounted"}, []string{"--version", "run", "exec", "kill", "rm"}},
		{"one value fails", withAgents, v160, "41", 0,
			[]string{"unexpected stat output"}, []string{"--version", "run", "exec", "kill", "rm"}},
		{"no mask: no version or exec call", noAgents, v011, "40\n40", 0, nil, []string{"run"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if tt.statRC < 0 {
				prev := appleMaskCheckTimeout
				appleMaskCheckTimeout = 50 * time.Millisecond
				t.Cleanup(func() { appleMaskCheckTimeout = prev })
			}
			command, calls := appleGateMock(t, tt.version, tt.statOut, tt.statRC)
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
			if got := readAppleGateCalls(t, calls); !slices.Equal(got, tt.wantCalls) {
				t.Errorf("CLI calls = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}

// The run args carry the --tmpfs mask, not a --mount value, and the mask
// check stats the agents dir and its parent in the started container.
func TestAppleContainerRuntime_Run_AgentsMaskArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	command, calls := appleGateMock(t, "container CLI version 1.6.0", "41\n40", 0)
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
	if !strings.Contains(log, "exec --user scion cid stat -c %d /workspace/.scion/agents /workspace/.scion") {
		t.Errorf("mask check call missing: %s", log)
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
