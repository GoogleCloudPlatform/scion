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
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// cloudRunRuntimeType is the runtime type (runtime.Runtime.Name()) of the
// Cloud Run Instances runtime, for both the "cloudrun" and the
// "cloudrun-instances" settings types. The cloudrun-sandbox runtime has its
// own name and is not affected.
const cloudRunRuntimeType = "cloudrun"

// cloudRunIdentity is the result of resolveCloudRunIdentity. The zero value
// means the dispatch is not a Cloud Run dispatch with the
// hub.cloudrun_native_identity experiment on, and no Cloud Run identity
// applies to it.
type cloudRunIdentity struct {
	// Identity is threaded onto StartOptions.CloudRunIdentity.
	Identity *api.CloudRunIdentity
	// SAEmail and ProjectID are set as informational env on an "assign"
	// dispatch, whose metadata mode is rewritten to passthrough. They are
	// empty for "block".
	SAEmail   string
	ProjectID string
	// Selection is the profile and runtime entry the allow-list or the
	// block account was read from. The start and restart handlers compare
	// it with their own later resolution
	// (rejectCloudRunIdentityRuntimeChange).
	Selection dispatchProfileSelection
}

// cloudRunPassthroughRefusal is the refusal of GCP identity mode
// "passthrough" (explicit or a project default) on Cloud Run Instances
// while the experiment is on.
const cloudRunPassthroughRefusal = "passthrough is not available on Cloud Run Instances; assign an account or use block"

// cloudRunInitFirewallEnv is the setting that turns on sciontool init's
// opt-in firewall (scionrt.CloudRunInitFirewallEnv).
const cloudRunInitFirewallEnv = scionrt.CloudRunInitFirewallEnv

// cloudRunInitFirewallRefusal is the refusal of the init firewall on a Cloud
// Run Instances "assign" dispatch while the experiment is on, an
// unsupported combination. The Cloud Run runtime refuses the same on the
// final instance env with the same text (scionrt.CloudRunInitFirewallRefusal).
const cloudRunInitFirewallRefusal = scionrt.CloudRunInitFirewallRefusal

// cloudRunInstancesRuntimeType is the settings type of a runtime entry that
// configures Cloud Run Instances through a cloudrun_instances block. Such
// an entry has no allow-list or block account setting.
const cloudRunInstancesRuntimeType = "cloudrun-instances"

// cloudRunNativeMarkerEnv is the resolved env key with which the hub marks
// a start or restart for native Cloud Run identity ("1"). It is never taken
// from the request's config env.
const cloudRunNativeMarkerEnv = "SCION_METADATA_NATIVE_CLOUDRUN"

