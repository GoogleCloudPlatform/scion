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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// Writes into an existing agent home, which the agent's container can
// change, confined to the home with the pkg/sciontool/dirfd primitives:
// every directory below the home is opened with O_NOFOLLOW relative to its
// parent's descriptor (a symbolic link at any component is refused, never
// followed; missing directories are created the same way), and a file is
// installed through that same parent descriptor by creating a temporary
// file with O_EXCL|O_NOFOLLOW and renaming it over the leaf, so an entry at
// the leaf (a link included) is replaced, never written through. Removals
// unlink the entry itself relative to its parent descriptor. The home
// directory itself is reached by its broker-side path, which lies in the
// broker-owned agent directory, outside every container mount.

// noChown is the chown step of dirfd's writers: files written into the
// home keep the broker's ownership, as provisioning writes them.
func noChown(fd, uid, gid int) error { return nil }

// splitHomeRel cleans rel (relative to the home) and splits it into its
// parent directory and leaf; it refuses a path that is not strictly below
// the home.
func splitHomeRel(rel string) (parent, leaf string, err error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("invalid agent home path %q", rel)
	}
	return filepath.Dir(clean), filepath.Base(clean), nil
}

// homeParentFd returns a no-follow descriptor for the directory parent
// (relative to agentHome), creating missing components.
func homeParentFd(agentHome, parent string) (int, error) {
	fd, under, err := dirfd.EnsureDirNoFollowUnderRoot(agentHome, filepath.Join(agentHome, parent), 0o755, 0, 0)
	if err != nil {
		return -1, err
	}
	if !under {
		return -1, fmt.Errorf("%s is not below the agent home", parent)
	}
	return fd, nil
}

// writeHomeFile writes data to rel inside the agent home at agentHome,
// confined to the home (see above).
func writeHomeFile(agentHome, rel string, data []byte, mode os.FileMode) error {
	parent, leaf, err := splitHomeRel(rel)
	if err != nil {
		return err
	}
	fd, err := homeParentFd(agentHome, parent)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	return dirfd.WriteAtNoFollowWithChown(fd, leaf, rel, data, mode, 0, 0, dirfd.ReplaceLeaf, noChown)
}

// removeHomeFile removes rel inside the agent home when it is there,
// confined to the home: an entry at rel that is a symbolic link is
// removed itself, never its target, and a missing parent means there is
// nothing to remove. A directory at rel is left alone. A symbolic link at
// a parent component is refused.
func removeHomeFile(agentHome, rel string) error {
	if _, _, err := splitHomeRel(rel); err != nil {
		return err
	}
	fd, leaf, err := dirfd.OpenParentNoFollowUnder(agentHome, filepath.FromSlash(rel))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
			return nil
		}
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstatat(fd, leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return nil
	}
	if err := dirfd.UnlinkAt(fd, leaf); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}
