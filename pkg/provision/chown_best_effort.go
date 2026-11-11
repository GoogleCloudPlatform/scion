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

// ChownBestEffortEnv is set to "1" by the Kubernetes runtime on the
// provisioning init container only when, before the pod was built, the
// broker created the NFS workspace directory (and the directories of shared
// dirs on the same claim) or found them with setgid and group write. Agents
// then reach those directories through their group; on exports that map
// root to an anonymous user the NFS server refuses the chown, and failing
// the pod over it would block agent create.
const ChownBestEffortEnv = "SCION_PROVISION_CHOWN_BEST_EFFORT"

// ChownBestEffortRequested reports whether getenv carries exactly the value
// "1" for ChownBestEffortEnv. Unset, empty or any other value keeps the
// strict behavior (a chown failure is fatal).
func ChownBestEffortRequested(getenv func(string) string) bool {
	return getenv(ChownBestEffortEnv) == "1"
}
