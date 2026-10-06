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

package runtimebroker

import (
	"github.com/GoogleCloudPlatform/scion/pkg/agent"
)

// agentFilesRunOwner returns the run recorded on disk as the owner of the
// agent's files (agent-info.json runId, ptone/scion#2675) when it differs
// from runID, and "" otherwise. Agent files are addressed by name only, so
// a delete or failure cleanup for run runID removes them only when no other
// run owns them.
//
// It returns "" -- the caller proceeds as before -- when runID is empty (a
// caller that names no run) and when no run is recorded (an agent
// provisioned before run IDs were recorded, or one provisioned but never
// started). An empty projectPath resolves the way DeleteAgentFiles resolves
// it.
func agentFilesRunOwner(agentName, projectPath, runID string) string {
	if runID == "" {
		return ""
	}
	if owner := agent.GetSavedRunID(agentName, projectPath); owner != "" && owner != runID {
		return owner
	}
	return ""
}

// otherRunInFlight reports whether a start of a run other than runID is in
// flight on this broker under any of keys (see
// launchRegistry.OtherRunInFlight): that start owns the agent's name, and
// may be creating its files and per-agent objects right now.
func (s *Server) otherRunInFlight(runID string, keys ...launchKey) bool {
	for _, k := range keys {
		if s.launchRegistry.OtherRunInFlight(k, runID) {
			return true
		}
	}
	return false
}
