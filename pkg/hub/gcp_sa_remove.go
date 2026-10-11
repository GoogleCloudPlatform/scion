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
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Removing a registered GCP service account (ptone/scion#4022, spec §3.7,
// decision Q3 = (b)).
//
// THE RULES THIS FILE ENFORCES:
//
//   - Before deleting, the hub computes an impact report: agents whose applied
//     identity references the account, defaults that point at it (project,
//     per-profile, hub) and broker profiles that map it.
//   - A default that points at the account BLOCKS the delete (409 sa_in_use,
//     report attached). ?force=true clears project and per-profile defaults
//     the caller may clear and then deletes.
//   - --force NEVER clears the hub default. Whoever may delete an account (for
//     a hub-scoped account that includes its creator, who need not be an
//     admin) must not thereby rewrite hub-wide settings, so a hub default
//     pointing at the account stays a 409 until a hub admin changes it.
//   - A cleared project default whose mode was "assign" becomes "block", not
//     unset. Agent creation already treats assign-without-account as block,
//     so this keeps behaviour, and it is least privilege: unsetting would let
//     the ladder fall through to a broader hub or runtime default.
//   - Agents referencing the account never block. They keep the reference
//     and fail at their next start with the existing "no longer available"
//     message.
//   - The report discloses nothing the caller could not otherwise see. Agents
//     are named only when the caller may read them (the agent-list rule);
//     the rest are counted per project, unnamed. A default in a project the
//     caller may not read is redacted to its tier and counted.
//   - Force clears only defaults in projects the caller may update (the
//     project settings permission). Any other referencing default keeps the
//     delete a 409 that says an admin of those projects must change it.
//   - The response lists the cleanup the hub cannot do: the broker mapping,
//     the Kubernetes ServiceAccount, the IAM bindings, and for a minted
//     account the account itself, which stays in GCP.

// ErrCodeSAInUse is the error code of a delete refused because defaults
// still reference the account.
const ErrCodeSAInUse = "sa_in_use"

// gcpSAImpactAgentLimit caps the agents listed in an impact report; the
// total is always reported in AgentCount.
const gcpSAImpactAgentLimit = 50

// gcpSAImpactPageSize is the page size of the store scans behind a report.
const gcpSAImpactPageSize = 500

// Default tiers named in GCPServiceAccountImpactDefault.Tier.
const (
	GCPSADefaultTierProject = "project"
	GCPSADefaultTierProfile = "profile"
	GCPSADefaultTierHub     = "hub"
)

// GCPServiceAccountImpactAgent is an agent whose applied GCP identity
// references the account.
type GCPServiceAccountImpactAgent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"projectId"`
}

// GCPServiceAccountImpactDefault is one default that points at the account.
//
// A default in a project the caller may not read is REDACTED: Tier and
// Clearable stay, ProjectID and Profile are dropped and Redacted is set, so
// the report never names a project the caller could not otherwise see.
type GCPServiceAccountImpactDefault struct {
	Tier      string `json:"tier"`
	ProjectID string `json:"projectId,omitempty"`
	Profile   string `json:"profile,omitempty"`
	// Clearable reports whether ?force=true, sent by this caller, clears
	// this default: true for a project or per-profile default in a project
	// the caller may update, false otherwise and always false for the hub
	// default.
	Clearable bool `json:"clearable"`
	Redacted  bool `json:"redacted,omitempty"`

	// project is the owning project's ID, kept server-side even when the
	// entry is redacted so a forced delete can clear it.
	project string
}

// GCPServiceAccountImpactMapping is a broker profile that maps the account
// to a Kubernetes ServiceAccount, as last reported by the broker.
type GCPServiceAccountImpactMapping struct {
	BrokerID   string `json:"brokerId"`
	BrokerName string `json:"brokerName,omitempty"`
	Profile    string `json:"profile"`
}

// GCPServiceAccountImpact is the impact report for removing one account.
type GCPServiceAccountImpact struct {
	ServiceAccountID string `json:"serviceAccountId"`
	// AgentCount is the total number of agents referencing the account.
	// Agents names at most gcpSAImpactAgentLimit of them, and only agents
	// the caller may read (the agent-list rule); VisibleAgentCount is how
	// many readable agents there are in all. The rest are reported only as
	// HiddenAgentCounts: one count per project, largest first, with no
	// project or agent names, so the report discloses nothing the caller
	// could not otherwise see.
	Agents            []GCPServiceAccountImpactAgent   `json:"agents"`
	AgentCount        int                              `json:"agentCount"`
	VisibleAgentCount int                              `json:"visibleAgentCount"`
	HiddenAgentCounts []int                            `json:"hiddenAgentCounts"`
	Defaults          []GCPServiceAccountImpactDefault `json:"defaults"`
	// HiddenDefaultCount is how many entries of Defaults are redacted.
	HiddenDefaultCount int                              `json:"hiddenDefaultCount"`
	BrokerMappings     []GCPServiceAccountImpactMapping `json:"brokerMappings"`
	// Managed is true for an account the hub minted. Removing the
	// registration does not delete it in GCP.
	Managed bool `json:"managed"`
	// ManualCleanup lists the steps the hub cannot do itself.
	ManualCleanup []string `json:"manualCleanup"`
}

// DeleteGCPServiceAccountResponse is the 200 body of a successful delete.
type DeleteGCPServiceAccountResponse struct {
	Deleted bool `json:"deleted"`
	// ClearedDefaults lists the defaults a forced delete cleared.
	ClearedDefaults []GCPServiceAccountImpactDefault `json:"clearedDefaults,omitempty"`
	Impact          GCPServiceAccountImpact          `json:"impact"`
}

// removeGCPServiceAccount is the delete body shared by the nested and flat
// routes. The caller has located the account and authorized ActionDelete.
func (s *Server) removeGCPServiceAccount(w http.ResponseWriter, r *http.Request, sa *store.GCPServiceAccount) {
	ctx := r.Context()

	force := false
	if raw := r.URL.Query().Get("force"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"invalid force parameter: expected true or false", nil)
			return
		}
		force = v
	}

	impact, err := s.gcpServiceAccountImpact(ctx, sa)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if len(impact.Defaults) > 0 && (!force || !allDefaultsClearable(impact.Defaults)) {
		s.logGCPServiceAccountAudit(ctx, GCPSAAuditDelete, sa, false, gcpSAAuditOutcomeRefusedInUse,
			map[string]string{"force": strconv.FormatBool(force), "defaults": strconv.Itoa(len(impact.Defaults))})
		writeError(w, http.StatusConflict, ErrCodeSAInUse, gcpSAInUseMessage(impact, force),
			map[string]interface{}{"impact": impact})
		return
	}

	var cleared []GCPServiceAccountImpactDefault
	if force && len(impact.Defaults) > 0 {
		cleared, err = s.clearGCPServiceAccountDefaults(ctx, sa.ID, impact.Defaults)
		s.applyCallerViewToDefaults(ctx, cleared)
		if err != nil {
			s.logGCPServiceAccountAudit(ctx, GCPSAAuditDelete, sa, false, gcpSAAuditOutcomeDeleteFailed,
				map[string]string{"force": "true", "defaults_cleared": strconv.Itoa(len(cleared))})
			writeGCPSARemoveFailure(w, err, cleared)
			return
		}
	}

	if err := s.store.DeleteGCPServiceAccount(ctx, sa.ID); err != nil {
		s.logGCPServiceAccountAudit(ctx, GCPSAAuditDelete, sa, false, gcpSAAuditOutcomeDeleteFailed,
			map[string]string{"force": strconv.FormatBool(force), "defaults_cleared": strconv.Itoa(len(cleared))})
		writeGCPSARemoveFailure(w, err, cleared)
		return
	}

	// Invalidate cached actAs decisions for the deleted SA so that any
	// subsequent check against this email goes to the inner checker.
	s.invalidateActAsCache(sa.Email)

	s.logGCPServiceAccountAudit(ctx, GCPSAAuditDelete, sa, true, gcpSAAuditOutcomeDeleted, map[string]string{
		"force":            strconv.FormatBool(force),
		"defaults_cleared": strconv.Itoa(len(cleared)),
		"agents":           strconv.Itoa(impact.AgentCount),
		"broker_mappings":  strconv.Itoa(len(impact.BrokerMappings)),
	})

	writeJSON(w, http.StatusOK, DeleteGCPServiceAccountResponse{
		Deleted:         true,
		ClearedDefaults: cleared,
		Impact:          impact,
	})
}

// writeGCPSARemoveFailure answers a delete that failed. Clearing and
// deleting are NOT atomic: a forced delete clears defaults first, so when it
// fails afterwards (a later project update, or the delete itself) the
// defaults it already cleared stay cleared. That state is safe (assign became
// block, and a retry is idempotent), but the caller must be told, so the
// cleared defaults (redacted for the caller) travel in the error details.
// With nothing cleared the error is written as before.
func writeGCPSARemoveFailure(w http.ResponseWriter, err error, cleared []GCPServiceAccountImpactDefault) {
	if len(cleared) == 0 {
		writeErrorFromErr(w, err, "")
		return
	}
	writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
		fmt.Sprintf("removing the service account failed after %d default(s) were cleared; they stay cleared. Retry the delete.", len(cleared)),
		map[string]interface{}{"clearedDefaults": cleared})
}

func allDefaultsClearable(defaults []GCPServiceAccountImpactDefault) bool {
	for _, d := range defaults {
		if !d.Clearable {
			return false
		}
	}
	return true
}

// gcpSAInUseMessage is the human-readable refusal. The structured report is
// in the error details; this text is what a client without a renderer shows.
// Redacted defaults are only counted, never named.
func gcpSAInUseMessage(impact GCPServiceAccountImpact, force bool) string {
	var parts []string
	hubBlocks, otherBlocks := false, false
	for _, d := range impact.Defaults {
		if d.Tier == GCPSADefaultTierHub {
			hubBlocks = true
			parts = append(parts, "the hub default")
			continue
		}
		if !d.Clearable {
			otherBlocks = true
		}
		switch {
		case d.Redacted:
			// Counted below, never named.
		case d.Tier == GCPSADefaultTierProfile:
			parts = append(parts, fmt.Sprintf("the default for profile %q in project %s", d.Profile, d.ProjectID))
		default:
			parts = append(parts, fmt.Sprintf("the default of project %s", d.ProjectID))
		}
	}
	if impact.HiddenDefaultCount > 0 {
		parts = append(parts, fmt.Sprintf("%d default(s) in other projects", impact.HiddenDefaultCount))
	}
	msg := "service account is in use by " + strings.Join(parts, ", ") + "."
	if hubBlocks {
		msg += " The hub default is never cleared by force: a hub admin must change it first."
	}
	if otherBlocks {
		msg += " Some defaults are in projects you cannot change: an admin of those projects must change them first."
	}
	switch {
	case hubBlocks || otherBlocks:
		if !force && !allDefaultsBlocked(impact.Defaults) {
			msg += " Then retry with force to clear the remaining defaults."
		}
	default:
		msg += " Retry with force to clear these defaults and remove the account;" +
			" a cleared 'assign' default becomes 'block', so new agents get no GCP identity until a new default is set."
	}
	if impact.AgentCount > 0 {
		msg += fmt.Sprintf(" %d agent(s) also reference it; they do not block removal and will fail at their next start.", impact.AgentCount)
	}
	return msg
}

func allDefaultsBlocked(defaults []GCPServiceAccountImpactDefault) bool {
	for _, d := range defaults {
		if d.Clearable {
			return false
		}
	}
	return true
}

// gcpServiceAccountImpact computes the impact report for removing sa.
//
// A project-scoped account is reachable only from its own project, so only
// that project's agents, defaults and provider brokers are scanned. Any other
// scope (hub, user) is scanned hub-wide.
//
// Agents are found with the store's AgentFilter.GCPServiceAccountID filter
// (the applied identity naming the account).
func (s *Server) gcpServiceAccountImpact(ctx context.Context, sa *store.GCPServiceAccount) (GCPServiceAccountImpact, error) {
	impact := GCPServiceAccountImpact{
		ServiceAccountID:  sa.ID,
		Agents:            []GCPServiceAccountImpactAgent{},
		HiddenAgentCounts: []int{},
		Defaults:          []GCPServiceAccountImpactDefault{},
		BrokerMappings:    []GCPServiceAccountImpactMapping{},
		Managed:           sa.Managed,
	}
	projectScoped := sa.Scope == store.ScopeProject

	// Agents. Names are listed only for agents the caller may read.
	identity := GetIdentityFromContext(ctx)
	hiddenByProject := map[string]int{}
	// GCPServiceAccountID narrows the scan in the store; the per-row check
	// below stays as a guard.
	agentFilter := store.AgentFilter{GCPServiceAccountID: sa.ID}
	if projectScoped {
		agentFilter.ProjectID = sa.ScopeID
	}
	cursor := ""
	for {
		page, err := s.store.ListAgents(ctx, agentFilter, store.ListOptions{
			Limit: gcpSAImpactPageSize, Cursor: cursor, SkipTotalCount: true,
		})
		if err != nil {
			return impact, fmt.Errorf("listing agents for the impact report: %w", err)
		}
		var matched []store.Agent
		for i := range page.Items {
			a := &page.Items[i]
			if a.AppliedConfig == nil || a.AppliedConfig.GCPIdentity == nil ||
				a.AppliedConfig.GCPIdentity.ServiceAccountID != sa.ID {
				continue
			}
			matched = append(matched, *a)
		}
		if len(matched) > 0 {
			// Names only for a USER caller, the same rule
			// applyCallerViewToDefaults applies to defaults.
			// readableAgentRows passes every row through for an agent
			// caller (its sibling-listing behaviour), which must not
			// turn into hub-wide agent names here.
			readable := map[string]bool{}
			if _, isUser := identity.(UserIdentity); isUser {
				rows, err := s.readableAgentRows(ctx, identity, matched)
				if err != nil {
					return impact, fmt.Errorf("authorizing agents for the impact report: %w", err)
				}
				for i := range rows {
					readable[rows[i].ID] = true
				}
			}
			for i := range matched {
				a := &matched[i]
				impact.AgentCount++
				if !readable[a.ID] {
					hiddenByProject[a.ProjectID]++
					continue
				}
				impact.VisibleAgentCount++
				if len(impact.Agents) < gcpSAImpactAgentLimit {
					impact.Agents = append(impact.Agents, GCPServiceAccountImpactAgent{
						ID: a.ID, Name: a.Name, ProjectID: a.ProjectID,
					})
				}
			}
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}

	for _, n := range hiddenByProject {
		impact.HiddenAgentCounts = append(impact.HiddenAgentCounts, n)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(impact.HiddenAgentCounts)))

	// Project and per-profile defaults.
	if projectScoped {
		project, err := s.store.GetProject(ctx, sa.ScopeID)
		switch {
		case err == nil:
			impact.Defaults = append(impact.Defaults, projectDefaultsReferencing(project, sa.ID)...)
		case errors.Is(err, store.ErrNotFound):
		default:
			return impact, fmt.Errorf("reading project for the impact report: %w", err)
		}
	} else {
		cursor = ""
		for {
			page, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
				Limit: gcpSAImpactPageSize, Cursor: cursor, SkipTotalCount: true,
			})
			if err != nil {
				return impact, fmt.Errorf("listing projects for the impact report: %w", err)
			}
			for i := range page.Items {
				impact.Defaults = append(impact.Defaults, projectDefaultsReferencing(&page.Items[i], sa.ID)...)
			}
			if page.NextCursor == "" || page.NextCursor == cursor {
				break
			}
			cursor = page.NextCursor
		}
	}

	// Hub default. Checked for every scope: the settings PUT only accepts a
	// hub-scoped account, but a file-mode settings.yaml is not validated.
	if s.hubAgentDefaults().DefaultGCPIdentityServiceAccountID == sa.ID {
		impact.Defaults = append(impact.Defaults, GCPServiceAccountImpactDefault{Tier: GCPSADefaultTierHub})
	}
	impact.HiddenDefaultCount = s.applyCallerViewToDefaults(ctx, impact.Defaults)

	// Broker profiles mapping the account, from the brokers' last report.
	brokers, err := s.gcpSAImpactBrokers(ctx, sa)
	if err != nil {
		return impact, err
	}
	for _, b := range brokers {
		for _, p := range b.Profiles {
			for _, m := range p.ServiceAccountMappings {
				if strings.EqualFold(m.GSA, sa.Email) {
					impact.BrokerMappings = append(impact.BrokerMappings, GCPServiceAccountImpactMapping{
						BrokerID: b.ID, BrokerName: b.Name, Profile: p.Name,
					})
					break
				}
			}
		}
	}
	sort.Slice(impact.BrokerMappings, func(i, j int) bool {
		a, b := impact.BrokerMappings[i], impact.BrokerMappings[j]
		if a.BrokerName != b.BrokerName {
			return a.BrokerName < b.BrokerName
		}
		return a.Profile < b.Profile
	})

	impact.ManualCleanup = gcpSAManualCleanup(impact)
	return impact, nil
}

// gcpSAImpactBrokers returns the brokers whose profiles could map sa: the
// project's providers for a project-scoped account, every broker otherwise.
func (s *Server) gcpSAImpactBrokers(ctx context.Context, sa *store.GCPServiceAccount) ([]store.RuntimeBroker, error) {
	if sa.Scope == store.ScopeProject {
		providers, err := s.store.GetProjectProviders(ctx, sa.ScopeID)
		if err != nil {
			return nil, fmt.Errorf("listing project providers for the impact report: %w", err)
		}
		var brokers []store.RuntimeBroker
		for _, p := range providers {
			b, err := s.store.GetRuntimeBroker(ctx, p.BrokerID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				return nil, fmt.Errorf("reading broker for the impact report: %w", err)
			}
			if b != nil {
				brokers = append(brokers, *b)
			}
		}
		return brokers, nil
	}
	var brokers []store.RuntimeBroker
	cursor := ""
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{
			Limit: gcpSAImpactPageSize, Cursor: cursor, SkipTotalCount: true,
		})
		if err != nil {
			return nil, fmt.Errorf("listing brokers for the impact report: %w", err)
		}
		brokers = append(brokers, page.Items...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return brokers, nil
}

// projectDefaultsReferencing returns the project-wide and per-profile
// defaults of project that name saID, per-profile entries sorted by profile.
func projectDefaultsReferencing(project *store.Project, saID string) []GCPServiceAccountImpactDefault {
	if project == nil || project.Annotations == nil {
		return nil
	}
	var out []GCPServiceAccountImpactDefault
	if project.Annotations[projectSettingDefaultGCPIdentitySAID] == saID {
		out = append(out, GCPServiceAccountImpactDefault{
			Tier: GCPSADefaultTierProject, ProjectID: project.ID, project: project.ID,
		})
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
		out = append(out, GCPServiceAccountImpactDefault{
			Tier: GCPSADefaultTierProfile, ProjectID: project.ID, Profile: profile, project: project.ID,
		})
	}
	return out
}

// applyCallerViewToDefaults decides, for the caller in ctx, which project
// and per-profile defaults it may clear and which it may see, in place. A
// default is clearable when the caller may update its project (the
// permission the project settings PUT requires) and redacted when the caller
// may not read its project. The hub default is never clearable. Only a user
// caller is granted either. It returns the number of redacted entries.
func (s *Server) applyCallerViewToDefaults(ctx context.Context, defaults []GCPServiceAccountImpactDefault) int {
	user, _ := GetIdentityFromContext(ctx).(UserIdentity)
	type access struct{ read, update bool }
	cache := map[string]access{}
	projectAccess := func(projectID string) access {
		if a, ok := cache[projectID]; ok {
			return a
		}
		var a access
		if user != nil && s.authzService != nil {
			res := Resource{Type: "project", ID: projectID}
			if p, err := s.store.GetProject(ctx, projectID); err == nil && p != nil {
				res.OwnerID = p.OwnerID
			}
			a.update = s.authzService.CheckAccess(ctx, user, res, ActionUpdate).Allowed
			a.read = a.update || s.authzService.CheckAccess(ctx, user, res, ActionRead).Allowed
		}
		cache[projectID] = a
		return a
	}

	hidden := 0
	for i := range defaults {
		d := &defaults[i]
		if d.Tier == GCPSADefaultTierHub || d.project == "" {
			d.Clearable = false
			continue
		}
		a := projectAccess(d.project)
		d.Clearable = a.update
		if !a.read {
			d.ProjectID = ""
			d.Profile = ""
			d.Redacted = true
			hidden++
		}
	}
	return hidden
}

// clearGCPServiceAccountDefaults clears the clearable defaults in defaults
// that still name saID, re-reading each project so a concurrent settings
// change is not overwritten. It returns the defaults it actually cleared.
// The hub default is never touched (see the file comment).
func (s *Server) clearGCPServiceAccountDefaults(ctx context.Context, saID string, defaults []GCPServiceAccountImpactDefault) ([]GCPServiceAccountImpactDefault, error) {
	var projectIDs []string
	seen := map[string]bool{}
	for _, d := range defaults {
		if !d.Clearable || d.project == "" || seen[d.project] {
			continue
		}
		seen[d.project] = true
		projectIDs = append(projectIDs, d.project)
	}

	var cleared []GCPServiceAccountImpactDefault
	for _, projectID := range projectIDs {
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return cleared, fmt.Errorf("reading project %s to clear its defaults: %w", projectID, err)
		}
		if project == nil {
			// Defensive: a store returning (nil, nil) has nothing to clear.
			continue
		}
		current := projectDefaultsReferencing(project, saID)
		if len(current) == 0 {
			continue
		}
		// RACE, ACCEPTED: this is a read-modify-write of the whole project
		// row with no version check, the same pattern as the settings PUT.
		// A settings write that interleaves can lose its change, and one
		// that lands after the impact scan can point a default at the
		// account just before it is deleted. Either way the outcome is the
		// existing "no longer available" error at the next agent start, so
		// force does not guarantee zero dangling defaults.
		clearProjectDefaultsReferencing(project, saID)
		if err := s.store.UpdateProject(ctx, project); err != nil {
			return cleared, fmt.Errorf("clearing defaults of project %s: %w", projectID, err)
		}
		// Same event a settings PUT publishes, so open settings views and
		// other subscribers drop the stale default.
		s.events.PublishProjectUpdated(ctx, project)
		cleared = append(cleared, current...)
	}
	return cleared, nil
}

// clearProjectDefaultsReferencing removes saID from project's defaults in
// place. A project default in mode "assign" becomes "block" (least
// privilege; agent creation already treats assign-without-account as block).
func clearProjectDefaultsReferencing(project *store.Project, saID string) {
	if project == nil || project.Annotations == nil {
		return
	}
	if project.Annotations[projectSettingDefaultGCPIdentitySAID] == saID {
		delete(project.Annotations, projectSettingDefaultGCPIdentitySAID)
		if project.Annotations[projectSettingDefaultGCPIdentityMode] == store.GCPMetadataModeAssign {
			project.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeBlock
		}
	}
	if byProfile := profileDefaultSAIDsFromAnnotations(project.Annotations); byProfile != nil {
		kept := make(map[string]string, len(byProfile))
		for profile, id := range byProfile {
			if id != saID {
				kept[profile] = id
			}
		}
		if len(kept) != len(byProfile) {
			setProfileDefaultSAIDsAnnotation(project.Annotations, kept)
		}
	}
}

// gcpSAManualCleanup lists the out-of-band steps the hub cannot perform.
// The broker, KSA and IAM steps are always listed: the broker report the
// hub holds can be stale or missing, so their absence from BrokerMappings
// does not prove there is nothing to clean up.
func gcpSAManualCleanup(impact GCPServiceAccountImpact) []string {
	steps := []string{
		"Broker mapping: remove this account from kubernetes_service_account_mappings on every broker profile that maps it (a broker operator task; see brokerMappings for the profiles last reported).",
		"Kubernetes ServiceAccount: delete the Kubernetes ServiceAccount that mapping pointed at, if nothing else uses it.",
		"IAM bindings: remove the Workload Identity binding (roles/iam.workloadIdentityUser) that lets that Kubernetes ServiceAccount impersonate this account, and the hub's token-creator grant on the account.",
	}
	if impact.Managed {
		steps = append(steps, "GCP account: this account was minted by the hub and is retained in GCP; delete it in GCP if it is no longer needed.")
	} else {
		steps = append(steps, "GCP account: the account itself is not deleted in GCP.")
	}
	if impact.AgentCount > 0 {
		steps = append(steps, fmt.Sprintf("Agents: %d agent(s) still reference this account and will fail at their next start until they are given another GCP identity.", impact.AgentCount))
	}
	return steps
}
