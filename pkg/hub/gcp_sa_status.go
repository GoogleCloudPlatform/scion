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

// Mapping states for one Kubernetes broker profile.
const (
	GCPSAMappingMapped      = "mapped"
	GCPSAMappingNotMapped   = "not_mapped"
	GCPSAMappingNotReported = "not_reported"
)

// Workload Identity binding states. Only "unknown" is produced today: the hub
// does not read IAM policy for this view, and the binding is never reported
// as bound without evidence.
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

// Next-step codes: the first missing link.
const (
	GCPSANextStepNotVerified = "not_verified"
	GCPSANextStepNotMapped   = "not_mapped"
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
type GCPServiceAccountProfileMapping struct {
	BrokerID   string `json:"brokerId"`
	BrokerName string `json:"brokerName"`
	Profile    string `json:"profile"`
	State      string `json:"state"`
	// KubernetesServiceAccount and Namespace are reserved for when brokers
	// report them (ptone/scion#3329 phase 4); always empty today.
	KubernetesServiceAccount string `json:"kubernetesServiceAccount,omitempty"`
	Namespace                string `json:"namespace,omitempty"`
}

// GCPServiceAccountBinding is the Workload Identity binding state.
type GCPServiceAccountBinding struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
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

	status := s.buildGCPServiceAccountStatus(ctx, project, sa,
		s.projectKubernetesProfileMappings(ctx, projectID))
	status.Agents = s.gcpServiceAccountAgents(ctx, projectID, sa.ID)
	writeJSON(w, http.StatusOK, status)
}

// buildGCPServiceAccountStatus assembles every section except agents, which
// needs the caller's identity.
func (s *Server) buildGCPServiceAccountStatus(ctx context.Context, project *store.Project, sa *store.GCPServiceAccount, profiles []kubernetesProfileMappings) GCPServiceAccountStatus {
	st := GCPServiceAccountStatus{
		Account: GCPServiceAccountStatusIdentity{
			ID: sa.ID, DisplayName: sa.DisplayName, Scope: sa.Scope, Email: sa.Email,
		},
		Verification: gcpSAVerificationOf(sa),
		Mappings:     gcpSAProfileMappings(sa.Email, profiles),
		WorkloadIdentityBinding: GCPServiceAccountBinding{
			State:  GCPSABindingUnknown,
			Reason: "not checked",
		},
		DefaultFor: s.gcpSADefaultsFor(project, sa.ID),
		Agents:     GCPServiceAccountAgents{Names: []string{}},
	}
	st.NextStep = gcpSANextStep(st.Verification.Verified, st.Mappings, st.DefaultFor)
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

// gcpSAProfileMappings maps each Kubernetes profile to a state for email,
// sorted by broker then profile name.
func gcpSAProfileMappings(email string, profiles []kubernetesProfileMappings) []GCPServiceAccountProfileMapping {
	out := make([]GCPServiceAccountProfileMapping, 0, len(profiles))
	key := strings.ToLower(email)
	for _, p := range profiles {
		state := GCPSAMappingNotReported
		if p.reported {
			state = GCPSAMappingNotMapped
			if p.gsas[key] {
				state = GCPSAMappingMapped
			}
		}
		out = append(out, GCPServiceAccountProfileMapping{
			BrokerID: p.brokerID, BrokerName: p.brokerName, Profile: p.profile, State: state,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].BrokerName != out[j].BrokerName {
			return out[i].BrokerName < out[j].BrokerName
		}
		return out[i].Profile < out[j].Profile
	})
	return out
}

// gcpSAMappedCount returns how many profiles map email and how many reported.
func gcpSAMappedCount(email string, profiles []kubernetesProfileMappings) (mapped, reported int) {
	key := strings.ToLower(email)
	for _, p := range profiles {
		if !p.reported {
			continue
		}
		reported++
		if p.gsas[key] {
			mapped++
		}
	}
	return mapped, reported
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
// it, the first of them. A profile that did not report is not a known
// missing link. The Workload Identity binding is not checked, so it never
// produces a step.
func gcpSANextStep(verified bool, mappings []GCPServiceAccountProfileMapping, defaults []GCPServiceAccountDefault) GCPServiceAccountNextStep {
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
		case GCPSAMappingMapped:
			anyMapped = true
		case GCPSAMappingNotMapped:
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
	return GCPServiceAccountNextStep{
		Code:    GCPSANextStepNone,
		Message: "Nothing missing that the hub can check. The Workload Identity binding is not checked.",
	}
}

func gcpSANotMappedStep(m *GCPServiceAccountProfileMapping) GCPServiceAccountNextStep {
	return GCPServiceAccountNextStep{
		Code:       GCPSANextStepNotMapped,
		BrokerName: m.BrokerName,
		Profile:    m.Profile,
		Message: fmt.Sprintf("No Kubernetes service account mapping on profile %s of broker %s. "+
			"A broker operator must add it to kubernetes_service_account_mappings.", m.Profile, m.BrokerName),
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
		slog.WarnContext(ctx, "GCP SA status: agent scope resolution failed", "error", err)
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
// broker profiles of a project that map it (MappedProfiles), that reported
// their mappings (ReportedProfiles) and that did not (UnreportedProfiles).
type GCPServiceAccountMappingSummary struct {
	MappedProfiles     int `json:"mappedProfiles"`
	ReportedProfiles   int `json:"reportedProfiles"`
	UnreportedProfiles int `json:"unreportedProfiles"`
}

// annotateGCPSAMappings sets the mapping summary on every listed account.
func annotateGCPSAMappings(items []GCPServiceAccountWithCapabilities, profiles []kubernetesProfileMappings) {
	for i := range items {
		mapped, reported := gcpSAMappedCount(items[i].Email, profiles)
		items[i].Mapping = &GCPServiceAccountMappingSummary{
			MappedProfiles:     mapped,
			ReportedProfiles:   reported,
			UnreportedProfiles: len(profiles) - reported,
		}
	}
}
