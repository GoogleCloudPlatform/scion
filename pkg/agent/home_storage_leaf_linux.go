//go:build linux

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

package agent

import (
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// nfsSuperMagic is the statfs type of an NFS mount.
const nfsSuperMagic = 0x6969

// homeDirMode is the mode of an agent's home directory on the export:
// setgid, owner and group full access, others search only.
const homeDirMode = unix.S_ISGID | 0o771

type osHomeLeafHost struct{}

func (osHomeLeafHost) CheckMount(hostBase string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(hostBase, &st); err != nil {
		return err
	}
	if st.Type != nfsSuperMagic {
		return fmt.Errorf("%s is not an NFS mount", hostBase)
	}
	return nil
}

// EnsureHome creates agentDirRel with the shared leaf helper (the same
// modes and default ACL as the other agent directories on the export),
// then homeName in it with mode 2771. A newly created home directory has
// the ACLs it inherited removed, so its permissions are exactly its mode,
// and is given the export group when the broker may set it (the setgid
// agent directory normally provides it already). An existing home
// directory is left as it is. It never writes inside the home.
func (osHomeLeafHost) EnsureHome(hostBase, agentDirRel, homeName string, gid int) error {
	leafFd, _, err := shareddirs.EnsureLeaf(hostBase, agentDirRel)
	if err != nil {
		return err
	}
	defer func() { _ = shareddirs.CloseFd(leafFd) }()

	created := true
	if err := unix.Mkdirat(leafFd, homeName, homeDirMode); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("mkdir %s: %w", homeName, err)
		}
		created = false
	}
	fd, err := unix.Openat(leafFd, homeName, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", homeName, err)
	}
	defer func() { _ = unix.Close(fd) }()
	if !created {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err == nil {
			slog.Debug("Agent home directory exists", "home", homeName, "uid", st.Uid, "gid", st.Gid, "mode", fmt.Sprintf("%04o", st.Mode&0o7777))
		}
		return nil
	}
	for _, name := range []string{"system.posix_acl_default", "system.posix_acl_access"} {
		if err := unix.Fremovexattr(fd, name); err != nil &&
			!errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
			return fmt.Errorf("remove %s from %s: %w", name, homeName, err)
		}
	}
	if err := unix.Fchown(fd, -1, gid); err != nil {
		slog.Info("Could not set the export group on the agent home; keeping the inherited group", "home", homeName, "gid", gid, "error", err)
	}
	if err := unix.Fchmod(fd, homeDirMode); err != nil {
		return fmt.Errorf("chmod %s: %w", homeName, err)
	}
	return nil
}
