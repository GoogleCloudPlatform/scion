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

package provision

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// stateDirMode is the mode PrepareStateDir gives a provisioning state
// directory the node created: setgid, so new entries keep its group, and
// group read, write and search, so the broker and the provisioning init
// container can both take and reclaim the lock in it.
const stateDirMode = 0o2770

// PrepareStateDir checks the provisioning state directory mounted into the
// Kubernetes init container at dir (the root of its own subPath mount) and,
// when fixOwnership is set, gives it to uid:gid with mode 2770.
//
// The broker normally creates the directory on the export before the pod
// exists, with setgid and group write (see ChownBestEffortEnv). When it
// could not (no export mount on the broker, or no permission), the kubelet
// creates the subPath as root and the caller passes fixOwnership. Only dir
// itself is changed, never anything in it.
//
// It fails closed: dir must be an absolute, clean path to a real directory.
// A symlink, a regular file or any other non-directory is an error,
// whatever fixOwnership says. The directory is opened with O_NOFOLLOW and
// changed through that descriptor, so a path swapped for a symlink between
// the check and the change is never followed. A failed chown is an error
// unless the directory already has the wanted owner (as on an export that
// maps root to the workspace owner, see checkChownEPERM).
func PrepareStateDir(dir string, uid, gid int, fixOwnership bool) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
		return fmt.Errorf("provisioning state directory %q: must be a clean absolute path other than /", dir)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("provisioning state directory %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("provisioning state directory %s: not a directory (mode %s)", dir, fi.Mode())
	}
	if !fixOwnership {
		return nil
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("provisioning state directory %s: open: %w", dir, err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fchown(fd, uid, gid); err != nil {
		if cerr := checkChownEPERM(dir, uid, gid, err); cerr != nil {
			return fmt.Errorf("provisioning state directory: %w", cerr)
		}
	}
	if err := unix.Fchmod(fd, stateDirMode); err != nil {
		return fmt.Errorf("provisioning state directory %s: chmod %o: %w", dir, stateDirMode, err)
	}
	return nil
}
