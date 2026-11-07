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
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The project agent, skill and template lists for an agent delegated
// credential (.design/agent-delegation.md §11.8). Each follows the hub
// agent list (listAgentsDelegated): the scope comes only from the delegated
// list arm, every candidate item is decided by the item filter, and the
// store query runs through listDelegated, which opens the request cursor
// against the route's delegated binding and seals the cursor it returns.
// A refusal by the list arm, and a server error, answers 200 with an empty
// list plus the arm's deny record. Only project-scoped items are ever
// listed: hub-catalog (global and core) and user-scoped skills and
// templates have no project parent, so the item filter never allows them,
// and the store query already excludes them.

// listProjectAgentsDelegated is GET /api/v1/projects/{projectId}/agents for
// an agent delegated credential. The scope is the delegated list arm for
// agent.list narrowed to this one project, so it is Explicit {P} or None;
// every agent is then decided for agent.read. The cursor binding is the
// project agent list's own key ("project-agents:<projectID>"), bound to the
// credential, grant, agent and boundary. It serves the default order only.
func (s *Server) listProjectAgentsDelegated(w http.ResponseWriter, r *http.Request, d *DelegatedAgentIdentity, projectID string) {
	if !admitDelegatedListHandler(w, r, opAgentList, projectAgentCollectionTarget(projectID)) {
		return
	}
	ctx, query := r.Context(), r.URL.Query()
	perfSetEndpoint(ctx, perfEndpointAgentsProjectLegacy)
	if query.Get("sort") != "" || query.Get("ids") != "" {
		writeError(w, http.StatusBadRequest, errCodeDelegatedListSortUnsupported,
			"a delegated credential lists agents in the default order only; sort and ids are not supported", nil)
		return
	}
	limit, err := parseAuthorizedListLimit(query.Get("limit"))
	if err != nil {
		BadRequest(w, err.Error())
		return
	}
	view := legacyAgentListView(query)
	c := listFilterCollectorFromContext(ctx)
	writeEmpty := func() {
		if c != nil {
			c.markComplete()
		}
		s.writeAgentList(ctx, w, view, ListAgentsResponse{
			Agents:       []AgentWithCapabilities{},
			ServerTime:   time.Now().UTC(),
			Capabilities: &Capabilities{Actions: []string{}},
		})
	}

	scope, err := s.delegatedListScope(ctx, d, "agent", "agent.list", projectID)
	if err != nil {
		slog.WarnContext(ctx, "listProjectAgentsDelegated: list scope failed; answering an empty list", "error", err)
	}
	if err != nil || scope.Mode != delegatedListExplicit || !slices.Equal(scope.ProjectIDs, []string{projectID}) {
		writeEmpty()
		return
	}

	filter, ok := projectAgentListFilter(w, query, projectID)
	if !ok {
		return
	}
	binding := scopedCursorBinding(sortSuffix("project-agents:"+projectID, "", ""), filter, d)
	result, err := listDelegated(ctx, s, d, query.Get("cursor"), limit, binding,
		func(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
			defer perfPhaseStart(ctx, perfPhaseListDBRead)()
			return s.store.ListAgents(ctx, filter, opts)
		},
		agentResource,
		func(a *store.Agent) string { return authorizedListCursor(a.Created, a.ID, binding) })
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	agents, _ := s.buildGlobalAgentPage(ctx, d, result.Items)
	if c != nil {
		c.markComplete()
	}
	s.writeAgentList(ctx, w, view, ListAgentsResponse{
		Agents:                agents,
		NextCursor:            result.NextCursor,
		TotalCount:            result.TotalCount,
		TotalCountApproximate: result.TotalCountApproximate,
		ServerTime:            time.Now().UTC(),
		Capabilities:          &Capabilities{Actions: []string{}},
	})
}

