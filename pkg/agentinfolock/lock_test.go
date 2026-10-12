//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package agentinfolock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
		name  string
		kind  string
		plant func(t *testing.T, lockPath string)
		check func(t *testing.T, lockPath string)
	}{
		{
			// The symlink points at an existing regular file, so code that
			// followed it would open and lock that file instead of warning.
			name: "symlink",
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
			name: "fifo",
			kind: "fifo",
			plant: func(t *testing.T, lockPath string) {
				if err := unix.Mkfifo(lockPath, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory",
			kind: "directory",
			plant: func(t *testing.T, lockPath string) {
				if err := os.Mkdir(lockPath, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// A regular file with a second link is refused too. This goes
			// through the fstat after a successful open.
			name: "hardlink",
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
		{
			// A unix socket fails the open (ENXIO), so this goes through
			// the lstat after a failed open.
			name: "socket",
			kind: "other",
			plant: func(t *testing.T, lockPath string) {
				l, err := net.Listen("unix", lockPath)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Close() })
			},
		},
	}
	// The "cannot be locked" warning (flock failing with an error other
	// than EWOULDBLOCK or EINTR) is not covered: a unit test has no
	// portable way to make flock fail on a file it can open.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A short directory keeps the socket path under the
			// sun_path limit.
			dir, err := os.MkdirTemp("", "ailk")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			info := filepath.Join(dir, "agent-info.json")
			tc.plant(t, LockPath(info))
			var debug, warn logSink
			ran := false
			if err := Update(info, Options{Logf: debug.logf, Warnf: warn.logf}, func() error { ran = true; return nil }); err != nil {
				t.Fatal(err)
			}
			want := "agent-info lock file is not a single-link regular file (" + tc.kind + "), writing without lock: " + LockPath(info)
			if !ran || warn.joined() != want {
				t.Fatalf("ran=%v warn=%q, want %q", ran, warn.joined(), want)
			}
			if got := debug.joined(); got != "" {
				t.Fatalf("unexpected debug log: %q", got)
			}

			// A second update of the same path logs at debug, not Warn.
			ran = false
			if err := Update(info, Options{Logf: debug.logf, Warnf: warn.logf}, func() error { ran = true; return nil }); err != nil {
				t.Fatal(err)
			}
			if !ran || warn.joined() != want || debug.joined() != want {
				t.Fatalf("second update: ran=%v warn=%q debug=%q, want warn once and debug %q", ran, warn.joined(), debug.joined(), want)
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
	if got := warn.joined(); !strings.HasPrefix(got, "agent-info lock file cannot be opened, writing without lock: "+LockPath(info)+": ") {
		t.Fatalf("warn = %q", got)
	}
}

// TestUpdateWarnRearmsAfterRecovery checks the once-only record: each
// problem warns once per lock path, and taking the lock re-arms the Warn.
func TestUpdateWarnRearmsAfterRecovery(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "home")
	info := filepath.Join(dir, "agent-info.json")
	lock := LockPath(info)
	var debug, warn logSink
	opts := Options{Logf: debug.logf, Warnf: warn.logf}
	update := func() {
		t.Helper()
		if err := Update(info, opts, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	lines := func(l *logSink) []string {
		l.mu.Lock()
		defer l.mu.Unlock()
		return append([]string(nil), l.msgs...)
	}
	notRegular := "agent-info lock file is not a single-link regular file (fifo), writing without lock: " + lock

	// 1. The directory is missing: "cannot be opened" warns.
	update()
	if w := lines(&warn); len(w) != 1 || !strings.HasPrefix(w[0], "agent-info lock file cannot be opened, writing without lock: "+lock+": ") {
		t.Fatalf("step 1 warn = %q", w)
	}
	// 2. A fifo is a different problem on the same path: it warns too.
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(lock, 0o644); err != nil {
		t.Fatal(err)
	}
	update()
	if w := lines(&warn); len(w) != 2 || w[1] != notRegular {
		t.Fatalf("step 2 warn = %q", w)
	}
	// 3. The same problem again: debug only.
	update()
	if w, d := lines(&warn), lines(&debug); len(w) != 2 || len(d) != 1 || d[0] != notRegular {
		t.Fatalf("step 3 warn = %q debug = %q", w, d)
	}
	// 4. The fifo is gone: the lock is taken, nothing is logged.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	update()
	if w, d := lines(&warn), lines(&debug); len(w) != 2 || len(d) != 1 {
		t.Fatalf("step 4 warn = %q debug = %q", w, d)
	}
	// 5. The lock file breaks again: the Warn is re-armed.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(lock, 0o644); err != nil {
		t.Fatal(err)
	}
	update()
	if w := lines(&warn); len(w) != 3 || w[2] != notRegular {
		t.Fatalf("step 5 warn = %q", w)
	}
}

// recordingHandler keeps every slog record it is given.
type recordingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestUpdateDefaultSlogBranch covers the zero Options the host uses: a
// structured slog.Warn with path and kind, then a slog.Debug repeat.
func TestUpdateDefaultSlogBranch(t *testing.T) {
	rec := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })

	info := filepath.Join(t.TempDir(), "agent-info.json")
	lock := LockPath(info)
	if err := unix.Mkfifo(lock, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Update(info, Options{}, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}

	const msg = "agent-info lock file is not a single-link regular file, writing without lock"
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var got []string
	for _, r := range rec.recs {
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		got = append(got, fmt.Sprintf("%s|%s|path=%s|kind=%s|n=%d", r.Level, r.Message, attrs["path"], attrs["kind"], len(attrs)))
	}
	want := []string{
		fmt.Sprintf("WARN|%s|path=%s|kind=fifo|n=2", msg, lock),
		fmt.Sprintf("DEBUG|%s|path=%s|kind=fifo|n=2", msg, lock),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("slog records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
