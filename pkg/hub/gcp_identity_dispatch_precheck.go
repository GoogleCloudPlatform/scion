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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

// identityNotMappedPrecheck is the hub's refusal of a GCP identity
// "assign" dispatch on a Kubernetes profile whose recent, complete
// service account report does not include the GCP service account
// (ptone/scion#3329 phase 4b). identityMappingDispatchError recognises it,
// so every path that relays a broker identity_not_mapped refusal relays
// this one the same way.
type identityNotMappedPrecheck struct {
	Account   string
	Profile   string
	Broker    string
	Namespace string
}

func (e *identityNotMappedPrecheck) Error() string {
	return ErrCodeIdentityNotMapped + ": " + e.translate().Message
}

// translate builds the hub's client-facing form of the refusal.
func (e *identityNotMappedPrecheck) translate() identityMappingError {
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

// kubernetesIdentityNotMapped decides, from the hub's stored copy of a
// broker's per-profile service account report, whether a dispatch for an
// agent with gcpID on profileName is certain to be refused by the broker
// because the GCP service account is not mapped there. It returns nil,
// meaning dispatch and let the broker decide, unless all of these hold:
//
//   - the mode is "assign" with a service account email;
//   - the hub has recorded the agent's profile (for an agent whose profile
//     the hub has not recorded, the broker picks one from the project's
//     settings, which the hub does not see) and the broker has a stored
//     profile of that name;
//   - the profile's Type is Kubernetes (gated on the type, not on whether
//     a report exists: a profile switched away from Kubernetes can keep an
//     old report);
//   - the report is complete, at most profileSAReportFreshFor old, and at
//     api.BrokerSAReportVersion or later (an older broker's report may be
//     complete for a profile dispatch would not use);
//   - the account is not ambiguous (more than one annotated KSA: the broker
//     decides) and is in none of the report's entries.
//
// The namespace is not resolved here: the report's entries carry the one
// the broker resolved for dispatch (resolveAssignNamespace).
func kubernetesIdentityNotMapped(broker *store.RuntimeBroker, profileName string, gcpID *store.GCPIdentityConfig, now time.Time) *identityNotMappedPrecheck {
	if broker == nil || gcpID == nil || gcpID.MetadataMode != store.GCPMetadataModeAssign || profileName == "" {
		return nil
	}
	account := strings.ToLower(strings.TrimSpace(gcpID.ServiceAccountEmail))
	if account == "" {
		return nil
	}
	var profile *store.BrokerProfile
	for i := range broker.Profiles {
		if broker.Profiles[i].Name == profileName {
			profile = &broker.Profiles[i]
			break
		}
	}
	if profile == nil || !isKubernetesRuntimeType(profile.Type) {
		return nil
	}
	if !profile.MappingsReported || !profile.MappingsComplete || profile.MappingsReportedAt == nil {
		return nil
	}
	// A report from a broker that predates api.BrokerSAReportVersion may
	// be complete while dispatch would not use the profile's entries (for
	// example under ForceRuntime), so it is unknown.
	if profile.MappingsReportVersion < api.BrokerSAReportVersion {
		return nil
	}
	if now.Sub(*profile.MappingsReportedAt) > profileSAReportFreshFor {
		return nil
	}
	for _, gsa := range profile.AmbiguousGSAs {
		if strings.EqualFold(gsa, account) {
			return nil
		}
	}
	namespace := ""
	for _, m := range profile.ServiceAccountMappings {
		if strings.EqualFold(m.GSA, account) {
			return nil
		}
		if namespace == "" {
			namespace = m.Namespace
		}
	}
	return &identityNotMappedPrecheck{
		Account:   account,
		Profile:   profile.Name,
		Broker:    broker.Name,
		Namespace: namespace,
	}
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
