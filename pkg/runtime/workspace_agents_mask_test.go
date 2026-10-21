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
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// stubAppleMaskVersionCheck replaces the once and the version check for
// the test and returns a pointer to the number of checks run.
func stubAppleMaskVersionCheck(t *testing.T) *int {
	t.Helper()
	prevOnce, prevCheck := appleMaskVersionOnce, appleMaskVersionCheck
	calls := 0
	appleMaskVersionOnce = &sync.Once{}
	appleMaskVersionCheck = func(context.Context, string) bool { calls++; return false }
	t.Cleanup(func() { appleMaskVersionOnce, appleMaskVersionCheck = prevOnce, prevCheck })
	return &calls
}

// The Apple runtime masks the in-project agents root with --tmpfs only
// when that directory already exists, and checks the CLI version once per
// process when it does.
func TestAppleContainerRuntime_Run_SharedWorkspaceAgentsMask(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mockContainer := filepath.Join(t.TempDir(), "mock-container")
	if err := os.WriteFile(mockContainer, []byte("#!/bin/sh\necho \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rt := &AppleContainerRuntime{Command: mockContainer}
	run := func(t *testing.T, root string) string {
		t.Helper()
		out, err := rt.Run(context.Background(), RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			RepoRoot:     root,
			Workspace:    root,
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return out
	}

	t.Run("agents dir exists", func(t *testing.T) {
		calls := stubAppleMaskVersionCheck(t)
		root := sharedRepoWithScionDir(t)
		for i := 0; i < 2; i++ {
			out := run(t, root)
			if !strings.Contains(out, "--tmpfs /workspace/.scion/agents") {
				t.Errorf("Apple run args have no --tmpfs mask of /workspace/.scion/agents: %q", out)
			}
			if strings.Contains(out, "type=tmpfs") {
				t.Errorf("Apple run args keep a --mount tmpfs value: %q", out)
			}
		}
		if *calls != 1 {
			t.Errorf("version check ran %d times across two Runs, want 1", *calls)
		}
	})

	t.Run("no agents dir", func(t *testing.T) {
		calls := stubAppleMaskVersionCheck(t)
		root := filepath.Join(t.TempDir(), "repo")
		if err := os.MkdirAll(filepath.Join(root, ".scion"), 0o755); err != nil {
			t.Fatal(err)
		}
		out := run(t, root)
		if strings.Contains(out, "--tmpfs") || strings.Contains(out, "type=tmpfs") {
			t.Errorf("Apple run args mask a missing agents dir: %q", out)
		}
		if *calls != 0 {
			t.Errorf("version check ran %d times with no mask, want 0", *calls)
		}
	})
}

func TestWarnAppleMaskVersion(t *testing.T) {
	for _, tt := range []struct {
		name, output     string
		fail, warn, hang bool
	}{
		{name: "below the floor", output: "container CLI version 0.11.0 (build: release, commit: abc1234)", warn: true},
		{name: "dev build below the floor", output: "container CLI version 0.10.0-27-g9fd15f0 (build: debug, commit: 9fd15f0)", warn: true},
		{name: "at the floor", output: "container CLI version 0.12.0 (build: release, commit: abc1234)"},
		{name: "above the floor", output: "container CLI version 1.6.0 (build: release, commit: abc1234)"},
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
				// exec, so the timeout kills the process holding the pipe.
				script = "#!/bin/sh\nexec sleep 5\n"
				prev := appleVersionTimeout
				appleVersionTimeout = 50 * time.Millisecond
				t.Cleanup(func() { appleVersionTimeout = prev })
			}
			start := time.Now()
			defer func() {
				if d := time.Since(start); d > 2*time.Second {
					t.Errorf("version check took %v, want it bounded by the timeout", d)
				}
			}()
			mock := filepath.Join(t.TempDir(), "mock-container")
			if err := os.WriteFile(mock, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			if got := warnAppleMaskVersion(context.Background(), mock); got != tt.warn {
				t.Errorf("warnAppleMaskVersion = %v, want %v", got, tt.warn)
			}
			var warns int
			for _, r := range h.records() {
				if r.level == slog.LevelWarn {
					warns++
					if !strings.Contains(r.msg, "0.12.0") {
						t.Errorf("warning %q does not name the 0.12.0 floor", r.msg)
					}
				}
			}
			if want := map[bool]int{true: 1}[tt.warn]; warns != want {
				t.Errorf("%d warnings, want %d", warns, want)
			}
			if !tt.warn {
				if _, ok := h.findRecord("Could not read the container CLI version"); ok != (tt.fail || tt.hang || tt.name == "unparseable") {
					t.Errorf("debug record present = %v", ok)
				}
			}
		})
	}
}
