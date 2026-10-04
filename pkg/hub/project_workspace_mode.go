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

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The scion.dev/workspace-mode label is create-only and server-owned
// (design #2703 §2.4 / D6): it is set only from the validated
// CreateProjectRequest.WorkspaceMode field, re-derived on clone, and never
// changed through the raw labels map on create, register or PATCH. The
// helpers below implement that matrix for each entry point.

// resolveCreateWorkspaceModeLabels validates a createProject request's
// workspace mode against the project's git-ness and returns the labels map
// to store. A raw labels entry for the workspace-mode key must equal the
// validated mode; when no mode is requested it is stripped.
func resolveCreateWorkspaceModeLabels(mode string, labels map[string]string, isGit bool) (map[string]string, error) {
	if err := store.ValidateWorkspaceMode(mode, isGit); err != nil {
		return nil, err
	}
	raw, hasRaw := labels[store.LabelWorkspaceMode]
	if mode == "" {
		if !hasRaw {
			return labels, nil
		}
		out := maps.Clone(labels)
		delete(out, store.LabelWorkspaceMode)
		return out, nil
	}
	if hasRaw && raw != mode {
		return nil, fmt.Errorf("label %s=%q conflicts with workspaceMode %q; set the workspace mode via workspaceMode only",
			store.LabelWorkspaceMode, raw, mode)
	}
	out := maps.Clone(labels)
	if out == nil {
		out = make(map[string]string, 1)
	}
	out[store.LabelWorkspaceMode] = mode
	return out, nil
}

// resolveRegisterWorkspaceModeLabels validates the workspace-mode label on a
// registerProject request that creates a new (linked) project. Linked
// non-git projects do not support workspace modes (out of scope for v1), so
// any value is rejected for them; git projects keep today's behaviour for
// known values. Unknown values are rejected for both.
func resolveRegisterWorkspaceModeLabels(labels map[string]string, isGit bool) error {
	mode, ok := labels[store.LabelWorkspaceMode]
	if !ok {
		return nil
	}
	if err := store.ValidateWorkspaceMode(mode, isGit); err != nil {
		return err
	}
	if !isGit && mode != "" {
		return fmt.Errorf("label %s is not supported for linked projects without a git remote", store.LabelWorkspaceMode)
	}
	return nil
}

// mergePatchWorkspaceModeLabel applies the server-owned workspace-mode rule
// to a PATCH that replaces a project's labels map wholesale. A replacement
// map without the key keeps the stored value; one that repeats the stored
// value is accepted; any other value (including removing a stored mode by
// setting it empty) is rejected.
func mergePatchWorkspaceModeLabel(stored, updates map[string]string) (map[string]string, error) {
	storedMode, storedHas := stored[store.LabelWorkspaceMode]
	newMode, newHas := updates[store.LabelWorkspaceMode]
	if newHas {
		if !storedHas || newMode != storedMode {
			return nil, fmt.Errorf("label %s is immutable after project creation", store.LabelWorkspaceMode)
		}
		return updates, nil
	}
	if !storedHas {
		return updates, nil
	}
	out := maps.Clone(updates)
	if out == nil {
		// maps.Clone(nil) is nil; a nil updates map still keeps the label.
		out = make(map[string]string, 1)
	}
	out[store.LabelWorkspaceMode] = storedMode
	return out, nil
}

// deriveCloneWorkspaceMode returns the workspace-mode label a project clone
// gets from its source's label: the source's mode is carried when it is
// valid for the CLONE's git-ness (i.e. after any gitRemote override), and
// dropped ("") otherwise. A non-git per-agent (empty-per-agent) template
// stays per-agent; with a git-remote override it becomes git per-agent
// (clone-per-agent). A legacy raw canonical "empty-per-agent" label is
// normalised to "per-agent" only on a non-git source, where
// Project.IsEmptyPerAgent honours it; on a git source it resolved to
// shared-plain, so it is dropped rather than promoted to clone-per-agent.
// Unknown values are dropped defensively.
func deriveCloneWorkspaceMode(srcLabel string, srcIsGit, cloneIsGit bool) string {
	if srcLabel == string(store.SharingModeEmptyPerAgent) {
		if srcIsGit {
			return ""
		}
		srcLabel = store.WorkspaceModePerAgent
	}
	if srcLabel == "" || store.ValidateWorkspaceMode(srcLabel, cloneIsGit) != nil {
		return ""
	}
	return srcLabel
}

