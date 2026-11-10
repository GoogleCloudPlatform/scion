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
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// profileSAReportFreshFor is how old a broker profile's service account
// report (BrokerProfile.MappingsReportedAt) may be for the hub to refuse a
// dispatch from it. A current broker sends its report hash on every
// heartbeat, and the hub moves MappingsReportedAt forward at least every
// profileSAReportConfirmInterval (5 minutes) while the hash matches, so 3x
// that interval tolerates two missed confirmations. An older report means
// unknown, and the broker decides.
const profileSAReportFreshFor = 3 * profileSAReportConfirmInterval

// identityCheckedByHubReport is the value of the checkedBy detail on an
// identity_not_mapped error the hub raised from the broker's report before
// dispatch, rather than one the broker returned.
const identityCheckedByHubReport = "hub_report"

// kubernetesSAReportMinVersion is the least broker report version
// (BrokerProfile.MappingsReportVersion) from which the hub decides a
// Kubernetes profile's mapping: version 2 reports a profile incomplete
// under ForceRuntime. Later versions add only Cloud Run reports, so a
// Kubernetes report at version 2 is decided as one at a later version.
const kubernetesSAReportMinVersion = 2

// cloudRunSAReportMinVersion is the least broker report version that
// carries Cloud Run reports (the assignable accounts and the native
// identity and block account booleans). The Cloud Run checks require it;
// the Kubernetes decision does not.
const cloudRunSAReportMinVersion = 3

// identityNotMappedPrecheck is the hub's refusal of a GCP identity
// "assign" dispatch on a Kubernetes profile whose recent, complete
// service account report does not include the GCP service account
// (ptone/scion#3329 phase 4b). identityMappingDispatchError recognises it,
// so every path that relays a broker identity_not_mapped refusal relays
// this one the same way.
//
// With CloudRun set it is instead the hub's refusal of a dispatch on a Cloud
// Run profile while the hub.cloudrun_native_identity experiment is on
// (cloudRunIdentityRefusal); CloudRun is the reason (cloudRunRefusal*
// values). That form never names a service account: Account is empty.
type identityNotMappedPrecheck struct {
	Account   string
	Profile   string
	Broker    string
	Namespace string
	// CloudRun, Agent and Mode are set only on a Cloud Run refusal: the
	// reason, the agent's name and the GCP identity mode the dispatch
	// would run with.
	CloudRun string
	Agent    string
	Mode     string
}

func (e *identityNotMappedPrecheck) Error() string {
	return ErrCodeIdentityNotMapped + ": " + e.translate().Message
}

// translate builds the hub's client-facing form of the refusal.
func (e *identityNotMappedPrecheck) translate() identityMappingError {
	if e.CloudRun != "" {
		return e.translateCloudRun()
	}
	details := map[string]interface{}{
		"docs":                            kubernetesIdentityMappingDocsURL,
		"checkedBy":                       identityCheckedByHubReport,
		api.BrokerErrDetailServiceAccount: e.Account,
		api.BrokerErrDetailProfile:        e.Profile,
	}
	if e.Broker != "" {
		details[api.BrokerErrDetailBroker] = e.Broker
	}
	where := "the agent's namespace"
	if e.Namespace != "" {
		details[api.BrokerErrDetailNamespace] = e.Namespace
		where = fmt.Sprintf("namespace %q", e.Namespace)
	}
	msg := fmt.Sprintf("GCP service account %q is not mapped on %s: "+
		"the broker's latest service account report lists no Kubernetes service account for it in %s. "+
		"To fix it, a broker operator adds it to kubernetes_service_account_mappings in that broker's settings, "+
		"or annotates a Kubernetes service account in %s with iam.gke.io/gcp-service-account=%s. "+
		"The broker rechecks annotations every 5 minutes; see %s",
		e.Account, identityMappingScopeText(e.Profile, "", e.Broker), where, where, e.Account,
		kubernetesIdentityMappingDocsURL)
	return identityMappingError{Code: ErrCodeIdentityNotMapped, Message: msg, Details: details}
}

