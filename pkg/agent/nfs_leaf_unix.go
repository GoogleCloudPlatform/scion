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

package agent

import (
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"golang.org/x/sys/unix"
)

// isNFSLeafPermissionError reports whether err means the broker was not
// allowed to create or open a directory (as opposed to a path that is not a
// usable directory, such as a symlink or a regular file).
func isNFSLeafPermissionError(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EROFS)
}

// ensureNFSLeaf creates rel under hostBase via projectfs.EnsureSharedLeafExpecting
// and reports whether the leaf was created now or already carries
// nfsLeafGroupAccessBits. expected is the share's configured nfs uid and
// gid (resolvedLeafOwner): existing upper directories with that shared
// layout are not warned about. Nothing is chowned.
func ensureNFSLeaf(hostBase, rel string, expected shareddirs.Owner) (prepared bool, err error) {
	prepared, _, err = ensureNFSLeafClaim(hostBase, rel, expected)
	return prepared, err
}

// ensureNFSLeafClaim is ensureNFSLeaf that also reports whether this call
// created the leaf directory (projectfs.EnsureSharedLeafExpecting's own
// report, not a separate check).
func ensureNFSLeafClaim(hostBase, rel string, expected shareddirs.Owner) (prepared, created bool, err error) {
	leafFd, existed, err := projectfs.EnsureSharedLeafExpecting(hostBase, rel, expected)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = shareddirs.CloseFd(leafFd) }()
	if !existed {
		return true, true, nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(leafFd, &st); err != nil {
		return false, false, fmt.Errorf("stat existing directory: %w", err)
	}
	return st.Mode&nfsLeafGroupAccessBits == nfsLeafGroupAccessBits, false, nil
}
