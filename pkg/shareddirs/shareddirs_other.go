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

//go:build !unix

package shareddirs

import (
	"errors"
	"fmt"
	"os"
)

// The shared-directory primitives anchor every step on directory
// descriptors (openat, O_NOFOLLOW, fchown and POSIX ACLs), which this
// platform lacks. Shared directories are managed by a runtime broker, which
// never runs here, so each operation fails with errors.ErrUnsupported.

func unsupported(op, path string) error {
	return fmt.Errorf("shareddirs: %s %s: %w on this platform", op, path, errors.ErrUnsupported)
}

// EnsureLeaf is unsupported on this platform.
func EnsureLeaf(hostBase, rel string) (leafFd int, alreadyExisted bool, err error) {
	return -1, false, unsupported("EnsureLeaf", rel)
}

// CloseFd is a no-op: no descriptor is ever returned on this platform.
func CloseFd(fd int) error { return nil }

// FdGID is unsupported on this platform.
func FdGID(fd int) (uint32, error) { return 0, unsupported("FdGID", "") }

// DeleteProjectTree is unsupported on this platform.
func DeleteProjectTree(hostBase, subPathRoot, projectID string) error {
	return unsupported("DeleteProjectTree", projectID)
}

// DeleteSharedDir is unsupported on this platform.
func DeleteSharedDir(hostBase, subPathRoot, projectID, name string) error {
	return unsupported("DeleteSharedDir", name)
}

// OpenAnchoredRoot is unsupported on this platform.
func OpenAnchoredRoot(hostBase, rel string) (*os.Root, error) {
	return nil, unsupported("OpenAnchoredRoot", rel)
}
