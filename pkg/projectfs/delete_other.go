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

package projectfs

import (
	"errors"
	"fmt"
	"time"
)

// Project storage deletion runs in the hub, which is unix-only (the Windows
// CLI gates the server off): the removals open every component without
// following symlinks (shareddirs). These methods always fail with
// errors.ErrUnsupported: no path-based fallback. See delete_unix.go.

// CheckTreeMounted always fails with errors.ErrUnsupported on this platform.
func (l Layout) CheckTreeMounted() error {
	return fmt.Errorf("check project tree: %w on this platform", errors.ErrUnsupported)
}

// CheckTreesMounted always fails with errors.ErrUnsupported on this
// platform.
func (l Layout) CheckTreesMounted() error {
	return fmt.Errorf("check project trees: %w on this platform", errors.ErrUnsupported)
}

// RemoveProjectTree always fails with errors.ErrUnsupported on this
// platform.
func (l Layout) RemoveProjectTree(projectID string) ([]Deletion, error) {
	return nil, fmt.Errorf("remove project tree: %w on this platform", errors.ErrUnsupported)
}

// RemoveAgentStorage always fails with errors.ErrUnsupported on this
// platform.
func (l Layout) RemoveAgentStorage(projectID, agent, agentID string) ([]Deletion, error) {
	return nil, fmt.Errorf("remove agent storage: %w on this platform", errors.ErrUnsupported)
}

// DeleteAfterPath always fails with errors.ErrUnsupported on this platform.
func (l Layout) DeleteAfterPath(projectID string) (Resolved, error) {
	return Resolved{}, fmt.Errorf("resolve deferred-deletion path: %w on this platform", errors.ErrUnsupported)
}

// MarkDeleteAfter always fails with errors.ErrUnsupported on this platform.
func (l Layout) MarkDeleteAfter(projectID string, after time.Time) ([]Deletion, error) {
	return nil, fmt.Errorf("defer project deletion: %w on this platform", errors.ErrUnsupported)
}

// DeferredDeletions always fails with errors.ErrUnsupported on this
// platform.
func (l Layout) DeferredDeletions() ([]DeferredDeletion, error) {
	return nil, fmt.Errorf("list deferred project deletions: %w on this platform", errors.ErrUnsupported)
}

// ClearDeleteAfter always fails with errors.ErrUnsupported on this platform.
func (l Layout) ClearDeleteAfter(projectID string) error {
	return fmt.Errorf("clear deferred project deletion: %w on this platform", errors.ErrUnsupported)
}
