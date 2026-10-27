//go:build unix

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

package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"golang.org/x/sys/unix"
)

// This file holds the publishing half of the credential state: it uses
// directory descriptors, O_NOFOLLOW and owner checks that only unix offers.
// See credential_state_publish_other.go.

// setCredentialStateDirForTest is SetCredentialStateDirForTest after its
// test-only check.
func setCredentialStateDirForTest(dir, base string) func() {
	oDir, oBase, oOwner, oOver := credentialStateDir, credentialStateBase, credentialStateOwner, credentialStateOverridden
	credentialStateDir, credentialStateBase, credentialStateOwner, credentialStateOverridden = dir, base, os.Getuid(), true
	return func() {
		credentialStateDir, credentialStateBase, credentialStateOwner, credentialStateOverridden = oDir, oBase, oOwner, oOver
	}
}

// canPublishCredentialState reports whether this process may publish: it
// runs as the owner uid (root in production). Elsewhere, for example when
// init runs as the agent user, the workload could change the file, so
// nothing is published.
func canPublishCredentialState() bool {
	if testing.Testing() && !credentialStateOverridden {
		return false
	}
	return os.Geteuid() == credentialStateOwner
}

// openCredentialStateDir returns a descriptor of the state directory after
// checking it and every directory from the base down to it. If create is
// true, a missing state directory is created (0755) first.
func openCredentialStateDir(create bool) (int, error) {
	owner := stateOwnerUID()
	dirFd, _, err := dirfd.OpenParentNoFollowOwnedBy(credentialStateBase, CredentialStatePath(), owner)
	if err == nil {
		return dirFd, nil
	}
	if !create || !errors.Is(err, syscall.ENOENT) {
		return -1, err
	}
	parentFd, dirName, perr := dirfd.OpenParentNoFollowOwnedBy(credentialStateBase, credentialStateDir, owner)
	if perr != nil {
		return -1, perr
	}
	mkErr := unix.Mkdirat(parentFd, dirName, 0o755)
	_ = syscall.Close(parentFd)
	if mkErr != nil && !errors.Is(mkErr, syscall.EEXIST) {
		return -1, mkErr
	}
	dirFd, _, err = dirfd.OpenParentNoFollowOwnedBy(credentialStateBase, CredentialStatePath(), owner)
	if err == nil && mkErr == nil {
		// Searchable by doctor whatever the umask was.
		_ = syscall.Fchmod(dirFd, 0o755)
	}
	return dirFd, err
}

// stateOwnerUID is credentialStateOwner as a uid.
func stateOwnerUID() uint32 {
	return uint32(credentialStateOwner) //nolint:gosec // a uid, never negative
}

// writeCredentialState writes st to the state file: a new temp file
// (O_EXCL, no-follow, unique name) in the checked directory, renamed over
// the file. Mode 0644: the state holds no value.
func writeCredentialState(st CredentialState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if len(data) > credentialStateMaxBytes {
		return fmt.Errorf("credential state too large")
	}
	dirFd, err := openCredentialStateDir(true)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(dirFd) }()
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := credentialStateTmpGlob + hex.EncodeToString(rnd[:])
	f, err := dirfd.CreateExclAt(dirFd, tmp, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(0o644)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = dirfd.RenameAt(dirFd, tmp, credentialStateFileName)
	}
	if werr != nil {
		_ = dirfd.UnlinkAt(dirFd, tmp)
	}
	return werr
}

// writeCredentialStateFn is writeCredentialState; a test seam.
var writeCredentialStateFn = writeCredentialState

// removeCredentialState unlinks the state file, so a state that could not
// be updated is never read as current.
func removeCredentialState() {
	dirFd, err := openCredentialStateDir(false)
	if err != nil {
		return
	}
	defer func() { _ = syscall.Close(dirFd) }()
	_ = dirfd.UnlinkAt(dirFd, credentialStateFileName)
}

// removeCredentialStateTemps unlinks temp files left by an earlier run.
func removeCredentialStateTemps() {
	dirFd, err := openCredentialStateDir(false)
	if err != nil {
		return
	}
	defer func() { _ = syscall.Close(dirFd) }()
	dup, err := syscall.Dup(dirFd)
	if err != nil {
		return
	}
	d := os.NewFile(uintptr(dup), credentialStateDir)
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return
	}
	for _, n := range names {
		if strings.HasPrefix(n, credentialStateTmpGlob) {
			_ = dirfd.UnlinkAt(dirFd, n)
		}
	}
}

