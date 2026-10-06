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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// hubIdentityOutcome is the result of recordHubProjectIdentity.
type hubIdentityOutcome string

const (
	// hubIdentityRecorded: the hub project ID was recorded.
	hubIdentityRecorded hubIdentityOutcome = "recorded"
	// hubIdentityMatching: the workspace already records the hub project ID.
	hubIdentityMatching hubIdentityOutcome = "matching"
	// hubIdentityNotBrokerCopy: the workspace is not known to be a copy
	// this broker populated from a hub upload (see brokerCopyOfHubWorkspace).
	hubIdentityNotBrokerCopy hubIdentityOutcome = "not_broker_copy"
	// hubIdentityNoEntry: the workspace has no .scion entry.
	hubIdentityNoEntry hubIdentityOutcome = "no_entry"
	// hubIdentityUnexpectedForm: the recorded or hub identity does not name
	// a directory directly under project-configs in the <slug>__<id8> form.
	hubIdentityUnexpectedForm hubIdentityOutcome = "unexpected_form"
	// hubIdentityTargetExists: both the current project config directory
	// and the one named after the hub project ID hold files.
	hubIdentityTargetExists hubIdentityOutcome = "target_exists"
	// hubIdentityInUse: another agent of the project may be using the
	// current project config directory.
	hubIdentityInUse hubIdentityOutcome = "in_use"
)

// brokerCopyOfHubWorkspace reports whether projectPath is a hub-managed
// workspace that this broker populated from a hub workspace upload for
// projectID, and that no hub keeps as its own on this host:
//   - projectPath, with symlinks resolved, is <globalDir>/projects/<slug>
//     with only <globalDir> resolved, and that entry is a directory, not a
//     symlink;
//   - no hub workspace record <globalDir>/hub-workspaces/<slug> exists;
//   - the broker workspace record <globalDir>/broker-workspaces/<slug>
//     holds exactly projectID.
//
// Anything missing, unreadable or different gives false.
func brokerCopyOfHubWorkspace(projectPath, slug, projectID string) bool {
	if projectPath == "" || slug == "" || projectID == "" {
		return false
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return false
	}
	resolvedGlobal, err := filepath.EvalSymlinks(globalDir)
	if err != nil {
		return false
	}
	want := filepath.Join(resolvedGlobal, "projects", slug)
	if info, err := os.Lstat(want); err != nil || !info.IsDir() {
		return false
	}
	got, err := filepath.EvalSymlinks(projectPath)
	if err != nil || filepath.Clean(got) != want {
		return false
	}

	hubRecord, err := config.HubWorkspaceRecordPath(slug)
	if err != nil {
		return false
	}
	if _, err := os.Lstat(hubRecord); !os.IsNotExist(err) {
		return false
	}

	brokerRecord, err := config.BrokerWorkspaceRecordPath(slug)
	if err != nil {
		return false
	}
	recorded, err := config.ReadWorkspaceRecord(brokerRecord)
	return err == nil && recorded == projectID
}

// configRootHoldsFiles reports whether root exists and contains any
// non-directory entry. Errors other than absence count as holding files.
func configRootHoldsFiles(root string) bool {
	if _, err := os.Lstat(root); err != nil {
		return !os.IsNotExist(err)
	}
	holds := false
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			holds = true
			return filepath.SkipAll
		}
		return nil
	})
	return holds || err != nil
}

// recordHubProjectIdentity records projectID, the hub project ID, as the
// identity of the hub-managed workspace at projectPath when the workspace
// is a broker copy of a hub workspace (brokerCopyOfHubWorkspace) whose
// identity differs. The project config directory then is
// <slug>__<first 8 chars of projectID>; the previous directory is not
// moved. Nothing is changed when:
//   - the recorded or hub identity does not name a directory directly
//     under project-configs in the <slug>__<id8> form;
//   - the previous directory exists and the hub-derived one holds files;
//   - inUse, called immediately before the change, reports true or fails.
func recordHubProjectIdentity(projectPath, slug, projectID string, inUse func() (bool, error)) (hubIdentityOutcome, error) {
	if !brokerCopyOfHubWorkspace(projectPath, slug, projectID) {
		return hubIdentityNotBrokerCopy, nil
	}
	current, err := config.ReadWorkspaceIdentity(projectPath)
	if err != nil {
		return "", err
	}
	if current == nil {
		return hubIdentityNoEntry, nil
	}
	if current.Matches(slug, projectID) {
		return hubIdentityMatching, nil
	}

	if !shareddirs.ValidProjectID(projectID) {
		return hubIdentityUnexpectedForm, nil
	}
	target, ok := config.ConfinedProjectConfigRoot(slug, projectID)
	if !ok {
		return hubIdentityUnexpectedForm, nil
	}
	if current.ID != "" {
		if !shareddirs.ValidProjectID(current.ID) {
			return hubIdentityUnexpectedForm, nil
		}
		previous, ok := config.ConfinedProjectConfigRoot(current.Slug, current.ID)
		if !ok {
			return hubIdentityUnexpectedForm, nil
		}
		if previous != target {
			if _, err := os.Lstat(previous); err == nil && configRootHoldsFiles(target) {
				return hubIdentityTargetExists, nil
			}
		}
	}

	busy, err := inUse()
	if err != nil || busy {
		return hubIdentityInUse, nil
	}

	if err := current.Write(slug, projectID); err != nil {
		return "", err
	}
	return hubIdentityRecorded, nil
}