// listSkillsDelegated is GET /api/v1/skills for an agent delegated
// credential. The scope is the delegated list arm for skill.list; the store
// query lists project-scoped skills only (no hub scope, no user scope):
// the scope's projects in Explicit mode, every project in AllProjects mode,
// where the item filter decides project admission per row. Excluded
// projects are enforced by that per-row admission, not pushed into the
// query. Every skill is decided for skill.read. The cursor binding is the
// "skills" key bound to the credential, grant, agent and boundary.
func (s *Server) listSkillsDelegated(w http.ResponseWriter, r *http.Request, d *DelegatedAgentIdentity, filter store.SkillFilter) {
	if !admitDelegatedListHandler(w, r, opSkillList, hubSkillCollectionTarget) {
		return
	}
	ctx, query := r.Context(), r.URL.Query()
	limit, err := parseAuthorizedListLimit(query.Get("limit"))
	if err != nil {
		BadRequest(w, err.Error())
		return
	}
	c := listFilterCollectorFromContext(ctx)
	writeEmpty := func() {
		if c != nil {
			c.markComplete()
		}
		writeJSON(w, http.StatusOK, ListSkillsResponse{Skills: []SkillWithCapabilities{}, TotalCount: 0})
	}

	scope, err := s.delegatedListScope(ctx, d, "skill", "skill.list", "")
	if err != nil {
		slog.WarnContext(ctx, "listSkillsDelegated: list scope failed; answering an empty list", "error", err)
	}
	if err != nil || scope.Mode == delegatedListNone {
		writeEmpty()
		return
	}
	// Only project-scoped skills are listed; a filter for another scope
	// lists nothing.
	if filter.Scope != "" && filter.Scope != store.SkillScopeProject {
		writeEmpty()
		return
	}
	if filter.ScopeID != "" && !scope.admitsProject(filter.ScopeID) {
		writeEmpty()
		return
	}
	access := &store.SkillAccessScope{}
	switch scope.Mode {
	case delegatedListExplicit:
		access.ProjectIDs = append([]string(nil), scope.ProjectIDs...)
	case delegatedListAllProjects:
		access.AllProjects = true
	}
	filter.AccessScope = access

	binding := scopedCursorBinding("skills", filter, d)
	result, err := listDelegated(ctx, s, d, query.Get("cursor"), limit, binding,
		func(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.Skill], error) {
			return s.store.ListSkills(ctx, filter, opts)
		},
		skillResource,
		func(sk *store.Skill) string { return authorizedListCursor(sk.Created, sk.ID, binding) })
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	resources := make([]Resource, len(result.Items))
	for i := range result.Items {
		resources[i] = skillResource(&result.Items[i])
	}
	caps := s.authzService.ComputeCapabilitiesBatch(ctx, d, resources, "skill")
	skills := make([]SkillWithCapabilities, 0, len(result.Items))
	for i := range result.Items {
		skills = append(skills, SkillWithCapabilities{Skill: result.Items[i], Cap: caps[i]})
	}
	if c != nil {
		c.markComplete()
	}
	writeJSON(w, http.StatusOK, ListSkillsResponse{
		Skills:                skills,
		NextCursor:            result.NextCursor,
		TotalCount:            result.TotalCount,
		TotalCountApproximate: result.TotalCountApproximate,
	})
}

// listTemplatesDelegated is GET /api/v1/templates for an agent delegated
// credential. The scope is the delegated list arm for template.list; the
// store query lists project-scoped templates only: TemplateFilter.ProjectIDs
// is the scope's projects in Explicit mode (intersected with a caller
// projectId or scopeId), and in AllProjects mode the query is narrowed only
// by a caller projectId or scopeId while the item filter decides project
// admission per row. scope=global and scope=user list nothing. Every
// template is decided for template.read, whose project is the template's
// ScopeID (templateResource). The cursor binding is the "templates" key
// bound to the credential, grant, agent and boundary; the request cursor
// is validated after it is opened, not before.
func (s *Server) listTemplatesDelegated(w http.ResponseWriter, r *http.Request, d *DelegatedAgentIdentity, filter store.TemplateFilter) {
	if !admitDelegatedListHandler(w, r, opTemplateList, hubTemplateCollectionTarget) {
		return
	}
	ctx, query := r.Context(), r.URL.Query()
	limit, err := parseAuthorizedListLimit(query.Get("limit"))
	if err != nil {
		BadRequest(w, err.Error())
		return
	}
	c := listFilterCollectorFromContext(ctx)
	writeEmpty := func() {
		if c != nil {
			c.markComplete()
		}
		writeJSON(w, http.StatusOK, ListTemplatesResponse{Templates: []TemplateWithCapabilities{}, TotalCount: 0})
	}

	scope, err := s.delegatedListScope(ctx, d, "template", "template.list", "")
	if err != nil {
		slog.WarnContext(ctx, "listTemplatesDelegated: list scope failed; answering an empty list", "error", err)
	}
	if err != nil || scope.Mode == delegatedListNone {
		writeEmpty()
		return
	}
	if filter.Scope != "" && filter.Scope != store.TemplateScopeProject {
		writeEmpty()
		return
	}
	filter.Scope = store.TemplateScopeProject
	filter.UserID = ""
	// A caller projectId or scopeId narrows the list to that one project,
	// which must be inside the scope; both must agree when both are set.
	callerProject := filter.ScopeID
	if filter.ProjectID != "" {
		if callerProject != "" && callerProject != filter.ProjectID {
			writeEmpty()
			return
		}
		callerProject = filter.ProjectID
	}
	filter.ScopeID, filter.ProjectID = "", ""
	switch {
	case callerProject != "":
		if !scope.admitsProject(callerProject) {
			writeEmpty()
			return
		}
		filter.ProjectIDs = []string{callerProject}
	case scope.Mode == delegatedListExplicit:
		filter.ProjectIDs = append([]string{}, scope.ProjectIDs...)
	}

	binding := scopedCursorBinding("templates", filter, d)
	result, err := listDelegated(ctx, s, d, query.Get("cursor"), limit, binding,
		func(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.Template], error) {
			return s.store.ListTemplates(ctx, filter, opts)
		},
		templateResource,
		func(t *store.Template) string { return authorizedListCursor(t.Created, t.ID, binding) })
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	resources := make([]Resource, len(result.Items))
	for i := range result.Items {
		resources[i] = templateResource(&result.Items[i])
	}
	caps := s.authzService.ComputeCapabilitiesBatch(ctx, d, resources, "template")
	templates := make([]TemplateWithCapabilities, 0, len(result.Items))
	for i := range result.Items {
		templates = append(templates, TemplateWithCapabilities{Template: result.Items[i], Cap: caps[i]})
	}
	if c != nil {
		c.markComplete()
	}
	writeJSON(w, http.StatusOK, ListTemplatesResponse{
		Templates:             templates,
		NextCursor:            result.NextCursor,
		TotalCount:            result.TotalCount,
		TotalCountApproximate: result.TotalCountApproximate,
	})
}