// newInstanceID returns a random ID for this init run.
func newInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// PublishCredentialState publishes c's credential state for sciontool
// doctor, when this process may (see canPublishCredentialState): it writes
// the current state at once, before returning, so a file left by an
// earlier run is never read as this run's, and then keeps the file up to
// date from a single writer goroutine until ctx ends. A failed write
// removes the file. It returns false when this process does not publish
// (it does not run as root, or c is nil).
//
// The file can only be trusted where the workload cannot become root.
// Losing the last update when the process exits is harmless: doctor runs
// only while init runs, and the next start writes a new file at once.
func PublishCredentialState(ctx context.Context, c *Client) bool {
	if c == nil || !canPublishCredentialState() {
		return false
	}
	c.cstate.update(func(st *CredentialState) {
		st.Instance = newInstanceID()
		st.InitStartedAt = time.Now().UTC()
	})
	removeCredentialStateTemps()
	changed := c.credentialStateChanged()
	w := &credentialStateWriter{client: c}
	if !w.writeLatest() {
		log.Error("Could not publish the credential state; sciontool doctor will report it as unavailable")
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				w.writeLatest()
			}
		}
	}()
	return true
}

// credentialStateWriter writes the latest state; one per client.
type credentialStateWriter struct {
	client  *Client
	lastSeq uint64
	failing bool
}

// writeLatest writes the client's current state unless it was already
// written. It reports whether the file holds the current state.
func (w *credentialStateWriter) writeLatest() bool {
	st := w.client.CredentialStateSnapshot()
	if st.Seq != 0 && st.Seq <= w.lastSeq {
		return true
	}
	if err := writeCredentialStateFn(st); err != nil {
		removeCredentialState()
		if !w.failing {
			log.Error("Could not update the credential state file: %v", err)
		}
		w.failing = true
		return false
	}
	w.failing = false
	w.lastSeq = st.Seq
	return true
}

// ReadCredentialState reads the published state. status is
// CredentialStateOK, CredentialStateUnavailable (no file) or
// CredentialStateUntrusted, with a fixed reason: the file or a directory
// above it is not owned by root or is writable by others, is a symlink,
// is not a single-link regular file, is too large or is not readable.
func ReadCredentialState() (st CredentialState, status, reason string) {
	owner := stateOwnerUID()
	dirFd, leaf, err := dirfd.OpenParentNoFollowOwnedBy(credentialStateBase, CredentialStatePath(), owner)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return st, CredentialStateUnavailable, ""
		}
		if errors.Is(err, dirfd.ErrNotOwned) {
			return st, CredentialStateUntrusted, "a directory above it is not owned by root or is writable by others"
		}
		return st, CredentialStateUntrusted, "a directory above it is not readable or is a symlink"
	}
	defer func() { _ = syscall.Close(dirFd) }()
	f, err := dirfd.OpenAt(dirFd, leaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return st, CredentialStateUnavailable, ""
		}
		return st, CredentialStateUntrusted, "the file is a symlink or is not readable"
	}
	defer func() { _ = f.Close() }()
	var fst syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &fst); err != nil {
		return st, CredentialStateUntrusted, "the file is not readable"
	}
	switch {
	case fst.Mode&syscall.S_IFMT != syscall.S_IFREG:
		return st, CredentialStateUntrusted, "the file is not a regular file"
	case fst.Uid != owner || fst.Mode&0o022 != 0:
		return st, CredentialStateUntrusted, "the file is not owned by root or is writable by others"
	case fst.Nlink != 1:
		return st, CredentialStateUntrusted, "the file has more than one link"
	case fst.Size > credentialStateMaxBytes:
		return st, CredentialStateUntrusted, "the file is too large"
	}
	data, err := io.ReadAll(io.LimitReader(f, credentialStateMaxBytes+1))
	if err != nil || len(data) > credentialStateMaxBytes {
		return st, CredentialStateUntrusted, "the file is not readable"
	}
	if err := json.Unmarshal(data, &st); err != nil || st.Version != credentialStateVersion {
		return CredentialState{}, CredentialStateUntrusted, "the file is not a readable state"
	}
	return st, CredentialStateOK, ""
}

// ReadTokenFileWithModTime is ReadTokenFile that also returns the file's
// modification time, from the same guarded read. ok is false when there is
// no usable value. An oversized file is warned about, as ReadTokenFile
// does.
func ReadTokenFileWithModTime() (token string, modTime time.Time, ok bool) {
	path := TokenFilePath()
	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		return "", time.Time{}, false
	}
	defer func() { _ = syscall.Close(dirFd) }()
	value, st, err := readTokenAtStat(dirFd, leaf, path)
	if err != nil {
		if errors.Is(err, dirfd.ErrTooLarge) {
			// As ReadTokenFile; err already names the path.
			warnTokenFile("Ignoring token file: %v", err)
		}
		return "", time.Time{}, false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", time.Time{}, false
	}
	return value, statModTime(&st), true
}
