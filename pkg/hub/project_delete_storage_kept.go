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
	"time"
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
// not visible. On a backend that is not shared it reports nothing.
func (s *Server) projectDeleteStorageKept() []storageKeptItem {
	if !s.workspaceBackendShared() {
		return nil
	}
	kept := []storageKeptItem{{Code: projectDeleteSharedStorageKeptCode, Message: projectDeleteSharedStorageKeptMessage}}
	layout, err := s.projectStorageLayout()
	if err != nil {
		return kept
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
		return kept
	case <-timer.C:
		return kept
	}
}
