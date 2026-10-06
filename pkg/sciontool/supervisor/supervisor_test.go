/*
Copyright 2025 The Scion Authors.
*/

package supervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// captureStderr redirects os.Stderr for the duration of fn and returns
// everything written to it. log.write always writes to whatever os.Stderr
// currently is (read fresh on each call, never cached), so this needs no
// change to the log package itself. Not safe to run with t.Parallel().
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	log.SetQuiet(false)
	t.Cleanup(func() { log.SetQuiet(false) })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = orig
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

func TestSupervisor_RunSuccessfulCommand(t *testing.T) {
	config := DefaultConfig()
	sup := New(config)

	ctx := context.Background()
	exitCode, err := sup.Run(ctx, []string{"true"})

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("expected exit code 0, got %d", exitCode)
	}
}

func TestSupervisor_RunFailingCommand(t *testing.T) {
	config := DefaultConfig()
	sup := New(config)

	ctx := context.Background()
	exitCode, err := sup.Run(ctx, []string{"false"})

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

// TestSupervisor_RunWithWorkingDir proves Config.WorkingDir sets the child's
// cmd.Dir: the substrate harness working directory (see
// commands.InitRunOptions.WorkingDir) only reaches the actual OS process if
// this field is honoured here. Using a relative-path file creation ("touch
// marker") rather than reading os.Stdout is deliberate: Run hardcodes
// s.cmd.Stdout = os.Stdout, so the only externally observable proof of the
// child's cwd is where a relative-path side effect lands.
func TestSupervisor_RunWithWorkingDir(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	config := DefaultConfig()
	config.WorkingDir = dir
	sup := New(config)

	ctx := context.Background()
	exitCode, err := sup.Run(ctx, []string{"sh", "-c", "touch marker"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "marker")); statErr != nil {
		t.Errorf("marker file not found in WorkingDir %s: %v (child did not start with the configured cwd)", dir, statErr)
	}
}

// TestSupervisor_RunWithoutWorkingDir_LeavesCmdDirUnset proves the
// non-substrate path is unchanged: when Config.WorkingDir is left at its
// zero value (every caller today except substrate-serve), the child must
// NOT be forced into any particular directory — it inherits this process's
// own cwd. A relative-path side effect
// (rather than asserting exec.Cmd.Dir directly, which is only observable
// during Run) proves the child actually ran with the supervisor process's
// own cwd rather than some other, unexpected directory.
func TestSupervisor_RunWithoutWorkingDir_LeavesCmdDirUnset(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	config := DefaultConfig() // WorkingDir left unset (zero value)
	sup := New(config)

	ctx := context.Background()
	exitCode, err := sup.Run(ctx, []string{"sh", "-c", "touch marker"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "marker")); statErr != nil {
		t.Errorf("marker file not found in this process's own cwd %s: %v (WorkingDir==\"\" must inherit the supervisor's cwd, not change it)", dir, statErr)
	}
}

// TestSupervisor_RunWithWorkingDir_SetsPWD proves that with a symlinked
// WorkingDir the child sees PWD set to the logical (symlinked) path, not
// the physical path getcwd() would report. sh, tmux and Node's
// process.cwd() prefer the inherited PWD over getcwd() only when the two
// resolve to the same physical directory; for a plain (non-symlinked)
// directory the shell recomputes PWD via getcwd() regardless of whether
// supervisor sets it, so this test uses a symlink to make the PWD-setting
// code the only thing that can produce the expected value.
func TestSupervisor_RunWithWorkingDir_SetsPWD(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "ws-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("os.Symlink(%s, %s): %v", real, link, err)
	}
	out := filepath.Join(real, "env.out")
	config := DefaultConfig()
	config.WorkingDir = link
	sup := New(config)

	ctx := context.Background()
	exitCode, err := sup.Run(ctx, []string{"sh", "-c", "printf '%s' \"$PWD\" > " + out})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	got, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("reading child's captured PWD: %v", readErr)
	}
	if string(got) != link {
		t.Errorf("child PWD = %q, want %q (the symlinked WorkingDir, not its physical target)", got, link)
	}
}

