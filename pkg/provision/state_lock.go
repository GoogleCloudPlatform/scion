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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WorkspaceProvisionLockDirs returns, in acquisition order, every
// directory whose provisioning lock a provisioner of the shared workspace
// at workspacePath may take, derived the way ProvisionShared derives them:
//
//   - the default sentinel directory (resolveSentinelDir of an input with
//     no SentinelDir: the workspace's parent, the project root), which the
//     hub-side Cloud Run provisioner locks;
//   - the project state directory (ProjectStateDir), which the Kubernetes
//     provisioning init container locks (after its legacy workspace lock).
//
// No provisioner takes both, so taking them in this fixed order cannot
// form a cycle with any of them.
func WorkspaceProvisionLockDirs(workspacePath string) []string {
	in := ProvisionInput{Resolved: ResolvedWorkspace{HostPath: workspacePath}}
	return []string{resolveSentinelDir(in), ProjectStateDir(workspacePath)}
}

// WithWorkspaceProvisionLocks runs fn while holding every provisioning lock
// a provisioner of the shared workspace at workspacePath may take
// (WorkspaceProvisionLockDirs), so one-shot work that places the
// workspace's content (the project storage migration, ptone/scion#5819)
// excludes a concurrent first provisioning on every runtime for its whole
// duration. The locks are the ones ProvisionShared takes (heartbeat, stale
// reclaim); fn runs under their combined context, which is cancelled when
// any of them is lost.
//
// Every lock directory must already exist: the caller creates the state
// directory with the broker's leaf contract (shareddirs.EnsureLeaf),
// because a directory created here would lack the setgid bit and default
// ACL the provisioning init container needs.
func WithWorkspaceProvisionLocks(ctx context.Context, workspacePath string, fn func(ctx context.Context) error) error {
	if workspacePath == "" || !filepath.IsAbs(workspacePath) {
		return errors.New("provision: an absolute workspace path is required")
	}
	dirs := WorkspaceProvisionLockDirs(workspacePath)
	for _, dir := range dirs {
		fi, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("provision: provisioning lock directory: %w", err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("provision: provisioning lock directory %s is not a directory", dir)
		}
	}
	held, err := acquireOrderedFileLocks(ctx, dirs, defaultFileLockWait())
	if err != nil {
		return fmt.Errorf("provision: acquire provisioning locks: %w", err)
	}
	runErr := fn(held.ctx)
	if relErr := held.release(); relErr != nil && runErr == nil {
		return fmt.Errorf("provision: release provisioning locks: %w", relErr)
	}
	return runErr
}