// Reasons of a Cloud Run refusal (identityNotMappedPrecheck.CloudRun), also
// sent as its "reason" detail.
const (
	cloudRunRefusalNotAttachable  = "cloudrun_account_not_attachable"
	cloudRunRefusalNoBlockAccount = "cloudrun_block_account_missing"
	cloudRunRefusalPassthrough    = "cloudrun_passthrough_unavailable"
	cloudRunRefusalUnsupported    = "cloudrun_native_identity_unsupported"
	cloudRunRefusalBrokerUpgrade  = "cloudrun_broker_upgrade_required"
	cloudRunRefusalProfileUnknown = "cloudrun_profile_unknown"
	cloudRunRefusalReportStale    = "cloudrun_report_not_current"
)

// translateCloudRun builds the client-facing form of a Cloud Run refusal.
// It names the agent, the profile and the broker, says who can fix it, and
// never names a service account.
func (e *identityNotMappedPrecheck) translateCloudRun() identityMappingError {
	details := map[string]interface{}{
		"checkedBy":                identityCheckedByHubReport,
		"reason":                   e.CloudRun,
		api.BrokerErrDetailProfile: e.Profile,
	}
	if e.Broker != "" {
		details[api.BrokerErrDetailBroker] = e.Broker
	}
	scope := identityMappingScopeText(e.Profile, "", e.Broker)
	var msg string
	switch e.CloudRun {
	case cloudRunRefusalNotAttachable:
		msg = fmt.Sprintf("agent %q: GCP identity mode %q on Cloud Run Instances: the agent's service account is not attachable on %s; "+
			"the broker's latest report does not list it in cloudrun.assignable_service_accounts. "+
			"A broker operator adds it to that list in the broker's global settings, or assign the agent an account the broker can attach",
			e.Agent, store.GCPMetadataModeAssign, scope)
	case cloudRunRefusalNoBlockAccount:
		msg = fmt.Sprintf("agent %q: GCP identity mode %q on Cloud Run Instances needs a block service account, and %s reports none. "+
			"A broker operator sets cloudrun.block_service_account in the broker's global settings",
			e.Agent, store.GCPMetadataModeBlock, scope)
		if e.Mode != store.GCPMetadataModeBlock {
			msg = fmt.Sprintf("agent %q: an unset GCP identity runs as mode %q on Cloud Run Instances, which needs a block service account, and %s reports none. "+
				"A broker operator sets cloudrun.block_service_account in the broker's global settings, or assign the agent an account",
				e.Agent, store.GCPMetadataModeBlock, scope)
		}
	case cloudRunRefusalPassthrough:
		msg = fmt.Sprintf("agent %q on %s: passthrough is not available on Cloud Run Instances; assign an account or use block",
			e.Agent, scope)
	case cloudRunRefusalProfileUnknown:
		where := fmt.Sprintf("the agent's profile on broker %q", e.Broker)
		if e.Profile != "" {
			where = fmt.Sprintf("profile %q of broker %q", e.Profile, e.Broker)
		}
		msg = fmt.Sprintf("agent %q: %s is not known to the hub, and that broker runs Cloud Run Instances with assigned identities while the %s experiment is on, "+
			"so the hub cannot tell how the agent's GCP identity would run. Re-register the broker so the hub records its profiles, or set a profile the hub knows",
			e.Agent, where, experiments.CloudRunNativeIdentity)
	case cloudRunRefusalReportStale:
		msg = fmt.Sprintf("agent %q: the latest Cloud Run report of %s is stale or incomplete (broker report stale or broker downgraded), "+
			"so the hub cannot check this agent's GCP identity while the %s experiment is on; check that the broker is running a current version and reporting, or turn the experiment off",
			e.Agent, scope, experiments.CloudRunNativeIdentity)
	case cloudRunRefusalUnsupported:
		msg = fmt.Sprintf("agent %q: %s does not support assigned identities on Cloud Run Instances (only a runtimes.<entry>.cloudrun entry does) while the %s experiment is on; "+
			"use a profile that does, or turn the experiment off",
			e.Agent, scope, experiments.CloudRunNativeIdentity)
	default:
		msg = fmt.Sprintf("agent %q: %s has not reported Cloud Run identity support at service account report version %d or later, "+
			"so it cannot run this agent's GCP identity while the %s experiment is on; upgrade the broker or turn the experiment off",
			e.Agent, scope, cloudRunSAReportMinVersion, experiments.CloudRunNativeIdentity)
	}
	return identityMappingError{Code: ErrCodeIdentityNotMapped, Message: msg, Details: details}
}

