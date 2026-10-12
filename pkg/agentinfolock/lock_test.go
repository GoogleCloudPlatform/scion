//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package agentinfolock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type logSink struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, fmt.Sprintf(format, args...))
}

func (l *logSink) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.msgs, "\n")
}

func TestUpdateHoldsLockDuringRMW(t *testing.T) {
	info := filepath.Join(t.TempDir(), "agent-info.json")
	var sink logSink
	err := Update(info, Options{Logf: sink.logf}, func() error {
		f, err := os.OpenFile(LockPath(info), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("lock not held during rmw: flock err = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.joined(); got != "" {
		t.Fatalf("unexpected log: %s", got)
	}
	fi, err := os.Lstat(LockPath(info))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o666 {
		t.Fatalf("lock file = %v, %v; want regular 0666", fi, err)
	}
}

func TestUpdateReturnsRMWError(t *testing.T) {
	want := errors.New("boom")
	if err := Update(filepath.Join(t.TempDir(), "agent-info.json"), Options{}, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("Update() = %v, want %v", err, want)
	}
}

func TestUpdateTimeoutLogsAndRunsUnlocked(t *testing.T) {
	// An explicit Options.Wait takes precedence over the test env wait.
	t.Setenv(TestWaitEnv, "10s")
	info := filepath.Join(t.TempDir(), "agent-info.json")
	f, err := os.OpenFile(LockPath(info), os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var sink logSink
	ran := false
	start := time.Now()
	if err := Update(info, Options{Wait: 30 * time.Millisecond, Logf: sink.logf}, func() error { ran = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond || elapsed > time.Second {
		t.Fatalf("elapsed %s, want about the 30ms wait", elapsed)
	}
	if !ran || !strings.Contains(sink.joined(), "timed out") {
		t.Fatalf("ran=%v log=%q", ran, sink.joined())
	}
}

func TestUpdateSkipsNonRegularLockAndLogs(t *testing.T) {
	cases := []struct {
		kind  string
		plant func(t *testing.T, lockPath string)
		check func(t *testing.T, lockPath string)
	}{
		{
			// The symlink points at an existing regular file, so code that
			// followed it would open and lock that file instead of warning.
			kind: "symlink",
			plant: func(t *testing.T, lockPath string) {
				target := filepath.Join(t.TempDir(), "elsewhere")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, lockPath); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, lockPath string) {
				target, err := os.Readlink(lockPath)
				if err != nil {
					t.Fatalf("lock path changed: %v", err)
				}
				fi, err := os.Lstat(target)
				if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || fi.Size() != 0 {
					t.Fatalf("symlink target changed: %v, %v", fi, err)
				}
			},
		},
		{
			kind: "fifo",
			plant: func(t *testing.T, lockPath string) {
				if err := unix.Mkfifo(lockPath, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			kind: "directory",
			plant: func(t *testing.T, lockPath string) {
				if err := os.Mkdir(lockPath, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// A regular file with a second link is refused too.
			kind: "other",
			plant: func(t *testing.T, lockPath string) {
				if err := os.WriteFile(lockPath, nil, 0o666); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(lockPath, lockPath+".2"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			info := filepath.Join(t.TempDir(), "agent-info.json")
			tc.plant(t, LockPath(info))
			var debug, warn logSink
			ran := false
			if err := Update(info, Options{Logf: debug.logf, Warnf: warn.logf}, func() error { ran = true; return nil }); err != nil {
				t.Fatal(err)
			}
			want := "agent-info lock file is not a regular file (" + tc.kind + "), writing without lock: " + LockPath(info)
			if !ran || warn.joined() != want {
				t.Fatalf("ran=%v warn=%q, want %q", ran, warn.joined(), want)
			}
			if got := debug.joined(); got != "" {
				t.Fatalf("unexpected debug log: %q", got)
			}
			if tc.check != nil {
				tc.check(t, LockPath(info))
			}
		})
	}
}

func TestUpdateMissingDirRunsUnlocked(t *testing.T) {
	info := filepath.Join(t.TempDir(), "missing", "agent-info.json")
	ran := false
	var warn logSink
	if err := Update(info, Options{Logf: func(string, ...any) {}, Warnf: warn.logf}, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("err=%v ran=%v", err, ran)
	}
	if got := warn.joined(); !strings.HasPrefix(got, "agent-info lock file cannot be opened, writing without lock: "+LockPath(info)) {
		t.Fatalf("warn = %q", got)
	}
}
