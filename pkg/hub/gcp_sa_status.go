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
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Per-account status view (ptone/scion#4018): one read model that answers,
// for a registered GCP service account seen from one project, whether it is
// verified, which Kubernetes broker profiles map it, what its Workload
// Identity binding is, which defaults point at it, which agents use it and
// what the first missing link is.
//
// It is served at GET /api/v1/projects/{pid}/gcp-service-accounts/{ref}/status
// rather than folded into the plain GET, because every section but identity
// and verification is relative to a project (its providers' profiles, its
// defaults, its agents), the plain GET is also served by the parentless flat
// route that has no project, and the plain GET's response is the stored row
// that existing clients decode as-is. The status read also costs broker and
// agent queries the plain GET should not pay.

// Mapping states for one Kubernetes broker profile: the shared decision's
// states (profileSAMappingDecision), plus "not_reported" for a profile
// with no report at all. A profile whose decision is not_required is not
// listed.
const (
	GCPSAMappingMapped      = SAMappingStateMapped
	GCPSAMappingCreatable   = SAMappingStateCreatable
	GCPSAMappingNotMapped   = SAMappingStateNotMapped
	GCPSAMappingUnknown     = SAMappingStateUnknown
	GCPSAMappingNotReported = "not_reported"
)

// Workload Identity binding states. When hub.k8s_workload_identity_setup is
// enabled, minted accounts read the GSA's IAM policy and report "bound" or
// "not_bound" per target, while bring-your-own accounts stay "unknown" ("not
// minted by this hub"); with the experiment off only "unknown" ("not checked")
// is returned.
const (
	GCPSABindingBound    = "bound"
	GCPSABindingNotBound = "not_bound"
	GCPSABindingUnknown  = "unknown"
)

// Default kinds an account can be the default for.
const (
	GCPSADefaultProject = "project"
	GCPSADefaultProfile = "profile"
	GCPSADefaultHub     = "hub"
)

// Next-step codes: the first missing link. GCPSANextStepNotBound is returned
// only when hub.k8s_workload_identity_setup is enabled and at least one
// Workload Identity target is not bound.
const (
	GCPSANextStepNotVerified = "not_verified"
	GCPSANextStepNotMapped   = "not_mapped"
	GCPSANextStepNotBound    = "not_bound"
	GCPSANextStepNone        = "none"
)

// gcpSAStatusAgentNameLimit caps the agent names returned; the count is
// always the full count.
const gcpSAStatusAgentNameLimit = 50

// GCPServiceAccountStatus is the per-account status view.
type GCPServiceAccountStatus struct {
	Account                 GCPServiceAccountStatusIdentity   `json:"account"`
	Verification            GCPServiceAccountVerification     `json:"verification"`
	Mappings                []GCPServiceAccountProfileMapping `json:"mappings"`
	WorkloadIdentityBinding GCPServiceAccountBinding          `json:"workloadIdentityBinding"`
	DefaultFor              []GCPServiceAccountDefault        `json:"defaultFor"`
	Agents                  GCPServiceAccountAgents           `json:"agents"`
	NextStep                GCPServiceAccountNextStep         `json:"nextStep"`
}

// GCPServiceAccountStatusIdentity identifies the account. The email is shown
// to anyone who may read the account in this project, agents included
// (design §9 Q1).
type GCPServiceAccountStatusIdentity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	Scope       string `json:"scope"`
	Email       string `json:"email"`
}

