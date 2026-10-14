// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// A failed start is recorded as HostState.StartFailedRunID in the
// host-owned HostStateFile (ptone/scion#4815). Some runtimes do not let
// the host write files in the container, so the "error" phase written to
// agent-info.json after a failed Runtime.Run may never land; the host
// state file does not depend on the agent home being writable. Writing it
// can still fail (a refused root, or a corrupt or symlinked file); the
// caller logs that and the start carries on.
//
// Start clears the record before Runtime.Run and sets it to the run ID
// when Run fails. List reads it only to decide the phase it displays (see
// startFailedForDisplay).

// errInvalidStartFailedRunID is returned for a run ID that is not a valid
// run ID (see runtime.ValidateRunID).
var errInvalidStartFailedRunID = errors.New("invalid start-failed run ID")

// setStartFailedRunID records runID ("" clears the record) as the failed
// start of t's agent. It locates the file with HostStateRootFor, so it
// addresses the same file as RecordRunOwner. A missing agent dir is a
// no-op, as it is for RecordRunOwner. Clearing an absent record writes
// nothing. A refusal or other error is returned for the caller to log.
func setStartFailedRunID(t RunOwnerTarget, runID string) error {
	if runID != "" && runtime.ValidateRunID(runID) != nil {
		return errInvalidStartFailedRunID
	}
	root, err := HostStateRootFor(t)
	if err != nil {
		return err
	}
	if runID == "" {
		hs, err := ReadHostState(root, t.AgentName)
		if err == nil && hs.StartFailedRunID == "" {
			return nil
		}
	}
	err = UpdateHostState(root, t.AgentName, func(hs *HostState) error {
		hs.StartFailedRunID = runID
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// startFailedForDisplay reports the failed-start run ID recorded for the
// agent List is about to display, or "" when none is known.
//
// It is display-only and fail-quiet: the result only decides the phase
// List shows. It must never feed a delete, cleanup or ownership decision,
// because the identity it uses is not fully broker-held (on the container
// path the project-id label, which a local CLI start fills from project
// settings; see the callers in list.go). Every refusal, error, missing
// file, or invalid value gives "" ("not known"), never a failure.
//
// listedAgentDir is the agent dir List reads the agent from. The overlay
// applies only when HostStateRootFor places HostStateFile in that same
// dir: with no hub project ID the resolver cannot tell a shared-workspace
// agent (whose state lives in an external dir) from a worktree agent, and
// would address the in-project dir instead. "Same dir" is sameDir, the
// check the host state writers use (agentDirRunOwnerRoot).
func startFailedForDisplay(t RunOwnerTarget, listedAgentDir string) string {
	root, err := HostStateRootFor(t)
	if err != nil {
		return ""
	}
	path, err := HostStatePath(root, t.AgentName)
	if err != nil || !sameDir(filepath.Dir(path), listedAgentDir) {
		return ""
	}
	hs, err := ReadHostState(root, t.AgentName)
	if err != nil || hs.StartFailedRunID == "" || runtime.ValidateRunID(hs.StartFailedRunID) != nil {
		return ""
	}
	return hs.StartFailedRunID
}
