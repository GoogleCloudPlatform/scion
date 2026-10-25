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

import "github.com/GoogleCloudPlatform/scion/pkg/config"

// projectStorageFromWorkspace returns the server.project_storage block with
// ws's backend and backend blocks, for tests written against the workspace
// view. Nil stays nil.
func projectStorageFromWorkspace(ws *config.V1WorkspaceStorageConfig) *config.V1ProjectStorageConfig {
	if ws == nil {
		return nil
	}
	return &config.V1ProjectStorageConfig{
		Backend:         ws.Backend,
		NFS:             ws.NFS,
		CloudRunVolume:  ws.CloudRunVolume,
		GKESharedVolume: ws.GKESharedVolume,
	}
}
