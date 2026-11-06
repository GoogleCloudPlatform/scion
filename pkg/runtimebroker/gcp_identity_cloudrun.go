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
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// cloudRunRuntimeType is the runtime type (runtime.Runtime.Name()) of the
// Cloud Run Instances runtime, for both the "cloudrun" and the
// "cloudrun-instances" settings types. The cloudrun-sandbox runtime has its
// own name and is not affected.
const cloudRunRuntimeType = "cloudrun"

// cloudRunIdentity is the result of resolveCloudRunIdentity. The zero value
// means the dispatch is not a Cloud Run dispatch with the
// hub.cloudrun_native_identity experiment on, and everything proceeds as
// before.
type cloudRunIdentity struct {
	// Identity is threaded onto StartOptions.CloudRunIdentity.
	Identity *api.CloudRunIdentity
	// SAEmail and ProjectID are set as informational env on an "assign"
	// dispatch, whose metadata mode is rewritten to passthrough.
	SAEmail   string
	ProjectID string
	// Selection is the profile and runtime entry the allow-list was read
	// from. The start and restart handlers compare it with their own later
	// resolution (rejectCloudRunIdentityRuntimeChange).
	Selection dispatchProfileSelection
}

// cloudRunNativeIdentityEnabled reports whether the hub sent the
// hub.cloudrun_native_identity experiment with this dispatch. Start and
// restart attach the hub defaults to ctx before buildStartContext; create
// attaches them only afterwards, so its request config is read as well.
// Only the hub's dispatch decides: no broker or project setting turns it on.
func cloudRunNativeIdentityEnabled(ctx context.Context, in startContextInputs) bool {
	if api.HubAgentDefaultsFromContext(ctx).ExperimentEnabled(experiments.CloudRunNativeIdentity) {
		return true
	}
	return in.Config != nil && in.Config.HubAgentDefaults.ExperimentEnabled(experiments.CloudRunNativeIdentity)
}

// resolveCloudRunIdentity resolves the service account a Cloud Run instance
// runs as when the hub sent the hub.cloudrun_native_identity experiment. It
// returns the zero value and no error for any other dispatch: another
// runtime, or the experiment off.
//
// GCP identity mode "assign" runs the instance as the assigned account:
//   - the account comes only from the hub's dispatch: the create request's
//     GCPIdentity, or on start and restart the resolved env, accepted only
//     when the resolved env itself carries SCION_METADATA_MODE=assign and
//     SCION_METADATA_MODE_SOURCE=hub. Never from broker settings, the
//     agent's own config or template env, or a project's settings. It is
//     lower-cased once;
//   - it must be listed in cloudrun.assignable_service_accounts of the
//     selected runtime entry in the broker's global settings. A project's
//     own settings.yaml value is ignored with a warning. An empty or unset
//     list allows none. A refusal names the agent only, never an account
//     or the list.
//
// Every other mode (block, the runtime default for an unset mode,
// passthrough, and a hub-default passthrough) is refused for now: it is not
// yet supported with the experiment on, and it is never run as the
// runtime's configured account.
//
// A flat Runtime Broker instance has no allow-list or block account
// setting, so "assign" and "block" there are refused with their own
// message, before any settings are read.
func (s *Server) resolveCloudRunIdentity(ctx context.Context, in startContextInputs, dispatchRuntimeType, gcpMetadataMode, profile string) (cloudRunIdentity, *startContextError) {
	if dispatchRuntimeType != cloudRunRuntimeType || !cloudRunNativeIdentityEnabled(ctx, in) {
		return cloudRunIdentity{}, nil
	}
	if s.isFlat() && (gcpMetadataMode == store.GCPMetadataModeAssign || gcpMetadataMode == store.GCPMetadataModeBlock) {
		return cloudRunIdentity{}, &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf("agent %q: flat Cloud Run instances do not support assigned identities yet (GCP identity mode %q with the %s experiment on)",
				in.Name, gcpMetadataMode, experiments.CloudRunNativeIdentity),
		}
	}
	if gcpMetadataMode != store.GCPMetadataModeAssign {
		return cloudRunIdentity{}, &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf("agent %q: GCP identity mode %q is not yet supported on Cloud Run Instances with the %s experiment on",
				in.Name, gcpMetadataMode, experiments.CloudRunNativeIdentity),
		}
	}

	var saEmail, projectID string
	if in.Config != nil && in.Config.GCPIdentity != nil {
		saEmail = in.Config.GCPIdentity.SAEmail
		projectID = in.Config.GCPIdentity.ProjectID
	} else if in.ResolvedEnv["SCION_METADATA_MODE_SOURCE"] == "hub" && in.ResolvedEnv["SCION_METADATA_MODE"] == store.GCPMetadataModeAssign {
		saEmail = in.ResolvedEnv["SCION_METADATA_SA_EMAIL"]
		projectID = in.ResolvedEnv["SCION_METADATA_PROJECT_ID"]
	}
	saEmail = strings.ToLower(saEmail)
	if saEmail == "" {
		return cloudRunIdentity{}, &startContextError{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("GCP identity mode %q requires a service account email, and none was provided", store.GCPMetadataModeAssign),
		}
	}

	sel, err := s.resolveDispatchProfileSelectionWithError(api.StartOptions{
		ProjectPath: in.ProjectPath,
		Profile:     profile,
	})
	if err != nil {
		return cloudRunIdentity{}, &startContextError{
			Status:  http.StatusInternalServerError,
			Message: fmt.Sprintf("loading the project's settings to select the runtime entry for GCP identity mode %q: %v", store.GCPMetadataModeAssign, err),
		}
	}
	// Read on its own, without the global layer underneath, so a list
	// inherited from global settings is not mistaken for one the project
	// set itself.
	if config.ProjectSettingsHasCloudRunAssignableServiceAccounts(in.ProjectPath, sel.RuntimeEntryName) {
		s.agentLifecycleLog.Warn(
			"cloudrun.assignable_service_accounts is set in a project's own settings.yaml; it is never consulted there, only the broker's global settings are used for this list",
			"agent", in.Name, "runtime_entry", sel.RuntimeEntryName)
	}
	notAttachable := func() *startContextError {
		s.agentLifecycleLog.Warn("GCP identity mode assign on Cloud Run Instances: the assigned account is not in cloudrun.assignable_service_accounts",
			"agent", in.Name, "runtime_entry", sel.RuntimeEntryName)
		return &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf("agent %q: GCP identity mode %q on Cloud Run Instances: account not attachable on this broker",
				in.Name, store.GCPMetadataModeAssign),
		}
	}
	// No runtime entry (a broker default with no settings entry, or a
	// selection that could not be resolved) has no list to read: refused
	// without loading the global settings.
	if sel.isFlat() || sel.RuntimeEntryName == "" {
		return cloudRunIdentity{}, notAttachable()
	}
	// Global file plus the DB-backed overlay only: no project layer and no
	// SCION_ environment provider.
	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		return cloudRunIdentity{}, &startContextError{
			Status:  http.StatusInternalServerError,
			Message: "loading the broker's global settings for the Cloud Run assignable service accounts: " + err.Error(),
		}
	}
	// settings.yaml can be hand-edited without any write-time validator, so
	// malformed entries are reported where the list is read. The warning
	// names the entry's position only, never its value.
	for _, w := range config.CloudRunAssignableServiceAccountWarnings(vs, sel.RuntimeEntryName) {
		s.agentLifecycleLog.Warn(w, "agent", in.Name, "runtime_entry", sel.RuntimeEntryName)
	}
	if !cloudRunAccountAssignable(vs, sel, saEmail) {
		return cloudRunIdentity{}, notAttachable()
	}
	return cloudRunIdentity{
		Identity:  &api.CloudRunIdentity{ServiceAccount: saEmail, Mode: store.GCPMetadataModeAssign},
		SAEmail:   saEmail,
		ProjectID: projectID,
		Selection: sel,
	}, nil
}

