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

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/internal/settingslockhook"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/filelock"
)

// settingsFileMu serialises settings-file read-modify-write cycles within
// this process. It is one mutex for every settings directory rather than one
// per path: settings.yaml, settings.yml and symlinks can name the same file,
// and writes are rare enough that a single lock costs nothing. Because it is
// process-wide, a process holds at most one settings-directory file lock at
// a time, so two processes can never wait on each other's locks in a cycle.
var settingsFileMu sync.Mutex

// settingsLockFileName is the lock file LockSettingsDir creates in a
// settings directory. It is created on first use, never removed, and kept
// out of git (see LockSettingsDir).
const settingsLockFileName = ".settings.lock"

// defaultSettingsLockWait bounds how long LockSettingsDir waits. Tests
// shorten it through internal/settingslockhook, the only override.
const defaultSettingsLockWait = 10 * time.Second

// ErrSettingsLockTimeout is returned (wrapped) by LockSettingsDir, and so by
// every settings write, when the settings lock is still held by someone else
// once the wait runs out.
var ErrSettingsLockTimeout = errors.New("timed out waiting for the settings lock")

// SettingsLock is a held settings-directory lock (see LockSettingsDir). The
// functions that replace a whole settings file take it as a parameter, so a
// settings write without the lock does not compile.
type SettingsLock struct {
	dir  string
	file *filelock.Lock // nil when the lock file could not be created
	held bool
}

// LockSettingsDir takes the settings lock for the settings directory dir
// (the global directory or a project's configuration directory) and returns
// it; call Unlock to release it. Hold it from the first read of a settings
// file in dir to the rename of its replacement, so that a concurrent writer's
// change is not lost. dir is created if it does not exist, since every
// caller is about to write a settings file there.
//
// The lock has two parts: the process-wide mutex settingsFileMu, and an
// exclusive flock on the file .settings.lock in dir (resolved through
// symlinks), which other processes take as well. The lock file is never
// removed. When this call creates it inside a git work tree, it is added to
// the repository's info/exclude (an untracked file), unless git already
// ignores it, so it never shows up as untracked.
//
// The wait for both parts is bounded by one deadline (10 s). When the lock
// is still held then, the error wraps ErrSettingsLockTimeout. The lock is not
// re-entrant: a nested LockSettingsDir in the same process (a bug) gets that
// error after the wait instead of deadlocking. The kernel releases the file
// lock when a process exits, so a crash cannot leave it stuck.
//
// If the lock file does not exist and cannot be created because the
// directory refuses new files (no write permission, or a read-only file
// system), only the in-process mutex is taken, with a warning. Writes there
// already fall back to rewriting the file in place (see
// writeSettingsFileAtomic) and are not coordinated across processes. An
// existing lock file this user may not write (created by another user) is
// opened read-only and still locks across processes.
//
// Readers do not need the lock: every settings write replaces the file with
// one rename, so a reader sees either the whole old file or the whole new
// one, never a partial file and never no file.
//
// Lock order: the settings lock is taken before any database write and may be
// held across database writes (the workstation server-config PUT holds it
// from reading settings.yaml, across its hub_settings writes, to the rename).
// Code holding a database transaction, or the hub server's s.mu, must
// therefore never take the settings lock.
func LockSettingsDir(dir string) (*SettingsLock, error) {
	wait := settingslockhook.Wait(defaultSettingsLockWait)
	deadline := time.Now().Add(wait)
	for !settingsFileMu.TryLock() {
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w for %s after %s: another settings write in this process did not finish (a nested settings write?)",
				ErrSettingsLockTimeout, dir, wait)
		}
		time.Sleep(2 * time.Millisecond)
	}
	l, err := lockSettingsDirFile(dir, deadline, wait)
	if err != nil {
		settingsFileMu.Unlock()
		return nil, err
	}
	return l, nil
}

// lockSettingsDirFile takes the cross-process part of LockSettingsDir. The
// caller holds settingsFileMu.
func lockSettingsDirFile(dir string, deadline time.Time, wait time.Duration) (*SettingsLock, error) {
	if err := os.MkdirAll(dir, 0755); err != nil && !isReadOnlyErr(err) {
		return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	lockDir := dir
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		lockDir = resolved
	}
	lockPath := filepath.Join(lockDir, settingsLockFileName)
	fl, err := filelock.Acquire(lockPath, 0644, time.Until(deadline))
	switch {
	case err == nil:
		if fl.Created {
			if gerr := util.EnsureGitIgnoresFile(lockDir, settingsLockFileName); gerr != nil {
				slog.Warn("settings: could not keep the settings lock file out of git", "path", lockPath, "error", gerr)
			}
		}
		return &SettingsLock{dir: dir, file: fl, held: true}, nil
	case errors.Is(err, filelock.ErrTimeout):
		return nil, fmt.Errorf("%w %s after %s: another process may be writing settings in %s; try again",
			ErrSettingsLockTimeout, lockPath, wait, dir)
	case isReadOnlyErr(err) && !dirWritable(lockDir):
		// The directory refuses new files (no write permission, or a
		// read-only file system) and has no lock file this user can open:
		// settings writes there fall back to rewriting the file in place,
		// and are coordinated within this process only.
		slog.Warn("settings: cannot create the settings lock file in a read-only directory; settings writes there are not coordinated with other processes",
			"path", lockPath, "error", err)
		return &SettingsLock{dir: dir, held: true}, nil
	case isReadOnlyErr(err):
		// An existing lock file this user cannot open, even read-only, in
		// a directory that is otherwise writable.
		return nil, fmt.Errorf("settings lock: cannot open the lock file %s: %w; check its owner and mode (it must be readable by this user), or remove it if no other process is using it", lockPath, err)
	default:
		return nil, fmt.Errorf("settings lock: %w", err)
	}
}

