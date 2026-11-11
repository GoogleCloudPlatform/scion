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

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeProvisionedWorktreeRepoRoot writes a broker-source record directly,
// without the mount check writeRepoRootRecord applies. Test-only: production
// writes go through writeRepoRootRecord so no writer skips that check.
func writeProvisionedWorktreeRepoRoot(agentDir, repoRoot string) error {
	data, err := json.Marshal(provisionedWorktreeState{RepoRoot: repoRoot, Source: repoRootSourceBroker})
	if err != nil {
		return err
	}
	return writeAgentInfoFile(filepath.Join(agentDir, provisionedWorktreeStateFile), data, 0o644)
}

// readProvisionedWorktreeRepoRoot reads the record's RepoRoot, ignoring the
// source field, or "" if the file is absent or does not parse. Test-only:
// production reads go through readRepoRootRecord, which requires a known
// source.
func readProvisionedWorktreeRepoRoot(agentDir string) string {
	data, err := os.ReadFile(filepath.Join(agentDir, provisionedWorktreeStateFile))
	if err != nil {
		return ""
	}
	var state provisionedWorktreeState
	if err := json.Unmarshal(data, &state); err != nil {
		return ""
	}
	return state.RepoRoot
}