// TestSupervisor_RunWithoutWorkingDir_DoesNotForcePWD proves the scoping:
// callers that leave WorkingDir at its zero value (every runtime other than
// substrate) must not get a PWD override at all. Inspects the constructed
// exec.Cmd.Env directly (this test is in-package) rather than round-tripping
// through a real shell, since a real shell independently recomputes $PWD via
// getcwd() whenever the inherited value doesn't match the process's actual
// cwd — that shell behavior, not supervisor.Run, would otherwise be what the
// test observed.
func TestSupervisor_RunWithoutWorkingDir_DoesNotForcePWD(t *testing.T) {
	wantPWD := os.Getenv("PWD")

	config := DefaultConfig() // WorkingDir left unset (zero value)
	sup := New(config)

	ctx := context.Background()
	exitCode, err := sup.Run(ctx, []string{"true"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	if got := getEnvVar(sup.cmd.Env, "PWD"); got != wantPWD {
		t.Errorf("child env PWD = %q, want the unchanged ambient value %q (WorkingDir==\"\" must not touch PWD)", got, wantPWD)
	}
}

func TestSupervisor_RunNoCommand(t *testing.T) {
	config := DefaultConfig()
	sup := New(config)

	ctx := context.Background()
	exitCode, err := sup.Run(ctx, []string{})

	if err != ErrNoCommand {
		t.Errorf("expected ErrNoCommand, got %v", err)
	}
	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

func TestSupervisor_RunNonExistentCommand(t *testing.T) {
	config := DefaultConfig()
	sup := New(config)

	ctx := context.Background()
	exitCode, err := sup.Run(ctx, []string{"/nonexistent/command/that/does/not/exist"})

	if err == nil {
		t.Error("expected error for non-existent command")
	}
	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

func TestSupervisor_RunWithSpecificExitCode(t *testing.T) {
	config := DefaultConfig()
	sup := New(config)

	ctx := context.Background()
	exitCode, err := sup.Run(ctx, []string{"sh", "-c", "exit 42"})

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if exitCode != 42 {
		t.Errorf("expected exit code 42, got %d", exitCode)
	}
}

func TestSupervisor_ContextCancellation(t *testing.T) {
	config := Config{
		GracePeriod: 100 * time.Millisecond,
	}
	sup := New(config)

	ctx, cancel := context.WithCancel(context.Background())

	// Start a long-running command
	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = sup.Run(ctx, []string{"sleep", "60"})
		close(done)
	}()

	// Wait until the child is actually running: Signal is a silent no-op
	// before then, so a fixed sleep made this flaky under load.
	waitStarted(t, sup, done)

	// Cancel the context
	cancel()

	// Wait for supervisor to complete
	select {
	case <-done:
		// Expected
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not complete after context cancellation")
	}

	if runErr != nil {
		t.Errorf("unexpected error: %v", runErr)
	}
	// Exit code depends on how the process was terminated
	// We just verify it completed
}

// waitStarted blocks until sup has started its child, failing the test if
// Run returns first or the child does not start within 5s.
func waitStarted(t *testing.T, sup *Supervisor, runDone <-chan struct{}) {
	t.Helper()
	select {
	case <-sup.Started():
	case <-runDone:
		t.Fatal("Run returned before the child process started")
	case <-time.After(5 * time.Second):
		t.Fatal("child process did not start within 5s")
	}
}

func TestSupervisor_Signal(t *testing.T) {
	config := Config{
		GracePeriod: 100 * time.Millisecond,
	}
	sup := New(config)

	ctx := context.Background()

	// Start a long-running command
	done := make(chan struct{})
	go func() {
		_, _ = sup.Run(ctx, []string{"sleep", "60"})
		close(done)
	}()

	// Wait until the child is actually running: Signal is a silent no-op
	// before then, so a fixed sleep made this flaky under load.
	waitStarted(t, sup, done)

	// Send SIGTERM
	if err := sup.Signal(syscall.SIGTERM); err != nil {
		t.Errorf("failed to send signal: %v", err)
	}

	// Wait for process to exit
	select {
	case <-done:
		// Expected
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after SIGTERM")
	}
}

func TestSupervisor_StartedNotClosedWhenStartFails(t *testing.T) {
	sup := New(DefaultConfig())
	if _, err := sup.Run(context.Background(), []string{"/nonexistent/command"}); err == nil {
		t.Fatal("expected Run to fail for a nonexistent command")
	}
	select {
	case <-sup.Started():
		t.Fatal("Started closed even though the child never started")
	default:
	}
}

// TestSupervisor_SecondRunAfterStartReturnsErrAlreadyStarted pins that a
// second Run on a Supervisor whose child has started returns
// ErrAlreadyStarted instead of panicking on the already-closed startedCh.
func TestSupervisor_SecondRunAfterStartReturnsErrAlreadyStarted(t *testing.T) {
	sup := New(DefaultConfig())
	if code, err := sup.Run(context.Background(), []string{"true"}); err != nil || code != 0 {
		t.Fatalf("first Run: code=%d err=%v, want 0, nil", code, err)
	}

	var (
		code int
		err  error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("second Run panicked: %v", r)
			}
		}()
		code, err = sup.Run(context.Background(), []string{"true"})
	}()
	if !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Run: err=%v, want ErrAlreadyStarted", err)
	}
	if code != 1 {
		t.Errorf("second Run: code=%d, want 1", code)
	}
}