// cloudRunAccountAssignable reports whether saEmail (already lower-cased)
// is listed in cloudrun.assignable_service_accounts of the selected
// runtime entry in the global settings vs. Entries are compared
// lower-cased and trimmed, since settings.yaml can be hand-edited. A flat
// instance selects no runtime entry and has no list (refused earlier, and
// here again), so it allows none.
func cloudRunAccountAssignable(vs *config.VersionedSettings, sel dispatchProfileSelection, saEmail string) bool {
	if vs == nil || sel.isFlat() || sel.RuntimeEntryName == "" {
		return false
	}
	entry, ok := vs.Runtimes[sel.RuntimeEntryName]
	if !ok || entry.CloudRun == nil {
		return false
	}
	for _, listed := range entry.CloudRun.AssignableServiceAccounts {
		if strings.ToLower(strings.TrimSpace(listed)) == saEmail {
			return true
		}
	}
	return false
}

// rejectCloudRunIdentityRuntimeChange refuses a start or restart whose
// later, authoritative resolution (resolvedRuntimeType, and lateSelection
// for the profile and runtime entry) does not match what buildStartContext
// resolved the Cloud Run identity for (early is the selection the
// allow-list was read from):
//
//   - an identity was resolved for Cloud Run (and an "assign" metadata env
//     rewritten to passthrough), but the later runtime is not Cloud Run,
//     where the identity would be ignored and the agent would run with
//     passthrough;
//   - an identity was resolved for one Cloud Run profile or runtime entry,
//     but the later resolution selects another, whose allow-list was never
//     checked;
//   - no identity was resolved, but the later runtime is Cloud Run with the
//     experiment on, where the instance would run as the runtime's
//     configured account.
//
// Each is refused before any side effect. The message names the agent and
// the runtime or selection change only, never an account.
func rejectCloudRunIdentityRuntimeChange(ctx context.Context, opts api.StartOptions, early *dispatchProfileSelection, resolvedRuntimeType string, lateSelection func() dispatchProfileSelection) *startContextError {
	lateGated := resolvedRuntimeType == cloudRunRuntimeType &&
		api.HubAgentDefaultsFromContext(ctx).ExperimentEnabled(experiments.CloudRunNativeIdentity)
	var msg string
	switch {
	case opts.CloudRunIdentity != nil && !lateGated:
		msg = fmt.Sprintf("agent %q: the GCP identity was resolved for Cloud Run Instances, but this dispatch now resolves to runtime %q; %s",
			opts.Name, resolvedRuntimeType, legacyIdentityChangeRemedy)
	case opts.CloudRunIdentity == nil && lateGated:
		msg = fmt.Sprintf("agent %q: the GCP identity was resolved for a runtime other than Cloud Run Instances, but this dispatch now resolves to Cloud Run Instances with the %s experiment on; %s",
			opts.Name, experiments.CloudRunNativeIdentity, legacyIdentityChangeRemedy)
	case opts.CloudRunIdentity != nil:
		var was dispatchProfileSelection
		if early != nil {
			was = *early
		}
		if late := lateSelection(); late != was {
			wasText, lateText, remedy := identityChangeWording(was, late, " and ")
			msg = fmt.Sprintf("agent %q: the GCP identity on Cloud Run Instances was resolved for %s, but this dispatch now resolves to %s; %s",
				opts.Name, wasText, lateText, remedy)
		}
	}
	if msg == "" {
		return nil
	}
	return &startContextError{Status: http.StatusConflict, Message: msg}
}