// GCPServiceAccountVerification is the hub's last token-creator check.
type GCPServiceAccountVerification struct {
	Status     string     `json:"status"`
	Verified   bool       `json:"verified"`
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// GCPServiceAccountProfileMapping is one Kubernetes broker profile's mapping
// state for the account. Mappings stay broker-owned; this is read-only.
//
// Every field after State is additive and omitted when the broker did not
// report it, so a report from an older broker reads as State alone.
type GCPServiceAccountProfileMapping struct {
	BrokerID   string `json:"brokerId"`
	BrokerName string `json:"brokerName"`
	Profile    string `json:"profile"`
	State      string `json:"state"`
	// KubernetesServiceAccount, Namespace and Source describe a mapped
	// account: the ServiceAccount the agent pod runs as, its namespace, and
	// whether the broker found it in kubernetes_service_account_mappings
	// ("mapped") or by annotation discovery ("discovered").
	KubernetesServiceAccount string `json:"kubernetesServiceAccount,omitempty"`
	Namespace                string `json:"namespace,omitempty"`
	Source                   string `json:"source,omitempty"`
	// ReportedAt is when the hub last stored or confirmed the profile's
	// report; clients show its age.
	ReportedAt *time.Time `json:"reportedAt,omitempty"`
	// Incomplete is true when the broker said its report may not list
	// every account the profile can serve, for IncompleteReason (for
	// example a ServiceAccount list that failed). A "not_mapped" state on
	// an incomplete report is then not conclusive.
	Incomplete       bool   `json:"incomplete,omitempty"`
	IncompleteReason string `json:"incompleteReason,omitempty"`
	// Ambiguous is true when more than one ServiceAccount in the namespace
	// is annotated with the account and none is mapped explicitly; the
	// broker refuses it at dispatch. State is then "unknown" with
	// UnknownReason ambiguous_mapping.
	Ambiguous bool `json:"ambiguous,omitempty"`
	// UnknownReason says why State is "unknown": the shared decision's
	// reason (SAMappingReason*), for example report_stale or
	// ambiguous_mapping.
	UnknownReason string `json:"unknownReason,omitempty"`
	// WorkloadIdentityBinding is this profile's Workload Identity IAM binding
	// state when hub.k8s_workload_identity_setup is enabled; omitted when off.
	WorkloadIdentityBinding *GCPServiceAccountProfileBinding `json:"workloadIdentityBinding,omitempty"`
}

// GCPServiceAccountBinding is the Workload Identity binding state.
type GCPServiceAccountBinding struct {
	State   string                           `json:"state"`
	Reason  string                           `json:"reason,omitempty"`
	Targets []GCPServiceAccountBindingTarget `json:"targets,omitempty"`
}

// GCPServiceAccountDefault names one default that points at the account.
type GCPServiceAccountDefault struct {
	Kind    string `json:"kind"`
	Profile string `json:"profile,omitempty"`
}

// GCPServiceAccountAgents lists the agents of this project whose applied GCP
// identity is the account, limited to agents the caller may see. Names holds
// at most gcpSAStatusAgentNameLimit entries; Count is the full count.
type GCPServiceAccountAgents struct {
	Count int      `json:"count"`
	Names []string `json:"names"`
}

// GCPServiceAccountNextStep is the first missing link.
type GCPServiceAccountNextStep struct {
	Code       string `json:"code"`
	BrokerName string `json:"brokerName,omitempty"`
	Profile    string `json:"profile,omitempty"`
	Message    string `json:"message"`
}

// getGCPServiceAccountStatus handles
// GET /api/v1/projects/{projectID}/gcp-service-accounts/{ref}/status. ref is
// an id, an email or a display name, resolved the same way as on assign.
//
// Authorization is the same as reading the account on the nested GET, plus:
// an agent may read it only from its own project, and a user must be able
// to read the project.
func (s *Server) getGCPServiceAccountStatus(w http.ResponseWriter, r *http.Request, projectID, ref string) {
	ctx := r.Context()

	if agent := GetAgentIdentityFromContext(ctx); agent != nil && agent.ProjectID() != projectID {
		NotFound(w, "GCP Service Account")
		return
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	// Project read for user callers. The view exposes more than the stored
	// row (project and hub defaults, provider broker and profile names), so a
	// user must be able to read the project, which keeps the "project members
	// see it" rule (design §9 Q1) holding if the hub-wide read policy is
	// narrowed later. Agents are not put through this check: Q1 includes the
	// project's agents, and their access is the own-project isolation above,
	// with the agents section gated on the project:read token scope
	// (gcpServiceAccountAgents).
	if GetAgentIdentityFromContext(ctx) == nil {
		if !s.authorize(w, r, projectResource(project), ActionRead) {
			return
		}
	}

	sa, err := s.resolveGCPServiceAccountRef(ctx, projectID, ref)
	if err != nil {
		if writeGCPSAAmbiguous(w, err) {
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "GCP Service Account")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if !sa.ReachableFromProject(projectID) {
		NotFound(w, "GCP Service Account")
		return
	}
	// The same read gate as getGCPServiceAccount: hub-scoped accounts get a
	// read check, project-scoped reads have none (see that handler).
	if sa.Scope == store.ScopeHub {
		if !s.authorizeGCPServiceAccount(w, r, sa, ActionRead) {
			return
		}
	}

	brokers, _ := s.projectProviderBrokers(ctx, projectID)
	status := s.buildGCPServiceAccountStatus(ctx, project, sa, brokers)
	status.Agents = s.gcpServiceAccountAgents(ctx, projectID, sa.ID)
	writeJSON(w, http.StatusOK, status)
}

// buildGCPServiceAccountStatus assembles every section except agents, which
// needs the caller's identity.
func (s *Server) buildGCPServiceAccountStatus(ctx context.Context, project *store.Project, sa *store.GCPServiceAccount, brokers []*store.RuntimeBroker) GCPServiceAccountStatus {
	now := gcpSAWIBindingNow()
	expOn := s.experimentEnabled(experiments.K8sWorkloadIdentitySetup)
	allowCreate := expOn && sa.Managed
	profiles := s.brokerKubernetesProfileMappings(brokers)
	mappings := gcpSAProfileMappingsFor(sa.Email, allowCreate, profiles, now)
	binding := GCPServiceAccountBinding{
		State:  GCPSABindingUnknown,
		Reason: GCPSABindingReasonNotChecked,
	}
	if expOn {
		binding = s.evaluateWorkloadIdentityStatus(ctx, sa, brokers, mappings, now)
	}
	st := GCPServiceAccountStatus{
		Account: GCPServiceAccountStatusIdentity{
			ID: sa.ID, DisplayName: sa.DisplayName, Scope: sa.Scope, Email: sa.Email,
		},
		Verification:            gcpSAVerificationOf(sa),
		Mappings:                mappings,
		WorkloadIdentityBinding: binding,
		DefaultFor:              s.gcpSADefaultsFor(project, sa.ID),
		Agents:                  GCPServiceAccountAgents{Names: []string{}},
	}
	st.NextStep = gcpSANextStepForBinding(st.Verification.Verified, st.Mappings, st.DefaultFor, st.WorkloadIdentityBinding)
	return st
}

func gcpSAVerificationOf(sa *store.GCPServiceAccount) GCPServiceAccountVerification {
	v := GCPServiceAccountVerification{
		Status:   sa.VerificationStatus,
		Verified: gcpServiceAccountVerified(sa),
		Error:    sa.VerificationError,
	}
	if v.Status == "" {
		v.Status = store.GCPVerificationUnverified
		if v.Verified {
			v.Status = store.GCPVerificationVerified
		}
	}
	if !sa.VerifiedAt.IsZero() {
		t := sa.VerifiedAt
		v.VerifiedAt = &t
	}
	return v
}

// gcpSAProfileMappingsFor maps each Kubernetes profile to a state for email,
// sorted by broker then profile name. The state is the shared decision's
// (kubernetesProfileMappings.decisionFor, the dispatch refusal's rule), so the
// view, the warnings and the refusal agree. A profile with no report at all
// is "not_reported"; one whose decision is not_required is left out.
func gcpSAProfileMappingsFor(email string, allowCreate bool, profiles []kubernetesProfileMappings, now time.Time) []GCPServiceAccountProfileMapping {
	out := make([]GCPServiceAccountProfileMapping, 0, len(profiles))
	key := strings.ToLower(email)
	for _, p := range profiles {
		m := GCPServiceAccountProfileMapping{
			BrokerID: p.brokerID, BrokerName: p.brokerName, Profile: p.profile, State: GCPSAMappingNotReported,
		}
		if p.reported {
			d := p.decisionFor(email, allowCreate, now)
			if d.State == SAMappingStateNotRequired {
				continue
			}
			m.State, m.UnknownReason = d.State, d.Reason
			switch m.State {
			case GCPSAMappingMapped:
				e := p.entries[key]
				m.KubernetesServiceAccount, m.Namespace, m.Source = e.KSA, e.Namespace, e.Source
			case GCPSAMappingCreatable:
				if ksa, err := k8s.ConventionalKSAName(email); err == nil {
					m.KubernetesServiceAccount = ksa
				}
				m.Namespace = d.Namespace
				m.Source = api.BrokerKSASourceCreated
			}
			m.ReportedAt = p.report.MappingsReportedAt
			if p.report.MappingsReported && !p.report.MappingsComplete {
				m.IncompleteReason = p.report.MappingsIncompleteReason
			}
			m.Incomplete = m.IncompleteReason != ""
			m.Ambiguous = d.Reason == SAMappingReasonAmbiguousMapping
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].BrokerName != out[j].BrokerName {
			return out[i].BrokerName < out[j].BrokerName
		}
		return out[i].Profile < out[j].Profile
	})
	return out
}

// gcpSAMappedCountFor counts, with the shared decision at now
// (kubernetesProfileMappings.decisionFor), the profiles whose decision for
// email is mapped (or creatable, when allowCreate is true) and the profiles
// that reported. A profile whose decision is not_required is counted in
// neither (the status view does not list it); notRequired counts those so the
// summary can leave them out of the unreported count too.
func gcpSAMappedCountFor(email string, allowCreate bool, profiles []kubernetesProfileMappings, now time.Time) (mapped, reported, notRequired int) {
	for _, p := range profiles {
		if !p.reported {
			continue
		}
		switch p.decisionFor(email, allowCreate, now).State {
		case SAMappingStateNotRequired:
			notRequired++
			continue
		case SAMappingStateMapped, SAMappingStateCreatable:
			mapped++
		}
		reported++
	}
	return mapped, reported, notRequired
}

// gcpSADefaultsFor lists the defaults that name saID: the project default
// (when its mode is assign), each per-profile project default, and the hub
// default (when its mode is assign).
func (s *Server) gcpSADefaultsFor(project *store.Project, saID string) []GCPServiceAccountDefault {
	out := []GCPServiceAccountDefault{}
	if ps := projectSettingsFromAnnotations(project); ps.DefaultGCPIdentityMode == store.GCPMetadataModeAssign &&
		ps.DefaultGCPIdentityServiceAccountID == saID {
		out = append(out, GCPServiceAccountDefault{Kind: GCPSADefaultProject})
	}
	byProfile := profileDefaultSAIDsFromAnnotations(project.Annotations)
	profiles := make([]string, 0, len(byProfile))
	for profile, id := range byProfile {
		if id == saID {
			profiles = append(profiles, profile)
		}
	}
	sort.Strings(profiles)
	for _, profile := range profiles {
		out = append(out, GCPServiceAccountDefault{Kind: GCPSADefaultProfile, Profile: profile})
	}
	if hd := s.hubAgentDefaults(); hd.DefaultGCPIdentityMode == store.GCPMetadataModeAssign &&
		hd.DefaultGCPIdentityServiceAccountID == saID {
		out = append(out, GCPServiceAccountDefault{Kind: GCPSADefaultHub})
	}
	return out
}

// gcpSANextStep derives the first missing link. Verification comes first.
// Then mapping: a profile whose project default is this account but which
// does not map it, or else, when Kubernetes profiles reported and none maps
// it, the first of them. A profile that did not report, or whose state is
// unknown, is not a known missing link, except an ambiguous mapping on a
// current, complete report (the broker refuses it, and an operator can fix
// it).
// The Workload Identity binding is not checked when gcpSANextStep is called
// directly (experiment-off behavior), so it never produces a step there.
func gcpSANextStep(verified bool, mappings []GCPServiceAccountProfileMapping, defaults []GCPServiceAccountDefault) GCPServiceAccountNextStep {
	return gcpSANextStepForBinding(verified, mappings, defaults, GCPServiceAccountBinding{
		State:  GCPSABindingUnknown,
		Reason: GCPSABindingReasonNotChecked,
	})
}

func gcpSANextStepForBinding(verified bool, mappings []GCPServiceAccountProfileMapping, defaults []GCPServiceAccountDefault, binding GCPServiceAccountBinding) GCPServiceAccountNextStep {
	if !verified {
		return GCPServiceAccountNextStep{
			Code: GCPSANextStepNotVerified,
			Message: "Not verified: the hub cannot obtain tokens for this account. " +
				"A project admin must grant the hub token-creator on it, then run verify.",
		}
	}
	defaultProfiles := map[string]bool{}
	for _, d := range defaults {
		if d.Kind == GCPSADefaultProfile {
			defaultProfiles[d.Profile] = true
		}
	}
	var firstUnmapped *GCPServiceAccountProfileMapping
	anyMapped := false
	for i := range mappings {
		m := &mappings[i]
		switch m.State {
		case GCPSAMappingMapped, GCPSAMappingCreatable:
			anyMapped = true
		case GCPSAMappingNotMapped, GCPSAMappingUnknown:
			if m.State == GCPSAMappingUnknown && !m.Ambiguous {
				continue
			}
			if defaultProfiles[m.Profile] {
				return gcpSANotMappedStep(m)
			}
			if firstUnmapped == nil {
				firstUnmapped = m
			}
		}
	}
	if !anyMapped && firstUnmapped != nil {
		return gcpSANotMappedStep(firstUnmapped)
	}
	switch binding.State {
	case GCPSABindingNotBound:
		return gcpSANotBoundStep(binding)
	case GCPSABindingBound:
		for _, t := range binding.Targets {
			if t.Stale {
				return GCPServiceAccountNextStep{
					Code: GCPSANextStepNone,
					Message: "Verification and mapping have nothing missing; stored Workload Identity targets are bound, " +
						"but one or more targets come from stale broker reports.",
				}
			}
		}
		return GCPServiceAccountNextStep{
			Code:    GCPSANextStepNone,
			Message: "Nothing missing that the hub can check.",
		}
	default:
		return GCPServiceAccountNextStep{
			Code:    GCPSANextStepNone,
			Message: gcpSAUnknownBindingNextStepMessage(binding),
		}
	}
}

func gcpSANotBoundStep(binding GCPServiceAccountBinding) GCPServiceAccountNextStep {
	var first *GCPServiceAccountBindingTarget
	notBoundCount := 0
	for i := range binding.Targets {
		if binding.Targets[i].State == GCPSABindingNotBound {
			notBoundCount++
			if first == nil {
				first = &binding.Targets[i]
			}
		}
	}
	if first == nil || first.ManualStep == "" {
		return GCPServiceAccountNextStep{
			Code: GCPSANextStepNotBound,
			Message: fmt.Sprintf("Workload Identity role %s is not granted on this account for one or more targets; "+
				"run each unbound target's manualStep gcloud command.", roleWorkloadIdentityUser),
		}
	}
	where := strings.Join(first.Profiles, ", ")
	target := first.Member
	if where != "" && target != "" {
		target = fmt.Sprintf("%s (%s)", target, where)
	} else if target == "" {
		target = where
	}
	msg := fmt.Sprintf("Workload Identity role %s is not granted on this account for %s. "+
		"Run the per-target manualStep gcloud command to grant it: %s",
		roleWorkloadIdentityUser, target, first.ManualStep)
	if notBoundCount > 1 {
		msg = fmt.Sprintf("Workload Identity role %s is not granted on this account for %d targets (first: %s). "+
			"Run each unbound target's manualStep gcloud command, starting with: %s",
			roleWorkloadIdentityUser, notBoundCount, target, first.ManualStep)
	}
	return GCPServiceAccountNextStep{
		Code:    GCPSANextStepNotBound,
		Message: msg,
	}
}

func gcpSAUnknownBindingNextStepMessage(binding GCPServiceAccountBinding) string {
	switch binding.Reason {
	case "", GCPSABindingReasonNotChecked:
		return "Nothing missing that the hub can check. The Workload Identity binding is not checked."
	case GCPSABindingReasonNotMinted:
		return "Nothing missing that the hub can check. The Workload Identity binding is not checked (not minted by this hub)."
	case GCPSABindingReasonNoKubernetesTargets:
		return "Nothing missing that the hub can check."
	case GCPSABindingReasonIAMClientUnconfigured, GCPSABindingReasonPermissionDenied, GCPSABindingReasonIAMReadFailed:
		return fmt.Sprintf("Verification and mapping have nothing missing; the Workload Identity binding could not be checked (%s).", binding.Reason)
	default:
		return fmt.Sprintf("Verification and mapping have nothing missing; the Workload Identity binding could not be checked for every Kubernetes target (%s).", binding.Reason)
	}
}

func gcpSANotMappedStep(m *GCPServiceAccountProfileMapping) GCPServiceAccountNextStep {
	msg := fmt.Sprintf("No Kubernetes service account mapping on profile %s of broker %s. "+
		"A broker operator must add it to kubernetes_service_account_mappings.", m.Profile, m.BrokerName)
	if m.Ambiguous {
		msg = fmt.Sprintf("More than one Kubernetes service account on profile %s of broker %s is annotated "+
			"with this account, so the broker refuses it. A broker operator must add an explicit entry to "+
			"kubernetes_service_account_mappings or remove the extra annotations.", m.Profile, m.BrokerName)
	}
	return GCPServiceAccountNextStep{
		Code:       GCPSANextStepNotMapped,
		BrokerName: m.BrokerName,
		Profile:    m.Profile,
		Message:    msg,
	}
}

// gcpServiceAccountAgents returns the agents of projectID whose applied GCP
// identity is saID, limited to the agents the caller may list. A failure to
// resolve the caller's scope yields an empty result: this section is
// informational and must not disclose agents on an authorization error.
func (s *Server) gcpServiceAccountAgents(ctx context.Context, projectID, saID string) GCPServiceAccountAgents {
	out := GCPServiceAccountAgents{Names: []string{}}
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return out
	}
	// The same token-scope gate listAgents applies (checkAgentReadScope): an
	// agent token without project:read sees no agents. Here the section is
	// left empty rather than failing the whole view.
	if agent := GetAgentIdentityFromContext(ctx); agent != nil && !agent.HasScope(ScopeProjectRead) {
		return out
	}
	scopeResult, err := s.authzService.ResolveListScopes(ctx, identity, "agent.list")
	if err != nil {
		logAuthzCheckFault(ctx, nil, "gcp_sa_agents_scope",
			"GCP SA status: agent scope resolution failed", err)
		return out
	}
	if scopeResult.Scopes.IsNone() {
		return out
	}
	filter := store.AgentFilter{ProjectID: projectID, GCPServiceAccountID: saID}
	if !scopeResult.Scopes.IsAll() {
		filter.AuthorizedProjectIDs = canonicalizeStringSlice(scopeResult.Scopes.ProjectIDs())
	}
	if len(scopeResult.ExcludedProjectIDs) > 0 {
		filter.ExcludedProjectIDs = canonicalizeStringSlice(append([]string{}, scopeResult.ExcludedProjectIDs...))
	}
	res, err := s.store.ListAgents(ctx, filter, store.ListOptions{Limit: gcpSAStatusAgentNameLimit})
	if err != nil {
		slog.WarnContext(ctx, "GCP SA status: listing agents failed", "error", err)
		return out
	}
	for _, a := range res.Items {
		out.Names = append(out.Names, a.Name)
	}
	sort.Strings(out.Names)
	out.Count = res.TotalCount
	if out.Count < len(out.Names) {
		out.Count = len(out.Names)
	}
	return out
}

// GCPServiceAccountMappingSummary counts, for one account, the Kubernetes
// broker profiles of a project whose shared mapping decision is mapped
// (MappedProfiles, the status view's "mapped"), that reported their
// mappings (ReportedProfiles) and that did not (UnreportedProfiles). A
// profile whose runtime needs no mapping is not counted.
type GCPServiceAccountMappingSummary struct {
	MappedProfiles     int `json:"mappedProfiles"`
	ReportedProfiles   int `json:"reportedProfiles"`
	UnreportedProfiles int `json:"unreportedProfiles"`
}

// annotateGCPSAMappings sets the mapping summary on every listed account.
func (s *Server) annotateGCPSAMappings(items []GCPServiceAccountWithCapabilities, profiles []kubernetesProfileMappings) {
	annotateGCPSAMappingsWithExperiment(items, profiles, s.experimentEnabled(experiments.K8sWorkloadIdentitySetup))
}

func annotateGCPSAMappingsWithExperiment(items []GCPServiceAccountWithCapabilities, profiles []kubernetesProfileMappings, expOn bool) {
	now := time.Now()
	for i := range items {
		allowCreate := expOn && items[i].Managed
		mapped, reported, notRequired := gcpSAMappedCountFor(items[i].Email, allowCreate, profiles, now)
		items[i].Mapping = &GCPServiceAccountMappingSummary{
			MappedProfiles:     mapped,
			ReportedProfiles:   reported,
			UnreportedProfiles: len(profiles) - reported - notRequired,
		}
	}
}
