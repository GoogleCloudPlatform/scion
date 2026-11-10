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
//
// It also lists every agent whose owner's user-scope rows not shared with
// progeny the owner material chain rule withholds at start
// (OwnerMaterialChainCensus), with the names (never values) of those rows;
// and every agent whose runtime user-scope reads are refused because its
// chain is not recorded (RuntimeReadCensus), with the names (never values)
// of the shared secrets it is refused. Row names are operator-only output.

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
// deliver permission; a legacy_* cohort when every permission is admitted and at least one only through the fallback (the
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
// served only through a cohort of the fallback (legacy_*); StrictWithheld
// counts agents the strict admission refuses (Withheld + Fallback).
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

	// OwnerMaterialChain is the owner material chain rule's part
	// (start_owner_material_chain.go): the agents whose owner's unshared
	// rows are withheld at start.
	OwnerMaterialChain OwnerMaterialChainCensus `json:"ownerMaterialChain"`

	// RuntimeRead is the runtime user-scope read's part (check 8 and the
	// agent secret list): every live agent with ancestry whose delegation
	// chain is not recorded, so its reads are refused (chain_unrecorded).
	RuntimeRead RuntimeReadCensus `json:"runtimeRead"`
}

// runtimeReadCohortRefused is the runtime read census cohort: the agent's
// runtime user-scope reads are refused (chain_unrecorded), and start
// withholds the same shared rows.
const runtimeReadCohortRefused = "runtime_read_refused"

// RuntimeReadCensusRow is one agent whose chain is not recorded. Shape is
// where the chain stops: no_edge, sentinel, unrecorded_edge,
// ambiguous_edges, inconsistent_principal (the agent's own edge, depth 0),
// or unrecorded_above at Depth with AncestorAgentID. RootUserID is the user
// whose secrets the agent reads (its ancestry root), and SharedSecrets
// names, never values, that user's secrets shared with progeny. Remedy is
// the owner action that records the chain.
type RuntimeReadCensusRow struct {
	AgentID         string   `json:"agentId"`
	ProjectID       string   `json:"projectId"`
	OwnerID         string   `json:"ownerId"`
	Cohort          string   `json:"cohort"`
	Shape           string   `json:"shape"`
	Depth           int      `json:"depth"`
	AncestorAgentID string   `json:"ancestorAgentId,omitempty"`
	RootUserID      string   `json:"rootUserId"`
	SharedSecrets   []string `json:"sharedSecrets"`
	SecretsError    string   `json:"secretsError,omitempty"`
	Remedy          string   `json:"remedy"`
}

// RuntimeReadCensus counts the listed agents by cohort and by shape.
type RuntimeReadCensus struct {
	ByCohort map[string]int         `json:"byCohort"`
	ByShape  map[string]int         `json:"byShape"`
	Rows     []RuntimeReadCensusRow `json:"rows"`
}

// OwnerMaterialChainCensusRow is one live agent whose owner's user-scope
// rows not shared with progeny the rule withholds. Reason is the withhold
// reason: terminal_user_not_owner (the recorded chain ends at another
// user), provenance_root_not_owner (the ancestry starts elsewhere, as for
// an agent-created sub-agent), unrecorded_hop_above_agent, chain_invalid,
// chain_lookup_error, or the cohort of a chain that is not recorded
// (legacy_no_edge, legacy_sentinel_edge, legacy_unadopted_edge,
// legacy_ambiguous_edges, legacy_inconsistent_principal). Depth is the
// length of the agent's stored ancestry. Secrets and EnvVars name the
// owner's rows not shared with progeny (names only, never values); an
// agent with none is listed with empty lists.
type OwnerMaterialChainCensusRow struct {
	AgentID   string   `json:"agentId"`
	ProjectID string   `json:"projectId"`
	OwnerID   string   `json:"ownerId"`
	Reason    string   `json:"reason"`
	Depth     int      `json:"depth"`
	Secrets   []string `json:"secrets"`
	EnvVars   []string `json:"envVars"`
	RowsError string   `json:"rowsError,omitempty"`
	// SharedSecrets are the owner's secrets shared with progeny that start
	// also withholds from the agent (erratum 2b section 3a), by name.
	SharedSecrets []string `json:"sharedSecrets"`
}

// OwnerMaterialChainCensus counts the listed agents and groups them by
// reason. Affected counts the listed agents with at least one row. Remedy
// is the owner action that restores an agent's delivery.
type OwnerMaterialChainCensus struct {
	Withheld int                           `json:"withheld"`
	Affected int                           `json:"affected"`
	ByReason map[string]int                `json:"byReason"`
	Remedy   string                        `json:"remedy"`
	Rows     []OwnerMaterialChainCensusRow `json:"rows"`
}