// tryLockSettingsDir takes the settings lock for dir only if it is free
// right now: it neither waits nor reports anything. ok is false when this
// process or another one holds the lock, or the lock file cannot be opened.
// The settings loaders use it for the legacy-key migration they run, which
// may happen while this process already holds the lock.
//
// dir must exist. In a directory that refuses new files and has no lock file,
// only the in-process mutex is taken, as LockSettingsDir does, without its
// warning.
func tryLockSettingsDir(dir string) (lock *SettingsLock, ok bool) {
	if !settingsFileMu.TryLock() {
		return nil, false
	}
	lockDir := dir
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		lockDir = resolved
	}
	lockPath := filepath.Join(lockDir, settingsLockFileName)
	fl, err := filelock.Acquire(lockPath, 0644, 0)
	switch {
	case err == nil:
		if fl.Created {
			if gerr := util.EnsureGitIgnoresFile(lockDir, settingsLockFileName); gerr != nil {
				slog.Warn("settings: could not keep the settings lock file out of git", "path", lockPath, "error", gerr)
			}
		}
		return &SettingsLock{dir: dir, file: fl, held: true}, true
	case isReadOnlyErr(err) && !dirWritable(lockDir):
		return &SettingsLock{dir: dir, held: true}, true
	default:
		settingsFileMu.Unlock()
		return nil, false
	}
}

// isReadOnlyErr reports whether err means the file system refused a new
// file: no permission, or a read-only file system.
func isReadOnlyErr(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// Unlock releases the lock. Calling it again does nothing.
func (l *SettingsLock) Unlock() {
	if l == nil || !l.held {
		return
	}
	l.held = false
	l.file.Unlock()
	settingsFileMu.Unlock()
}

// checkFor returns an error unless l is a held lock for the directory that
// contains path. Writers call it so that a released lock, or the lock of
// another directory, cannot be used for a write.
func (l *SettingsLock) checkFor(path string) error {
	if l == nil || !l.held {
		return fmt.Errorf("write of %s without the settings lock (internal error)", path)
	}
	if filepath.Clean(filepath.Dir(path)) != filepath.Clean(l.dir) {
		return fmt.Errorf("write of %s under the settings lock for %s (internal error)", path, l.dir)
	}
	return nil
}

// ErrSkipSave, returned by a LoadModifySaveVersionedSettings modify
// function, ends the cycle without writing and without error.
var ErrSkipSave = errors.New("settings: nothing to save")

// LoadModifySaveVersionedSettings loads the settings file in dir, applies
// modify, and saves the result, holding the settings-file lock from the
// read to the write so a concurrent writer's change is not lost
// by the whole-struct rewrite. modify may return ErrSkipSave to write
// nothing. It must not write settings itself (the lock is not re-entrant).
//
// The save is the struct round-trip SaveVersionedSettings makes, so
// comments and unknown keys in the file are not kept; prefer
// UpdateVersionedSetting or PrepareSettingsPathEdits for single keys.
func LoadModifySaveVersionedSettings(dir string, modify func(*VersionedSettings) error) error {
	lock, err := LockSettingsDir(dir)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	vs, err := loadSingleFileVersioned(lock, dir)
	if err != nil {
		return err
	}
	if err := modify(vs); err != nil {
		if errors.Is(err, ErrSkipSave) {
			return nil
		}
		return err
	}
	return writeVersionedSettingsFile(lock, dir, newSettingsFilePath(dir), vs)
}

// RewriteSettingsFile rewrites the settings file in dir (YAML or JSON, as
// GetSettingsPath finds it) under the settings lock: rewrite gets the path
// and the current bytes, read under the lock, and returns the new bytes,
// which replace the file atomically. rewrite may return ErrSkipSave to
// write nothing. Without a settings file it does nothing. Prefer the
// in-place editors (UpdateVersionedSetting, PrepareSettingsPathEdits) for
// versioned YAML; this is for whole-file rewrites such as legacy files.
func RewriteSettingsFile(dir string, rewrite func(path string, data []byte) ([]byte, error)) error {
	lock, err := LockSettingsDir(dir)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	path := GetSettingsPath(dir)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	out, err := rewrite(path, data)
	if err != nil {
		if errors.Is(err, ErrSkipSave) {
			return nil
		}
		return err
	}
	return writeSettingsFileAtomic(lock, path, out)
}

// WriteSettingsFileLocked replaces the settings file at path with data
// through the locked writer (writeSettingsFileAtomic): atomically, following
// a symlinked file, and in place in a read-only directory. lock must be the
// held settings lock for path's directory, and the caller must have read
// the file it is replacing under that lock. For callers outside this
// package that rewrite the whole file themselves (the hub's server-config
// and GitHub App writes).
func WriteSettingsFileLocked(lock *SettingsLock, path string, data []byte) error {
	return writeSettingsFileAtomic(lock, path, data)
}

// CreateSettingsFileIfMissing writes data to the settings file at path
// unless a file already exists there, checked under the settings lock for
// path's directory so a concurrent writer's new file is never replaced. A
// dangling symlink at path counts as missing: its target is created and the
// link kept, as a plain os.WriteFile would.
func CreateSettingsFileIfMissing(path string, data []byte) error {
	dir := filepath.Dir(path)
	lock, err := LockSettingsDir(dir)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	// os.Stat, as the hub's own pre-check does: a dangling symlink counts as
	// missing, and the write then creates its target (keeping the link).
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeSettingsFileAtomic(lock, path, data)
}
