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

package brokerconvert

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/capacityplan"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const capacityDecision = capacityplan.OutcomeOperatorDecisionRequired

// FlagEnforcementNotVisible replaces the planner's not_enforced flag: the
// Hub-wide enforcement switch is not visible to the CLI.
const FlagEnforcementNotVisible = "enforcement_not_visible: whether the Hub enforces Runtime Broker agent limits is not visible from the CLI"

// FlagProposedKeys notes that the allocations are keyed by proposed
// instance key, since no Runtime Broker ID exists before registration.
const FlagProposedKeys = "proposed_instance_keys: allocations are keyed by the proposed instance keys; Runtime Broker IDs are assigned at registration"

// ErrFlatRuntimeBroker refuses a host whose Hub row is already a flat
// Runtime Broker.
var ErrFlatRuntimeBroker = errors.New("this host is registered as a flat Runtime Broker; there is nothing to convert")

// ErrNotRegistered is returned when the host has no Runtime Broker ID.
var ErrNotRegistered = errors.New("this host is not registered as a Runtime Broker; run with --skip-hub for the local conversion only")

// errEmptyResponse stands for a Hub call that returned neither a value nor
// an error. The plan treats it as a failed request: a section note, or an
// error for the legacy row, never a nil dereference.
var errEmptyResponse = errors.New("the Hub returned an empty response")

// maxAgentPages bounds the agent list paging, so a Hub that keeps
// returning a cursor cannot loop the command forever.
const maxAgentPages = 1000

// CollectHub adds the Hub sections to p: the legacy Runtime Broker row,
// the capacity plan, the bound agents and the providing projects. It
// sends GET requests only. It returns an error, and the command exits 1,
// only when the legacy row cannot be used; a failure of any other call is
// noted in its section.
func CollectHub(ctx context.Context, client hubclient.Client, brokerID string, p *Plan) error {
	if brokerID == "" {
		return ErrNotRegistered
	}
	brokers := client.RuntimeBrokers()
	row, err := brokers.Get(ctx, brokerID)
	if err == nil && row == nil {
		err = errEmptyResponse
	}
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			return fmt.Errorf("the Hub has no Runtime Broker with ID %s, the ID this host is registered under; check it with 'scion hub brokers info'", brokerID)
		}
		return fmt.Errorf("reading Runtime Broker %s from the Hub: %w", brokerID, err)
	}
	if row.RuntimeTarget != nil {
		return ErrFlatRuntimeBroker
	}
	p.LegacyBrokerID = row.ID
	p.LegacyBroker = legacyBroker(row, p)

	agents, activeCount, agentsOK := collectAgents(ctx, client, row.ID, p)
	p.Agents = agents
	p.Projects = collectProjects(ctx, client, brokers, row.ID, p)
	p.Capacity = collectCapacity(ctx, brokers, row.ID, activeCount, agentsOK, p)
	p.summarize()
	return nil
}

func legacyBroker(row *hubclient.RuntimeBroker, p *Plan) *LegacyBroker {
	lb := &LegacyBroker{ID: row.ID, Name: row.Name, Profiles: []string{}}
	hub := map[string]bool{}
	for _, prof := range row.Profiles {
		if prof.Name != "" && !hub[prof.Name] {
			hub[prof.Name] = true
			lb.Profiles = append(lb.Profiles, prof.Name)
		}
	}
	sort.Strings(lb.Profiles)
	local := map[string]bool{}
	for _, m := range p.ProfileMap {
		local[m.Profile] = true
		if !hub[m.Profile] {
			lb.LocalOnly = append(lb.LocalOnly, m.Profile)
		}
	}
	for _, name := range lb.Profiles {
		if !local[name] {
			lb.HubOnly = append(lb.HubOnly, name)
		}
	}
	return lb
}

// What a section note names: a noun phrase for each secondary Hub read.
const (
	whatAgentList        = "the agent list"
	whatProjectList      = "the projects this Runtime Broker provides"
	whatProjectSettings  = "this project's settings"
	whatRuntimeBrokerSet = "the Runtime Broker's settings"
)

// noteFor turns a failed secondary Hub call into a section note with fixed
// text: what was not read, and the HTTP status when there is one. The
// error's own text is never used, since it can carry the Hub's URL or a
// server message. A visibility answer (401, 403, 404) is a partial view;
// any other failure also marks the plan incomplete.
func noteFor(p *Plan, what string, err error) string {
	if apiclient.IsForbiddenError(err) || apiclient.IsUnauthorizedError(err) || apiclient.IsNotFoundError(err) {
		return "not visible to you: " + what
	}
	p.hubIncomplete = true
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode > 0 {
		return fmt.Sprintf("could not be read from the Hub (HTTP %d): %s", apiErr.StatusCode, what)
	}
	return "could not be read from the Hub (request failed): " + what
}

func isTerminalPhase(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseStopped, state.PhaseError, state.PhaseHalted:
		return true
	}
	return false
}

