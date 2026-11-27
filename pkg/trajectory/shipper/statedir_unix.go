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

package shipper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// verifyStateDirAncestors checks via Lstat that <home>/.scion and
// <home>/.scion/trajectory-shipper (when they exist) are real directories
// (never symlinks) owned by an expected UID before privatefs.OpenTree is
// invoked. This protects root-privileged callers (such as sciontool) from
// symlink redirection of the state directory.
func verifyStateDirAncestors(homeDir string) error {
	homeInfo, err := os.Stat(homeDir)
	if err != nil {
		return fmt.Errorf("stat home dir %s: %w", homeDir, err)
	}
	homeStat, ok := homeInfo.Sys().(*syscall.Stat_t)
	if !ok || homeStat == nil {
		return fmt.Errorf("stat home dir %s: missing unix stat info", homeDir)
	}

	euid := os.Geteuid()
	ancestors := []string{
		filepath.Join(homeDir, ".scion"),
		StateDir(homeDir),
	}
	for _, dir := range ancestors {
		fi, err := os.Lstat(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("lstat state dir ancestor %s: %w", dir, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing state dir ancestor %s: %w", dir, privatefs.ErrReparsePoint)
		}
		if !fi.IsDir() {
			return fmt.Errorf("state dir ancestor %s is not a directory", dir)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st == nil {
			return fmt.Errorf("state dir ancestor %s: missing unix stat info", dir)
		}
		// When running as root, allow either root (0) or the home directory's
		// owner UID; when non-root, require ownership by the current euid.
		if euid == 0 {
			if st.Uid != 0 && st.Uid != homeStat.Uid {
				return fmt.Errorf("state dir ancestor %s owned by uid %d, want 0 or %d: %w",
					dir, st.Uid, homeStat.Uid, privatefs.ErrNotPrivate)
			}
		} else if int(st.Uid) != euid {
			return fmt.Errorf("state dir ancestor %s owned by uid %d, want %d: %w",
				dir, st.Uid, euid, privatefs.ErrNotPrivate)
		}
	}
	return nil
}