// TestSupervisor_RunRefusedWhileClaimed pins the running claim itself,
// independently of s.started: while another Run holds the claim (here set
// directly, as an in-progress Run that has not started its child yet would),
// Run returns ErrAlreadyStarted, leaves the claim in place for its owner, and
// does not close Started.
func TestSupervisor_RunRefusedWhileClaimed(t *testing.T) {
	sup := New(DefaultConfig())
	sup.mu.Lock()
	sup.running = true
	sup.mu.Unlock()

	if _, err := sup.Run(context.Background(), []string{"true"}); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("Run while claimed: err=%v, want ErrAlreadyStarted", err)
	}
	sup.mu.Lock()
	running, started := sup.running, sup.started
	sup.mu.Unlock()
	if !running {
		t.Error("a refused Run must not release the claim held by another Run")
	}
	if started {
		t.Error("a refused Run must not mark the Supervisor started")
	}
	select {
	case <-sup.Started():
		t.Fatal("Started closed by a refused Run")
	default:
	}
}

// TestSupervisor_RunRetryAfterFailedStart pins that a Run that fails before
// starting the child does not use up the Supervisor: a later Run may still
// start one.
func TestSupervisor_RunRetryAfterFailedStart(t *testing.T) {
	sup := New(DefaultConfig())
	if _, err := sup.Run(context.Background(), []string{"/nonexistent/command"}); err == nil {
		t.Fatal("expected the first Run to fail for a nonexistent command")
	}
	if code, err := sup.Run(context.Background(), []string{"true"}); err != nil || code != 0 {
		t.Fatalf("retry Run: code=%d err=%v, want 0, nil", code, err)
	}
	select {
	case <-sup.Started():
	default:
		t.Fatal("Started not closed after the retried Run started its child")
	}
}

// TestSupervisor_ConcurrentRunWhileRunningReturnsErrAlreadyStarted pins that
// a Run racing an in-progress Run is refused rather than starting a second
// child.
func TestSupervisor_ConcurrentRunWhileRunningReturnsErrAlreadyStarted(t *testing.T) {
	sup := New(Config{GracePeriod: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = sup.Run(ctx, []string{"sleep", "30"})
	}()
	select {
	case <-sup.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("first Run did not start its child")
	}

	if _, err := sup.Run(context.Background(), []string{"true"}); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("concurrent Run: err=%v, want ErrAlreadyStarted", err)
	}

	cancel()
	select {
	case <-firstDone:
	case <-time.After(15 * time.Second):
		t.Fatal("first Run did not return after cancellation")
	}
}

func TestSupervisor_Done(t *testing.T) {
	config := DefaultConfig()
	sup := New(config)

	ctx := context.Background()

	go func() { _, _ = sup.Run(ctx, []string{"true"}) }()

	select {
	case <-sup.Done():
		// Expected
	case <-time.After(5 * time.Second):
		t.Fatal("Done channel not closed after process exit")
	}
}