// ownerMaterialChainRemedy is the census remedy line.
const ownerMaterialChainRemedy = "the owner reincarnates the agent from a CLI session (scion reincarnate <agent>, keeping its role) or re-creates it"

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
	rep.OwnerMaterialChain = OwnerMaterialChainCensus{ByReason: map[string]int{}, Remedy: ownerMaterialChainRemedy}
	ownerRows := map[string]ownerUnsharedRowNames{}
	rep.RuntimeRead = RuntimeReadCensus{ByCohort: map[string]int{}, ByShape: map[string]int{}}
	sharedRows := map[string]rootSharedSecretNames{}
	cursor := ""
	for {
		page, err := a.store.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: 200, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return nil, fmt.Errorf("census: list agents: %w", err)
		}
		for i := range page.Items {
			row := a.launchDeliveryCensusRow(ctx, &page.Items[i])
			rep.Rows = append(rep.Rows, row)
			if oc, ok := a.ownerMaterialChainCensusRow(ctx, &page.Items[i], ownerRows); ok {
				omc := &rep.OwnerMaterialChain
				omc.Rows = append(omc.Rows, oc)
				omc.ByReason[oc.Reason]++
				omc.Withheld++
				if len(oc.Secrets)+len(oc.EnvVars) > 0 {
					omc.Affected++
				}
			}
			if rr, ok := a.runtimeReadCensusRow(ctx, &page.Items[i], sharedRows); ok {
				rep.RuntimeRead.Rows = append(rep.RuntimeRead.Rows, rr)
				rep.RuntimeRead.ByCohort[rr.Cohort]++
				rep.RuntimeRead.ByShape[rr.Shape]++
			}
			rep.Agents++
			rep.ByCohort[row.Cohort]++
			switch {
			case row.Cohort == "delivers":
			case strings.HasPrefix(row.Cohort, "legacy_"):
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
	omcRows := rep.OwnerMaterialChain.Rows
	sort.Slice(omcRows, func(i, j int) bool { return omcRows[i].AgentID < omcRows[j].AgentID })
	rrRows := rep.RuntimeRead.Rows
	sort.Slice(rrRows, func(i, j int) bool { return rrRows[i].AgentID < rrRows[j].AgentID })
	return rep, nil
}

// addUATFacts adds one row's intrinsic-set facts to the report counts.
func (rep *LaunchDeliveryCensusReport) addUATFacts(row LaunchDeliveryCensusRow) {
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
// recorded from a user access token whose launch material is withheld because its root user is not admitted to the
// agent's project, as for a session-created agent.
const launchDeliveryCohortUATRootNotAdmitted = "uat_root_not_admitted"

// ownerUnsharedRowNames are an owner's user-scope rows not shared with
// progeny, by name, the owner's secrets shared with progeny (sharedSecrets),
// or the error listing them.
type ownerUnsharedRowNames struct {
	secrets, envVars []string
	sharedSecrets    []string
	err              error
}

// ownerMaterialChainCensusRow applies the rule's row decision, as a start
// does, to agent's owner rows. It returns the row and true when the rule
// withholds them. rows caches the owner row names by owner.
func (a *AuthzService) ownerMaterialChainCensusRow(ctx context.Context, ag *store.Agent, rows map[string]ownerUnsharedRowNames) (OwnerMaterialChainCensusRow, bool) {
	check := newOwnerChainCheck(a, ag)
	if check == nil || !ag.DeletedAt.IsZero() {
		return OwnerMaterialChainCensusRow{}, false
	}
	o := check.decide(ctx, ag.OwnerID)
	if o.deliver {
		return OwnerMaterialChainCensusRow{}, false
	}
	row := OwnerMaterialChainCensusRow{AgentID: ag.ID, ProjectID: ag.ProjectID, OwnerID: ag.OwnerID,
		Reason: o.reason, Depth: len(ag.Ancestry), Secrets: []string{}, EnvVars: []string{}, SharedSecrets: []string{}}
	names, ok := rows[ag.OwnerID]
	if !ok {
		names = a.ownerUnsharedRowNames(ctx, ag.OwnerID)
		rows[ag.OwnerID] = names
	}
	if names.err != nil {
		row.RowsError = names.err.Error()
	} else {
		row.Secrets = append(row.Secrets, names.secrets...)
		row.EnvVars = append(row.EnvVars, names.envVars...)
		// The owner's shared secrets are listed when start withholds them
		// too, decided as start and the read-only views decide them
		// (ownerChainCheck.allowsSharedSecret).
		if !check.allowsSharedSecret(ctx, ag.OwnerID) {
			row.SharedSecrets = append(row.SharedSecrets, names.sharedSecrets...)
		}
	}
	return row, true
}

// ownerUnsharedRowNames lists ownerID's user-scope secrets and env vars not
// shared with progeny, by name. Secret metadata is listed without values;
// the store has no key-only env var list, so each env var row's value is
// cleared as soon as the row is read and only its key is kept.
func (a *AuthzService) ownerUnsharedRowNames(ctx context.Context, ownerID string) ownerUnsharedRowNames {
	var out ownerUnsharedRowNames
	secrets, err := a.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeUser, ScopeID: ownerID})
	if err != nil {
		out.err = fmt.Errorf("list secrets: %w", err)
		return out
	}
	for _, sec := range secrets {
		// Hub-internal secrets are skipped, as start and the read skip
		// them (rootSharedSecretNames).
		if sec.SecretType == store.SecretTypeInternal {
			continue
		}
		if sec.AllowProgeny {
			out.sharedSecrets = append(out.sharedSecrets, sec.Key)
		} else {
			out.secrets = append(out.secrets, sec.Key)
		}
	}
	envVars, err := a.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeUser, ScopeID: ownerID})
	if err != nil {
		out.err = fmt.Errorf("list env vars: %w", err)
		return out
	}
	// The store has no key-only list: drop each value as soon as the row
	// is read, so none is held past this loop.
	for i := range envVars {
		envVars[i].Value = ""
		if !envVars[i].AllowProgeny {
			out.envVars = append(out.envVars, envVars[i].Key)
		}
	}
	sort.Strings(out.secrets)
	sort.Strings(out.envVars)
	sort.Strings(out.sharedSecrets)
	return out
}

