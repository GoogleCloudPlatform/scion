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

package maintenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// installLayout builds <root>/releases/<tag>/scion for each tag and points
// current at the first one. It returns the root.
func installLayout(t *testing.T, tags ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "opt-scion")
	for _, tag := range tags {
		dir := filepath.Join(root, releasesDirName, tag)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, binaryName), []byte("bin "+tag), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if len(tags) > 0 {
		if err := os.Symlink(filepath.Join(releasesDirName, tags[0]), filepath.Join(root, currentLinkName)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func symlinkOpts(root string) DetectOptions {
	return DetectOptions{
		InstallRoot: root,
		Executable: func() (string, error) {
			return filepath.Join(root, currentLinkName, binaryName), nil
		},
		Runner: &fakeRunner{},
		Signal: func() error { return errors.New("signal not expected") },
		GOOS:   "linux",
	}.withDefaults()
}

func readLink(t *testing.T, root, name string) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("readlink %s: %v", name, err)
	}
	return target
}

func writeBin(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scion")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSymlinkInstaller_Preflight(t *testing.T) {
	t.Run("usable layout", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022")
		if err := newSymlinkInstaller(symlinkOpts(root)).Preflight(context.Background()); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
	})

	t.Run("binary outside releases", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022")
		opts := symlinkOpts(root)
		opts.Executable = func() (string, error) { return writeBin(t, "x"), nil }
		err := newSymlinkInstaller(opts).Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "is not under") {
			t.Fatalf("Preflight = %v, want 'is not under'", err)
		}
	})

	t.Run("relative root", func(t *testing.T) {
		err := newSymlinkInstaller(symlinkOpts("opt/scion")).Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not an absolute path") {
			t.Fatalf("Preflight = %v", err)
		}
	})

	t.Run("missing root", func(t *testing.T) {
		opts := symlinkOpts(filepath.Join(t.TempDir(), "nope"))
		if err := newSymlinkInstaller(opts).Preflight(context.Background()); err == nil {
			t.Fatal("Preflight succeeded without an install root")
		}
	})

	t.Run("current is not a symlink", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022")
		opts := symlinkOpts(root)
		opts.Executable = func() (string, error) {
			return filepath.Join(root, releasesDirName, "nightly-20261022", binaryName), nil
		}
		cur := filepath.Join(root, currentLinkName)
		if err := os.Remove(cur); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cur, 0o755); err != nil {
			t.Fatal(err)
		}
		err := newSymlinkInstaller(opts).Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not a symlink") {
			t.Fatalf("Preflight = %v, want 'not a symlink'", err)
		}
	})

	t.Run("current owned by another user", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022")
		opts := symlinkOpts(root)
		opts.UID = func() int { return os.Getuid() + 1 }
		err := newSymlinkInstaller(opts).Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not owned by the hub user") {
			t.Fatalf("Preflight = %v, want ownership error", err)
		}
	})

	t.Run("releases not writable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root can write read-only directories")
		}
		root := installLayout(t, "nightly-20261022")
		rel := filepath.Join(root, releasesDirName)
		if err := os.Chmod(rel, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(rel, 0o755) })
		err := newSymlinkInstaller(symlinkOpts(root)).Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not writable") {
			t.Fatalf("Preflight = %v, want 'not writable'", err)
		}
	})
}

