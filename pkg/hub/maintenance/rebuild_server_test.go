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

//go:build linux

package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/unitdrift"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const testGitRelease = "git-0123456789ab"

// rebuildFixture is a fake checkout plus git, make and go stubs on PATH.
// The go stub writes, at its -o path, a binary that reports the commit
// passed in -ldflags (or reportCommit when set), so VerifyGitBuild sees
// what the real build would stamp.
type rebuildFixture struct {
	repo  string
	calls string
}

func newRebuildFixture(t *testing.T, reportCommit string) *rebuildFixture {
	t.Helper()
	f := &rebuildFixture{
		repo:  t.TempDir(),
		calls: filepath.Join(t.TempDir(), "calls.log"),
	}
	stubs := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("git", fmt.Sprintf(`echo "git $*" >> '%s'
case "$1 $2" in
  "rev-parse HEAD") echo %s ;;
  "rev-parse --abbrev-ref") echo main ;;
esac
`, f.calls, testCommit))
	write("make", fmt.Sprintf("echo \"make $*\" >> '%s'\n", f.calls))
	write("go", fmt.Sprintf(`echo "go $*" >> '%s'
out=""; ld=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -ldflags) ld="$2"; shift 2 ;;
    *) shift ;;
  esac
done
commit=$(printf '%%s\n' "$ld" | sed -n 's/.*pkg\/version\.Commit=\([0-9a-f]*\).*/\1/p')
report='%s'
[ -n "$report" ] && commit="$report"
{
  echo '#!/bin/sh'
  echo "echo '{\"version\":\"\",\"commit\":\"$commit\"}'"
} > "$out"
chmod 755 "$out"
`, f.calls, reportCommit))
	t.Setenv("PATH", stubs+":"+os.Getenv("PATH"))
	return f
}

func (f *rebuildFixture) callLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func (f *rebuildFixture) executor(inst Installer) *RebuildServerExecutor {
	return NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:       f.repo,
		Installer:      inst,
		CurrentRelease: "nightly-20261022",
		UnitLocator: func(context.Context, string) (unitdrift.UnitInfo, error) {
			return unitdrift.UnitInfo{}, errors.New("no systemd in tests")
		},
		UnitRevision: func(context.Context, string) (string, error) { return "abc1234", nil },
	})
}

// symlinkInstallerForRebuild returns a symlink installer over a layout
// whose current release is tags[0] and a counter of restart signals.
func symlinkInstallerForRebuild(t *testing.T, tags ...string) (*symlinkInstaller, string, *int) {
	t.Helper()
	root := installLayout(t, tags...)
	signals := 0
	opts := symlinkOpts(root)
	opts.UID = os.Getuid
	opts.Signal = func() error { signals++; return nil }
	return newSymlinkInstaller(opts), root, &signals
}

