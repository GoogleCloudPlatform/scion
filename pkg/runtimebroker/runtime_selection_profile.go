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
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// runtimeClassificationProfile is the single source of the profile a
// start/restart runs an existing agent with — used by buildStartContext's
// preliminary classification (the default GCP metadata mode when the Hub
// sends none, the Kubernetes assign mapping, the hub endpoint and extra
// hosts), by the handlers' authoritative runtime selection (which also
// decides whether the bare-image local-exists check runs, and so whether
// the image_registry prefix is applied), and by the start itself
// (opts.Profile):
//
//   - the provisioned profile recorded in the agent's broker-side image
//     provenance, read from exactly the agent dir Start/GetAgent will use
//     (agentName and sharedWorkspace as the start/restart runs with);
//   - else, for an agent without a provenance file, legacyProfile() (see
//     hubSentProfile); the agent's agent-info.json is never consulted;
//   - else, for an existing but unusable provenance file, an
//     *agent.ImageProvenanceError (409, re-provision), never a fallback;
//   - for a shared-workspace agent, the dir is the broker-side external one
//     located from the Hub-supplied project ID; an undeterminable external
//     root, or (restart, mustExist) a missing external agent dir, is an
//     *agent.AgentStateDirError (409), never the in-project root.
//
// (ptone/scion#1799.) An empty project path resolves as Start resolves it
// (config.GetResolvedProjectDir), so both read the same record.
//
// provisioned reports whether the profile came from image provenance; the
// handlers then resolve it in profileStrictProvisioned mode.
func runtimeClassificationProfile(projectPath, agentName string, sharedWorkspace bool, hubProjectID string, mustExist bool, legacyProfile func() (string, error)) (profile string, provisioned bool, err error) {
	profile, ok, err := agent.ProvisionedProfile(projectPath, agentName, sharedWorkspace, hubProjectID, mustExist)
	if err != nil {
		return "", false, err
	}
	if !ok {
		profile, err := legacyProfile()
		return profile, false, err
	}
	return profile, true, nil
}

// profileRecordMissingError refuses a start or restart of an agent that has
// no image provenance record (it was provisioned before the record
// existed) when the Hub sent no profile either (an older Hub). The broker
// then has no source for the agent's profile; it never guesses one.
type profileRecordMissingError struct {
	Agent string
}

func (e *profileRecordMissingError) Error() string {
	return fmt.Sprintf("agent %q has no broker-side record of its settings profile (it was provisioned by an older version) and the hub sent no profile: re-provision the agent (scion reincarnate, or delete and re-create it)", e.Agent)
}

// hubSentProfile is the legacyProfile source for a start or restart of an
// agent without an image provenance record: the profile the Hub sent with
// the request (hubProfile; nil when an older Hub sent none). With no Hub
// profile, an agent that is already provisioned on this broker is refused
// with a *profileRecordMissingError; one that is not (the start will
// provision it) runs with no profile, as a create without one does.
func hubSentProfile(projectPath, agentName string, sharedWorkspace bool, hubProjectID string, hubProfile *string) func() (string, error) {
	return func() (string, error) {
		if hubProfile != nil {
			return *hubProfile, nil
		}
		if agent.IsProvisioned(projectPath, agentName, sharedWorkspace, hubProjectID) {
			return "", &profileRecordMissingError{Agent: agentName}
		}
		return "", nil
	}
}

// startProfileSource is the legacyProfile source the start and restart
// handlers pass to runtimeSelectionOpts. A flat instance has no profiles
// (they are refused there), so it keeps opts.Profile as it is.
func (s *Server) startProfileSource(opts api.StartOptions, agentName string, hubProfile *string) func() (string, error) {
	if s.isFlat() {
		profile := opts.Profile
		return func() (string, error) { return profile, nil }
	}
	return hubSentProfile(opts.ProjectPath, agentName, opts.SharedWorkspace, opts.HubProjectID, hubProfile)
}

// runtimeSelectionOpts returns opts with Profile replaced by
// runtimeClassificationProfile, with legacyProfile as its source for an
// agent without image provenance (startProfileSource). agentName is the
// agent's real name (the dir Start reads), which on restart can differ
// from the URL id. opts itself is not modified. The bool reports whether
// the profile came from image provenance.
func runtimeSelectionOpts(opts api.StartOptions, agentName string, mustExist bool, legacyProfile func() (string, error)) (api.StartOptions, bool, error) {
	profile, provisioned, err := runtimeClassificationProfile(opts.ProjectPath, agentName, opts.SharedWorkspace, opts.HubProjectID, mustExist, legacyProfile)
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
	var me *profileRecordMissingError
	if errors.As(err, &me) {
		s.agentLifecycleLog.Warn("agent profile unknown", "operation", op, "agent_id", agentID, "error", err)
		writeError(w, http.StatusConflict, ErrCodeConflict, "Failed to "+op+": "+me.Error(), nil)
		return
	}
	s.agentLifecycleLog.Error("runtime selection failed", "operation", op, "agent_id", agentID, "error", err)
	RuntimeError(w, "Failed to "+op+": "+err.Error())
}

// classificationProfile is runtimeClassificationProfile for
// buildStartContext's preliminary classification on start/restart, under
// in.Name; an agent without image provenance takes the Hub-sent profile
// (in.HubProfile, see hubSentProfile).
func classificationProfile(in startContextInputs) (string, bool, error) {
	sharedWorkspace := in.SharedWorkspace || (in.Config != nil && in.Config.SharedWorkspace)
	return runtimeClassificationProfile(in.ProjectPath, in.Name, sharedWorkspace, in.ProjectID, in.Operation == opHTTPRestart,
		hubSentProfile(in.ProjectPath, in.Name, sharedWorkspace, in.ProjectID, in.HubProfile))
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