func TestSupervisor_ExitCode(t *testing.T) {
	config := DefaultConfig()
	sup := New(config)

	ctx := context.Background()
	_, _ = sup.Run(ctx, []string{"sh", "-c", "exit 7"})

	if code := sup.ExitCode(); code != 7 {
		t.Errorf("expected exit code 7, got %d", code)
	}
}

func TestSupervisor_RootlessEnvVars(t *testing.T) {
	// In rootless mode, the supervisor should set HOME/USER/LOGNAME
	// to the scion user without dropping privileges via Credential.
	config := Config{
		GracePeriod: 10 * time.Second,
		Username:    "scion",
		Rootless:    true,
		// UID and GID are 0 (no privilege drop)
	}
	sup := New(config)

	ctx := context.Background()
	// Run a command that prints the HOME env var
	exitCode, err := sup.Run(ctx, []string{"sh", "-c", "echo $HOME"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("expected exit code 0, got %d", exitCode)
	}
	// We can't easily capture stdout in this test harness, but we verify
	// the command ran successfully with rootless config. The env var
	// setting is tested via the unit test for setEnvVar.
}

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()

	if config.GracePeriod != 10*time.Second {
		t.Errorf("expected default grace period 10s, got %s", config.GracePeriod)
	}
}

func TestSetEnvVar(t *testing.T) {
	t.Run("replaces existing var", func(t *testing.T) {
		env := []string{"FOO=bar", "PATH=/usr/bin"}
		env = setEnvVar(env, "FOO", "baz")
		if len(env) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(env))
		}
		if env[0] != "FOO=baz" {
			t.Errorf("expected FOO=baz, got %s", env[0])
		}
	})

	t.Run("appends new var", func(t *testing.T) {
		env := []string{"FOO=bar"}
		env = setEnvVar(env, "NEW", "value")
		if len(env) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(env))
		}
		if env[1] != "NEW=value" {
			t.Errorf("expected NEW=value, got %s", env[1])
		}
	})
}

