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
// would refuse at start or restart, and why. It runs the same admission a start
// runs, for each deliver permission, without reading any secret or env value,
// and writes nothing: it only lists agents and reads the records the admission
// reads (agents, delegation edges, holds, users, role bindings). Callers should
// still open the store read-only (hack/fland-delivery-census does).

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// LaunchDeliveryCensusPermissions are the deliver permissions launch selection
// asks for (material_selection.go deliverPermissionForKind).
var LaunchDeliveryCensusPermissions = []string{
	"secret.deliver",
	"env_var.deliver",
	"skill_injection.deliver",
}

// LaunchDeliveryVerdict is the admission outcome for one deliver permission.
type LaunchDeliveryVerdict struct {
	Allowed bool      `json:"allowed"`
	Cause   DenyCause `json:"cause,omitempty"`
	Reason  string    `json:"reason,omitempty"`
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
}

// LaunchDeliveryCensusRow is one live agent's census result. Cohort is
// "delivers" when every deliver permission is admitted.
type LaunchDeliveryCensusRow struct {
	AgentID   string                           `json:"agentId"`
	ProjectID string                           `json:"projectId"`
	OwnerID   string                           `json:"ownerId"`
	Phase     string                           `json:"phase"`
	Cohort    string                           `json:"cohort"`
	MintError string                           `json:"mintError,omitempty"`
	Edge      LaunchDeliveryEdgeFacts          `json:"edge"`
	Verdicts  map[string]LaunchDeliveryVerdict `json:"verdicts"`
}

// LaunchDeliveryCensusReport is the whole census.
type LaunchDeliveryCensusReport struct {
	Agents   int                       `json:"agents"`
	Withheld int                       `json:"withheld"`
	ByCohort map[string]int            `json:"byCohort"`
	ByCause  map[string]int            `json:"byCause"`
	Rows     []LaunchDeliveryCensusRow `json:"rows"`
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
	rep := &LaunchDeliveryCensusReport{ByCohort: map[string]int{}, ByCause: map[string]int{}}
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
			if row.Cohort != "delivers" {
				rep.Withheld++
			}
			for _, p := range LaunchDeliveryCensusPermissions {
				if v, ok := row.Verdicts[p]; ok && !v.Allowed {
					rep.ByCause[p+":"+string(v.Cause)]++
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

func (a *AuthzService) launchDeliveryCensusRow(ctx context.Context, ag *store.Agent) LaunchDeliveryCensusRow {
	row := LaunchDeliveryCensusRow{AgentID: ag.ID, ProjectID: ag.ProjectID, OwnerID: ag.OwnerID, Phase: ag.Phase,
		Verdicts: map[string]LaunchDeliveryVerdict{}}
	row.Edge = a.launchDeliveryEdgeFacts(ctx, ag)
	h, err := a.newHubDeliveryIdentity(ctx, ag.ID)
	if err != nil {
		// A start of this agent fails hard (the credential cannot be minted).
		row.MintError = err.Error()
		row.Cohort = "mint_error"
		return row
	}
	var firstDeny *LaunchDeliveryVerdict
	walkPassed, targetHeld := false, false
	for _, p := range LaunchDeliveryCensusPermissions {
		o := a.hubDeliveryChainAdmission(ctx, h, p, ag.ID, nil)
		v := LaunchDeliveryVerdict{Allowed: o.ok, Cause: o.cause, Reason: o.reason}
		row.Verdicts[p] = v
		if !o.ok && firstDeny == nil {
			vv := v
			firstDeny = &vv
			// hubDeliveryChainAdmission prefixes a provenance-root refusal
			// (the walk passed) with this text.
			walkPassed = strings.HasPrefix(o.reason, "delivery provenance root denied")
			targetHeld = o.reason == "agent "+ag.ID+" is held"
		}
	}
	if firstDeny == nil {
		row.Cohort = "delivers"
		return row
	}
	row.Cohort = launchDeliveryCohort(row.Edge, firstDeny.Cause, walkPassed, targetHeld)
	return row
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
	case cause == DenyCauseCeilingEffectExceeded && f.SourceCredentialKind == string(store.SourceCredentialUAT):
		return "uat_created"
	case f.DelegatorAgentGone:
		return "deleted_or_held_parent"
	default:
		// The agent's own edge passes the depth-0 checks above; the refusal is
		// at an ancestor hop (or a cause the facts above do not name).
		return "ancestor_" + string(cause)
	}
}
