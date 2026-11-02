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

import "github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"

// ProjectStorageMigrateExecutor is the one-shot project storage migration
// (ptone/scion#5819): on a hub whose
// workspace backend is shared it moves each project's legacy trees into the
// project storage layout and then deletes the project's legacy GCS
// workspace copy. The engine is pkg/projectfs/migrate; this executor only
// gathers the hub's Layouts, projects and object storage.
//
// It supports a dry run (nothing changes; the log is the plan) and is safe
// to re-run (a second run reports moves=0), so it is in
// rerunnableMigrations. On a local workspace backend it does nothing. A run
// with a failed step returns an error, so the migration is marked failed
// and the Files tab notice stays until a later run succeeds.
type ProjectStorageMigrateExecutor struct {
	srv *Server
}

// projectStorageMigrateKey is the maintenance operation key.
const projectStorageMigrateKey = entadapter.ProjectStorageMigrateKey
