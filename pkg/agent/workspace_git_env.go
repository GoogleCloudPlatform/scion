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

import "strings"

// workspaceGitEnvKey is the env key that says whether the agent's workspace
// is a git repository. The runtime broker computes it for every start; the
// Kubernetes runtime's shared-repository guard reads it.
const workspaceGitEnvKey = "SCION_WORKSPACE_GIT"

// enforceWorkspaceGitEnv returns env ("KEY=VALUE" entries) with every
// SCION_WORKSPACE_GIT entry removed and, when set, the single entry
// SCION_WORKSPACE_GIT=value appended: the value the caller passed in
// StartOptions.Env before any settings-driven env layer was merged. A value
// from the harness-config env, the config env layer or the hub default env
// therefore never reaches the container, and an absent caller value stays
// absent.
func enforceWorkspaceGitEnv(env []string, value string, set bool) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == workspaceGitEnvKey {
			continue
		}
		out = append(out, kv)
	}
	if set {
		out = append(out, workspaceGitEnvKey+"="+value)
	}
	return out
}