func TestSymlinkInstaller_StageActivate(t *testing.T) {
	ctx := context.Background()
	root := installLayout(t, "nightly-20261022")
	inst := newSymlinkInstaller(symlinkOpts(root))

	staged, err := inst.Stage(ctx, "nightly-20261023", writeBin(t, "new"))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if want := filepath.Join(root, releasesDirName, "nightly-20261023"); staged != want {
		t.Fatalf("staged = %q, want %q", staged, want)
	}
	if _, err := os.Stat(staged + partialSuffix); !os.IsNotExist(err) {
		t.Fatalf("partial directory left behind: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(staged, binaryName))
	if err != nil || string(got) != "new" {
		t.Fatalf("staged binary = %q, %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(staged, binaryName)); fi.Mode().Perm() != 0o755 {
		t.Fatalf("staged mode = %v, want 0755", fi.Mode().Perm())
	}
	// Staging does not touch current.
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
		t.Fatalf("current after Stage = %q", cur)
	}

	if err := inst.Activate(ctx, staged); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261023" {
		t.Fatalf("current = %q, want relative releases/nightly-20261023", cur)
	}
	if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261022" {
		t.Fatalf("previous = %q, want releases/nightly-20261022", prev)
	}
	if _, err := os.Lstat(filepath.Join(root, currentLinkName+".new")); !os.IsNotExist(err) {
		t.Fatal("current.new left behind")
	}
	// The resolved binary through current is the new one.
	got, err = os.ReadFile(filepath.Join(root, currentLinkName, binaryName))
	if err != nil || string(got) != "new" {
		t.Fatalf("current/scion = %q, %v", got, err)
	}

	rels := inst.Retained()
	if len(rels) != 2 {
		t.Fatalf("Retained = %+v, want 2 releases", rels)
	}
	flags := map[string][2]bool{}
	for _, r := range rels {
		flags[r.Tag] = [2]bool{r.Current, r.Previous}
	}
	if flags["nightly-20261023"] != [2]bool{true, false} || flags["nightly-20261022"] != [2]bool{false, true} {
		t.Fatalf("Retained flags = %v", flags)
	}
}

func TestSymlinkInstaller_StageRefusals(t *testing.T) {
	ctx := context.Background()
	root := installLayout(t, "nightly-20261022")
	inst := newSymlinkInstaller(symlinkOpts(root))
	bin := writeBin(t, "x")

	if _, err := inst.Stage(ctx, "nightly-20261022", bin); err == nil {
		t.Fatal("Stage replaced the active release")
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", ".hidden", "x.partial"} {
		if _, err := inst.Stage(ctx, bad, bin); err == nil {
			t.Errorf("Stage(%q) succeeded", bad)
		}
	}
	if err := inst.Activate(ctx, filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Fatal("Activate accepted a directory outside releases/")
	}
	if err := inst.Activate(ctx, filepath.Join(root, releasesDirName, "missing")); err == nil {
		t.Fatal("Activate accepted a release without a binary")
	}
}

func TestSymlinkInstaller_StageReplacesStalePartialAndRetainedCopy(t *testing.T) {
	ctx := context.Background()
	root := installLayout(t, "nightly-20261022", "nightly-20261020")
	partial := filepath.Join(root, releasesDirName, "nightly-20261020"+partialSuffix)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := newSymlinkInstaller(symlinkOpts(root))
	staged, err := inst.Stage(ctx, "nightly-20261020", writeBin(t, "fresh"))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(staged, binaryName))
	if string(got) != "fresh" {
		t.Fatalf("retained copy not replaced: %q", got)
	}
}