func TestGetEnvVar(t *testing.T) {
	env := []string{"FOO=bar", "EMPTY=", "PATH=/usr/bin:/usr/local/bin"}

	t.Run("found", func(t *testing.T) {
		if got := getEnvVar(env, "FOO"); got != "bar" {
			t.Errorf("expected 'bar', got %q", got)
		}
	})

	t.Run("not found", func(t *testing.T) {
		if got := getEnvVar(env, "MISSING"); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("empty value", func(t *testing.T) {
		if got := getEnvVar(env, "EMPTY"); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("value with special chars", func(t *testing.T) {
		if got := getEnvVar(env, "PATH"); got != "/usr/bin:/usr/local/bin" {
			t.Errorf("expected '/usr/bin:/usr/local/bin', got %q", got)
		}
	})
}

func TestRemoveEnvVar(t *testing.T) {
	t.Run("removes present var", func(t *testing.T) {
		env := []string{"FOO=bar", "BAZ=qux", "PATH=/usr/bin"}
		result := removeEnvVar(env, "BAZ")
		if len(result) != 2 {
			t.Fatalf("expected 2 entries, got %d: %v", len(result), result)
		}
		for _, e := range result {
			if e == "BAZ=qux" {
				t.Error("BAZ should have been removed")
			}
		}
	})

	t.Run("no-op for absent var", func(t *testing.T) {
		env := []string{"FOO=bar", "PATH=/usr/bin"}
		result := removeEnvVar(env, "MISSING")
		if len(result) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(result))
		}
	})
}

func TestApplyExtraPath(t *testing.T) {
	t.Run("prepends to existing PATH", func(t *testing.T) {
		env := []string{"PATH=/usr/bin", "SCION_EXTRA_PATH=/home/scion/bin"}
		extraPath := getEnvVar(env, "SCION_EXTRA_PATH")
		if extraPath == "" {
			t.Fatal("SCION_EXTRA_PATH not found")
		}
		currentPath := getEnvVar(env, "PATH")
		newPath := extraPath + ":" + currentPath
		env = setEnvVar(env, "PATH", newPath)
		env = removeEnvVar(env, "SCION_EXTRA_PATH")

		if got := getEnvVar(env, "PATH"); got != "/home/scion/bin:/usr/bin" {
			t.Errorf("expected '/home/scion/bin:/usr/bin', got %q", got)
		}
		if got := getEnvVar(env, "SCION_EXTRA_PATH"); got != "" {
			t.Errorf("SCION_EXTRA_PATH should be removed, got %q", got)
		}
	})

	t.Run("handles missing PATH", func(t *testing.T) {
		env := []string{"SCION_EXTRA_PATH=/home/scion/bin"}
		extraPath := getEnvVar(env, "SCION_EXTRA_PATH")
		currentPath := getEnvVar(env, "PATH")
		var newPath string
		if currentPath != "" {
			newPath = extraPath + ":" + currentPath
		} else {
			newPath = extraPath
		}
		env = setEnvVar(env, "PATH", newPath)
		env = removeEnvVar(env, "SCION_EXTRA_PATH")

		if got := getEnvVar(env, "PATH"); got != "/home/scion/bin" {
			t.Errorf("expected '/home/scion/bin', got %q", got)
		}
	})

	t.Run("handles multiple colon-separated entries", func(t *testing.T) {
		env := []string{"PATH=/usr/bin", "SCION_EXTRA_PATH=/home/scion/bin:/home/scion/.local/bin"}
		extraPath := getEnvVar(env, "SCION_EXTRA_PATH")
		currentPath := getEnvVar(env, "PATH")
		newPath := extraPath + ":" + currentPath
		env = setEnvVar(env, "PATH", newPath)
		env = removeEnvVar(env, "SCION_EXTRA_PATH")

		if got := getEnvVar(env, "PATH"); got != "/home/scion/bin:/home/scion/.local/bin:/usr/bin" {
			t.Errorf("expected '/home/scion/bin:/home/scion/.local/bin:/usr/bin', got %q", got)
		}
	})

	t.Run("no SCION_EXTRA_PATH is no-op", func(t *testing.T) {
		env := []string{"PATH=/usr/bin", "FOO=bar"}
		extraPath := getEnvVar(env, "SCION_EXTRA_PATH")
		if extraPath != "" {
			t.Fatal("should not have found SCION_EXTRA_PATH")
		}
		// PATH should remain unchanged
		if got := getEnvVar(env, "PATH"); got != "/usr/bin" {
			t.Errorf("expected '/usr/bin', got %q", got)
		}
	})
}

func TestMergeEnvOverlay_Helper(t *testing.T) {
	t.Run("runtime env wins over overlay", func(t *testing.T) {
		env := []string{"FOO=runtime", "PATH=/usr/bin"}
		overlay := map[string]string{"FOO": "from-overlay", "BAR": "added"}
		got := mergeEnvOverlay(env, overlay)
		if v := getEnvVar(got, "FOO"); v != "runtime" {
			t.Errorf("expected runtime FOO to win, got %q", v)
		}
		if v := getEnvVar(got, "BAR"); v != "added" {
			t.Errorf("expected BAR appended, got %q", v)
		}
	})

	t.Run("nil overlay is passthrough", func(t *testing.T) {
		env := []string{"X=1"}
		got := mergeEnvOverlay(env, nil)
		if len(got) != 1 || got[0] != "X=1" {
			t.Fatalf("expected passthrough, got %v", got)
		}
	})

	t.Run("deterministic order", func(t *testing.T) {
		env := []string{}
		overlay := map[string]string{"B": "2", "A": "1", "C": "3"}
		got := mergeEnvOverlay(env, overlay)
		// Sorted alphabetically for reproducibility.
		want := []string{"A=1", "B=2", "C=3"}
		if len(got) != len(want) {
			t.Fatalf("len mismatch: %v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
			}
		}
	})
}

// TestChownRecursive_ChownsUnconditionallyAndSurvivesSymlink is a thin
// call-site test proving chownRecursive delegates to the shared
// dirfd.ChownTreeNoFollow walk unconditionally (every entry, not just
// root-owned ones — unlike chownTreeRootOwned) and never follows a symlink.
// The deeper intermediate-directory-swap race itself is covered once,
// thoroughly, at the dirfd level
// (TestChownTreeNoFollow_SurvivesIntermediateDirSwapMidWalk).
func TestChownRecursive_ChownsUnconditionallyAndSurvivesSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	victimFile := filepath.Join(victim, "secret")
	if err := os.WriteFile(victimFile, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	before := lstatCtime(t, filepath.Join(root, "a"))
	victimBefore := lstatCtime(t, victimFile)
	waitForCtimeAfter(t, before)

	uid, gid := os.Getuid(), os.Getgid()
	if err := chownRecursive(root, uid, gid, false); err != nil {
		t.Fatalf("chownRecursive: %v", err)
	}

	if lstatCtime(t, filepath.Join(root, "a")) == before {
		t.Error("expected \"a\" to be chowned (unconditional, unlike chownTreeRootOwned's root-owned-only filter)")
	}
	if lstatCtime(t, victimFile) != victimBefore {
		t.Error("victim file behind the symlink was chowned — the symlink was followed")
	}
}

// TestChownRecursive_Enforced_SkipsHardlinkedRegularFile proves the
// hard-link guard is enabled when requirePrivilegeDrop is true: a regular
// file with more than one hard link is left unchowned, and the skip is
// logged at the real WARN level (log.Warn), not an Info line carrying an
// inline "WARN:" substring.
func TestChownRecursive_Enforced_SkipsHardlinkedRegularFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	before := lstatCtime(t, target)
	waitForCtimeAfter(t, before)

	uid, gid := os.Getuid(), os.Getgid()
	var runErr error
	output := captureStderr(t, func() {
		runErr = chownRecursive(root, uid, gid, true)
	})
	if runErr != nil {
		t.Fatalf("chownRecursive: %v", runErr)
	}
	if lstatCtime(t, target) != before {
		t.Error("hard-linked file was chowned despite requirePrivilegeDrop=true")
	}
	if !strings.Contains(output, "[sciontool] WARN:") {
		t.Errorf("expected the hard-link skip to be logged at WARN level, got: %s", output)
	}
	if strings.Contains(output, "INFO: WARN:") {
		t.Errorf("expected a real WARN log line, not Info with an inline WARN substring, got: %s", output)
	}
}

// TestChownRecursive_NonEnforced_ChownsHardlinkedRegularFile proves the
// gating's other half: the hard-link guard is disabled (historical
// behaviour) when requirePrivilegeDrop is false.
func TestChownRecursive_NonEnforced_ChownsHardlinkedRegularFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	before := lstatCtime(t, target)
	waitForCtimeAfter(t, before)

	uid, gid := os.Getuid(), os.Getgid()
	if err := chownRecursive(root, uid, gid, false); err != nil {
		t.Fatalf("chownRecursive: %v", err)
	}
	if lstatCtime(t, target) == before {
		t.Error("expected the hard-linked file to be chowned when requirePrivilegeDrop is false")
	}
}