func collectAgents(ctx context.Context, client hubclient.Client, brokerID string, p *Plan) (*AgentsSection, int, bool) {
	sec := &AgentsSection{Items: []AgentEntry{}}
	opts := &hubclient.ListAgentsOptions{RuntimeBrokerID: brokerID}
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page == maxAgentPages {
			p.hubIncomplete = true
			sec.Note = fmt.Sprintf("incomplete: %s has more than %d pages", whatAgentList, maxAgentPages)
			break
		}
		resp, err := client.Agents().List(ctx, opts)
		if err == nil && resp == nil {
			err = errEmptyResponse
		}
		if err != nil {
			sec.Note = noteFor(p, whatAgentList, err)
			return sec, 0, false
		}
		for _, a := range resp.Agents {
			if seen[a.ID] {
				continue
			}
			seen[a.ID] = true
			if isTerminalPhase(a.Phase) {
				sec.TerminalCount++
				continue
			}
			profile := ""
			if a.AppliedConfig != nil {
				profile = a.AppliedConfig.Profile
			}
			sec.Items = append(sec.Items, AgentEntry{
				ID:         a.ID,
				Name:       a.Name,
				ProjectID:  a.ProjectID,
				Project:    a.Project,
				Phase:      a.Phase,
				ProfileRef: p.refFor(profile),
			})
		}
		if resp.Page.NextCursor == "" || resp.Page.NextCursor == opts.Page.Cursor {
			break
		}
		opts.Page.Cursor = resp.Page.NextCursor
	}
	sort.Slice(sec.Items, func(i, j int) bool {
		a, b := sec.Items[i], sec.Items[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	return sec, len(sec.Items), sec.Note == ""
}

func collectProjects(ctx context.Context, client hubclient.Client, brokers hubclient.RuntimeBrokerService, brokerID string, p *Plan) *ProjectsSection {
	sec := &ProjectsSection{Items: []ProjectEntry{}}
	resp, err := brokers.ListProjects(ctx, brokerID)
	if err == nil && resp == nil {
		err = errEmptyResponse
	}
	if err != nil {
		sec.Note = noteFor(p, whatProjectList, err)
		return sec
	}
	for _, info := range resp.Projects {
		e := ProjectEntry{ID: info.ProjectID, Name: info.ProjectName}
		settings, err := client.Projects().GetSettings(ctx, info.ProjectID)
		if err == nil && settings == nil {
			err = errEmptyResponse
		}
		if err != nil {
			e.Note = noteFor(p, whatProjectSettings, err)
			sec.Items = append(sec.Items, e)
			continue
		}
		if settings.ActiveProfile != nil && *settings.ActiveProfile != "" {
			ref := p.refFor(*settings.ActiveProfile)
			e.ActiveProfile = &ref
		}
		var keys []string
		for k := range settings.DefaultGCPIdentityServiceAccountIDByProfile {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			e.ServiceAccountProfiles = append(e.ServiceAccountProfiles, p.refFor(k))
		}
		sec.Items = append(sec.Items, e)
	}
	sort.Slice(sec.Items, func(i, j int) bool {
		a, b := sec.Items[i], sec.Items[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	return sec
}

// collectCapacity plans the per-instance limits from the legacy Runtime
// Broker's Hub settings: its own maxAgents, the inherited limit and the
// active reservation count. The enforcement switch is not visible to the
// CLI, so the planner's not_enforced flag is replaced by
// FlagEnforcementNotVisible.
func collectCapacity(ctx context.Context, brokers hubclient.RuntimeBrokerService, brokerID string, activeAgents int, agentsOK bool, p *Plan) Capacity {
	getter, ok := brokers.(hubclient.RuntimeBrokerSettingsGetter)
	if !ok {
		return Capacity{Status: CapacityUnavailable, Note: "this Hub client cannot read Runtime Broker settings"}
	}
	settings, err := getter.GetSettings(ctx, brokerID)
	if err == nil && settings == nil {
		err = errEmptyResponse
	}
	if err != nil {
		return Capacity{Status: CapacityUnavailable, Note: noteFor(p, whatRuntimeBrokerSet, err)}
	}
	ids := make([]string, 0, len(p.Instances))
	for _, inst := range p.Instances {
		ids = append(ids, inst.Key)
	}
	eff := settings.Effective.MaxAgents
	l := capacityplan.LegacyCapacity{
		LegacyBrokerID: brokerID,
		Override:       settings.Settings.MaxAgents,
		// The CLI cannot see the enforcement switch; it is reported by
		// FlagEnforcementNotVisible instead of the planner's flag.
		Enforced: true,
	}
	inheritedKnown := eff.Inherited.Value != nil
	if inheritedKnown {
		// The Hub's own resolution of the inherited limit (bindings, then
		// the default; <= 0 is unlimited) stands in for both.
		l.SystemDefault = *eff.Inherited.Value
	}
	switch {
	case eff.Count != nil:
		l.ActiveReserved = int(*eff.Count)
	case agentsOK:
		l.ActiveReserved = activeAgents
	}
	plan := capacityplan.PlanInstanceCapacity(l, ids, nil)

	c := Capacity{Status: CapacityPlanned, Outcome: plan.Outcome, MaxAgents: settings.Settings.MaxAgents}
	for _, id := range ids {
		if v, ok := plan.Allocations[id]; ok {
			c.Allocations = append(c.Allocations, Allocation{Instance: id, MaxAgents: v})
		}
	}
	sort.Slice(c.Allocations, func(i, j int) bool { return c.Allocations[i].Instance < c.Allocations[j].Instance })
	for _, r := range plan.Reasons {
		switch {
		case strings.HasPrefix(r, capacityplan.ReasonInheritedAggregate) && !inheritedKnown:
			c.Reasons = append(c.Reasons, fmt.Sprintf("%s: legacy Runtime Broker %s has no maxAgents setting; each of the %d instances would inherit the Hub's limit by itself, so the aggregate changes",
				capacityplan.ReasonInheritedAggregate, brokerID, len(ids)))
		case strings.HasPrefix(r, capacityplan.ReasonActiveAboveAllocation), strings.HasPrefix(r, capacityplan.ReasonUnknownReceiver):
			c.Flags = append(c.Flags, r)
		default:
			c.Reasons = append(c.Reasons, r)
		}
	}
	c.Flags = append(c.Flags, FlagEnforcementNotVisible)
	if len(c.Allocations) > 0 {
		c.Flags = append(c.Flags, FlagProposedKeys)
	}
	return c
}
