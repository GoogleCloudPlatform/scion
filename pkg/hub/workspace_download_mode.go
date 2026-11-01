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

import "github.com/GoogleCloudPlatform/scion/pkg/storage"

// SetWorkspaceStorage gives the dispatcher the hub's storage source
// (Server.GetStorage). buildCreateRequestFor sets the create request's
// WorkspaceDownloadMode to signed URLs only while that storage is GCS, the
// one backend the broker-self workspace download serves. Nil means the hub
// has no storage, and the mode is never set.
func (d *HTTPAgentDispatcher) SetWorkspaceStorage(source func() storage.Storage) {
	d.workspaceStorage = source
}

// workspaceStorageIsGCS reports whether the hub's current storage is GCS.
func (d *HTTPAgentDispatcher) workspaceStorageIsGCS() bool {
	if d.workspaceStorage == nil {
		return false
	}
	stor := d.workspaceStorage()
	return stor != nil && stor.Provider() == storage.ProviderGCS
}
