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

package hub

import (
	"context"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// The Hub is the only remover of a deleted project's tree on shared project
// storage. When the Hub cannot see that storage, the tree is kept: a
// project delete then answers 200 with a projectDeleteResult instead of an
// unqualified 204, and Hub health reports project_storage_tree as degraded.
// The code and message are fixed strings: they never carry a path, an
// export, a share or an ID.
const (
	projectDeleteSharedStorageKeptCode    = "shared-project-storage"
	projectDeleteSharedStorageKeptMessage = "The Hub cannot see the shared project storage, so the deleted project's shared tree was kept. Hub health reports " + projectStorageTreeCheck + " as degraded."
)

// projectDeleteSharedStorageKeptReason is the fixed reason of the ERROR audit
// line the delete writes for a kept shared tree (no path, export or share;
// the line names only the project ID).
const projectDeleteSharedStorageKeptReason = "the Hub cannot see the shared project storage; the project's shared tree is kept for manual removal"

// projectDeleteStorageCheckTimeout bounds the visibility check on the delete
// request path: a stale or hung mount must not block the handler, so a
// timeout counts as "not visible". A variable so tests can shorten it.
var projectDeleteStorageCheckTimeout = 2 * time.Second

// projectDeleteResult is the body of a project delete that answers 200.
type projectDeleteResult struct {
	StorageKept []storageKeptItem `json:"storageKept,omitempty"`
}

// storageKeptItem names one storage the delete kept, with a fixed code and a
// fixed message.
type storageKeptItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// projectDeleteStorageKept reports, on a shared backend, whether the Hub
// cannot see the shared project storage, so the background cleanup cannot
// remove the deleted project's tree. It runs the same probe as the health
// check (projectStorageTreeProbe), bounded by
// projectDeleteStorageCheckTimeout; a timeout or an unusable layout counts as
// not visible. On a backend that is not shared it reports nothing. Each kept
// tree is also one ERROR audit line (projectfs.AuditMessage) naming the
// project ID.
func (s *Server) projectDeleteStorageKept(ctx context.Context, projectID string) (kept []storageKeptItem) {
	defer func() {
		if len(kept) > 0 {
			s.auditSharedStorageKept(ctx, projectID)
		}
	}()
	if !s.workspaceBackendShared() {
		return nil
	}
	keptItems := []storageKeptItem{{Code: projectDeleteSharedStorageKeptCode, Message: projectDeleteSharedStorageKeptMessage}}
	layout, err := s.projectStorageLayout()
	if err != nil {
		return keptItems
	}
	if !layout.Backend.Shared() && !layout.SharedDirLayout().Backend.Shared() {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- projectStorageTreeProbe(layout) }()
	timer := time.NewTimer(projectDeleteStorageCheckTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		return keptItems
	case <-timer.C:
		return keptItems
	}
}

// auditSharedStorageKept writes the ERROR audit line of a kept shared tree on
// the storage-deletion audit channel: deleter, tree, project ID, outcome and
// the fixed reason, no path.
func (s *Server) auditSharedStorageKept(ctx context.Context, projectID string) {
	s.projectsLogger().Log(ctx, slog.LevelError, projectfs.AuditMessage,
		"audit", "storage_delete",
		"deleter", string(projectfs.DeleterHub),
		"tree", string(projectfs.TreeProjectRoot),
		"project_id", projectID,
		"outcome", string(projectfs.OutcomeKept),
		"reason", projectDeleteSharedStorageKeptReason)
}
