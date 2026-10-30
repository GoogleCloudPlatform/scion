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

package projectfs

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// Agent homes and secrets on a shared Layout (one share for homes, the
// Kubernetes path shape canonical, ptone/scion#5825). Every runtime that
// keeps an agent's home or secrets on the share (the Kubernetes NFS home,
// the Cloud Run agent runtime) resolves them here, so they agree for the
// same (project, agent):
//
//	<SubPathRoot>/<projectID>/agents/<agent slug>/home-<agentID>
//	<SubPathRoot>/<projectID>/agents/<agent slug>/secrets-<agentID>
//
// The agent ID (the hub agent UUID) keeps two incarnations of one slug
// apart. Homes resolve on the Layout shared dirs resolve on
// (SharedDirLayout), the share homes: nfs names.

const (
	agentHomePrefix    = "home-"
	agentSecretsPrefix = "secrets-"
)

// agentIDPattern is a canonical (lowercase) UUID, the form hub agent IDs
// take.
var agentIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidAgentID reports whether id is a hub agent ID usable in a home path:
// a canonical lowercase UUID.
func ValidAgentID(id string) bool {
	return agentIDPattern.MatchString(id)
}

// AgentHome resolves agent's home on the share: <AgentRoot>/home-<agentID>.
// agent is the agent slug, agentID the hub agent ID (ValidAgentID).
func (l Layout) AgentHome(projectID, agent, agentID string) (Resolved, error) {
	return l.agentLeaf(projectID, agent, agentID, agentHomePrefix)
}

// AgentSecrets resolves agent's secrets directory on the share:
// <AgentRoot>/secrets-<agentID>.
func (l Layout) AgentSecrets(projectID, agent, agentID string) (Resolved, error) {
	return l.agentLeaf(projectID, agent, agentID, agentSecretsPrefix)
}

func (l Layout) agentLeaf(projectID, agent, agentID, prefix string) (Resolved, error) {
	sl := l.SharedDirLayout()
	if !ValidAgentID(agentID) || agentID == agent {
		return Resolved{}, fmt.Errorf("projectfs: invalid agent ID %q", agentID)
	}
	ar, err := sl.AgentRoot(projectID, agent)
	if err != nil {
		return Resolved{}, err
	}
	return sl.shared(filepath.Join(ar.RelPath, prefix+agentID), Mount{}), nil
}

// ProjectWorkspaceDirName is the name of a project's workspace directory
// below its project root on a shared backend (<ProjectRoot>/workspace, the
// project-level workspace Resolve gives).
const ProjectWorkspaceDirName = workspaceDir

// LegacyCloudRunAgentDir is the retired Cloud Run per-agent directory below
// a project root (relative path projectRel): agents/<agent ID>. Only the
// project-storage migration (which moves its home and secrets) and the
// Cloud Run runtime's refusal to start an agent whose home has not moved
// yet read it (ptone/scion#5825); nothing places anything there.
func LegacyCloudRunAgentDir(projectRel, agentID string) string {
	return filepath.Join(projectRel, agentsDir, agentID)
}