func TestRebuildServer_SymlinkInstallsGitRelease(t *testing.T) {
	f := newRebuildFixture(t, "")
	inst, root, signals := symlinkInstallerForRebuild(t, "nightly-20261022")

	var got *PendingRestart
	var log bytes.Buffer
	err := f.executor(inst).RunWithBeforeRestart(context.Background(), &log, nil,
		func(_ context.Context, p PendingRestart) error {
			got = &p
			if *signals != 0 {
				t.Error("restart requested before the hook ran")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}

	if target := readLink(t, root, currentLinkName); target != filepath.Join(releasesDirName, testGitRelease) {
		t.Fatalf("current -> %q, want releases/%s", target, testGitRelease)
	}
	if target := readLink(t, root, prevLinkName); target != filepath.Join(releasesDirName, "nightly-20261022") {
		t.Fatalf("previous -> %q, want the old release", target)
	}
	out, err := exec.Command(filepath.Join(root, releasesDirName, testGitRelease, binaryName), "version", "--format", "json").Output()
	if err != nil || !strings.Contains(string(out), testCommit) {
		t.Fatalf("installed binary reports %q (%v), want commit %s", out, err, testCommit)
	}
	if *signals != 1 {
		t.Fatalf("restart signals = %d, want 1", *signals)
	}

	want := PendingRestart{From: "nightly-20261022", To: testGitRelease, Strategy: StrategySymlink, Mode: UpdateModeGit, Commit: testCommit}
	if got == nil || *got != want {
		t.Fatalf("hook got %+v, want %+v", got, want)
	}

	calls := f.callLog(t)
	if !strings.Contains(calls, "-ldflags -X github.com/GoogleCloudPlatform/scion/pkg/version.Commit="+testCommit+" -X github.com/GoogleCloudPlatform/scion/pkg/version.BuildTime=") {
		t.Fatalf("go build without the commit/build-time ldflags:\n%s", calls)
	}
	if strings.Contains(calls, "-o "+filepath.Join(f.repo, SudoStagingName)) {
		t.Fatalf("symlink rebuild must not build into the starter-hub staging path:\n%s", calls)
	}
	if strings.Contains(log.String(), "Systemd unit check") {
		t.Fatalf("the starter-hub unit check must not run on the symlink layout:\n%s", log.String())
	}
}

func TestRebuildServer_SymlinkRetention(t *testing.T) {
	f := newRebuildFixture(t, "")
	tags := []string{"nightly-20261022", "nightly-20261016", "nightly-20261017", "nightly-20261018",
		"nightly-20261019", "nightly-20261020", "nightly-20261021"}
	inst, root, _ := symlinkInstallerForRebuild(t, tags...)
	// Oldest first by mtime, current newest.
	base := time.Now().Add(-time.Hour)
	for i, tag := range tags[1:] {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(root, releasesDirName, tag), at, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.executor(inst).Run(context.Background(), &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, releasesDirName))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != RetainedReleases {
		t.Fatalf("releases after rebuild = %v, want %d retained", names, RetainedReleases)
	}
	for _, keep := range []string{testGitRelease, "nightly-20261022"} {
		if _, err := os.Stat(filepath.Join(root, releasesDirName, keep)); err != nil {
			t.Fatalf("%s pruned: %v (left %v)", keep, err, names)
		}
	}
	if _, err := os.Stat(filepath.Join(root, releasesDirName, "nightly-20261016")); !os.IsNotExist(err) {
		t.Fatalf("oldest release kept: %v (left %v)", err, names)
	}
}

func TestRebuildServer_AlreadyActiveBuildIsNotRebuilt(t *testing.T) {
	f := newRebuildFixture(t, "")
	inst, _, signals := symlinkInstallerForRebuild(t, testGitRelease)
	err := f.executor(inst).Run(context.Background(), &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "already the active build") {
		t.Fatalf("Run = %v, want already the active build", err)
	}
	if calls := f.callLog(t); strings.Contains(calls, "go build") || strings.Contains(calls, "make web") {
		t.Fatalf("built although the commit is active:\n%s", calls)
	}
	if *signals != 0 {
		t.Fatal("restart requested")
	}
}

func TestRebuildServer_HookFailureRollsBackSymlink(t *testing.T) {
	f := newRebuildFixture(t, "")
	inst, root, signals := symlinkInstallerForRebuild(t, "nightly-20261022")
	var log bytes.Buffer
	err := f.executor(inst).RunWithBeforeRestart(context.Background(), &log, nil,
		func(context.Context, PendingRestart) error { return errors.New("store down") })
	if err == nil || !strings.Contains(err.Error(), "was rolled back") {
		t.Fatalf("Run = %v, want a rolled-back failure", err)
	}
	if target := readLink(t, root, currentLinkName); target != filepath.Join(releasesDirName, "nightly-20261022") {
		t.Fatalf("current -> %q after the rollback, want the old release", target)
	}
	if *signals != 0 {
		t.Fatal("restart requested after the hook failed")
	}
}

func TestRebuildServer_PreflightFailureBuildsNothing(t *testing.T) {
	f := newRebuildFixture(t, "")
	inst := NewUnsupportedInstaller("no layout", MigrateLayoutHint)
	err := f.executor(inst).Run(context.Background(), &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "installer preflight failed") {
		t.Fatalf("Run = %v, want a preflight failure", err)
	}
	if calls := f.callLog(t); calls != "" {
		t.Fatalf("commands ran before the preflight failed:\n%s", calls)
	}
}

func TestRebuildServer_UnverifiedBuildIsNotInstalled(t *testing.T) {
	// The build reports another commit: the ldflags did not take effect.
	f := newRebuildFixture(t, "ffffffffffffffffffffffffffffffffffffffff")
	inst, root, signals := symlinkInstallerForRebuild(t, "nightly-20261022")
	err := f.executor(inst).Run(context.Background(), &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "commit mismatch") {
		t.Fatalf("Run = %v, want a commit mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(root, releasesDirName, testGitRelease)); !os.IsNotExist(err) {
		t.Fatalf("unverified build staged: %v", err)
	}
	if *signals != 0 {
		t.Fatal("restart requested")
	}
}

func TestRebuildServer_SudoInstallStarterHub(t *testing.T) {
	f := newRebuildFixture(t, "")
	r := &fakeRunner{allowed: map[string]bool{}}
	for _, a := range []string{ruleInstall, ruleNoBlock} {
		r.allowed[strings.ReplaceAll(a, "$REPO", f.repo)] = true
	}
	inst := newSudoInstaller(DetectOptions{
		RepoPath: f.repo,
		Runner:   r,
	}.withDefaults())

	var got *PendingRestart
	var log bytes.Buffer
	err := f.executor(inst).RunWithBeforeRestart(context.Background(), &log, nil,
		func(_ context.Context, p PendingRestart) error { got = &p; return nil })
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	staged := filepath.Join(f.repo, SudoStagingName)
	out, err := exec.Command(staged, "version", "--format", "json").Output()
	if err != nil || !strings.Contains(string(out), testCommit) {
		t.Fatalf("staged %s reports %q (%v)", staged, out, err)
	}
	// Exactly the starter hub's sudoers commands, preflighted with -l.
	wantRuns := []string{
		"sudo -n -l install -m 755 " + staged + " /usr/local/bin/scion",
		"sudo -n -l systemctl restart --no-block scion-hub",
		"sudo -n install -m 755 " + staged + " /usr/local/bin/scion",
		"sudo -n systemctl restart --no-block scion-hub",
	}
	if strings.Join(r.runs, "\n") != strings.Join(wantRuns, "\n") {
		t.Fatalf("sudo commands =\n%s\nwant\n%s", strings.Join(r.runs, "\n"), strings.Join(wantRuns, "\n"))
	}
	if got == nil || got.To != testGitRelease || got.Mode != UpdateModeGit || got.Strategy != StrategySudoInstall {
		t.Fatalf("hook got %+v", got)
	}
	// The starter-hub unit check still runs, and its report closes the
	// log written before the restart.
	if !strings.Contains(log.String(), "\n==> Systemd unit check\nSystemd unit check skipped: ") {
		t.Fatalf("unit check report missing:\n%s", log.String())
	}
}

func TestSudoInstaller_PreflightSudoAbsent(t *testing.T) {
	opts, _ := sudoOpts(t, ruleInstall, ruleNoBlock)
	opts.Runner = missingSudoRunner{}
	err := newSudoInstaller(opts).Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no sudoers rule allows `install -m 755") {
		t.Fatalf("Preflight = %v, want the install rule reported missing", err)
	}
	inst := Detect(context.Background(), opts)
	if inst.Name() != StrategyUnsupported {
		t.Fatalf("Detect without sudo = %s, want unsupported", inst.Name())
	}
}

// missingSudoRunner fails every command as if sudo were not installed.
type missingSudoRunner struct{}

func (missingSudoRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, exec.ErrNotFound
}

func (missingSudoRunner) Start(string, ...string) (<-chan error, error) {
	return nil, exec.ErrNotFound
}

func TestRebuildServer_RestartFailureRollsBackSymlink(t *testing.T) {
	f := newRebuildFixture(t, "")
	inst, root, _ := symlinkInstallerForRebuild(t, "nightly-20261022")
	inst.signal = func() error { return errors.New("cannot signal") }
	hooked := false
	err := f.executor(inst).RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, nil,
		func(context.Context, PendingRestart) error { hooked = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "restarting the hub failed") || !strings.Contains(err.Error(), "was rolled back") {
		t.Fatalf("Run = %v, want a rolled-back restart failure", err)
	}
	if !hooked {
		t.Fatal("the hook must run before the restart")
	}
	if target := readLink(t, root, currentLinkName); target != filepath.Join(releasesDirName, "nightly-20261022") {
		t.Fatalf("current -> %q after the rollback, want the old release", target)
	}
}

func TestRebuildServer_BuildFailureInstallsNothing(t *testing.T) {
	f := newRebuildFixture(t, "")
	// A failing go build: the stub is replaced by one that exits 1.
	goStub := filepath.Join(filepath.SplitList(os.Getenv("PATH"))[0], "go")
	if err := os.WriteFile(goStub, []byte("#!/bin/sh\necho 'compile error' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	inst, root, signals := symlinkInstallerForRebuild(t, "nightly-20261022")
	err := f.executor(inst).Run(context.Background(), &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "Building server binary failed: compile error") {
		t.Fatalf("Run = %v, want the build failure", err)
	}
	if _, err := os.Stat(filepath.Join(root, releasesDirName, testGitRelease)); !os.IsNotExist(err) {
		t.Fatalf("a failed build was staged: %v", err)
	}
	if target := readLink(t, root, currentLinkName); target != filepath.Join(releasesDirName, "nightly-20261022") {
		t.Fatalf("current -> %q, want unchanged", target)
	}
	if *signals != 0 {
		t.Fatal("restart requested")
	}
}

// datedReleases answers ReleaseByTag with a fixed publish date and counts
// lookups.
type datedReleases struct {
	at      time.Time
	lookups []string
}

func (d *datedReleases) ReleaseByTag(_ context.Context, tag string) (*Release, error) {
	d.lookups = append(d.lookups, tag)
	return &Release{Tag: tag, PublishedAt: d.at}, nil
}

// The boot-failure guard's previous_safe needs a dated preview/stable
// "from"; a Git or nightly "from" is never looked up.
func TestRebuildServer_FromPublishedAtForTheGuard(t *testing.T) {
	published := time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)
	cases := []struct {
		current     string
		wantDate    time.Time
		wantLookups int
	}{
		{"v0.3.0-preview.4", published, 1},
		{"v1.0.0", published, 1},
		{"nightly-20261022", time.Time{}, 0},
		{"git-89abcdef0123", time.Time{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.current, func(t *testing.T) {
			f := newRebuildFixture(t, "")
			src := &datedReleases{at: published}
			e := NewRebuildServerExecutor(RebuildServerOptions{
				RepoPath:       f.repo,
				Installer:      &fakeInstaller{},
				CurrentRelease: tc.current,
				Releases:       src,
			})
			var got PendingRestart
			if err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, nil,
				func(_ context.Context, p PendingRestart) error { got = p; return nil }); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !got.FromPublishedAt.Equal(tc.wantDate) || len(src.lookups) != tc.wantLookups {
				t.Fatalf("FromPublishedAt = %v (lookups %v), want %v (%d lookups)", got.FromPublishedAt, src.lookups, tc.wantDate, tc.wantLookups)
			}
			// What the boot-failure guard gets from that "from".
			safe := PreviousSafe(tc.current, got.FromPublishedAt, "nightly-20261009")
			if wantSafe := tc.current != "git-89abcdef0123"; safe != wantSafe {
				t.Fatalf("previous_safe for from %s = %v, want %v", tc.current, safe, wantSafe)
			}
		})
	}
}

// unitDriftStore records hub setting deletions.
type unitDriftStore struct {
	store.Store
	deleted []string
}

func (u *unitDriftStore) DeleteHubSetting(_ context.Context, section string) error {
	u.deleted = append(u.deleted, section)
	return nil
}

// On the symlink layout the starter-hub unit check does not run, and a
// drift record left from before is removed, so its banner cannot go stale.
func TestRebuildServer_SymlinkClearsUnitDriftRecord(t *testing.T) {
	f := newRebuildFixture(t, "")
	inst, _, _ := symlinkInstallerForRebuild(t, "nightly-20261022")
	st := &unitDriftStore{}
	e := NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:       f.repo,
		Installer:      inst,
		CurrentRelease: "nightly-20261022",
		Store:          st,
	})
	var log bytes.Buffer
	if err := e.Run(context.Background(), &log, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(st.deleted) != 1 || st.deleted[0] != HubSettingSectionUnitDrift {
		t.Fatalf("deleted = %v, want [%s]", st.deleted, HubSettingSectionUnitDrift)
	}
	if strings.Contains(log.String(), "Checking systemd unit against template") {
		t.Fatalf("the unit check must not run on the symlink layout:\n%s", log.String())
	}
}

// Design D4: a rebuild takes the same pre-update database snapshot as
// update-binary, after the build is verified and before anything is
// installed, and records its path for the pending update.
func TestRebuildServer_SnapshotBeforeActivation(t *testing.T) {
	f := newRebuildFixture(t, "")
	inst := &fakeInstaller{}
	snaps := &fakeDBSnapshotter{calls: &inst.calls}
	e := NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:       f.repo,
		Installer:      inst,
		CurrentRelease: "git-89abcdef0123",
		Snapshotter:    snaps,
	})
	var got PendingRestart
	var log bytes.Buffer
	err := e.RunWithBeforeRestart(context.Background(), &log, nil, func(_ context.Context, p PendingRestart) error {
		inst.calls = append(inst.calls, "hook")
		got = p
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	want := []string{"preflight", "snapshot", "stage:" + testGitRelease, "activate:staged-" + testGitRelease, "hook", "restart"}
	if strings.Join(inst.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", inst.calls, want)
	}
	wantPath := fakeSnapshotDir + "/hub-git-89abcdef0123-20261029T070000Z.db"
	if got.Snapshot != wantPath {
		t.Fatalf("pending snapshot = %q, want %q", got.Snapshot, wantPath)
	}
	if len(snaps.froms) != 1 || snaps.froms[0] != "git-89abcdef0123" {
		t.Fatalf("snapshot from = %v", snaps.froms)
	}
	if !strings.Contains(log.String(), "\n==> Database snapshot\n") {
		t.Fatalf("missing the snapshot step in the log:\n%s", log.String())
	}
	// The snapshot comes after the build was verified.
	if strings.Index(log.String(), "Build verified") > strings.Index(log.String(), "==> Database snapshot") {
		t.Fatalf("snapshot before the build was verified:\n%s", log.String())
	}
}

func TestRebuildServer_SnapshotFailureInstallsNothing(t *testing.T) {
	f := newRebuildFixture(t, "")
	inst := &fakeInstaller{}
	e := NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:    f.repo,
		Installer:   inst,
		Snapshotter: &fakeDBSnapshotter{calls: &inst.calls, err: errors.New("disk full")},
	})
	hooked := false
	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, nil,
		func(context.Context, PendingRestart) error { hooked = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "database snapshot failed, nothing was installed: disk full") {
		t.Fatalf("Run = %v, want the snapshot failure", err)
	}
	if strings.Join(inst.calls, ",") != "preflight,snapshot" || hooked {
		t.Fatalf("calls = %v (hooked %v), want nothing installed after the snapshot", inst.calls, hooked)
	}
}

// Without snapshot support (PostgreSQL) a rebuild goes ahead without one,
// as an upgrade through update-binary does.
func TestRebuildServer_UnsupportedSnapshotStillRebuilds(t *testing.T) {
	f := newRebuildFixture(t, "")
	inst := &fakeInstaller{}
	e := NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:    f.repo,
		Installer:   inst,
		Snapshotter: &fakeDBSnapshotter{calls: &inst.calls, unsupported: "PostgreSQL databases are not snapshotted"},
	})
	var got PendingRestart
	var log bytes.Buffer
	if err := e.RunWithBeforeRestart(context.Background(), &log, nil,
		func(_ context.Context, p PendingRestart) error { got = p; return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Snapshot != "" || strings.Contains(strings.Join(inst.calls, ","), "snapshot") {
		t.Fatalf("snapshot %q, calls %v: want none taken", got.Snapshot, inst.calls)
	}
	if !strings.Contains(log.String(), "No database snapshot taken: PostgreSQL databases are not snapshotted. The rebuild goes ahead without one.\n") ||
		strings.Contains(log.String(), "downgrades") {
		t.Fatalf("missing the rebuild's no-snapshot line:\n%s", log.String())
	}
}
