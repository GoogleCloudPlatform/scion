/*
Copyright 2025 The Scion Authors.
*/

package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

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

	// Give the process time to start
	time.Sleep(50 * time.Millisecond)

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

	// Give the process time to start
	time.Sleep(50 * time.Millisecond)

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
	time.Sleep(15 * time.Millisecond)

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
// file with more than one hard link is left unchowned.
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
	time.Sleep(15 * time.Millisecond)

	uid, gid := os.Getuid(), os.Getgid()
	if err := chownRecursive(root, uid, gid, true); err != nil {
		t.Fatalf("chownRecursive: %v", err)
	}
	if lstatCtime(t, target) != before {
		t.Error("hard-linked file was chowned despite requirePrivilegeDrop=true")
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
	time.Sleep(15 * time.Millisecond)

	uid, gid := os.Getuid(), os.Getgid()
	if err := chownRecursive(root, uid, gid, false); err != nil {
		t.Fatalf("chownRecursive: %v", err)
	}
	if lstatCtime(t, target) == before {
		t.Error("expected the hard-linked file to be chowned when requirePrivilegeDrop is false")
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

// TestSupervisor_Run_NoRequirePrivilegeDropRunsWithoutCredentials proves
// that with RequirePrivilegeDrop left at its zero value, every one of a set
// of UID/GID pairs — including a non-root UID paired with a root (0) GID,
// which the credential-drop predicate itself (UID>0 && GID>0) does not
// treat the same as UID>0 alone — still runs the child without a
// Credential, a plain "no drop" rather than an error.
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
