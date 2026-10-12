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
	"context"
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Instance ownership (ptone/scion#3274). A manager serving a flat Runtime
// Broker instance has an owner: the instance's Runtime Broker ID. It labels
// every runtime object it creates with api.LabelRuntimeBrokerID, sees only
// runtime objects carrying that label (an unlabeled object belongs to no
// instance), sees only file-only agents the instance's durable ownership
// record claims, and never passes an unresolved name or ID straight to the
// runtime. A manager without an owner (legacy) behaves as before.

// ErrNotOwned is returned when an operation targets an object or agent the
// manager's instance does not own (or ownership cannot be established).
var ErrNotOwned = errors.New("not owned by this Runtime Broker instance")

// OwnerScope configures instance ownership on a manager.
type OwnerScope struct {
	// RuntimeBrokerID is the owning instance's Runtime Broker ID.
	RuntimeBrokerID string
	// FileAgentOwned reports whether the instance's durable ownership record
	// claims the file-only agent slug in the project (projectID may be
	// empty when the project has none). Nil means no file-only agent is
	// owned.
	FileAgentOwned func(projectID, slug string) bool
	// EntryUnresolved, when set, reports a runtime object carrying the
	// owner label whose ownership is nevertheless unresolved (its agent or
	// slug is claimed by another configured instance too). Such an object
	// is not owned.
	EntryUnresolved func(labels map[string]string) bool
	// EntryPathTrusted reports whether the project path an owned runtime
	// object's labels name may be written (List's terminal-state
	// convergence of agent-info.json). Nil means no label-derived path is
	// written.
	EntryPathTrusted func(projectPath, projectID string) bool
	// LaunchHandleOwned reports whether CleanupLaunch may delete the object
	// a launch handle names: the instance recorded it, or it is in the
	// journal of one of the instance's own launches whose recording
	// failed. Nil means no handle is owned.
	LaunchHandleOwned func(h api.ResourceHandle) bool
	// WorkspaceLock, when set, takes the host's workspace lock on shared
	// paths (all at once) for provisioning steps that change them (a
	// repository's worktrees and sharer markers); it returns the release.
	WorkspaceLock func(ctx context.Context, paths ...string) (func(), error)
}

type workspaceLockKey struct{}

// withWorkspaceLock carries an owned manager's workspace lock to the
// provisioning code it calls.
func (m *AgentManager) withWorkspaceLock(ctx context.Context) context.Context {
	if m.owner == nil || m.owner.WorkspaceLock == nil {
		return ctx
	}
	return context.WithValue(ctx, workspaceLockKey{}, m.owner.WorkspaceLock)
}

// lockWorkspace takes the workspace lock carried by ctx on paths, or does
// nothing without one (a legacy manager, the CLI).
func lockWorkspace(ctx context.Context, paths ...string) (func(), error) {
	fn, _ := ctx.Value(workspaceLockKey{}).(func(context.Context, ...string) (func(), error))
	if fn == nil {
		return func() {}, nil
	}
	return fn(ctx, paths...)
}

// SetOwner restricts the manager to one instance's objects. Call it once,
// before the manager is used.
func (m *AgentManager) SetOwner(scope OwnerScope) {
	m.owner = &scope
}

// OwnerRuntimeBrokerID returns the owning instance's Runtime Broker ID, or
// "" for a legacy manager.
func (m *AgentManager) OwnerRuntimeBrokerID() string {
	if m.owner == nil {
		return ""
	}
	return m.owner.RuntimeBrokerID
}

// ownsEntry reports whether a runtime entry belongs to this manager: always
// for a legacy manager; for an owned manager only when the reserved label
// names its instance and its ownership is not unresolved.
func (m *AgentManager) ownsEntry(a api.AgentInfo) bool {
	if m.owner == nil {
		return true
	}
	if a.Labels[api.LabelRuntimeBrokerID] != m.owner.RuntimeBrokerID {
		return false
	}
	return m.owner.EntryUnresolved == nil || !m.owner.EntryUnresolved(a.Labels)
}

// ownsFileAgent reports whether a file-only agent belongs to this manager.
func (m *AgentManager) ownsFileAgent(projectID, slug string) bool {
	if m.owner == nil {
		return true
	}
	return m.owner.FileAgentOwned != nil && m.owner.FileAgentOwned(projectID, slug)
}

// mayWriteEntryPath reports whether List may write agent files under the
// project path a runtime entry's labels name: always for a legacy manager;
// for an owned manager only when its EntryPathTrusted accepts the path.
func (m *AgentManager) mayWriteEntryPath(a api.AgentInfo) bool {
	if m.owner == nil {
		return true
	}
	return m.owner.EntryPathTrusted != nil && m.owner.EntryPathTrusted(a.ProjectPath, a.Labels["scion.project_id"])
}

// ownsLaunchHandle reports whether CleanupLaunch may delete the object a
// handle names: always for a legacy manager.
func (m *AgentManager) ownsLaunchHandle(h api.ResourceHandle) bool {
	if m.owner == nil {
		return true
	}
	return m.owner.LaunchHandleOwned != nil && m.owner.LaunchHandleOwned(h)
}

// listRuntime lists runtime entries, keeping only owned ones for an owned
// manager. A list error is returned as is; it is never retried without the
// filter.
func (m *AgentManager) listRuntime(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	agents, err := m.Runtime.List(ctx, filter)
	if err != nil || m.owner == nil {
		return agents, err
	}
	owned := agents[:0]
	for _, a := range agents {
		if m.ownsEntry(a) {
			owned = append(owned, a)
		}
	}
	return owned, nil
}

// ownerLabels returns the reserved owner label to add to a new runtime
// object, or nil for a legacy manager.
func (m *AgentManager) ownerLabels() map[string]string {
	if m.owner == nil {
		return nil
	}
	return map[string]string{api.LabelRuntimeBrokerID: m.owner.RuntimeBrokerID}
}