// SA mapping decision states (SAMappingDecision.State). "mapped" means the
// broker's report lists a Kubernetes service account for the GCP service
// account; it does not mean ready, because the Workload Identity IAM
// binding is not visible to the broker or the hub.
const (
	SAMappingStateMapped      = "mapped"
	SAMappingStateNotMapped   = "not_mapped"
	SAMappingStateNotRequired = "not_required"
	SAMappingStateUnknown     = "unknown"
)

// SA mapping decision reasons (SAMappingDecision.Reason), set only when the
// state is unknown. These codes are shared by the dispatch refusal, the
// assign picker and the per-account status view.
const (
	SAMappingReasonNoBroker            = "no_broker"
	SAMappingReasonNoProfile           = "no_profile"
	SAMappingReasonNoAccount           = "no_account"
	SAMappingReasonProfileNotOnBroker  = "profile_not_on_broker"
	SAMappingReasonRuntimeUnrecognized = "runtime_unrecognized"
	SAMappingReasonReportMissing       = "report_missing"
	SAMappingReasonReportIncomplete    = "report_incomplete"
	SAMappingReasonReportOldVersion    = "report_old_version"
	SAMappingReasonReportStale         = "report_stale"
	SAMappingReasonAmbiguousMapping    = "ambiguous_mapping"
)

// SAMappingDecision is the hub's answer, from its stored copy of one broker
// profile's service account report, to whether a GCP service account is
// mapped on that profile.
type SAMappingDecision struct {
	State  string
	Reason string
	// Account is the normalized (lowercased) GCP service account.
	Account string
	// Profile and Broker name the profile and broker decided on, when known.
	Profile string
	Broker  string
	// Namespace is the namespace the report's entries carry: the matched
	// entry's for mapped, the first entry's for not_mapped.
	Namespace string
}

// profileSAMappingDecision decides, from the hub's stored copy of broker's
// per-profile service account report, whether account is mapped on
// profileName. It is the one decision behind the dispatch refusal
// (kubernetesIdentityNotMapped refuses only on not_mapped), the assign
// picker's assignStatus and the per-account status view, so they agree for
// the same inputs.
//
// The state is unknown, never not_mapped, unless all of these hold:
//
//   - a broker, a profile name and an account are given;
//   - the broker has a stored profile of that name (for a profile the hub
//     has not recorded, the broker decides);
//   - the profile's Type is Kubernetes (gated on the type, not on whether a
//     report exists: a profile switched away from Kubernetes can keep an
//     old report);
//   - the report is complete, was confirmed within profileSAReportFreshFor,
//     and is at kubernetesSAReportMinVersion or later (a report below it
//     may be complete for a profile dispatch would not use);
//   - the account is not ambiguous (more than one annotated KSA: the broker
//     decides).
//
// A profile whose Type is a local runtime (docker, podman, container) needs
// no mapping and gives not_required. Any other non-Kubernetes Type is
// unknown: a custom runtime key can name a Kubernetes runtime the hub
// cannot see.
func profileSAMappingDecision(broker *store.RuntimeBroker, profileName, account string, now time.Time) SAMappingDecision {
	d := SAMappingDecision{
		State:   SAMappingStateUnknown,
		Account: strings.ToLower(strings.TrimSpace(account)),
		Profile: profileName,
	}
	if broker == nil {
		d.Reason = SAMappingReasonNoBroker
		return d
	}
	d.Broker = broker.Name
	if profileName == "" {
		d.Reason = SAMappingReasonNoProfile
		return d
	}
	if d.Account == "" {
		d.Reason = SAMappingReasonNoAccount
		return d
	}
	for i := range broker.Profiles {
		if broker.Profiles[i].Name == profileName {
			return profileSAMappingDecisionOn(broker.Name, &broker.Profiles[i], account, now)
		}
	}
	d.Reason = SAMappingReasonProfileNotOnBroker
	return d
}

