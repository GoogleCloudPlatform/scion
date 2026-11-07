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
	"net/http"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/capacityplan"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Error codes of GET /api/v1/runtime-brokers/{id}/conversion-plan.
const (
	ErrCodeRuntimeBrokerNotLegacy    = "runtime_broker_not_legacy"
	ErrCodeConversionPlanNotReported = "conversion_plan_not_reported"
)

// Mapping statuses of a profile reference in the conversion plan response.
const (
	conversionPlanMapped         = "mapped"
	conversionPlanNoMapping      = "no_mapping"
	conversionPlanProfileUnknown = "profile_unknown"
)

// conversionPlanFlagProposedKeys notes that capacity allocations are keyed
// by proposed instance keys.
const conversionPlanFlagProposedKeys = "proposed_instance_keys: allocations are keyed by the proposed instance keys; Runtime Broker IDs are assigned at registration"

// conversionPlanAgentPage is the page size of the bound-agent scan, and
// conversionPlanMaxAgentPages bounds the number of pages it reads.
const (
	conversionPlanAgentPage     = 200
	conversionPlanMaxAgentPages = 1000
)

// errConversionPlanAgentPages fails the bound-agent scan closed when the
// store keeps returning a next cursor past conversionPlanMaxAgentPages.
var errConversionPlanAgentPages = errors.New("bound-agent scan exceeded its page limit")

// handleBrokerConversionPlan serves GET
// /api/v1/runtime-brokers/{id}/conversion-plan: the legacy Runtime Broker's
// stored conversion plan snapshot, plus the bound agents, the providing
// projects and the capacity plan, which the Hub computes itself. It is
// read-only. It needs the flat Runtime Brokers experiment (checked here,
// in the Hub) and broker read access; agents and projects are filtered by
// the caller's own read access, and hidden ones are only counted.
func (s *Server) handleBrokerConversionPlan(w http.ResponseWriter, r *http.Request, brokerID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.experimentEnabled(experiments.FlatRuntimeBrokers) {
		NotFound(w, "route")
		return
	}
	ctx := r.Context()

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		logAuthzDenial(r, nil, Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionRead, "no identity")
		Unauthorized(w)
		return
	}
	userIdent, ok := identity.(UserIdentity)
	if !ok {
		logAuthzDenial(r, identity, Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionRead, "non-user identity")
		Forbidden(w)
		return
	}

	// writeStoreErr: a nonexistent broker and a denied one get the same
	// body (see getRuntimeBroker).
	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		writeStoreErr(w, err, "RuntimeBroker")
		return
	}
	if decision := s.authzService.CheckAccess(ctx, userIdent, brokerResource(broker), ActionRead); !decision.Allowed {
		logAuthzDenial(r, userIdent, brokerResource(broker), ActionRead, decision.Reason)
		NotFound(w, "RuntimeBroker")
		return
	}
	if broker.IsFlat() {
		writeError(w, http.StatusConflict, ErrCodeRuntimeBrokerNotLegacy,
			"this Runtime Broker is a flat Runtime Broker; a conversion plan exists only for a legacy Runtime Broker", nil)
		return
	}
	if broker.ConversionPlanPresentation == nil {
		writeError(w, http.StatusNotFound, ErrCodeConversionPlanNotReported,
			"this Runtime Broker has not reported a conversion plan; upgrade the Runtime Broker", nil)
		return
	}
	plan := *broker.ConversionPlanPresentation

	agents, err := s.conversionPlanAgents(ctx, identity, brokerID, plan.ProfileMap)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	projects, err := s.conversionPlanProjects(ctx, identity, brokerID, plan.ProfileMap)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	capacity, err := s.conversionPlanCapacity(ctx, brokerID, plan.Instances)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, api.ConversionPlanResponse{
		BrokerID:   brokerID,
		ObservedAt: broker.ConversionPlanObservedAt,
		Plan:       plan,
		Agents:     agents,
		Projects:   projects,
		Capacity:   capacity,
	})
}

// conversionPlanRef maps a profile name through the snapshot's profile map.
func conversionPlanRef(profileMap map[string]string, profile string) api.ConversionPlanProfileRef {
	if profile == "" {
		return api.ConversionPlanProfileRef{Mapping: conversionPlanProfileUnknown}
	}
	if key, ok := profileMap[profile]; ok && key != "" {
		return api.ConversionPlanProfileRef{Profile: profile, Mapping: conversionPlanMapped, Instance: key}
	}
	return api.ConversionPlanProfileRef{Profile: profile, Mapping: conversionPlanNoMapping}
}

// conversionPlanAgents lists the agents bound to brokerID that identity
// can read, failing closed on an authorization error. Readable agents in a
// terminal phase are counted, not listed; unreadable ones are counted as
// hidden.
func (s *Server) conversionPlanAgents(ctx context.Context, identity Identity, brokerID string, profileMap map[string]string) (api.ConversionPlanAgents, error) {
	out := api.ConversionPlanAgents{Items: []api.ConversionPlanAgent{}}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages >= conversionPlanMaxAgentPages {
			return out, errConversionPlanAgentPages
		}
		page, err := s.store.ListAgents(ctx, store.AgentFilter{RuntimeBrokerID: brokerID},
			store.ListOptions{Limit: conversionPlanAgentPage, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return out, err
		}
		if len(page.Items) > 0 {
			allowed, err := s.authzService.AuthorizeReadBatch(ctx, identity, agentResources(page.Items))
			if err != nil {
				return out, err
			}
			for i := range page.Items {
				a := &page.Items[i]
				switch {
				case i >= len(allowed) || !allowed[i]:
					out.HiddenCount++
				case isTerminalAgentPhase(a.Phase):
					out.TerminalCount++
				default:
					profile := ""
					if a.AppliedConfig != nil {
						profile = a.AppliedConfig.Profile
					}
					out.Items = append(out.Items, api.ConversionPlanAgent{
						ID:                       a.ID,
						Name:                     a.Name,
						ProjectID:                a.ProjectID,
						Phase:                    a.Phase,
						ConversionPlanProfileRef: conversionPlanRef(profileMap, profile),
					})
				}
			}
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].ID < out.Items[j].ID })
	return out, nil
}

