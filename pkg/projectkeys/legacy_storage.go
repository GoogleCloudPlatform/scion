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

package projectkeys

// LegacyProjectWorkspaceObjectDir is the last path element of a hub-managed
// project's retired object-storage workspace copy,
// [hubs/<hub id>/]workspaces/<project id>/<this>/, written by the GCS
// workspace sync that ptone/scion#5813 removes. Its only reader
// is the one-shot project storage migration (ptone/scion#5819), which
// deletes those objects; both go with ptone/scion#5839.
const LegacyProjectWorkspaceObjectDir = "grove-workspace"

// Legacy project identity markers a hub-managed project tree seeded by an
// older hub may carry: the project-id file and settings keys from before
// the project rename. Their only reader is the project storage migration's
// identity check of slug-keyed legacy trees (ptone/scion#5819), which
// treats them like the current markers; removed with it in
// ptone/scion#5839.
const (
	LegacyProjectIDFile          = "grove-id"
	LegacyConfigProjectIDKey     = "grove_id"
	LegacyConfigHubProjectIDKey  = "hub.grove_id"
	LegacyConfigHubProjectIDJSON = "hub.groveId"
)