// profileSAMappingDecisionOn is profileSAMappingDecision for a profile the
// caller already holds, on the broker named brokerName. The per-account
// status view and the not-mapped warnings use it on the profile rows they
// collected, so they apply the same rule as the dispatch refusal. A nil
// profile is unknown with reason no_profile: there is no profile to decide
// on.
func profileSAMappingDecisionOn(brokerName string, profile *store.BrokerProfile, account string, now time.Time) SAMappingDecision {
	d := SAMappingDecision{
		State:   SAMappingStateUnknown,
		Account: strings.ToLower(strings.TrimSpace(account)),
		Broker:  brokerName,
	}
	if profile == nil {
		d.Reason = SAMappingReasonNoProfile
		return d
	}
	d.Profile = profile.Name
	if d.Account == "" {
		d.Reason = SAMappingReasonNoAccount
		return d
	}
	if !isKubernetesRuntimeType(profile.Type) {
		if localOnlyProfileTypes[profile.Type] {
			d.State = SAMappingStateNotRequired
			return d
		}
		d.Reason = SAMappingReasonRuntimeUnrecognized
		return d
	}
	if !profile.MappingsReported {
		d.Reason = SAMappingReasonReportMissing
		return d
	}
	if !profile.MappingsComplete {
		d.Reason = SAMappingReasonReportIncomplete
		return d
	}
	// A report below kubernetesSAReportMinVersion may be complete while
	// dispatch would not use the profile's entries (for example under
	// ForceRuntime), so it is unknown.
	if profile.MappingsReportVersion < kubernetesSAReportMinVersion {
		d.Reason = SAMappingReasonReportOldVersion
		return d
	}
	if profile.MappingsReportedAt == nil || now.Sub(*profile.MappingsReportedAt) > profileSAReportFreshFor {
		d.Reason = SAMappingReasonReportStale
		return d
	}
	for _, gsa := range profile.AmbiguousGSAs {
		if strings.EqualFold(gsa, d.Account) {
			d.Reason = SAMappingReasonAmbiguousMapping
			return d
		}
	}
	firstNamespace := ""
	for _, m := range profile.ServiceAccountMappings {
		if strings.EqualFold(m.GSA, d.Account) {
			d.State = SAMappingStateMapped
			d.Namespace = m.Namespace
			return d
		}
		if firstNamespace == "" {
			firstNamespace = m.Namespace
		}
	}
	d.State = SAMappingStateNotMapped
	d.Namespace = firstNamespace
	return d
}

// kubernetesIdentityNotMapped decides whether a dispatch for an agent with
// gcpID on profileName is certain to be refused by the broker because the
// GCP service account is not mapped there. It returns nil, meaning dispatch
// and let the broker decide, unless the mode is "assign" and
// profileSAMappingDecision says not_mapped.
func kubernetesIdentityNotMapped(broker *store.RuntimeBroker, profileName string, gcpID *store.GCPIdentityConfig, now time.Time) *identityNotMappedPrecheck {
	if gcpID == nil || gcpID.MetadataMode != store.GCPMetadataModeAssign {
		return nil
	}
	d := profileSAMappingDecision(broker, profileName, gcpID.ServiceAccountEmail, now)
	if d.State != SAMappingStateNotMapped {
		return nil
	}
	return &identityNotMappedPrecheck{
		Account:   d.Account,
		Profile:   d.Profile,
		Broker:    d.Broker,
		Namespace: d.Namespace,
	}
}

