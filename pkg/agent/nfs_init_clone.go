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

import "github.com/GoogleCloudPlatform/scion/pkg/api"

// nfsInitGitClone returns the clone settings for RunConfig.GitCloneForInit,
// which only the Kubernetes runtime reads (the workspace-provision init
// container of an NFS-backed workspace):
//   - the agent's GitClone, when set (clone-per-agent, worktree-per-agent);
//   - otherwise, for a shared-plain git project, its workspace clone
//     settings, so the first agent to start clones the repository into the
//     shared workspace and later agents reuse it;
//   - otherwise nil (the init container only creates and chowns the
//     workspace).
func nfsInitGitClone(opts api.StartOptions) *api.GitCloneConfig {
	if opts.GitClone != nil {
		return opts.GitClone
	}
	if opts.SharedWorkspace && opts.SharedWorkspaceClone != nil && opts.SharedWorkspaceClone.URL != "" {
		return opts.SharedWorkspaceClone
	}
	return nil
}
