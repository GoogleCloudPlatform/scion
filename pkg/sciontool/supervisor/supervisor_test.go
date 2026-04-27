/*
Copyright 2025 The Scion Authors.
*/

package supervisor

import (
	"context"
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
		sup.Run(ctx, []string{"sleep", "60"})
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

	go sup.Run(ctx, []string{"true"})

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
	sup.Run(ctx, []string{"sh", "-c", "exit 7"})

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
