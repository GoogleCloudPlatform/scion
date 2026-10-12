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

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// keptTestClock sets keptAgentDirNow to a settable clock for the rest of
// the test and returns a pointer to its time.
func keptTestClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	orig := keptAgentDirNow
	keptAgentDirNow = func() time.Time { return now }
	t.Cleanup(func() { keptAgentDirNow = orig })
	return &now
}

// keptTestRetention sets the kept-directory retention for the rest of the
// test.
func keptTestRetention(t *testing.T, d time.Duration) {
	t.Helper()
	orig := keptAgentDirRetention
	keptAgentDirRetention = func() time.Duration { return d }
	t.Cleanup(func() { keptAgentDirRetention = orig })
}

// keptWorktreeWithWork builds staleWorktreeLayout with an untracked file in
// the workspace, so handleAgentDirWithoutConfig keeps the agent dir.
func keptWorktreeWithWork(t *testing.T) (base, repo, agentDir, agentHome, ws string) {
	t.Helper()
	base, repo, agentDir, agentHome, ws = staleWorktreeLayout(t)
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, repo, agentDir, agentHome, ws
}

// readMarkerT reads the marker in agentDir and fails the test if it is
// missing or invalid.
func readMarkerT(t *testing.T, agentDir string) keptAgentDirRecord {
	t.Helper()
	rec, ok := readKeptAgentDirRecord(agentDir)
	if !ok {
		t.Fatalf("expected a valid %s in %s", keptAgentDirMarker, agentDir)
	}
	return rec
}

// writeMarkerT writes a marker with keptAt and reason into agentDir.
func writeMarkerT(t *testing.T, agentDir string, keptAt time.Time) {
	t.Helper()
	data, err := json.Marshal(keptAgentDirRecord{KeptAt: keptAt, Reason: keptReasonWorkspaceMayHoldWork})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, keptAgentDirMarker), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// requireRemoved fails the test unless path does not exist.
func requireRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed (lstat err=%v)", path, err)
	}
}

// requireNoPurgeLeftovers fails the test if root holds a renamed directory
// from a removal.
func requireNoPurgeLeftovers(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), keptAgentDirPurgePrefix) {
			t.Fatalf("unexpected leftover %s in %s", e.Name(), root)
		}
	}
}

// Keeping a dir for its workspace writes the marker with the time and
// reason, and a later keep does not move keptAt.
func TestHandleAgentDirWithoutConfig_KeepRecordsMarkerOnce(t *testing.T) {
	keptTestRetention(t, 0)
	now := keptTestClock(t)
	base, _, agentDir, agentHome, ws := keptWorktreeWithWork(t)
	first := *now

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, ws)
	rec := readMarkerT(t, agentDir)
	if !rec.KeptAt.Equal(first) || rec.Reason != keptReasonWorkspaceMayHoldWork {
		t.Fatalf("marker = %+v, want keptAt %v and reason %q", rec, first, keptReasonWorkspaceMayHoldWork)
	}

	*now = now.Add(time.Hour)
	err = handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, ws)
	if rec := readMarkerT(t, agentDir); !rec.KeptAt.Equal(first) {
		t.Fatalf("keptAt = %v after a second keep, want the first %v", rec.KeptAt, first)
	}
}

// A workspace that cannot be checked is kept with the check-failed reason.
func TestHandleAgentDirWithoutConfig_UncheckableKeepRecordsReason(t *testing.T) {
	keptTestRetention(t, 0)
	keptTestClock(t)
	base, repo, agentDir, agentHome, ws := noConfigGitProject(t)
	broken := filepath.Join(repo, ".git", "worktrees", "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(ws, ".git")
	files := map[string]string{
		gitFile:                            "gitdir: " + broken,
		filepath.Join(broken, "commondir"): filepath.Join(repo, ".git"),
		filepath.Join(broken, "gitdir"):    gitFile,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, gitFile)
	if rec := readMarkerT(t, agentDir); rec.Reason != keptReasonWorkspaceCheckFailed {
		t.Fatalf("reason = %q, want %q", rec.Reason, keptReasonWorkspaceCheckFailed)
	}
}

// Within the retention window a kept dir still gets the conflict error.
func TestHandleAgentDirWithoutConfig_KeptDirInsideWindowStillConflicts(t *testing.T) {
	keptTestRetention(t, 72*time.Hour)
	now := keptTestClock(t)
	base, _, agentDir, agentHome, ws := keptWorktreeWithWork(t)
	requireKeptWithConflict(t, handleAgentDirWithoutConfig("a", agentDir, agentHome, ws), base, ws)

	*now = now.Add(72*time.Hour - time.Second)
	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, filepath.Join(ws, "notes.txt"))
}