// waitForCtimeAfter blocks until a freshly changed file would get a ctime
// strictly later than before. The kernel stamps ctime from a coarse clock,
// so a fixed sleep cannot guarantee that a chown right after the "before"
// snapshot produces a different ctime; probing the clock directly does.
func waitForCtimeAfter(t *testing.T, before syscall.Timespec) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "ctime-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for mode := os.FileMode(0o600); ; mode ^= 0o100 {
		if err := os.Chmod(probe, mode); err != nil {
			t.Fatal(err)
		}
		now := lstatCtime(t, probe)
		if now.Sec > before.Sec || (now.Sec == before.Sec && now.Nsec > before.Nsec) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ctime clock did not advance past %v within 5s", before)
		}
		time.Sleep(time.Millisecond)
	}
}

func lstatCtime(t *testing.T, path string) syscall.Timespec {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no *syscall.Stat_t for %s", path)
	}
	return statCtime(st)
}

// TestSupervisor_Run_RequirePrivilegeDropRefusesUndroppableCredentials proves
// that, with RequirePrivilegeDrop set, a Config whose UID or GID
// fails the credential drop's own UID>0 && GID>0 predicate must make Run
// return (1, ErrPrivilegeDropRequired) WITHOUT starting the child, rather
// than silently running it with no Credential (i.e. as whatever this
// process is, root in production). The child would create a marker file;
// its absence proves nothing was executed.
func TestSupervisor_Run_RequirePrivilegeDropRefusesUndroppableCredentials(t *testing.T) {
	cases := []struct {
		name     string
		uid, gid int
	}{
		{name: "uid0", uid: 0, gid: 1000},
		{name: "gid0", uid: 1000, gid: 0},
		{name: "both0", uid: 0, gid: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			config := DefaultConfig()
			config.UID = tc.uid
			config.GID = tc.gid
			config.RequirePrivilegeDrop = true
			sup := New(config)

			exitCode, err := sup.Run(context.Background(), []string{"sh", "-c", "touch " + marker})
			if !errors.Is(err, ErrPrivilegeDropRequired) {
				t.Errorf("Run(UID=%d, GID=%d, RequirePrivilegeDrop) err = %v, want ErrPrivilegeDropRequired", tc.uid, tc.gid, err)
			}
			if exitCode != 1 {
				t.Errorf("exit code = %d, want 1", exitCode)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Errorf("the child ran (marker exists, stat err=%v); it must not start without a credential drop in enforced mode", statErr)
			}
		})
	}
}

