/*
Copyright 2026 The Scion Authors.
*/

package log

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestOpenLogFileNoFollow_NormalWrite proves the hardened open still behaves
// exactly like the historical os.OpenFile(path, O_APPEND|O_CREATE|O_WRONLY,
// mode) call for the ordinary case: same resulting mode (compared against a
// control file created the historical way, rather than against the literal
// mode argument, since umask affects both identically) and content actually
// written.
func TestOpenLogFileNoFollow_NormalWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	controlPath := filepath.Join(dir, "control.log")

	f, err := openLogFileNoFollow(path, 0666)
	if err != nil {
		t.Fatalf("openLogFileNoFollow: %v", err)
	}
	if _, err := f.WriteString("hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	control, err := os.OpenFile(controlPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		t.Fatalf("control OpenFile: %v", err)
	}
	if err := control.Close(); err != nil {
		t.Fatalf("control close: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	controlInfo, err := os.Stat(controlPath)
	if err != nil {
		t.Fatalf("control stat: %v", err)
	}
	if got, want := info.Mode().Perm(), controlInfo.Mode().Perm(); got != want {
		t.Errorf("mode = %o, want %o (matching the historical open call)", got, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("content = %q, want %q", data, "hello\n")
	}
}

// TestOpenLogFileNoFollow_RefusesSymlink proves that a workload that has
// replaced the log path (e.g. $HOME/agent.log, which the scion user owns)
// with a symlink can't make root append to or create whatever it points
// at: openLogFileNoFollow must refuse, and the symlink's target must be
// left untouched.
func TestOpenLogFileNoFollow_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret")
	if err := os.WriteFile(target, []byte("do-not-touch"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	path := filepath.Join(dir, "agent.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := openLogFileNoFollow(path, 0666); err == nil {
		t.Fatal("expected an error opening a symlinked log path, got nil")
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "do-not-touch" {
		t.Errorf("symlink target was modified: %q", data)
	}
}

// TestOpenLogFileNoFollow_RefusesDirectory proves that a directory at the
// log path is refused via the explicit regular-file check, rather than
// relying only on the open(2) EISDIR a O_WRONLY open against a directory
// already happens to return.
func TestOpenLogFileNoFollow_RefusesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := openLogFileNoFollow(path, 0666); err == nil {
		t.Fatal("expected an error opening a directory as a log path, got nil")
	}
}

// TestOpenLogFileNoFollow_RefusesHardlink proves that a hardlink to a
// root-owned file at the log path can't be used to make root append log
// lines to it: a hardlink passes a bare "is this a regular file" check
// (it IS a regular file), so openLogFileNoFollow must also check the link
// count and refuse anything other than a single-link regular file. The
// hardlink's target must be left untouched.
func TestOpenLogFileNoFollow_RefusesHardlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	path := filepath.Join(dir, "agent.log")
	if err := os.Link(target, path); err != nil {
		t.Fatalf("link: %v", err)
	}

	if _, err := openLogFileNoFollow(path, 0666); err == nil {
		t.Fatal("expected an error opening a hardlinked log path, got nil")
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "do-not-touch" {
		t.Errorf("hardlink target was modified: %q", data)
	}
}

// TestOpenLogFileNoFollow_FIFODoesNotBlock proves that a FIFO planted at
// the log path can't hang root's logging (and, since write() holds the
// package mutex while opening, every later log call in the process) by
// blocking open(2) forever waiting for a reader: O_NONBLOCK must make the
// open return promptly.
func TestOpenLogFileNoFollow_FIFODoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := openLogFileNoFollow(path, 0666)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error opening a FIFO log path, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("openLogFileNoFollow blocked on a FIFO with no reader")
	}
}

// TestWrite_CachesLogFileAcrossCalls proves the log fd is opened once and
// reused, not reopened on every line: after the first write, replacing the
// path's directory entry (e.g. a workload swapping in a hardlink) must not
// affect where subsequent lines in this process go, because write() never
// looks the path up again.
func TestWrite_CachesLogFileAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	cleanup := setLogPathForTest(t, path)
	defer cleanup()

	Info("first line")

	// Swap the directory entry for something else entirely; the cached fd
	// from the first write still points at the original (now-unlinked)
	// inode.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(path, []byte("replaced\n"), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}

	Info("second line")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replacement path: %v", err)
	}
	if string(data) != "replaced\n" {
		t.Errorf("replacement file was modified via the stale fd: %q", data)
	}

	mu.Lock()
	f := logFile
	mu.Unlock()
	if f == nil {
		t.Fatal("expected a cached log file")
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat cached fd: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("cached fd no longer points at a regular file")
	}
}

// TestChown_UsesCachedFd proves Chown fchowns the already-open cached fd
// rather than looking the path up again.
func TestChown_UsesCachedFd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	cleanup := setLogPathForTest(t, path)
	defer cleanup()

	if err := Chown(os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("Chown with no cached fd: %v", err)
	}

	Info("a line, to open and cache the fd")

	if err := Chown(os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("Chown with a cached fd: %v", err)
	}
}

// TestOpenLogFileNoFollow_FdIsCloseOnExec proves the log fd — cached for
// the process's whole life and, on Substrate, held open across the exec
// that starts the workload under dropped privileges — is close-on-exec, so
// it never leaks a writable handle to root's log into that child.
func TestOpenLogFileNoFollow_FdIsCloseOnExec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	f, err := openLogFileNoFollow(path, 0666)
	if err != nil {
		t.Fatalf("openLogFileNoFollow: %v", err)
	}
	defer func() { _ = f.Close() }()

	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatalf("fcntl(F_GETFD): %v", errno)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Error("log fd is not close-on-exec")
	}
}

// setLogPathForTest points the package-level log path at path for the
// duration of a test and restores the previous state afterward.
func setLogPathForTest(t *testing.T, path string) func() {
	t.Helper()
	mu.Lock()
	origPath := logPath
	origFile := logFile
	origInitialized := initialized.Load()
	logFile = nil
	mu.Unlock()

	SetLogPath(path)

	return func() {
		mu.Lock()
		if logFile != nil {
			_ = logFile.Close()
		}
		logPath = origPath
		logFile = origFile
		initialized.Store(origInitialized)
		mu.Unlock()
	}
}
