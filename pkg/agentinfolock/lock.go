/*
Copyright 2026 The Scion Authors.
*/

// Package agentinfolock serializes read-modify-write updates of an agent's
// agent-info.json across processes (ptone/scion#4681).
//
// Several processes update agent-info.json: the broker or CLI on the host
// (pkg/agent) and, inside the container, sciontool init (PID 1), every
// `sciontool hook` invocation and `sciontool status`. Each update reads the
// whole file, changes a few fields and writes the whole file back through a
// temp file and rename. Without a lock, two updates that overlap lose one
// side's fields. On bind-mount runtimes the host and the container share the
// file, so the race also crosses that boundary.
//
// Update takes an advisory flock(2) on a sibling lock file,
// agent-info.json.lock, in the same directory. The lock cannot be on
// agent-info.json itself, because the rename replaces its inode.
//
// The lock is best effort and never blocks or fails an update: hooks and the
// broker's agent list must stay non-blocking. When the lock file is not a
// single-link regular file (the workload owns the agent home and can put
// anything there), or cannot be opened or locked, the update runs without
// the lock. A warning naming the file type and path is logged the first time
// for each lock path in a process, and at debug after that. When another
// holder keeps the lock longer than the wait, the update also runs without
// it (logged at debug). The unlocked path calls the same rmw func, so the
// read and write are the same either way.
//
// flock is advisory and per open file description, which covers processes on
// one kernel, including a Linux host and a bind-mounted container. It does
// not cover a host and container in different kernels (apple container,
// Docker Desktop on macOS); see ptone/scion#4681 Q1.
package agentinfolock

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// LockSuffix is appended to the agent-info.json path to name its lock file.
const LockSuffix = ".lock"

// DefaultWait bounds how long Update waits for another holder. It is well
// under the 2 s session-state lock wait, since the hook path must not stall.
const DefaultWait = 200 * time.Millisecond

// Update retries a held lock with a backoff from minPoll up to maxPoll. A
// short first interval matters: a holder that writes again at once takes
// the lock back within microseconds, and a waiter that only polls every few
// milliseconds can miss every gap and time out (lock starvation).
const (
	minPoll = 50 * time.Microsecond
	maxPoll = time.Millisecond
)

// Options tunes Update. The zero value uses DefaultWait, slog.Debug and
// slog.Warn.
type Options struct {
	// Wait bounds how long to wait for the lock. Zero means DefaultWait, or
	// in test binaries only, TestWaitEnv when it is set. A non-zero Wait
	// always takes precedence over TestWaitEnv.
	Wait time.Duration
	// Logf receives the timeout message. Nil means slog.Debug.
	Logf func(format string, args ...any)
	// Warnf receives the first message for a lock file that is not a
	// single-link regular file or cannot be opened or locked; repeats for
	// the same lock path go to Logf. Nil means slog.Warn (and slog.Debug
	// for repeats), with kind, path and error as attributes.
	Warnf func(format string, args ...any)
}

// TestWaitEnv sets the lock wait in test binaries only (it is ignored
// unless testing.Testing() reports true) when Options.Wait is zero; an
// explicit Options.Wait takes precedence. Tests that must never reach the
// unlocked fallback set it long, for example "10s". It is an environment
// variable rather than a Go API so the same tests also build against code
// without this package.
const TestWaitEnv = "SCION_TEST_AGENTINFO_LOCK_WAIT"

func (o Options) wait() time.Duration {
	if o.Wait > 0 {
		return o.Wait
	}
	if testing.Testing() {
		if d, err := time.ParseDuration(os.Getenv(TestWaitEnv)); err == nil && d > 0 {
			return d
		}
	}
	return DefaultWait
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
		return
	}
	slog.Debug("agent-info lock", "detail", fmt.Sprintf(format, args...))
}

// Fixed messages for an unusable lock file, so a structured backend can
// match them; the kind, path and error go in attributes.
const (
	msgNotRegular = "agent-info lock file is not a single-link regular file, writing without lock"
	msgCannotOpen = "agent-info lock file cannot be opened, writing without lock"
	msgCannotLock = "agent-info lock file cannot be locked, writing without lock"
)

// warned records the lock paths already reported at Warn in this process.
// It grows by one entry per unusable lock path, at most one per agent.
var warned sync.Map

// warnUnusable reports an unusable lock file: at Warn the first time for
// lockPath in this process, at debug after that, so a lock file that stays
// unusable does not log a warning on every hook or update. kind (a file
// type word) and err may be empty.
func (o Options) warnUnusable(msg, kind, lockPath string, err error) {
	_, repeat := warned.LoadOrStore(lockPath, struct{}{})
	if o.Warnf != nil || (repeat && o.Logf != nil) {
		text := msg + ": " + lockPath
		if kind != "" {
			text = fmt.Sprintf("%s (%s): %s", msg, kind, lockPath)
		}
		if err != nil {
			text += ": " + err.Error()
		}
		if repeat {
			o.logf("%s", text)
		} else {
			o.Warnf("%s", text)
		}
		return
	}
	attrs := []any{"path", lockPath}
	if kind != "" {
		attrs = append(attrs, "kind", kind)
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	if repeat {
		slog.Debug(msg, attrs...)
	} else {
		slog.Warn(msg, attrs...)
	}
}

// LockPath returns the lock file path for an agent-info.json path.
func LockPath(infoPath string) string {
	return filepath.Join(filepath.Dir(infoPath), filepath.Base(infoPath)+LockSuffix)
}

// Update runs rmw, a read-modify-write of the file at infoPath, while
// holding the agent-info lock when it can be taken. It returns rmw's error.
// It never fails or blocks beyond opts.Wait because of the lock itself.
func Update(infoPath string, opts Options, rmw func() error) error {
	unlock, ok := acquire(LockPath(infoPath), opts)
	if ok {
		defer unlock()
	}
	return rmw()
}