// TestSupervisor_Run_NoRequirePrivilegeDropRunsWithoutCredentials is the
// enforced hard-error's non-enforced twin: with RequirePrivilegeDrop=false,
// every one of the same UID/GID pairs that refuses to run in enforced mode
// must still run the child without a Credential — including a non-root UID
// paired with a root (0) GID, which the credential-drop predicate itself
// (UID>0 && GID>0) does not treat the same as UID>0 alone. Non-substrate
// runtimes commonly pass a non-root UID with GID 0, so this pairing must
// stay a plain "no drop" rather than an error whenever enforcement is off.
func TestSupervisor_Run_NoRequirePrivilegeDropRunsWithoutCredentials(t *testing.T) {
	cases := []struct {
		name     string
		uid, gid int
	}{
		{name: "both0", uid: 0, gid: 0},
		{name: "gid0", uid: 1000, gid: 0},
		{name: "uid0", uid: 0, gid: 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			config := DefaultConfig()
			config.UID = tc.uid
			config.GID = tc.gid
			sup := New(config)

			exitCode, err := sup.Run(context.Background(), []string{"sh", "-c", "touch " + marker})
			if err != nil || exitCode != 0 {
				t.Fatalf("Run(UID=%d, GID=%d) = (%d, %v), want (0, nil)", tc.uid, tc.gid, exitCode, err)
			}
			if _, statErr := os.Stat(marker); statErr != nil {
				t.Errorf("child did not run: %v", statErr)
			}
		})
	}
}

// TestSupervisor_Run_RequirePrivilegeDropIgnoresRootless pins that the
// enforced-mode refusal above does not exempt Config.Rootless: Rootless only
// selects which HOME/USER/LOGNAME env block the child gets (see Run's own
// env-setting block), it never authorises starting an un-dropped child when
// RequirePrivilegeDrop is set. A UID/GID pair that fails the credential
// drop's own predicate must still refuse and must still not start the child,
// regardless of Rootless.
func TestSupervisor_Run_RequirePrivilegeDropIgnoresRootless(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	config := DefaultConfig()
	config.RequirePrivilegeDrop = true
	config.Rootless = true
	config.Username = "scion"
	exitCode, err := New(config).Run(context.Background(), []string{"sh", "-c", "touch " + marker})
	if !errors.Is(err, ErrPrivilegeDropRequired) || exitCode != 1 {
		t.Fatalf("Run(Rootless, RequirePrivilegeDrop) = (%d, %v), want (1, ErrPrivilegeDropRequired)", exitCode, err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Errorf("child ran despite RequirePrivilegeDrop")
	}
}
