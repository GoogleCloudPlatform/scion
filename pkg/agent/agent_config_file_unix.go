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

//go:build unix

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"golang.org/x/sys/unix"
)

// WriteAgentConfigFile writes data as agentDir's scion-agent.json, the one
// way the broker writes that file. It never writes through a symbolic link:
//   - agentDir itself is opened without following a link at its last
//     component (openAgentDirNoFollow), so a linked agent directory cannot
//     redirect the write into another directory;
//   - an existing entry that is not a plain single-link regular file is
//     refused (errAgentConfigEntryRefused, wrapping
//     config.ErrAgentStateConflict) and left as it is;
//   - the content goes to a new temporary file created in agentDir through
//     a handle on the directory with O_CREAT|O_EXCL|O_NOFOLLOW, is written
//     and synced, and is then renamed over the name with renameat, which
//     replaces whatever entry is there (a link placed there concurrently included)
//     rather than its target. The directory is synced afterwards.
func WriteAgentConfigFile(agentDir string, data []byte) (retErr error) {
	dir, err := openAgentDirNoFollow(agentDir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	dfd := int(dir.Fd())
	if err := checkAgentConfigEntryAt(dfd); err != nil {
		return err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fmt.Errorf("temporary name: %w", err)
	}
	tmp := "." + agentConfigFileName + ".tmp-" + hex.EncodeToString(rnd[:])
	f, err := dirfd.CreateExclAt(dfd, tmp, 0o644)
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", agentConfigFileName, err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = unix.Unlinkat(dfd, tmp, 0)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", agentConfigFileName, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", agentConfigFileName, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", agentConfigFileName, err)
	}
	if err := unix.Renameat(dfd, tmp, dfd, agentConfigFileName); err != nil {
		return fmt.Errorf("rename %s into place: %w", agentConfigFileName, err)
	}
	renamed = true
	if err := unix.Fsync(dfd); err != nil {
		return fmt.Errorf("sync agent directory: %w", err)
	}
	return nil
}

// ReadAgentConfigFile reads agentDir's scion-agent.json without following a
// symbolic link at the file (dirfd.ReadAtNoFollow: a plain single-link
// regular file only, bounded). A missing file satisfies
// errors.Is(err, fs.ErrNotExist); a refused entry is errAgentConfigEntryRefused.
func ReadAgentConfigFile(agentDir string) ([]byte, error) {
	dir, err := openAgentDirNoFollow(agentDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	dfd := int(dir.Fd())
	if err := checkAgentConfigEntryAt(dfd); err != nil {
		return nil, err
	}
	data, err := dirfd.ReadAtNoFollow(dfd, agentConfigFileName, maxAgentConfigFileBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if errors.Is(err, dirfd.ErrNotSingleLinkRegular) || errors.Is(err, unix.ELOOP) {
			return nil, errAgentConfigEntryRefused
		}
		return nil, fmt.Errorf("read %s: %w", agentConfigFileName, err)
	}
	return data, nil
}

// checkAgentConfigEntryAt refuses an existing scion-agent.json entry in the
// directory dfd that is not a plain single-link regular file (lstat, never
// following a link). An absent entry is fine.
func checkAgentConfigEntryAt(dfd int) error {
	var st unix.Stat_t
	err := unix.Fstatat(dfd, agentConfigFileName, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", agentConfigFileName, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return errAgentConfigEntryRefused
	}
	return nil
}

// openAgentDirNoFollow opens the agent directory for the confined config
// I/O with O_DIRECTORY|O_NOFOLLOW, so the directory itself must not be a
// symbolic link (the components above it are the project's or the broker's
// own layout and are opened as usual). A linked or non-directory agent dir
// is refused with errAgentDirRefused, which never names a path.
func openAgentDirNoFollow(agentDir string) (*os.File, error) {
	clean := filepath.Clean(agentDir)
	fd, err := unix.Open(clean, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, errAgentDirRefused
		}
		return nil, fmt.Errorf("open agent directory: %w", err)
	}
	return os.NewFile(uintptr(fd), clean), nil
}
