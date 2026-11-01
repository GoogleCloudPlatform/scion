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

// Launch delivery census: a READ-ONLY report of which live agents the launch
// material delivery admission (material_delivery.go, hubDeliveryChainAdmission)
// would refuse at start or restart, which of those the legacy fallback
// (material_delivery_legacy.go) still serves, and why. It runs the same
// admission and fallback a start runs, for each deliver permission, without
// reading any secret or env value,
// and writes nothing: it only lists agents and reads the records the admission
// reads (agents, delegation edges, holds, users, role bindings). Callers should
// still open the store read-only (hack/delivery-census does).
//
// For agents whose own edge was recorded from a user access token
// (uatRecordedHop) it also reports the intrinsic set's effect: the stored
// role against the role the chain lets its tokens carry, and how many of the
// owner's progeny-shared user secrets the user secret parent ceiling admits,
// and how many of those only through the intrinsic rule. It counts rows by
// ID and never decrypts or prints a value or a row name.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// LaunchDeliveryCensusPermissions are the deliver permissions launch selection
// asks for (material_selection.go deliverPermissionForKind).
var LaunchDeliveryCensusPermissions = []string{
	"secret.deliver",
	"env_var.deliver",
	"skill_injection.deliver",
}

// LaunchDeliveryVerdict is the admission outcome a start or restart gets for
// one deliver permission, legacy fallback (material_delivery_legacy.go)
// included. StrictAllowed and StrictCause are the outcome without the
// fallback; LegacyCohort is set when the fallback admits it.
type LaunchDeliveryVerdict struct {
	Allowed       bool      `json:"allowed"`
	Cause         DenyCause `json:"cause,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	StrictAllowed bool      `json:"strictAllowed"`
	StrictCause   DenyCause `json:"strictCause,omitempty"`
	LegacyCohort  string    `json:"legacyCohort,omitempty"`
}

// LaunchDeliveryEdgeFacts are the facts of the agent's own active edges in its
// project (depth 0), used to name the cohort.
type LaunchDeliveryEdgeFacts struct {
	ActiveEdges          int    `json:"activeEdges"`
	DelegatorType        string `json:"delegatorType,omitempty"`
	DelegatorID          string `json:"delegatorId,omitempty"`
	Role                 string `json:"role,omitempty"`
	ProvenanceVersion    int    `json:"provenanceVersion"`
	SourceCredentialKind string `json:"sourceCredentialKind,omitempty"`
	CeilingKind          string `json:"ceilingKind,omitempty"`
	Sentinel             bool   `json:"sentinel"`
	DelegatorAgentGone   bool   `json:"delegatorAgentGone"`
	// UATRecorded: the edge was recorded from a user access token
	// (uatRecordedHop), so the intrinsic set applies to it.
	UATRecorded bool `json:"uatRecorded"`
	// InitiatorCredentialKind is the edge's initiator credential kind. A
	// scheduler-recorded edge with initiator "uat" carries a token's list
	// but is not a uatRecordedHop: scheduler-recorded hops are not token
	// hops. Counted, and unchanged.
	InitiatorCredentialKind string `json:"initiatorCredentialKind,omitempty"`
}

// LaunchDeliveryUATFacts are the intrinsic-set facts of an agent whose own
// edge is UATRecorded.
type LaunchDeliveryUATFacts struct {
	// StoredRole is the agent's stored role; FilteredRole is the highest of
	// full, baseline and readonly, no higher than StoredRole, whose scopes
	// the agent's chain ceiling allows ("none" when none fits, "" when the
	// chain could not be evaluated).
	StoredRole   string `json:"storedRole"`
	FilteredRole string `json:"filteredRole"`
	// OwnerSharedRows counts the owner's user secrets shared with progeny;
	// OwnerSharedAdmitted those the user secret parent ceiling admits (the
	// start-time check); OwnerSharedIntrinsic those of the admitted whose
	// S3c passed only through the intrinsic rule.
	OwnerSharedRows      int `json:"ownerSharedRows"`
	OwnerSharedAdmitted  int `json:"ownerSharedAdmitted"`
	OwnerSharedIntrinsic int `json:"ownerSharedIntrinsic"`
	// OwnerSharedError is set when the owner's rows could not be listed.
	OwnerSharedError string `json:"ownerSharedError,omitempty"`
}

// LaunchDeliveryCensusRow is one live agent's census result. Cohort is what
// a start delivers: "delivers" when the strict admission admits every
// deliver permission; a legacy_* cohort or uat_descendant_legacy when every
// permission is admitted and at least one only through the fallback (the
// first such cohort); otherwise the withheld cohort of the first refused
// permission. StrictCohort is the same classification without the fallback.
// UAT is set when the agent's own edge is UATRecorded.
type LaunchDeliveryCensusRow struct {
	AgentID      string                           `json:"agentId"`
	ProjectID    string                           `json:"projectId"`
	OwnerID      string                           `json:"ownerId"`
	Phase        string                           `json:"phase"`
	Cohort       string                           `json:"cohort"`
	StrictCohort string                           `json:"strictCohort"`
	MintError    string                           `json:"mintError,omitempty"`
	Edge         LaunchDeliveryEdgeFacts          `json:"edge"`
	Verdicts     map[string]LaunchDeliveryVerdict `json:"verdicts"`
	UAT          *LaunchDeliveryUATFacts          `json:"uat,omitempty"`
}

// LaunchDeliveryCensusReport is the whole census. Withheld counts agents a
// start refuses at least one deliver permission; Fallback counts agents
// served only through a cohort of the fallback (legacy_* and
// uat_descendant_legacy); StrictWithheld counts agents the strict admission
// refuses (Withheld + Fallback). UATDescendantLegacy counts the
// uat_descendant_legacy agents, which the fallback retires once it reads
// zero.
//
// The UAT* counts cover agents whose own edge is UATRecorded: UATCreated
// such agents; UATRoleFiltered those whose FilteredRole is below their
// StoredRole; UATOwnerSharedRows, UATOwnerSharedAdmitted and
// UATOwnerSharedIntrinsic the sums of their owner-row facts;
// UATRootNotAdmitted those withheld launch material because their root user
// is not admitted to the project (cohort uat_root_not_admitted).
// SchedulerUATInitiated counts scheduler-recorded own edges whose initiator
// credential was a user access token (scheduler-recorded hops are not token
// hops, so the intrinsic set does not change them).
type LaunchDeliveryCensusReport struct {
	Agents                  int `json:"agents"`
	Withheld                int `json:"withheld"`
	Fallback                int `json:"fallback"`
	StrictWithheld          int `json:"strictWithheld"`
	UATDescendantLegacy     int `json:"uatDescendantLegacy"`
	UATCreated              int `json:"uatCreated"`
	UATRoleFiltered         int `json:"uatRoleFiltered"`
	UATOwnerSharedRows      int `json:"uatOwnerSharedRows"`
	UATOwnerSharedAdmitted  int `json:"uatOwnerSharedAdmitted"`
	UATOwnerSharedIntrinsic int `json:"uatOwnerSharedIntrinsic"`
	UATRootNotAdmitted      int `json:"uatRootNotAdmitted"`
	SchedulerUATInitiated   int `json:"schedulerUatInitiated"`

	ByCohort      map[string]int            `json:"byCohort"`
	ByCause       map[string]int            `json:"byCause"`
	ByStrictCause map[string]int            `json:"byStrictCause"`
	Rows          []LaunchDeliveryCensusRow `json:"rows"`
}

// LaunchDeliveryCensus runs the launch delivery admission for every live agent
// (deleted agents excluded) and classifies each refusal. devLocal must match
// the hub's dev-auth setting (setDevLocalAuthorityEnabled), because the walk
// refuses local-development provenance without it. It writes nothing.
func (a *AuthzService) LaunchDeliveryCensus(ctx context.Context, devLocal bool) (*LaunchDeliveryCensusReport, error) {
	if a == nil || a.store == nil {
		return nil, fmt.Errorf("census: no authorization service or store")
	}
	a.setDevLocalAuthorityEnabled(devLocal)
	rep := &LaunchDeliveryCensusReport{ByCohort: map[string]int{}, ByCause: map[string]int{}, ByStrictCause: map[string]int{}}
	cursor := ""
	for {
		page, err := a.store.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: 200, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return nil, fmt.Errorf("census: list agents: %w", err)
		}
		for i := range page.Items {
			row := a.launchDeliveryCensusRow(ctx, &page.Items[i])
			rep.Rows = append(rep.Rows, row)
			rep.Agents++
			rep.ByCohort[row.Cohort]++
			switch {
			case row.Cohort == "delivers":
			case strings.HasPrefix(row.Cohort, "legacy_"), row.Cohort == legacyDeliveryCohortUATDescendant:
				rep.Fallback++
				rep.StrictWithheld++
			default:
				rep.Withheld++
				rep.StrictWithheld++
			}
			rep.addUATFacts(row)
			for _, p := range LaunchDeliveryCensusPermissions {
				v, ok := row.Verdicts[p]
				if !ok {
					continue
				}
				if !v.Allowed {
					rep.ByCause[p+":"+string(v.Cause)]++
				}
				if !v.StrictAllowed {
					rep.ByStrictCause[p+":"+string(v.StrictCause)]++
				}
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	sort.Slice(rep.Rows, func(i, j int) bool { return rep.Rows[i].AgentID < rep.Rows[j].AgentID })
	return rep, nil
}

// addUATFacts adds one row's intrinsic-set facts to the report counts.
func (rep *LaunchDeliveryCensusReport) addUATFacts(row LaunchDeliveryCensusRow) {
	if row.Cohort == legacyDeliveryCohortUATDescendant {
		rep.UATDescendantLegacy++
	}
	if row.Cohort == launchDeliveryCohortUATRootNotAdmitted {
		rep.UATRootNotAdmitted++
	}
	if row.Edge.SourceCredentialKind == string(store.SourceCredentialScheduler) && row.Edge.InitiatorCredentialKind == store.InitiatorCredentialKindUAT {
		rep.SchedulerUATInitiated++
	}
	if row.UAT == nil {
		return
	}
	rep.UATCreated++
	if row.UAT.FilteredRole != "" && CompareRoles(AgentRole(row.UAT.FilteredRole), AgentRole(row.UAT.StoredRole)) < 0 {
		rep.UATRoleFiltered++
	}
	rep.UATOwnerSharedRows += row.UAT.OwnerSharedRows
	rep.UATOwnerSharedAdmitted += row.UAT.OwnerSharedAdmitted
	rep.UATOwnerSharedIntrinsic += row.UAT.OwnerSharedIntrinsic
}

// launchDeliveryCohortUATRootNotAdmitted names an agent whose own edge was
// recorded from a user access token, or a uat_descendant_legacy agent, whose
// launch material is withheld because its root user is not admitted to the
// agent's project, as for a session-created agent.
const launchDeliveryCohortUATRootNotAdmitted = "uat_root_not_admitted"

func (a *AuthzService) launchDeliveryCensusRow(ctx context.Context, ag *store.Agent) LaunchDeliveryCensusRow {
	row := LaunchDeliveryCensusRow{AgentID: ag.ID, ProjectID: ag.ProjectID, OwnerID: ag.OwnerID, Phase: ag.Phase,
		Verdicts: map[string]LaunchDeliveryVerdict{}}
	row.Edge = a.launchDeliveryEdgeFacts(ctx, ag)
	if row.Edge.UATRecorded {
		row.UAT = a.launchDeliveryUATFacts(ctx, ag)
	}
	h, err := a.newLaunchDeliveryIdentity(ctx, ag.ID, true)
	if err != nil {
		// A start of this agent fails hard (the credential cannot be minted).
		row.MintError = err.Error()
		row.Cohort, row.StrictCohort = "mint_error", "mint_error"
		return row
	}
	var strictDeny, deny *hubDeliveryAdmissionOutcome
	legacy := ""
	for _, p := range LaunchDeliveryCensusPermissions {
		strict := a.hubDeliveryChainAdmission(ctx, h, p, ag.ID, nil)
		eff := strict
		if !strict.ok {
			eff = a.hubDeliveryLegacyFallback(ctx, h, p, ag.ID, strict, nil)
		}
		row.Verdicts[p] = LaunchDeliveryVerdict{
			Allowed: eff.ok, Cause: eff.cause, Reason: eff.reason,
			StrictAllowed: strict.ok, StrictCause: strict.cause, LegacyCohort: eff.legacyCohort,
		}
		if !strict.ok && strictDeny == nil {
			strictDeny = &strict
		}
		if !eff.ok && deny == nil {
			deny = &eff
		}
		if eff.legacyCohort != "" && legacy == "" {
			legacy = eff.legacyCohort
		}
	}
	row.StrictCohort = "delivers"
	if strictDeny != nil {
		row.StrictCohort = launchDeliveryOutcomeCohort(row.Edge, *strictDeny, ag.ID)
	}
	switch {
	case deny != nil:
		row.Cohort = launchDeliveryOutcomeCohort(row.Edge, *deny, ag.ID)
	case legacy != "":
		row.Cohort = legacy
	default:
		row.Cohort = "delivers"
	}
	return row
}

// launchDeliveryOutcomeCohort names the cohort of a refused admission.
func launchDeliveryOutcomeCohort(f LaunchDeliveryEdgeFacts, o hubDeliveryAdmissionOutcome, agentID string) string {
	// hubDeliveryChainAdmission prefixes a provenance-root refusal (the walk
	// passed) with this text.
	walkPassed := strings.HasPrefix(o.reason, "delivery provenance root denied")
	tokenChain := f.UATRecorded || o.deniedCohort == legacyDeliveryCohortUATDescendant
	if walkPassed && tokenChain && strings.HasPrefix(o.reason, deliveryRootNotAdmittedReason) {
		return launchDeliveryCohortUATRootNotAdmitted
	}
	targetHeld := o.reason == "agent "+agentID+" is held"
	return launchDeliveryCohort(f, o.cause, walkPassed, targetHeld)
}

// launchDeliveryUATFacts computes the intrinsic-set facts of agent ag, whose
// own edge is UATRecorded. It reads what start reads and writes nothing.
func (a *AuthzService) launchDeliveryUATFacts(ctx context.Context, ag *store.Agent) *LaunchDeliveryUATFacts {
	stored, additional := agentRoleAndScopes(ag)
	facts := &LaunchDeliveryUATFacts{StoredRole: string(stored)}
	if chain, err := a.chainEffectCeiling(ctx, ag); err == nil {
		facts.FilteredRole = string(AgentRoleNone)
		for _, candidate := range []AgentRole{AgentRoleFull, AgentRoleBaseline, AgentRoleReadOnly} {
			if roleFitsCeiling(chain.Ceiling, candidate) {
				facts.FilteredRole = string(minRole(stored, candidate))
				break
			}
		}
	}
	// The owner path of start (startSecretResolveOpts) is installed only for
	// an agent with an owner and a stored ancestry, and decides only rows
	// shared with progeny.
	if ag.OwnerID == "" || len(ag.Ancestry) == 0 {
		return facts
	}
	rows, err := a.store.ListProgenySecrets(ctx, ag.Ancestry)
	if err != nil {
		facts.OwnerSharedError = "owner secret lookup failed"
		return facts
	}
	ident := &agentIdentityWrapper{AgentTokenClaims: &AgentTokenClaims{
		Claims:    jwt.Claims{Subject: ag.ID},
		ProjectID: ag.ProjectID,
		Ancestry:  ag.Ancestry,
		Scopes:    append(ScopesForRole(stored), additional...),
	}}
	for i := range rows {
		r := &rows[i]
		if r.Scope != store.ScopeUser || r.ScopeID != ag.OwnerID || !r.AllowProgeny {
			continue
		}
		facts.OwnerSharedRows++
		d := a.EvaluateUserSecretParentCeiling(ctx, ident, userSecretResource(r.ID, r.ScopeID))
		if !d.Allowed {
			continue
		}
		facts.OwnerSharedAdmitted++
		if d.uatIntrinsic {
			facts.OwnerSharedIntrinsic++
		}
	}
	return facts
}

// launchDeliveryEdgeFacts reads the agent's own active edges in its project.
func (a *AuthzService) launchDeliveryEdgeFacts(ctx context.Context, ag *store.Agent) LaunchDeliveryEdgeFacts {
	var f LaunchDeliveryEdgeFacts
	active, err := a.activeProjectEdges(ctx, ag.ID, ag.ProjectID)
	if err != nil {
		return f
	}
	f.ActiveEdges = len(active)
	if len(active) != 1 {
		return f
	}
	e := active[0]
	f.DelegatorType, f.DelegatorID, f.Role = e.DelegatorType, e.DelegatorID, e.Role
	f.ProvenanceVersion = e.ProvenanceVersion
	f.SourceCredentialKind = string(e.SourceCredentialKind)
	f.CeilingKind = string(e.Kind)
	f.Sentinel = isMigrationSentinel(e)
	f.UATRecorded = uatRecordedHop(e.DelegatorType, e.DelegatorID, e.AuthorityProvenance, e.EffectCeiling)
	f.InitiatorCredentialKind = e.InitiatorCredentialKind
	if e.DelegatorType == store.DelegationPrincipalAgent {
		parent, perr := a.store.GetAgent(ctx, e.DelegatorID)
		if perr != nil || parent == nil || !parent.DeletedAt.IsZero() {
			f.DelegatorAgentGone = true
		} else if held, herr := a.store.HasActiveAgentHold(ctx, parent.ID); herr == nil && held {
			f.DelegatorAgentGone = true
		}
	}
	return f
}

// launchDeliveryCohort names the legacy cohort (legacy-audit-v1 section C) from
// the first refused permission's cause and the agent's own edge facts. The
// order follows the walk's depth-0 checks (hubDeliveryChainWalk), so a cohort
// names the first check the agent's own edge fails.
func launchDeliveryCohort(f LaunchDeliveryEdgeFacts, cause DenyCause, walkPassed, targetHeld bool) string {
	switch {
	case cause == DenyCauseCeilingError:
		return "lookup_error"
	case walkPassed:
		// The walk admitted every hop; the provenance root was refused.
		return "root_" + string(cause)
	case targetHeld:
		return "target_held"
	case f.ActiveEdges == 0:
		return "no_edge"
	case f.ActiveEdges > 1:
		return "duplicate_edges"
	case f.Sentinel:
		return "sentinel_edge"
	case cause == DenyCauseCeilingSourceNotAllowed && f.SourceCredentialKind == string(store.SourceCredentialDevLocal):
		return "dev_local_edge"
	case !knownProvenanceVersion(f.ProvenanceVersion):
		return "unadopted_edge"
	case cause == DenyCauseCeilingEffectExceeded && f.SourceCredentialKind == string(store.SourceCredentialSystemMigration):
		role := f.Role
		if role == "" {
			role = "none"
		}
		return "adopted_" + role
	case f.DelegatorAgentGone:
		return "deleted_or_held_parent"
	default:
		// The agent's own edge passes the depth-0 checks above; the refusal is
		// at an ancestor hop (or a cause the facts above do not name).
		return "ancestor_" + string(cause)
	}
}
