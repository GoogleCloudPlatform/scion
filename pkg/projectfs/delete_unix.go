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

//go:build unix

package projectfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// sharedLayouts returns the distinct shared Layouts a project's trees can
// be on: l, and the transitional shared-dir split when it is another share
// (shared dirs and nfs homes resolve there).
func (l Layout) sharedLayouts() []Layout {
	var out []Layout
	seen := map[string]bool{}
	for _, c := range []Layout{l, l.SharedDirLayout()} {
		c.SharedDirs = nil
		if !c.Backend.Shared() || seen[c.Key()] {
			continue
		}
		seen[c.Key()] = true
		out = append(out, c)
	}
	return out
}

// checkShareHostBase refuses an nfs Layout whose host base is not
// <mount root>/<share id> for a share ID that is one plain path element: a
// share ID such as ".." would move the host base to or above the mount
// root, and a deletion there could reach a tree beside it.
func (l Layout) checkShareHostBase() error {
	if l.Backend != BackendNFS || l.Share.ID == "" {
		return nil
	}
	id := l.Share.ID
	if id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00") || filepath.Base(filepath.Clean(l.HostBase)) != id {
		return fmt.Errorf("%w: nfs share %q does not give a host base below the mount root (host base %q)", ErrRefused, id, l.HostBase)
	}
	return nil
}

// deleteShared removes r (a directory resolved on a shared Layout) with
// shareddirs.DeleteTree: every component below the host base is opened
// without following symlinks, and a symlink at any of them is refused. The
// host base itself (the operator's mount point) may sit behind a symlink.
func deleteShared(tree TreeKind, r Resolved) Deletion {
	base, err := r.resolvedHostBase()
	if err != nil {
		return deletion(tree, r.HostPath, false, fmt.Errorf("%w: host base of %s: %w", ErrRefused, r.HostPath, err))
	}
	existed, err := shareddirs.DeleteTree(base, r.RelPath)
	if errors.Is(err, shareddirs.ErrTreeRefused) {
		err = fmt.Errorf("%w: %w", ErrRefused, err)
	}
	return deletion(tree, r.HostPath, existed, err)
}

// DeleteProject removes projectID's project root <SubPathRoot>/<projectID>
// (workspace, project configuration, shared dirs, provisioning state,
// worktrees and agent directories) from every shared Layout of l (l and
// its transitional shared-dir split), and reports one Deletion per root.
//
// The removal is confined: every component below the host base is a plain
// directory or the removal is refused (a symlink is never followed), and
// a symlink inside the tree is unlinked, never followed. An invalid
// project ID is refused before anything is touched. A missing root is
// OutcomeAbsent, so the call is idempotent; an unusable host base (the
// share not mounted) is OutcomeFailed.
//
// On the local backend it removes nothing and returns no Deletion: the
// local slug-keyed trees keep their existing deleters.
func (l Layout) DeleteProject(projectID string) ([]Deletion, error) {
	if err := validProjectID(projectID); err != nil {
		return nil, err
	}
	var out []Deletion
	for _, sl := range l.sharedLayouts() {
		r, err := sl.ProjectRoot(projectID)
		if err != nil {
			return out, err
		}
		if err := sl.checkShareHostBase(); err != nil {
			out = append(out, deletion(TreeProjectRoot, r.HostPath, false, err))
			continue
		}
		out = append(out, deleteShared(TreeProjectRoot, r))
	}
	return out, nil
}

// DeleteAgent removes agent's directory <SubPathRoot>/<projectID>/agents/
// <agent slug> (its own workspace in the per-agent modes, its homes and
// secrets) from every shared Layout of l, with DeleteProject's confinement,
// and reports one Deletion per directory. An invalid project ID or agent
// slug is refused before anything is touched; a missing directory is
// OutcomeAbsent. On the local backend it removes nothing.
//
// The directory is keyed by slug, so a caller must not use it while
// another agent of the project with the same slug may still use it; it
// then deletes only this agent's own leaves (DeleteAgentLeaves).
func (l Layout) DeleteAgent(projectID, agent string) ([]Deletion, error) {
	if err := validProjectID(projectID); err != nil {
		return nil, err
	}
	if err := validAgentSlug(agent); err != nil {
		return nil, err
	}
	var out []Deletion
	for _, sl := range l.sharedLayouts() {
		r, err := sl.AgentRoot(projectID, agent)
		if err != nil {
			return out, err
		}
		if err := sl.checkShareHostBase(); err != nil {
			out = append(out, deletion(TreeAgentRoot, r.HostPath, false, err))
			continue
		}
		out = append(out, deleteShared(TreeAgentRoot, r))
	}
	return out, nil
}

// DeleteAgentLeaves removes only the leaves keyed by the agent's ID below
// its agent directory, home-<agentID> and secrets-<agentID> (AgentHome,
// AgentSecrets), on the Layout homes resolve on. It is the agent delete
// for an agent directory another agent of the same slug still uses.
func (l Layout) DeleteAgentLeaves(projectID, agent, agentID string) ([]Deletion, error) {
	if !l.SharedDirLayout().Backend.Shared() {
		if err := validProjectID(projectID); err != nil {
			return nil, err
		}
		return nil, nil
	}
	home, err := l.AgentHome(projectID, agent, agentID)
	if err != nil {
		return nil, err
	}
	secrets, err := l.AgentSecrets(projectID, agent, agentID)
	if err != nil {
		return nil, err
	}
	if err := l.SharedDirLayout().checkShareHostBase(); err != nil {
		return []Deletion{deletion(TreeAgentHome, home.HostPath, false, err), deletion(TreeAgentSecrets, secrets.HostPath, false, err)}, nil
	}
	return []Deletion{deleteShared(TreeAgentHome, home), deleteShared(TreeAgentSecrets, secrets)}, nil
}
