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
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"golang.org/x/sys/unix"
)

// nfsRemovalStateDir returns the provisioning state directory whose lock
// RemoveNFSWorktree takes, the only lock the provisioning init containers
// take. The path is derived the same way as the runtime's mount
// (runtime.NFSProvisionStateSubPath) and the directory is created like
// before a pod starts (ensureNFSLeaf, no symlink following). expected is
// the share's configured nfs uid and gid, for checking existing upper
// directories only (ensureNFSLeaf).
//
// When the broker is not allowed to create, open or write in the directory
// (a permission error, or no write and search access to an existing one),
// or the path is a symlink, a regular file or otherwise unusable, it
// returns an error and the worktree is left in place: no lock is taken in
// the workspace root.
func nfsRemovalStateDir(hostBase, rel, projectID string, expected shareddirs.Owner) (string, error) {
	stateRel, err := runtime.NFSProvisionStateSubPath(rel, projectID)
	if err != nil {
		return "", fmt.Errorf("workspace_storage nfs: %w; left in place", err)
	}
	stateDir := filepath.Join(hostBase, stateRel)
	_, err = ensureNFSLeaf(hostBase, stateRel, expected)
	if err == nil {
		err = unix.Access(stateDir, unix.W_OK|unix.X_OK)
	}
	if err != nil {
		return "", fmt.Errorf("workspace_storage nfs: prepare the provisioning state directory %q: %w; left in place", stateRel, err)
	}
	return stateDir, nil
}