// dispatchWorkspaceMode returns the workspace-mode value sent to the broker
// (wire WorkspaceMode, start spec, SCION_WORKSPACE_MODE) for project. The
// broker resolves it label-only (store.ResolveWorkspaceSharingMode), without
// knowing the project's git-ness, so the value must resolve there to the
// same mode as project.SharingMode() on the hub (design #2703 §2.3):
//   - empty-per-agent sends the canonical value, never the bare "per-agent"
//     label, which the broker would map to clone-per-agent;
//   - a stored label that does not fit the project's git-ness (e.g. a legacy
//     raw "empty-per-agent" on a git project, or "worktree-per-agent" on a
//     non-git one) is dropped, so the broker sees an unlabelled project,
//     matching the hub's shared-plain resolution;
//   - any other label is forwarded unchanged, as before.
//
// A nil project yields "": there is no mode to assert. This matches the
// dispatcher's own no-project path (resolveDispatchProjectInfo returns empty
// info, with no project slug), and inventing a mode would be wrong; an
// empty-per-agent project is never nil, so nothing is opened up by it.
func dispatchWorkspaceMode(project *store.Project) string {
	if project == nil {
		return ""
	}
	mode := project.SharingMode()
	if mode == store.SharingModeEmptyPerAgent {
		return string(store.SharingModeEmptyPerAgent)
	}
	label := project.Labels[store.LabelWorkspaceMode]
	if store.ResolveWorkspaceSharingMode(label) != mode {
		return ""
	}
	return label
}

// syncsHubProjectWorkspace reports whether agents of project mount the hub's
// project workspace directory, so the hub keeps it in sync with remote
// brokers (GCS upload on create, sync-back on stop/sync). True for
// hub-managed (non-git) and shared-workspace git projects; false for
// per-agent git modes and for empty-per-agent, whose per-agent directories
// are private and broker-local (design #2703).
func syncsHubProjectWorkspace(project *store.Project) bool {
	if project == nil || project.IsEmptyPerAgent() {
		return false
	}
	return project.GitRemote == "" || project.IsSharedWorkspace()
}

// errBrokerLacksEmptyPerAgent is returned when an empty-per-agent agent would
// be dispatched to a runtime broker that does not advertise the
// emptyPerAgentWorkspace capability. Handlers map it to 412.
var errBrokerLacksEmptyPerAgent = errors.New("runtime broker does not support empty-per-agent workspaces; upgrade the broker")

// brokerLacksEmptyPerAgentError wraps errBrokerLacksEmptyPerAgent with the
// broker's display name.
type brokerLacksEmptyPerAgentError struct{ broker string }

func (e *brokerLacksEmptyPerAgentError) Error() string {
	return fmt.Sprintf("broker %s does not support empty-per-agent workspaces; upgrade the broker", e.broker)
}

func (e *brokerLacksEmptyPerAgentError) Unwrap() error { return errBrokerLacksEmptyPerAgent }

// checkEmptyPerAgentBrokerCapability fails closed when project is
// empty-per-agent and broker does not advertise the emptyPerAgentWorkspace
// capability (design #2703 D3). An old broker given an empty Workspace could
// mount the shared project directory, breaking isolation. Returns nil for
// every other project.
func checkEmptyPerAgentBrokerCapability(project *store.Project, broker *store.RuntimeBroker) error {
	if project == nil || !project.IsEmptyPerAgent() {
		return nil
	}
	if broker != nil && broker.Capabilities != nil && broker.Capabilities.EmptyPerAgentWorkspace {
		return nil
	}
	name := ""
	if broker != nil {
		name = broker.Name
		if name == "" {
			name = broker.ID
		}
	}
	return &brokerLacksEmptyPerAgentError{broker: name}
}

// writeEmptyPerAgentCapabilityError writes the 412 response for err if it is
// an errBrokerLacksEmptyPerAgent error and reports whether it did.
func writeEmptyPerAgentCapabilityError(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, errBrokerLacksEmptyPerAgent) {
		return false
	}
	writeError(w, http.StatusPreconditionFailed, ErrCodeUnsupportedCapability, err.Error(), nil)
	return true
}

// requireEmptyPerAgentBrokerCapability is the handler-side gate: it loads
// brokerID and writes a 412 when the project is empty-per-agent and the
// broker lacks the capability. Returns false when a response was written.
func (s *Server) requireEmptyPerAgentBrokerCapability(ctx context.Context, w http.ResponseWriter, project *store.Project, brokerID string) bool {
	if project == nil || !project.IsEmptyPerAgent() || brokerID == "" {
		return true
	}
	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	return !writeEmptyPerAgentCapabilityError(w, checkEmptyPerAgentBrokerCapability(project, broker))
}

// requireEmptyPerAgentBrokerCapabilityForAgent is
// requireEmptyPerAgentBrokerCapability for an existing agent: it loads the
// agent's project and checks its assigned broker. A project lookup error
// fails closed; a missing project has nothing to gate.
func (s *Server) requireEmptyPerAgentBrokerCapabilityForAgent(ctx context.Context, w http.ResponseWriter, agent *store.Agent) bool {
	if agent.ProjectID == "" {
		return true
	}
	project, err := s.store.GetProject(ctx, agent.ProjectID)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	return s.requireEmptyPerAgentBrokerCapability(ctx, w, project, agent.RuntimeBrokerID)
}
