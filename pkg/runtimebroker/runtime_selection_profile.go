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
	"errors"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// runtimeClassificationProfile is the single source of the profile a
// start/restart classifies and selects the agent's runtime with — used both
// by buildStartContext's preliminary classification (the default GCP
// metadata mode when the Hub sends none, the Kubernetes assign mapping, the
// hub endpoint and extra hosts) and by the handlers' authoritative runtime
// selection (which also decides whether the bare-image local-exists check
// runs, and so whether the image_registry prefix is applied):
//
//   - the provisioned profile recorded in the agent's broker-side image
//     provenance, read from exactly the agent dir Start/GetAgent will use
//     (agentName and sharedWorkspace as the start/restart runs with);
//   - else, for an agent without a provenance file, legacyProfile() — the
//     agent-info.json profile (agent.GetSavedProfile), as before;
//   - else, for an existing but unusable provenance file, an
//     *agent.ImageProvenanceError (409, re-provision), never a fallback;
//   - for a shared-workspace agent, the dir is the broker-side external one
//     located from the Hub-supplied project ID; an undeterminable external
//     root, or (restart, mustExist) a missing external agent dir, is an
//     *agent.AgentStateDirError (409), never the in-project root.
//
// (ptone/scion#1799.) With no project path there is nothing to read, so the
// legacy profile applies.
//
// provisioned reports whether the profile came from image provenance; the
// handlers then resolve it without the agent-info.json recorded-runtime
// fallback (profileStrictProvisioned).
func runtimeClassificationProfile(projectPath, agentName string, sharedWorkspace bool, hubProjectID string, mustExist bool, legacyProfile func() string) (profile string, provisioned bool, err error) {
	if projectPath == "" {
		return legacyProfile(), false, nil
	}
	profile, ok, err := agent.ProvisionedProfile(projectPath, agentName, sharedWorkspace, hubProjectID, mustExist)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return legacyProfile(), false, nil
	}
	return profile, true, nil
}

// runtimeSelectionOpts returns the options the start/restart runtime
// selection (resolveManagerForOpts and the checks that re-resolve it) runs
// against: opts with Profile replaced by runtimeClassificationProfile.
// opts.Profile on entry is the agent-info.json profile (the legacy value).
// agentName is the agent's real name (the dir Start reads), which on
// restart can differ from the URL id. Only the runtime selection changes:
// opts itself, and every other use of opts.Profile, is left as it was.
// The bool reports whether the profile came from image provenance.
func runtimeSelectionOpts(opts api.StartOptions, agentName string, mustExist bool) (api.StartOptions, bool, error) {
	saved := opts.Profile
	profile, provisioned, err := runtimeClassificationProfile(opts.ProjectPath, agentName, opts.SharedWorkspace, opts.HubProjectID, mustExist, func() string { return saved })
	if err != nil {
		return opts, false, err
	}
	opts.Profile = profile
	return opts, provisioned, nil
}

// writeImageProvenanceError reports a runtimeSelectionOpts failure. An
// *agent.ImageProvenanceError is a conflict with the agent's on-disk state
// (409, re-provision); its message carries no host path, which is logged
// instead. Any other error keeps the generic runtime-error mapping.
func (s *Server) writeImageProvenanceError(w http.ResponseWriter, err error, op, agentID string) {
	var pe *agent.ImageProvenanceError
	if errors.As(err, &pe) {
		s.agentLifecycleLog.Error("image provenance unusable", "operation", op, "agent_id", agentID, "path", pe.Path, "error", pe.Err)
		writeError(w, http.StatusConflict, ErrCodeConflict, "Failed to "+op+": "+pe.Error(), nil)
		return
	}
	var de *agent.AgentStateDirError
	if errors.As(err, &de) {
		s.agentLifecycleLog.Error("agent state directory unavailable", "operation", op, "agent_id", agentID, "path", de.Path, "error", de.Err)
		writeError(w, http.StatusConflict, ErrCodeConflict, "Failed to "+op+": "+de.Error(), nil)
		return
	}
	s.agentLifecycleLog.Error("runtime selection failed", "operation", op, "agent_id", agentID, "error", err)
	RuntimeError(w, "Failed to "+op+": "+err.Error())
}

// classificationProfile is runtimeClassificationProfile for
// buildStartContext's preliminary classification on start/restart; the
// legacy profile is the saved one, read under in.Name as before.
func classificationProfile(in startContextInputs) (string, bool, error) {
	sharedWorkspace := in.SharedWorkspace || (in.Config != nil && in.Config.SharedWorkspace)
	return runtimeClassificationProfile(in.ProjectPath, in.Name, sharedWorkspace, in.ProjectID, in.Operation == opHTTPRestart, func() string {
		return agent.GetSavedProfile(in.Name, in.ProjectPath)
	})
}

// rejectRuntimeClassificationMismatch is the late start/restart check that
// buildStartContext's preliminary runtime classification (which chose the
// GCP metadata env, the assign mapping, the hub endpoint and extra hosts)
// matches the authoritative runtime the agent will actually run on. Both are
// sourced from the provisioned profile for a provenance-recorded agent, so a
// mismatch means the two resolutions disagree and the env built for one
// would be applied to the other; refuse rather than start it.
func rejectRuntimeClassificationMismatch(early, late string) *startContextError {
	if early == late || (isKubernetesRuntimeName(early) && isKubernetesRuntimeName(late)) {
		return nil
	}
	return &startContextError{
		Status: http.StatusConflict,
		Message: "the runtime this start was classified for (" + early + ") differs from the runtime it resolves to (" + late +
			"); the agent's profile or runtime settings changed between the two lookups — retry, or restore the profile or runtime settings the agent was provisioned with",
	}
}