// otherProjectAgentsInUse reports whether an agent of projectID other than
// agentID is known to this broker in any phase other than stopped or error.
// A listing failure counts as in use.
func (s *Server) otherProjectAgentsInUse(ctx context.Context, projectID, agentID string) (bool, error) {
	if s.manager == nil {
		return true, nil
	}
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true", "scion.project_id": projectID})
	if err != nil {
		return true, err
	}
	for _, a := range agents {
		if label, ok := a.Labels["scion.project_id"]; ok && label != projectID {
			continue
		}
		if agentID != "" && a.ID == agentID {
			continue
		}
		switch state.Phase(a.Phase) {
		case state.PhaseStopped, state.PhaseError:
		default:
			return true, nil
		}
	}
	return false, nil
}

// alignHubManagedProjectIdentity applies recordHubProjectIdentity for an
// agent start or create and logs the outcome by project ID.
func (s *Server) alignHubManagedProjectIdentity(ctx context.Context, agentID, projectPath, slug, projectID string) {
	outcome, err := recordHubProjectIdentity(projectPath, slug, projectID, func() (bool, error) {
		return s.otherProjectAgentsInUse(ctx, projectID, agentID)
	})
	switch {
	case err != nil:
		s.agentLifecycleLog.Warn("Failed to record hub project ID for hub-managed project",
			append([]any{"agent_id", agentID, "project_id", projectID}, identityErrorAttrs(err)...)...)
	case outcome == hubIdentityRecorded:
		s.agentLifecycleLog.Info("Recorded hub project ID for hub-managed project",
			"agent_id", agentID, "project_id", projectID)
	case outcome == hubIdentityTargetExists, outcome == hubIdentityInUse, outcome == hubIdentityUnexpectedForm:
		s.agentLifecycleLog.Warn("Hub project ID not recorded for hub-managed project",
			"agent_id", agentID, "project_id", projectID, "reason", string(outcome))
	}
}

// recordBrokerWorkspaceCopy writes the broker workspace record
// (<globalDir>/broker-workspaces/<slug>, holding projectID) after a
// successful download of a hub workspace upload. It is written only when
// the hub-managed project path did not exist when the create request
// arrived (existedBefore false), and never while a hub workspace record
// exists for slug. A directory that existed before keeps whatever record it
// already carries.
func recordBrokerWorkspaceCopy(slug, projectID string, existedBefore bool) error {
	if slug == "" || projectID == "" {
		return nil
	}
	hubRecord, err := config.HubWorkspaceRecordPath(slug)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(hubRecord); !os.IsNotExist(err) {
		return nil
	}
	if existedBefore {
		// A directory present before the download keeps whatever record it
		// already has; none is created for it.
		return nil
	}
	brokerRecord, err := config.BrokerWorkspaceRecordPath(slug)
	if err != nil {
		return err
	}
	return config.WriteWorkspaceRecord(brokerRecord, projectID)
}

// hubWorkspaceRecordExists reports whether a hub workspace record exists for
// slug. Errors other than absence count as existing.
func hubWorkspaceRecordExists(slug string) bool {
	hubRecord, err := config.HubWorkspaceRecordPath(slug)
	if err != nil {
		return true
	}
	_, err = os.Lstat(hubRecord)
	return !os.IsNotExist(err)
}

// identityErrorAttrs returns log attributes describing err without any
// filesystem path: for a filesystem error, its operation and the underlying
// system error; for any other error, a fixed classification.
func identityErrorAttrs(err error) []any {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &pathErr):
		return []any{"op", pathErr.Op, "error", fmt.Sprint(pathErr.Err)}
	case errors.As(err, &linkErr):
		return []any{"op", linkErr.Op, "error", fmt.Sprint(linkErr.Err)}
	default:
		return []any{"error", "not a filesystem error"}
	}
}