// Once the window has passed, the kept dir is removed without running git
// in its workspace, its worktree entry is pruned, and the call succeeds so
// the caller provisions the agent again.
func TestHandleAgentDirWithoutConfig_ExpiredKeptDirIsRemoved(t *testing.T) {
	keptTestRetention(t, 72*time.Hour)
	now := keptTestClock(t)
	base, repo, agentDir, agentHome, ws := keptWorktreeWithWork(t)
	requireKeptWithConflict(t, handleAgentDirWithoutConfig("a", agentDir, agentHome, ws), base, ws)

	*now = now.Add(72 * time.Hour)
	gitRuns := countWorktreeGit(t)
	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig after the window: %v", err)
	}
	requireRemoved(t, agentDir)
	requireNoPurgeLeftovers(t, filepath.Dir(agentDir))
	if *gitRuns != 0 {
		t.Errorf("git ran %d times in the kept workspace, want 0", *gitRuns)
	}
	if _, err := os.Stat(worktreeAdminDir(repo, "workspace")); !os.IsNotExist(err) {
		t.Errorf("expected the removed workspace's worktree entry to be pruned (stat err=%v)", err)
	}
}

// A retention of 0 keeps a kept dir forever.
func TestHandleAgentDirWithoutConfig_ZeroRetentionKeepsForever(t *testing.T) {
	keptTestRetention(t, 0)
	now := keptTestClock(t)
	base, _, agentDir, agentHome, ws := keptWorktreeWithWork(t)
	requireKeptWithConflict(t, handleAgentDirWithoutConfig("a", agentDir, agentHome, ws), base, ws)

	*now = now.Add(10 * 365 * 24 * time.Hour)
	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, filepath.Join(ws, "notes.txt"))
}

// A marker that is a link is ignored: the dir is kept, and the link's
// target is neither read as a record nor written.
func TestHandleAgentDirWithoutConfig_LinkedMarkerIsIgnored(t *testing.T) {
	keptTestRetention(t, time.Hour)
	now := keptTestClock(t)
	base, _, agentDir, agentHome, ws := keptWorktreeWithWork(t)
	target := filepath.Join(base, "marker-target.json")
	data, _ := json.Marshal(keptAgentDirRecord{KeptAt: now.Add(-48 * time.Hour), Reason: "x"})
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(agentDir, keptAgentDirMarker)); err != nil {
		t.Fatal(err)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, filepath.Join(ws, "notes.txt"))
	if got, _ := os.ReadFile(target); string(got) != string(data) {
		t.Fatalf("link target content = %q, want it unchanged", got)
	}
}

// readKeptAgentDirRecord accepts a regular marker file and refuses a link
// to one, which the removal's re-check relies on.
func TestReadKeptAgentDirRecord_LinkedMarkerIsRefused(t *testing.T) {
	base := t.TempDir()
	regular := filepath.Join(base, "regular")
	linked := filepath.Join(base, "linked")
	for _, d := range []string{regular, linked} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	keptAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeMarkerT(t, regular, keptAt)
	if err := os.Symlink(filepath.Join(regular, keptAgentDirMarker), filepath.Join(linked, keptAgentDirMarker)); err != nil {
		t.Fatal(err)
	}

	if rec, ok := readKeptAgentDirRecord(regular); !ok || !rec.KeptAt.Equal(keptAt) {
		t.Fatalf("regular marker: got %+v, %v; want keptAt %v", rec, ok, keptAt)
	}
	if rec, ok := readKeptAgentDirRecord(linked); ok {
		t.Fatalf("linked marker was read as %+v, want it refused", rec)
	}
}