func TestSymlinkInstaller_Prune(t *testing.T) {
	ctx := context.Background()
	// current = r00 (oldest); r01..r07 are older installs. previous will be
	// r00 after activating new.
	tags := []string{"nightly-20261000"}
	for i := 1; i <= 7; i++ {
		tags = append(tags, fmt.Sprintf("nightly-202610%02d", i))
	}
	root := installLayout(t, tags...)
	base := time.Now().Add(-time.Hour)
	for i, tag := range tags {
		// r00 (current) is the oldest; r07 the newest.
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(root, releasesDirName, tag), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	inst := newSymlinkInstaller(symlinkOpts(root))
	staged, err := inst.Stage(ctx, "nightly-20261023", writeBin(t, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Activate(ctx, staged); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, r := range inst.Retained() {
		got = append(got, r.Tag)
	}
	// Newest 5: new, r07, r06, r05, r04; plus previous (r00) which is old.
	want := map[string]bool{
		"nightly-20261023": true, "nightly-20261007": true, "nightly-20261006": true,
		"nightly-20261005": true, "nightly-20261004": true, "nightly-20261000": true,
	}
	if len(got) != len(want) {
		t.Fatalf("retained %v, want %v", got, want)
	}
	for _, tag := range got {
		if !want[tag] {
			t.Fatalf("retained %v, want %v", got, want)
		}
	}
	if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261000" {
		t.Fatalf("previous = %q", prev)
	}
}

func TestSymlinkInstaller_Deactivate(t *testing.T) {
	ctx := context.Background()
	root := installLayout(t, "nightly-20261022")
	inst := newSymlinkInstaller(symlinkOpts(root))
	if err := inst.Deactivate(ctx); err == nil {
		t.Fatal("Deactivate without an activation succeeded")
	}
	staged, err := inst.Stage(ctx, "nightly-20261023", writeBin(t, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Activate(ctx, staged); err != nil {
		t.Fatal(err)
	}
	if err := inst.Deactivate(ctx); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
		t.Fatalf("current = %q", cur)
	}
	// There was no previous before the activation, so none is left.
	if _, err := os.Lstat(filepath.Join(root, prevLinkName)); !os.IsNotExist(err) {
		t.Fatalf("previous left behind: %v", err)
	}
	if err := inst.Deactivate(ctx); err == nil {
		t.Fatal("second Deactivate succeeded")
	}
}

func TestSymlinkInstaller_Restart(t *testing.T) {
	called := false
	opts := symlinkOpts(t.TempDir())
	opts.Signal = func() error { called = true; return nil }
	if err := newSymlinkInstaller(opts).Restart(context.Background()); err != nil || !called {
		t.Fatalf("Restart = %v, signalled = %v", err, called)
	}
}

// fakeRunner records commands and answers `sudo -n -l <argv>` from allowed.
type fakeRunner struct {
	mu      sync.Mutex
	allowed map[string]bool // joined argv after "sudo -n -l"
	runs    []string
	starts  []string
	failRun map[string]error // joined full command → error
	block   map[string]bool  // joined full command → block until ctx is done
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.runs = append(f.runs, cmd)
	if f.block[cmd] {
		f.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	defer f.mu.Unlock()
	if err := f.failRun[cmd]; err != nil {
		return []byte("boom"), err
	}
	if name == "sudo" && len(args) >= 2 && args[0] == "-n" && args[1] == "-l" {
		if f.allowed[strings.Join(args[2:], " ")] {
			return []byte("/usr/bin/" + strings.Join(args[2:], " ")), nil
		}
		return []byte("Sorry, user scion is not allowed"), errors.New("exit status 1")
	}
	return nil, nil
}

func (f *fakeRunner) Start(name string, args ...string) (<-chan error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, strings.Join(append([]string{name}, args...), " "))
	// Started commands keep "running" (the restart is under way).
	return make(chan error, 1), nil
}

func sudoOpts(t *testing.T, allowed ...string) (DetectOptions, *fakeRunner) {
	t.Helper()
	repo := t.TempDir()
	r := &fakeRunner{allowed: map[string]bool{}}
	for _, a := range allowed {
		r.allowed[strings.ReplaceAll(a, "$REPO", repo)] = true
	}
	return DetectOptions{
		InstallRoot: filepath.Join(t.TempDir(), "no-opt-scion"),
		RepoPath:    repo,
		Runner:      r,
		Executable:  func() (string, error) { return "/usr/local/bin/scion", nil },
		GOOS:        "linux",
	}.withDefaults(), r
}

const (
	ruleInstall   = "install -m 755 $REPO/scion.rebuild /usr/local/bin/scion"
	ruleNoBlock   = "systemctl restart --no-block scion-hub"
	rulePlain     = "systemctl restart scion-hub"
	ruleNoBlockTk = "sudo -n systemctl restart --no-block scion-hub"
	rulePlainTk   = "sudo -n systemctl restart scion-hub"
)

func TestSudoInstaller_PreflightAndRestart(t *testing.T) {
	ctx := context.Background()

	t.Run("both restart rules prefers --no-block", func(t *testing.T) {
		opts, r := sudoOpts(t, ruleInstall, ruleNoBlock, rulePlain)
		inst := newSudoInstaller(opts)
		if err := inst.Preflight(ctx); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if err := inst.Restart(ctx); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		if got := r.runs[len(r.runs)-1]; got != ruleNoBlockTk {
			t.Fatalf("restart ran %q, want %q", got, ruleNoBlockTk)
		}
		if len(r.starts) != 0 {
			t.Fatalf("unexpected Start: %v", r.starts)
		}
	})

	t.Run("plain rule only falls back to plain restart", func(t *testing.T) {
		opts, r := sudoOpts(t, ruleInstall, rulePlain)
		inst := newSudoInstaller(opts)
		inst.restartGrace = 10 * time.Millisecond
		if err := inst.Preflight(ctx); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if err := inst.Restart(ctx); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		if len(r.starts) != 1 || r.starts[0] != rulePlainTk {
			t.Fatalf("starts = %v, want [%q]", r.starts, rulePlainTk)
		}
	})

	// The plain restart is judged on real exit results: sudo is mapped to a
	// shell script run by ExecRunner, so Wait returns genuine
	// *exec.ExitErrors.
	plainRestart := func(t *testing.T, script string) error {
		t.Helper()
		opts, r := sudoOpts(t, ruleInstall, rulePlain)
		inst := newSudoInstaller(opts)
		if err := inst.Preflight(ctx); err != nil {
			t.Fatal(err)
		}
		inst.restartGrace = 5 * time.Second
		inst.runner = shellStartRunner{fakeRunner: r, script: script}
		return inst.Restart(ctx)
	}

	t.Run("plain restart exiting non-zero fails Restart", func(t *testing.T) {
		if err := plainRestart(t, "exit 1"); err == nil || !strings.Contains(err.Error(), "exit status 1") {
			t.Fatalf("Restart = %v, want the child's exit status", err)
		}
	})

	t.Run("plain restart killed by the unit stop is under way", func(t *testing.T) {
		// systemd's control-group kill SIGTERMs the sudo/systemctl child
		// together with the hub.
		if err := plainRestart(t, "kill -TERM $$; sleep 5"); err != nil {
			t.Fatalf("Restart = %v, want nil for a signal-terminated child", err)
		}
	})

	t.Run("plain restart exiting cleanly", func(t *testing.T) {
		if err := plainRestart(t, "exit 0"); err != nil {
			t.Fatalf("Restart = %v", err)
		}
	})

	t.Run("hung sudo install hits the deadline", func(t *testing.T) {
		opts, r := sudoOpts(t, ruleInstall, ruleNoBlock)
		inst := newSudoInstaller(opts)
		staged, err := inst.Stage(ctx, "nightly-20261023", writeBin(t, "x"))
		if err != nil {
			t.Fatal(err)
		}
		r.block = map[string]bool{"sudo -n install -m 755 " + staged + " /usr/local/bin/scion": true}
		tctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if err := inst.Activate(tctx, staged); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Activate = %v, want deadline exceeded", err)
		}
	})

	t.Run("missing install rule", func(t *testing.T) {
		opts, _ := sudoOpts(t, ruleNoBlock, rulePlain)
		err := newSudoInstaller(opts).Preflight(ctx)
		if err == nil || !strings.Contains(err.Error(), "install -m 755") {
			t.Fatalf("Preflight = %v, want missing install rule", err)
		}
	})

	t.Run("missing restart rules", func(t *testing.T) {
		opts, _ := sudoOpts(t, ruleInstall)
		err := newSudoInstaller(opts).Preflight(ctx)
		if err == nil || !strings.Contains(err.Error(), "systemctl restart") {
			t.Fatalf("Preflight = %v, want missing restart rule", err)
		}
	})

	t.Run("preflight only lists, never runs", func(t *testing.T) {
		opts, r := sudoOpts(t, ruleInstall, ruleNoBlock)
		if err := newSudoInstaller(opts).Preflight(ctx); err != nil {
			t.Fatal(err)
		}
		for _, c := range r.runs {
			if !strings.HasPrefix(c, "sudo -n -l ") {
				t.Fatalf("Preflight ran %q", c)
			}
		}
	})
}

func TestSudoInstaller_StageActivate(t *testing.T) {
	ctx := context.Background()
	opts, r := sudoOpts(t, ruleInstall, ruleNoBlock)
	inst := newSudoInstaller(opts)
	staged, err := inst.Stage(ctx, "nightly-20261023", writeBin(t, "new"))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if want := filepath.Join(opts.RepoPath, SudoStagingName); staged != want {
		t.Fatalf("staged = %q, want %q", staged, want)
	}
	if got, _ := os.ReadFile(staged); string(got) != "new" {
		t.Fatalf("staged content = %q", got)
	}
	if err := inst.Activate(ctx, staged); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	want := "sudo -n install -m 755 " + staged + " /usr/local/bin/scion"
	if got := r.runs[len(r.runs)-1]; got != want {
		t.Fatalf("Activate ran %q, want %q", got, want)
	}
	for _, c := range r.runs {
		if strings.Contains(c, " cp ") {
			t.Fatalf("unexpected backup copy: %q", c)
		}
	}
	if err := inst.Activate(ctx, "/tmp/other"); err == nil {
		t.Fatal("Activate accepted a path other than the staging file")
	}

	r.failRun = map[string]error{want: errors.New("exit status 1")}
	if err := inst.Activate(ctx, staged); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Activate failure = %v, want output in error", err)
	}
}

func TestDetect(t *testing.T) {
	ctx := context.Background()

	t.Run("symlink first", func(t *testing.T) {
		root := installLayout(t, "nightly-20261022")
		opts := symlinkOpts(root)
		opts.RepoPath = t.TempDir()
		inst := Detect(ctx, opts)
		if inst.Name() != StrategySymlink || !inst.Status().Supported {
			t.Fatalf("Detect = %s %+v", inst.Name(), inst.Status())
		}
		if r := opts.Runner.(*fakeRunner); len(r.runs) != 0 {
			t.Fatalf("sudo consulted although symlink layout works: %v", r.runs)
		}
	})

	t.Run("sudo-install when repo path and rules", func(t *testing.T) {
		opts, _ := sudoOpts(t, ruleInstall, rulePlain)
		inst := Detect(ctx, opts)
		if inst.Name() != StrategySudoInstall || !inst.Status().Supported {
			t.Fatalf("Detect = %s %+v", inst.Name(), inst.Status())
		}
	})

	t.Run("no repo path is not sudo-install, even with blanket sudo", func(t *testing.T) {
		opts, r := sudoOpts(t)
		opts.RepoPath = ""
		// Blanket sudo: every sudo -l query succeeds.
		r.allowed = nil
		blanket := &blanketRunner{}
		opts.Runner = blanket
		inst := Detect(ctx, opts)
		st := inst.Status()
		if inst.Name() != StrategyUnsupported || st.Supported {
			t.Fatalf("Detect = %s %+v", inst.Name(), st)
		}
		if blanket.calls != 0 {
			t.Fatalf("sudo consulted without a repo path: %d calls", blanket.calls)
		}
		if st.MigrateHint == "" || !strings.Contains(st.Reason, "no maintenance repo path") {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("unsupported reports reasons and hint", func(t *testing.T) {
		opts, _ := sudoOpts(t, rulePlain)
		inst := Detect(ctx, opts)
		st := inst.Status()
		if st.Strategy != StrategyUnsupported || st.Supported {
			t.Fatalf("status = %+v", st)
		}
		if !strings.Contains(st.Reason, "symlink layout:") || !strings.Contains(st.Reason, "sudo-install:") {
			t.Fatalf("reason = %q", st.Reason)
		}
		if st.MigrateHint != MigrateLayoutHint {
			t.Fatalf("hint = %q", st.MigrateHint)
		}
		err := inst.Preflight(ctx)
		if !errors.Is(err, ErrInstallerUnsupported) || !strings.Contains(err.Error(), "migrate-layout") {
			t.Fatalf("Preflight = %v", err)
		}
		if _, err := inst.Stage(ctx, "x", "y"); err == nil {
			t.Fatal("Stage succeeded")
		}
		if inst.Activate(ctx, "x") == nil || inst.Restart(ctx) == nil || inst.Retained() != nil {
			t.Fatal("unsupported installer allowed an action")
		}
	})

	t.Run("non-linux", func(t *testing.T) {
		opts := symlinkOpts(installLayout(t, "nightly-20261022"))
		opts.GOOS = "darwin"
		if inst := Detect(ctx, opts); inst.Name() != StrategyUnsupported {
			t.Fatalf("Detect on darwin = %s", inst.Name())
		}
	})
}

// blanketRunner allows every sudo command, like google-sudoers.
type blanketRunner struct{ calls int }

func (b *blanketRunner) Run(context.Context, string, ...string) ([]byte, error) {
	b.calls++
	return nil, nil
}
func (b *blanketRunner) Start(string, ...string) (<-chan error, error) {
	b.calls++
	return make(chan error), nil
}

// shellStartRunner is a fakeRunner whose started commands are replaced by
// `sh -c script`, run for real so the exit result is a genuine exec error.
type shellStartRunner struct {
	*fakeRunner
	script string
}

func (r shellStartRunner) Start(name string, args ...string) (<-chan error, error) {
	_, _ = r.fakeRunner.Start(name, args...)
	return ExecRunner{}.Start("sh", "-c", r.script)
}