// conversionPlanProjects lists the projects brokerID provides that
// identity can read, with the same filter as GET .../projects
// (getBrokerProjects), failing closed. A project the caller cannot read is
// counted as hidden; its name, ID and settings are never returned.
func (s *Server) conversionPlanProjects(ctx context.Context, identity Identity, brokerID string, profileMap map[string]string) (api.ConversionPlanProjects, error) {
	out := api.ConversionPlanProjects{Items: []api.ConversionPlanProject{}}
	providers, err := s.store.GetBrokerProjects(ctx, brokerID)
	if err != nil {
		return out, err
	}
	if len(providers) == 0 {
		return out, nil
	}
	projects := make([]*store.Project, len(providers))
	resources := make([]Resource, len(providers))
	for i, p := range providers {
		project, err := s.store.GetProject(ctx, p.ProjectID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			resources[i] = Resource{Type: permissions.ResourceProject, ID: p.ProjectID}
		case err != nil:
			return out, err
		default:
			projects[i] = project
			resources[i] = projectResource(project)
		}
	}
	allowed, err := s.authzService.AuthorizeReadBatch(ctx, identity, resources)
	if err != nil {
		return out, err
	}
	for i, p := range providers {
		if i >= len(allowed) || !allowed[i] {
			out.HiddenCount++
			continue
		}
		entry := api.ConversionPlanProject{ID: p.ProjectID}
		if project := projects[i]; project != nil {
			entry.Name = project.Name
			settings := projectSettingsFromAnnotations(project)
			if settings.ActiveProfile != nil && *settings.ActiveProfile != "" {
				ref := conversionPlanRef(profileMap, *settings.ActiveProfile)
				entry.ActiveProfile = &ref
			}
			names := make([]string, 0, len(settings.DefaultGCPIdentityServiceAccountIDByProfile))
			for name := range settings.DefaultGCPIdentityServiceAccountIDByProfile {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				entry.ServiceAccountProfiles = append(entry.ServiceAccountProfiles, conversionPlanRef(profileMap, name))
			}
		}
		out.Items = append(out.Items, entry)
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].ID < out.Items[j].ID })
	return out, nil
}

// conversionPlanCapacity plans the per-instance limits that would replace
// brokerID's max_agents_per_broker limit, with the Hub's own values: the
// broker's maxAgents setting, the inherited limit (an entitlement binding
// or the limit definition's default), the enforcement switch and the
// broker's active reservations. The instance IDs are the proposed keys.
func (s *Server) conversionPlanCapacity(ctx context.Context, brokerID string, instances []api.RuntimeTargetPresentation) (api.ConversionPlanCapacity, error) {
	l := capacityplan.LegacyCapacity{LegacyBrokerID: brokerID, Enforced: s.brokerQuotasEnforced()}
	override, ok, err := s.brokerSettingLimitOverride(ctx, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, brokerID)
	if err != nil {
		return api.ConversionPlanCapacity{}, err
	}
	if ok {
		v := override
		l.Override = &v
	}
	limitDef := s.lookupAgentLimitDefinition(ctx)
	inherited, source, err := s.inheritedBrokerLimit(ctx, brokerID, limitDef)
	if err != nil {
		return api.ConversionPlanCapacity{}, err
	}
	if source == BrokerLimitSourceEntitlement {
		l.BrokerBindings = []int64{inherited}
	} else {
		l.SystemDefault = inherited
	}
	if limitDef != nil && s.quotaService != nil {
		n, err := s.store.CountActiveReservations(ctx, limitDef.ID, brokerID, store.QuotaScopeBroker, brokerID)
		if err != nil {
			return api.ConversionPlanCapacity{}, err
		}
		l.ActiveReserved = int(n)
	}

	ids := make([]string, 0, len(instances))
	for _, inst := range instances {
		ids = append(ids, inst.Key)
	}
	plan := capacityplan.PlanInstanceCapacity(l, ids, nil)
	out := api.ConversionPlanCapacity{Outcome: plan.Outcome, MaxAgents: l.Override}
	for id, v := range plan.Allocations {
		out.Allocations = append(out.Allocations, api.ConversionPlanAllocation{Instance: id, MaxAgents: v})
	}
	sort.Slice(out.Allocations, func(i, j int) bool { return out.Allocations[i].Instance < out.Allocations[j].Instance })
	for _, r := range plan.Reasons {
		if strings.HasPrefix(r, capacityplan.ReasonActiveAboveAllocation) || strings.HasPrefix(r, capacityplan.ReasonUnknownReceiver) ||
			strings.HasPrefix(r, capacityplan.ReasonNotEnforced) {
			out.Flags = append(out.Flags, r)
		} else {
			out.Reasons = append(out.Reasons, r)
		}
	}
	if len(out.Allocations) > 0 {
		out.Flags = append(out.Flags, conversionPlanFlagProposedKeys)
	}
	return out, nil
}