// rootSharedSecretNames are a user's user-scope secrets shared with
// progeny, by name, or the error listing them.
type rootSharedSecretNames struct {
	names []string
	err   error
}

// runtimeReadCensusRow computes ag's chain verdict as the runtime read does
// (userSecretChainVerdict for the stored agent) and, when the chain is not
// recorded, returns its row. An agent without ancestry is skipped: the
// runtime read refuses it before the chain (target_unresolved). shared
// caches the root user's shared secret names by user.
func (a *AuthzService) runtimeReadCensusRow(ctx context.Context, ag *store.Agent, shared map[string]rootSharedSecretNames) (RuntimeReadCensusRow, bool) {
	if ag == nil || !ag.DeletedAt.IsZero() || len(ag.Ancestry) == 0 {
		return RuntimeReadCensusRow{}, false
	}
	chain := a.userSecretChainVerdict(ctx, &storedAgentIdentity{agent: ag}, nil)
	if chain.decision.Allowed || !chain.unrecorded {
		return RuntimeReadCensusRow{}, false
	}
	row := RuntimeReadCensusRow{AgentID: ag.ID, ProjectID: ag.ProjectID, OwnerID: ag.OwnerID,
		Cohort: runtimeReadCohortRefused, Shape: chain.shape, Depth: chain.shapeDepth,
		RootUserID: ag.Ancestry[0], SharedSecrets: []string{}, Remedy: ownerMaterialChainRemedy}
	if chain.shapeDepth > 0 {
		row.AncestorAgentID = chain.shapeAgentID
	}
	names, ok := shared[row.RootUserID]
	if !ok {
		names = a.rootSharedSecretNames(ctx, row.RootUserID)
		shared[row.RootUserID] = names
	}
	if names.err != nil {
		row.SecretsError = names.err.Error()
	} else {
		row.SharedSecrets = append(row.SharedSecrets, names.names...)
	}
	return row, true
}

// rootSharedSecretNames lists userID's user-scope secrets shared with
// progeny, by name: the rows the runtime read would return. Secret metadata
// is listed without values; hub-internal secrets are skipped, as the read
// skips them.
func (a *AuthzService) rootSharedSecretNames(ctx context.Context, userID string) rootSharedSecretNames {
	var out rootSharedSecretNames
	secrets, err := a.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeUser, ScopeID: userID})
	if err != nil {
		out.err = fmt.Errorf("list secrets: %w", err)
		return out
	}
	for _, sec := range secrets {
		if sec.AllowProgeny && sec.SecretType != store.SecretTypeInternal {
			out.names = append(out.names, sec.Key)
		}
	}
	sort.Strings(out.names)
	return out
}

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
	if walkPassed && f.UATRecorded && strings.HasPrefix(o.reason, deliveryRootNotAdmittedReason) {
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
	// These facts count the parent ceiling's admissions of the owner's rows
	// shared with progeny (startSecretResolveOpts), which hold a row to a
	// stored ancestry. An agent with no stored ancestry has its shared rows
	// decided on the owner material chain verdict instead (listed by the
	// owner material chain census), so it is not counted here.
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
