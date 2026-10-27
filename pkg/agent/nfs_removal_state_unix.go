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
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"golang.org/x/sys/unix"
)

// nfsRemovalStateDir returns the provisioning state directory whose lock
// RemoveNFSWorktree takes after the legacy lock in the workspace, the same
// order as the provisioning init containers. The path is derived the same
// way as the runtime's mount (runtime.NFSProvisionStateSubPath) and the
// directory is created like before a pod starts (ensureNFSLeaf, no symlink
// following).
//
// When the broker is not allowed to create, open or write in the directory
// (a permission error, or no write and search access to an existing one),
// it returns "" with a warning: the removal then takes only the legacy lock,
// which every provisioning init container and broker takes first while
// legacy support exists, so it still excludes them all. A symlink, a
// regular file or any other unusable path is an error, and the worktree is
// left in place.
//
// TODO(ptone/scion#2974): when the legacy lock is dropped, this fallback
// must go: the state-directory lock is then the only lock, and a removal
// that cannot take it must fail.
func nfsRemovalStateDir(hostBase, rel, projectID string) (string, error) {
	stateRel, err := runtime.NFSProvisionStateSubPath(rel, projectID)
	if err != nil {
		return "", fmt.Errorf("workspace_storage nfs: %w; left in place", err)
	}
	stateDir := filepath.Join(hostBase, stateRel)
	_, err = ensureNFSLeaf(hostBase, stateRel)
	if err == nil {
		err = unix.Access(stateDir, unix.W_OK|unix.X_OK)
	}
	if err != nil && isNFSLeafPermissionError(err) {
		slog.Warn("workspace_storage nfs: the broker cannot use the provisioning state directory; removing the worktree under the legacy provisioning lock only",
			"path", stateDir, "error", err)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("workspace_storage nfs: prepare the provisioning state directory %q: %w; left in place", stateRel, err)
	}
	return stateDir, nil
}
