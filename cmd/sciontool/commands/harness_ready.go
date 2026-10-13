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

package commands

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// Harness readiness files. `sciontool init` writes the init-run file at the
// start of each run; `sciontool hook` writes the ready marker on the first
// qualifying event. The agent runtime reads both before pasting a buffered
// message. Variables only so tests can point them at a temp directory.
var (
	initRunFilePath      = state.HarnessInitRunFile
	harnessReadyFilePath = state.HarnessReadyFile
)

// readinessFileMode is the mode of both files. init may run as root, and the
// harness (and so the hook) runs as the agent user, which must be able to
// read the init-run file.
const readinessFileMode os.FileMode = 0o644

// readinessFileMaxBytes bounds reads of either file; both hold one short line.
const readinessFileMaxBytes = 256

// writeInitRunFile starts a new readiness run: it removes any init-run file
// and ready marker left by an earlier run, then atomically writes a new
// init-run file with a fresh nonce and the mode for harness (SCION_HARNESS).
// It returns the nonce. Removing the stale marker first means a marker from
// the previous run can never satisfy the runtime's readiness check.
//
// A failure to remove a stale file (other than it not existing) is logged at
// Warn and is not fatal: a stale marker carries the previous run's nonce, so
// it cannot satisfy the runtime's check for this run anyway.
//
// The init-run file is 0644, so the agent user can read the nonce and write
// its own ready marker. That only affects message delivery timing for that
// agent's own harness.
func writeInitRunFile(harness string) (string, error) {
	for _, p := range []string{harnessReadyFilePath, initRunFilePath} {
		if err := removeNoFollow(p); err != nil {
			log.Warn("Harness readiness: removing stale %s: %v", p, err)
		}
	}

	nonce, err := randomHex(16)
	if err != nil {
		return "", err
	}
	content := state.FormatInitRun(nonce, hooks.HarnessReadyMode(harness))
	if err := writeFileAtomicNoFollow(initRunFilePath, []byte(content), readinessFileMode); err != nil {
		return "", err
	}
	return nonce, nil
}

// harnessReadyMarkerMatches reports whether the ready marker carries nonce.
// Used by init's readiness watcher; same file checks as the runtime probe
// (no symlinks, bounded read).
func harnessReadyMarkerMatches(nonce string) bool {
	data, err := dirfd.ReadFileNoFollow(harnessReadyFilePath, readinessFileMaxBytes)
	if err != nil {
		return false
	}
	got, ok := state.ParseReadyMarker(string(data))
	return ok && got == nonce
}

// markHarnessReadyFn is markHarnessReady; a variable so tests can observe
// when the marker is written relative to the local status write.
var markHarnessReadyFn = markHarnessReady

// markHarnessReady writes the ready marker for the current init run when ev
// is a qualifying readiness event. It never fails the hook and never waits:
// there is no network, retry or sleep, and every error is logged at debug
// level and dropped. A missing or garbled init-run file means there is no
// run to mark.
//
// The init-run file is read even when a marker exists: comparing the
// marker's nonce with the current run's is the only way to tell a current
// marker from a stale one (for example one written by a hook of the previous
// run that raced init's cleanup), and a stale marker left in place would
// hold delivery until the deadline.
func markHarnessReady(ev *hooks.Event) {
	if !hooks.IsReadinessEvent(ev) {
		return
	}
	data, err := dirfd.ReadFileNoFollow(initRunFilePath, readinessFileMaxBytes)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Debug("harness ready marker: reading init-run file: %v", err)
		}
		return
	}
	nonce, _, ok := state.ParseInitRun(string(data))
	if !ok {
		log.Debug("harness ready marker: init-run file is not well formed")
		return
	}
	if existing, err := dirfd.ReadFileNoFollow(harnessReadyFilePath, readinessFileMaxBytes); err == nil {
		if got, ok := state.ParseReadyMarker(string(existing)); ok && got == nonce {
			return
		}
	}
	if err := writeFileAtomicNoFollow(harnessReadyFilePath, []byte(state.FormatReadyMarker(nonce)), readinessFileMode); err != nil {
		log.Debug("harness ready marker: write: %v", err)
	}
}

// writeFileAtomicNoFollow writes data to path by creating a temp file with a
// random name next to it (O_CREAT|O_EXCL|O_NOFOLLOW), setting mode
// explicitly (so the umask does not apply), and renaming it over path. Every
// directory component is opened without following symlinks. The rename
// replaces a symlink at path itself and never writes through it. The temp
// file is removed if any step fails.
func writeFileAtomicNoFollow(path string, data []byte, mode os.FileMode) (err error) {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(dirFd) }()

	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := "." + leaf + ".tmp-" + suffix
	f, err := dirfd.CreateExclAt(dirFd, tmp, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	defer func() {
		if err != nil {
			_ = dirfd.UnlinkAt(dirFd, tmp)
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err = dirfd.RenameAt(dirFd, tmp, leaf); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// removeNoFollow removes path's directory entry, never following a symlink
// in any component; a symlink at the leaf is removed, not its target. A
// missing entry is not an error.
func removeNoFollow(path string) error {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = syscall.Close(dirFd) }()
	if err := dirfd.UnlinkAt(dirFd, leaf); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}