// cloudRunNativeMarked reports whether the hub marked this dispatch for
// native Cloud Run identity. The create request's GCPIdentity field
// NativeCloudRun is read only for a create (opCreate). Start and restart
// read only SCION_METADATA_NATIVE_CLOUDRUN=1 in the resolved env, and only
// when the resolved env also carries SCION_METADATA_MODE_SOURCE=hub; a
// GCPIdentity on a start or restart request is not consulted. The merged
// env and the config env are never read.
func cloudRunNativeMarked(in startContextInputs) bool {
	if in.Operation == opCreate {
		return in.Config != nil && in.Config.GCPIdentity != nil && in.Config.GCPIdentity.NativeCloudRun
	}
	return in.ResolvedEnv["SCION_METADATA_MODE_SOURCE"] == "hub" && in.ResolvedEnv[cloudRunNativeMarkerEnv] == "1"
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

// hubDefaultPassthrough reports whether a passthrough mode came from the
// hub's default rung or a confined project-default grant (the
// RequireLocalRuntime flag): on a create, the
// create request's GCPIdentity when present; otherwise, and on every start
// or restart, the env (resolved env, overridden by the config env). A
// GCPIdentity on a start or restart request is not consulted, as for the
// mode, the account and the native marker.
// It is the only reading of that flag; buildStartContext computes it once
// and passes it to resolveCloudRunIdentity and to the env re-stamp.
// downgradeUnverifiedHubDefaultPassthrough turns such a passthrough into
// block on a runtime that is neither local nor Kubernetes, which includes
// Cloud Run; that runs after resolveCloudRunIdentity, so the resolver takes
// the flag as an argument.
func hubDefaultPassthrough(in startContextInputs) bool {
	if in.Operation == opCreate && in.Config != nil && in.Config.GCPIdentity != nil {
		return in.Config.GCPIdentity.RequireLocalRuntime
	}
	v := in.ResolvedEnv["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"]
	if in.Config != nil {
		for _, e := range in.Config.Env {
			if k, val, ok := strings.Cut(e, "="); ok && k == "SCION_METADATA_REQUIRE_LOCAL_RUNTIME" {
				v = val
			}
		}
	}
	return v == "true"
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
//     selected runtime entry in the broker's global settings;
//   - the hub must have marked the dispatch for native Cloud Run identity
//     (cloudRunNativeMarked); without the marker the dispatch is refused.
//     The check relies on the hub writing the marker key when it decides
//     native and deleting it otherwise.
//
// GCP identity mode "block", an unset mode (block is the default on Cloud
// Run) and a hub-default or confined project-default passthrough run the
// instance as cloudrun.block_service_account of the selected runtime entry
// in the broker's global settings. Without one the dispatch is refused; the
// runtime's own service account is never used in its place.
//
// Explicit passthrough, and a project-default passthrough without the
// RequireLocalRuntime flag, are refused (cloudRunPassthroughRefusal).
//
// A project's own settings.yaml value for either setting is ignored with a
// warning. A flat Runtime Broker instance and a runtime entry whose
// effective type in the global settings is cloudrun-instances have neither
// setting, so assign and block are refused there with their own message.
// Every refusal names the agent and never an account or the list.
//
// On a create the mode, the account and the marker come from the create
// request's GCPIdentity (the mode through gcpMetadataMode). On a start or
// restart all three come only from the hub's resolved env: the mode is
// resolved again here from the resolved env alone, so a GCPIdentity on
// such a request is never mixed in. buildStartContext then sets the env
// mode from the identity this returns.
//
// hubDefault is hubDefaultPassthrough(in), computed once by the caller.
func (s *Server) resolveCloudRunIdentity(ctx context.Context, in startContextInputs, dispatchRuntimeType, gcpMetadataMode, profile string, hubDefault bool) (cloudRunIdentity, *startContextError) {
	if dispatchRuntimeType != cloudRunRuntimeType || !cloudRunNativeIdentityEnabled(ctx, in) {
		return cloudRunIdentity{}, nil
	}
	mode := gcpMetadataMode
	if in.Operation != opCreate {
		// Start and restart requests carry no config env, so the resolved
		// env alone decides, with the SOURCE=hub rule of
		// effectiveGCPMetadataMode.
		mode = effectiveGCPMetadataMode(false, nil, in.ResolvedEnv)
	}
	if mode == store.GCPMetadataModePassthrough && hubDefault {
		mode = store.GCPMetadataModeBlock
	}
	if s.isFlat() && (mode == store.GCPMetadataModeAssign || mode == store.GCPMetadataModeBlock) {
		return cloudRunIdentity{}, cloudRunFlatRefusal(in.Name, mode)
	}

	var saEmail, projectID string
	switch mode {
	case store.GCPMetadataModePassthrough:
		return cloudRunIdentity{}, &startContextError{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("agent %q: %s", in.Name, cloudRunPassthroughRefusal),
		}
	case store.GCPMetadataModeAssign:
		if !cloudRunNativeMarked(in) {
			return cloudRunIdentity{}, &startContextError{
				Status:  http.StatusBadRequest,
				Message: fmt.Sprintf("agent %q: the hub did not mark this dispatch for native Cloud Run identity", in.Name),
			}
		}
		if in.Operation == opCreate {
			if in.Config != nil && in.Config.GCPIdentity != nil {
				saEmail = in.Config.GCPIdentity.SAEmail
				projectID = in.Config.GCPIdentity.ProjectID
			}
		} else if in.ResolvedEnv["SCION_METADATA_MODE_SOURCE"] == "hub" && in.ResolvedEnv["SCION_METADATA_MODE"] == store.GCPMetadataModeAssign {
			saEmail = in.ResolvedEnv["SCION_METADATA_SA_EMAIL"]
			projectID = in.ResolvedEnv["SCION_METADATA_PROJECT_ID"]
		}
		saEmail = strings.ToLower(saEmail)
		if saEmail == "" {
			return cloudRunIdentity{}, &startContextError{
				Status:  http.StatusBadRequest,
				Message: fmt.Sprintf("agent %q: GCP identity mode %q requires a service account email, and none was provided", in.Name, store.GCPMetadataModeAssign),
			}
		}
	case store.GCPMetadataModeBlock:
	default:
		return cloudRunIdentity{}, &startContextError{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("agent %q: GCP identity mode %q is not valid on Cloud Run Instances", in.Name, mode),
		}
	}

	sel, err := s.resolveDispatchProfileSelectionWithError(api.StartOptions{
		ProjectPath: in.ProjectPath,
		Profile:     profile,
	})
	if err != nil {
		return cloudRunIdentity{}, s.cloudRunSettingsUnreadable(in.Name, cloudRunProfileSettingKey(profile))
	}
	// Read on its own, without the global layer underneath, so a value
	// inherited from global settings is not mistaken for one the project
	// set itself.
	if mode == store.GCPMetadataModeAssign && config.ProjectSettingsHasCloudRunAssignableServiceAccounts(in.ProjectPath, sel.RuntimeEntryName) {
		s.agentLifecycleLog.Warn(
			"cloudrun.assignable_service_accounts is set in a project's own settings.yaml; it is never consulted there, only the broker's global settings are used for this list",
			"agent", in.Name, "runtime_entry", sel.RuntimeEntryName)
	}
	if mode == store.GCPMetadataModeBlock && config.ProjectSettingsHasCloudRunBlockServiceAccount(in.ProjectPath, sel.RuntimeEntryName) {
		s.agentLifecycleLog.Warn(
			"cloudrun.block_service_account is set in a project's own settings.yaml; it is never consulted there, only the broker's global settings are used for this account",
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
	if sel.isFlat() {
		return cloudRunIdentity{}, cloudRunFlatRefusal(in.Name, mode)
	}
	// No runtime entry (a broker default with no settings entry, or a
	// selection that could not be resolved) has no settings to read: refused
	// without loading the global settings.
	if sel.RuntimeEntryName == "" {
		if mode == store.GCPMetadataModeBlock {
			return cloudRunIdentity{}, cloudRunBlockUnsetRefusal(in.Name, "")
		}
		return cloudRunIdentity{}, notAttachable()
	}
	// Global file plus the DB-backed overlay only: no project layer and no
	// SCION_ environment provider.
	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		return cloudRunIdentity{}, s.cloudRunSettingsUnreadable(in.Name, "runtimes."+sel.RuntimeEntryName+".cloudrun")
	}
	if cloudRunInstancesEntry(vs, sel.RuntimeEntryName) {
		return cloudRunIdentity{}, cloudRunInstancesRefusal(in.Name, sel.RuntimeEntryName, mode)
	}

	if mode == store.GCPMetadataModeBlock {
		// settings.yaml can be hand-edited without any write-time validator,
		// so a malformed value is reported where it is read, by setting name
		// only.
		for _, w := range config.CloudRunBlockServiceAccountWarnings(vs, sel.RuntimeEntryName) {
			s.agentLifecycleLog.Warn(w, "agent", in.Name, "runtime_entry", sel.RuntimeEntryName)
		}
		blockSA := ""
		if entry, ok := vs.Runtimes[sel.RuntimeEntryName]; ok && entry.CloudRun != nil {
			blockSA = strings.ToLower(strings.TrimSpace(entry.CloudRun.BlockServiceAccount))
		}
		if blockSA == "" {
			return cloudRunIdentity{}, cloudRunBlockUnsetRefusal(in.Name, sel.RuntimeEntryName)
		}
		return cloudRunIdentity{
			Identity:  &api.CloudRunIdentity{ServiceAccount: blockSA, Mode: store.GCPMetadataModeBlock},
			Selection: sel,
		}, nil
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

// refuseCloudRunInitFirewall refuses a Cloud Run "assign" dispatch (identity
// is the resolved Cloud Run identity, nil for any other dispatch) whose final
// env asks sciontool init for its firewall, or carries a value init cannot
// parse (scionrt.CloudRunInitFirewallRefused, which reads the value as init
// does). env is the merged env after every layer the broker applies (the
// hub's resolved env, then the config env). The settings, harness-config
// and harness env sources are refused by the Cloud Run runtime check
// (checkCloudRunInitFirewall). "block" and an off value are not refused.
// The refusal names the agent and the setting, never its value or an
// account.
func refuseCloudRunInitFirewall(agentName string, identity *api.CloudRunIdentity, env map[string]string) *startContextError {
	if identity == nil || identity.Mode != store.GCPMetadataModeAssign {
		return nil
	}
	value, ok := env[cloudRunInitFirewallEnv]
	if !ok {
		return nil
	}
	refusal := scionrt.CloudRunInitFirewallRefused(value)
	if refusal == "" {
		return nil
	}
	return &startContextError{
		Status:  http.StatusBadRequest,
		Message: fmt.Sprintf("agent %q: %s", agentName, refusal),
	}
}

// cloudRunFlatRefusal refuses assign or block (mode) on a flat Runtime
// Broker instance, which has no allow-list or block account setting.
func cloudRunFlatRefusal(agentName, mode string) *startContextError {
	return &startContextError{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf("agent %q: flat Cloud Run instances do not support assigned identities yet (GCP identity mode %q with the %s experiment on)",
			agentName, mode, experiments.CloudRunNativeIdentity),
	}
}

// cloudRunProfileSettingKey is the setting the runtime entry of a dispatch
// is selected by: the named profile's runtime, or the active profile when
// the dispatch names none.
func cloudRunProfileSettingKey(profile string) string {
	if profile == "" {
		return "active_profile"
	}
	return "profiles." + profile + ".runtime"
}

// cloudRunSettingsUnreadable refuses a dispatch whose settings could not be
// loaded. The log and the refusal name the setting key only: a settings
// loader error can quote file content, so its text is never logged or
// returned.
func (s *Server) cloudRunSettingsUnreadable(agentName, key string) *startContextError {
	s.agentLifecycleLog.Warn("Cloud Run identity: a setting could not be read", "agent", agentName, "setting", key)
	return &startContextError{
		Status:  http.StatusInternalServerError,
		Message: fmt.Sprintf("agent %q: %s could not be read", agentName, key),
	}
}

// cloudRunInstancesRefusal refuses assign or block (mode) on a runtime entry
// of type cloudrun-instances, which has no allow-list or block account
// setting.
func cloudRunInstancesRefusal(agentName, entryName, mode string) *startContextError {
	return &startContextError{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf("agent %q: runtime entry %q of type %s does not support assigned identities (GCP identity mode %q with the %s experiment on)",
			agentName, entryName, cloudRunInstancesRuntimeType, mode, experiments.CloudRunNativeIdentity),
	}
}

// cloudRunBlockUnsetRefusal refuses block on Cloud Run Instances when the
// selected runtime entry (entryName, or "" when none was selected) sets no
// block account. It names the setting to configure and no account.
func cloudRunBlockUnsetRefusal(agentName, entryName string) *startContextError {
	if entryName == "" {
		entryName = "<entry>"
	}
	return &startContextError{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf("agent %q: GCP identity mode %q on Cloud Run Instances needs a block service account; set runtimes.%s.cloudrun.block_service_account on the broker",
			agentName, store.GCPMetadataModeBlock, entryName),
	}
}

// cloudRunInstancesEntry reports whether the runtime entry entryName in the
// global settings vs configures Cloud Run Instances through a
// cloudrun_instances block: its effective type is cloudrun-instances (the
// type field, else the entry's name, as VersionedSettings.ResolveRuntime
// takes it), or it has that block and no cloudrun block.
func cloudRunInstancesEntry(vs *config.VersionedSettings, entryName string) bool {
	if vs == nil {
		return false
	}
	entry, ok := vs.Runtimes[entryName]
	if !ok {
		return false
	}
	runtimeType := entry.Type
	if runtimeType == "" {
		runtimeType = entryName
	}
	return runtimeType == cloudRunInstancesRuntimeType || (entry.CloudRun == nil && entry.CloudRunInstances != nil)
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
// later, authoritative resolution (resolvedRuntimeType, lateSelection for
// the profile and runtime entry, and pinnedEntry) does not match what
// buildStartContext resolved the Cloud Run identity for (early is the
// selection the allow-list or block account was read from):
//
//   - an identity was resolved for Cloud Run (and an "assign" metadata env
//     rewritten to passthrough), but the later runtime is not Cloud Run,
//     where the identity would be ignored;
//   - an identity was resolved for one Cloud Run profile or runtime entry,
//     but the later resolution selects another, whose settings were never
//     read;
//   - an identity was resolved, but the later resolution runs the agent on
//     the runtime its container was found on (pinnedEntry is non-nil: the
//     global runtime entry that runtime was built from, or "" when not
//     recorded), and that entry is not confirmed to be the early one;
//   - no identity was resolved, but the later runtime is Cloud Run with the
//     experiment on, where the instance would run as the runtime's
//     configured account.
//
// Each is refused before any side effect. The message names the agent and
// the runtime or selection change only, never an account.
func rejectCloudRunIdentityRuntimeChange(ctx context.Context, opts api.StartOptions, early *dispatchProfileSelection, resolvedRuntimeType string, lateSelection func() dispatchProfileSelection, pinnedEntry *string) *startContextError {
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
		} else if pinnedEntry != nil && *pinnedEntry == "" {
			msg = fmt.Sprintf("agent %q: the GCP identity on Cloud Run Instances was resolved for runtime entry %q, but the agent runs on the runtime its container was found on, which records no runtime entry; recreate the agent or restart it with its saved profile",
				opts.Name, was.RuntimeEntryName)
		} else if pinnedEntry != nil && *pinnedEntry != was.RuntimeEntryName {
			msg = fmt.Sprintf("agent %q: the GCP identity on Cloud Run Instances was resolved for runtime entry %q, but the agent runs on the runtime its container was found on, built from runtime entry %q; %s",
				opts.Name, was.RuntimeEntryName, *pinnedEntry, legacyIdentityChangeRemedy)
		}
	}
	if msg == "" {
		return nil
	}
	return &startContextError{Status: http.StatusConflict, Message: msg}
}