// A kept dir whose external home has data since is not removed after the
// window: the home rules apply instead.
func TestPurgeExpiredKeptAgentDir_HomeDataKeepsDir(t *testing.T) {
	now := keptTestClock(t)
	_, _, agentDir, agentHome, ws := keptWorktreeWithWork(t)
	writeMarkerT(t, agentDir, now.Add(-2*time.Hour))
	if err := os.WriteFile(filepath.Join(agentHome, ".bashrc"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if purgeExpiredKeptAgentDir("a", agentDir, agentHome, time.Hour) {
		t.Fatal("purge removed a kept dir whose home has data")
	}
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); err != nil {
		t.Fatalf("expected the workspace to be kept: %v", err)
	}
}

// The sweep removes another agent's expired kept dir, and leaves a dir
// without a marker, a dir inside its window, a dir with a config file, an
// entry named like a removal in progress, and a link to a dir.
func TestSweepExpiredKeptAgentDirs(t *testing.T) {
	keptTestRetention(t, time.Hour)
	now := keptTestClock(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "agents")
	dirs := map[string]string{}
	inFlight := keptAgentDirPurgePrefix + "0123456789ab"
	for _, name := range []string{"expired", "nomarker", "recent", "configured", "self", inFlight} {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(d, "workspace"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "workspace", "work.txt"), []byte("w\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dirs[name] = d
	}
	old := now.Add(-2 * time.Hour)
	writeMarkerT(t, dirs["expired"], old)
	writeMarkerT(t, dirs["recent"], now.Add(-30*time.Minute))
	writeMarkerT(t, dirs["configured"], old)
	writeMarkerT(t, dirs["self"], old)
	writeMarkerT(t, dirs[inFlight], old)
	linkTarget := filepath.Join(tmp, "outside")
	if err := os.MkdirAll(filepath.Join(linkTarget, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linkTarget, "workspace", "work.txt"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMarkerT(t, linkTarget, old)
	link := filepath.Join(root, "linked")
	if err := os.Symlink(linkTarget, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirs["configured"], "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	sweepExpiredKeptAgentDirs(root, "self", func(name string) string { return filepath.Join(root, name, "home") })

	requireRemoved(t, dirs["expired"])
	for _, name := range []string{"nomarker", "recent", "configured", "self", inFlight} {
		if _, err := os.Stat(filepath.Join(dirs[name], "workspace", "work.txt")); err != nil {
			t.Errorf("expected %s to be kept: %v", name, err)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("expected the link entry to be kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(linkTarget, "workspace", "work.txt")); err != nil {
		t.Errorf("expected the link target to be kept: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), keptAgentDirPurgePrefix) && e.Name() != inFlight {
			t.Errorf("unexpected leftover %s in %s", e.Name(), root)
		}
	}
}

// A dir that no longer shows an expired marker once moved aside (another
// process re-provisioned the name in between) is put back, not removed.
func TestPurgeExpiredKeptAgentDir_RecheckRestoresFreshDir(t *testing.T) {
	now := keptTestClock(t)
	root := filepath.Join(t.TempDir(), "agents")
	agentDir := filepath.Join(root, "a")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMarkerT(t, agentDir, now.Add(-2*time.Hour))
	if err := os.WriteFile(filepath.Join(agentDir, "fresh.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := keptAgentDirBeforeRecheck
	keptAgentDirBeforeRecheck = func(tomb string) { _ = os.Remove(filepath.Join(tomb, keptAgentDirMarker)) }
	t.Cleanup(func() { keptAgentDirBeforeRecheck = orig })

	if purgeExpiredKeptAgentDir("a", agentDir, filepath.Join(agentDir, "home"), time.Hour) {
		t.Fatal("purge reported a removal of a dir whose marker was gone at the recheck")
	}
	if _, err := os.Stat(filepath.Join(agentDir, "fresh.txt")); err != nil {
		t.Fatalf("expected the dir to be put back with its content: %v", err)
	}
	requireNoPurgeLeftovers(t, root)
}

// An explicit local delete removes a kept dir that has no agent record.
func TestDeleteAgentFiles_RemovesKeptDirWithoutRecord(t *testing.T) {
	keptTestRetention(t, 0)
	keptTestClock(t)
	base, repo, agentDir, agentHome, ws := keptWorktreeWithWork(t)
	requireKeptWithConflict(t, handleAgentDirWithoutConfig("a", agentDir, agentHome, ws), base, ws)

	if _, err := DeleteAgentFiles("a", filepath.Join(repo, ".scion"), false); err != nil {
		t.Fatalf("DeleteAgentFiles: %v", err)
	}
	requireRemoved(t, agentDir)
}

// The retention comes from the environment first, then the global
// settings file; unset or invalid values give the default.
func TestResolveKeptAgentDirRetention(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(keptAgentDirRetentionEnv, "")
	if got := resolveKeptAgentDirRetention(); got != keptAgentDirDefaultRetention {
		t.Fatalf("unset: got %v, want the default %v", got, keptAgentDirDefaultRetention)
	}

	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := "schema_version: \"1\"\nserver:\n  hub:\n    soft_delete_retention: 48h\n"
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveKeptAgentDirRetention(); got != 48*time.Hour {
		t.Fatalf("settings file: got %v, want 48h", got)
	}

	t.Setenv(keptAgentDirRetentionEnv, "2h")
	if got := resolveKeptAgentDirRetention(); got != 2*time.Hour {
		t.Fatalf("env: got %v, want 2h", got)
	}

	t.Setenv(keptAgentDirRetentionEnv, "soon")
	if got := resolveKeptAgentDirRetention(); got != keptAgentDirDefaultRetention {
		t.Fatalf("invalid: got %v, want the default %v", got, keptAgentDirDefaultRetention)
	}
}

// When a dir with a populated home and an agent record is adopted (a config
// file is written from the record), a marker from an earlier keep is
// removed, so a later loss of the config cannot reuse its keptAt.
func TestHandleAgentDirWithoutConfig_AdoptionRemovesMarker(t *testing.T) {
	keptTestRetention(t, time.Hour)
	now := keptTestClock(t)
	base := t.TempDir()
	agentDir := filepath.Join(base, "agents", "a")
	agentHome := filepath.Join(base, "external", "a", "home")
	for _, d := range []string{agentDir, agentHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMarkerT(t, agentDir, now.Add(-2*time.Hour))
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"),
		[]byte(`{"name":"a","template":"default","harnessConfig":"generic"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, filepath.Join(agentDir, "workspace")); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "scion-agent.json")); err != nil {
		t.Fatalf("expected the adopted dir to have a config file: %v", err)
	}
	requireRemoved(t, filepath.Join(agentDir, keptAgentDirMarker))
}