// gcpIdentityPrecheck runs the hub's GCP identity checks before a
// dispatch (create, create with gather, finalize env, provision, start and
// restart), before any token, run or broker call:
//
//   - the Cloud Run checks (cloudRunIdentityPrecheck), only while the
//     hub.cloudrun_native_identity experiment is on;
//   - the Kubernetes mapping check (kubernetesIdentityPrecheck);
//   - the native decision (markCloudRunNative), recorded on the agent and
//     returned on the context the request builders read.
//
// It returns the first refusal or failure, with the ctx it was given.
func (d *HTTPAgentDispatcher) gcpIdentityPrecheck(ctx context.Context, agent *store.Agent) (context.Context, error) {
	if err := d.cloudRunIdentityPrecheck(ctx, agent); err != nil {
		return ctx, err
	}
	if err := d.kubernetesIdentityPrecheck(ctx, agent); err != nil {
		return ctx, err
	}
	return d.markCloudRunNative(ctx, agent)
}

// kubernetesIdentityPrecheck returns the hub's identity_not_mapped refusal
// for agent's next dispatch when its broker's recent, complete report for
// the agent's Kubernetes profile does not include the agent's GCP service
// account, and nil otherwise (kubernetesIdentityNotMapped). A broker that
// cannot be read is left to the dispatch itself.
func (d *HTTPAgentDispatcher) kubernetesIdentityPrecheck(ctx context.Context, agent *store.Agent) error {
	if agent == nil || agent.AppliedConfig == nil || agent.RuntimeBrokerID == "" {
		return nil
	}
	gcpID := agent.AppliedConfig.GCPIdentity
	if gcpID == nil || gcpID.MetadataMode != store.GCPMetadataModeAssign || agent.AppliedConfig.Profile == "" {
		return nil
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil || broker == nil {
		return nil
	}
	if refusal := kubernetesIdentityNotMapped(broker, agent.AppliedConfig.Profile, gcpID, time.Now()); refusal != nil {
		d.log.Info("refusing dispatch: GCP service account not in the broker profile's service account report",
			"agent_id", agent.ID, "broker", broker.ID, "profile", refusal.Profile, "service_account", refusal.Account)
		return refusal
	}
	return nil
}

// isCloudRunProfileType reports whether a stored profile Type is a Cloud
// Run Instances runtime: "cloudrun" (the runtimes.<entry>.cloudrun form) or
// "cloudrun-instances". The broker runs both through the same runtime, so
// both are checked before a dispatch; only "cloudrun" can report native
// identity support. cloudrun-sandbox is a separate runtime and is not
// included.
func isCloudRunProfileType(t string) bool {
	return t == cloudRunProfileType || t == "cloudrun-instances"
}

// cloudRunIdentityRefusal decides, from the hub's stored copy of broker's
// service account reports, whether a dispatch with gcpID for the agent on
// profileName is refused before it reaches the broker. The caller calls it
// only while the hub.cloudrun_native_identity experiment is on. In order:
//
//   - the profile does not resolve (resolveAgentRuntimeProfileType) on a
//     broker that runs native Cloud Run (brokerRunsNativeCloudRun):
//     refused (cloudRunRefusalProfileUnknown), since the broker may resolve
//     it to a native entry the hub cannot check; on any other broker it is
//     left to the broker;
//   - the resolved profile's Type is not a Cloud Run type
//     (isCloudRunProfileType): left to the broker (and the Kubernetes
//     check);
//   - the report is missing or below cloudRunSAReportMinVersion: refused
//     for every mode (cloudRunRefusalBrokerUpgrade), since such a broker
//     would run the instance as its configured account;
//   - the report is not complete, or was not confirmed within
//     profileSAReportFreshFor: refused for every mode
//     (cloudRunRefusalReportStale), since a downgraded broker can leave a
//     version-3 report behind that no heartbeat renews;
//   - otherwise passthrough is refused; a profile without native identity
//     support refuses assign and block; assign is refused for an account
//     the report does not list; block, and an unset mode (block on Cloud
//     Run), are refused when no block account is reported.
//
// A passthrough granted by the hub default or by a confined project default
// (RequireLocalRuntime) is run as block on Cloud Run by the broker, so it is
// decided as block. An assign with no account is left to the broker and the
// start gate.
func cloudRunIdentityRefusal(broker *store.RuntimeBroker, profileName, agentName string, gcpID *store.GCPIdentityConfig, now time.Time) *identityNotMappedPrecheck {
	if broker == nil {
		return nil
	}
	mode := ""
	if gcpID != nil {
		mode = gcpID.MetadataMode
		if mode == store.GCPMetadataModePassthrough && gcpID.RequireLocalRuntime {
			mode = store.GCPMetadataModeBlock
		}
	}
	name, typ, ok := resolveAgentRuntimeProfileType(broker, profileName)
	if !ok {
		if brokerRunsNativeCloudRun(broker) {
			return &identityNotMappedPrecheck{Profile: profileName, Broker: broker.Name, CloudRun: cloudRunRefusalProfileUnknown, Agent: agentName, Mode: mode}
		}
		return nil
	}
	if !isCloudRunProfileType(typ) {
		return nil
	}
	var profile *store.BrokerProfile
	for i := range broker.Profiles {
		if broker.Profiles[i].Name == name {
			profile = &broker.Profiles[i]
			break
		}
	}
	if profile == nil {
		return nil
	}
	refuse := func(reason string) *identityNotMappedPrecheck {
		return &identityNotMappedPrecheck{Profile: name, Broker: broker.Name, CloudRun: reason, Agent: agentName, Mode: mode}
	}
	if !profile.MappingsReported || profile.MappingsReportVersion < cloudRunSAReportMinVersion {
		return refuse(cloudRunRefusalBrokerUpgrade)
	}
	if !profile.MappingsComplete || profile.MappingsReportedAt == nil || now.Sub(*profile.MappingsReportedAt) > profileSAReportFreshFor {
		return refuse(cloudRunRefusalReportStale)
	}
	if mode == store.GCPMetadataModePassthrough {
		return refuse(cloudRunRefusalPassthrough)
	}
	if !profile.CloudRunNativeIdentity {
		return refuse(cloudRunRefusalUnsupported)
	}
	if mode == store.GCPMetadataModeAssign {
		account := strings.TrimSpace(gcpID.ServiceAccountEmail)
		if account == "" {
			return nil
		}
		for _, m := range profile.ServiceAccountMappings {
			if strings.EqualFold(strings.TrimSpace(m.GSA), account) {
				return nil
			}
		}
		return refuse(cloudRunRefusalNotAttachable)
	}
	if !profile.CloudRunBlockAccount {
		return refuse(cloudRunRefusalNoBlockAccount)
	}
	return nil
}

// cloudRunIdentityPrecheck returns the hub's refusal of agent's next
// dispatch from cloudRunIdentityRefusal while the
// hub.cloudrun_native_identity experiment is in the dispatch experiments,
// and nil otherwise. With the experiment off it reads nothing. A broker
// that cannot be read is left to the dispatch itself.
func (d *HTTPAgentDispatcher) cloudRunIdentityPrecheck(ctx context.Context, agent *store.Agent) error {
	if agent == nil || agent.RuntimeBrokerID == "" || d.store == nil {
		return nil
	}
	if !slices.Contains(d.dispatchExperiments(), experiments.CloudRunNativeIdentity) {
		return nil
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil || broker == nil {
		return nil
	}
	var profileName string
	var gcpID *store.GCPIdentityConfig
	if agent.AppliedConfig != nil {
		profileName = agent.AppliedConfig.Profile
		gcpID = agent.AppliedConfig.GCPIdentity
	}
	refusal := cloudRunIdentityRefusal(broker, profileName, agent.Name, gcpID, time.Now())
	if refusal == nil {
		return nil
	}
	d.log.Info("refusing dispatch: the broker's Cloud Run report does not allow this agent's GCP identity",
		"agent_id", agent.ID, "broker", broker.ID, "profile", refusal.Profile, "reason", refusal.CloudRun)
	return refusal
}
