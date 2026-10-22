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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project visitors listing and reading agents, and reading their own
// visitor grants and DMs (ptone/scion#4603).
//
//	GET /api/v1/projects/{projectId}/agents            the project's agents, visitor view
//	GET /api/v1/projects/{projectId}/agents/{agentId}  one agent (ID or slug), visitor view
//	GET /api/v1/users/me/visitor-grants                the visitor's own grants
//	GET /api/v1/users/me/visitor-dms                   the visitor's own DMs with agents
//
// routeGuard admits a visitor on the agent routes only through
// admitVisitorSubRoute (a current grant on the project, and agent.list or
// agent.read), checked on every request. The handlers answer a visitor
// with VisitorAgentView only, never the agent record.

// VisitorAgentView is the whole of what a project visitor sees of an
// agent: its name, slug and status. It is built field by field from the
// agent record (visitorAgentView), so a field added to store.Agent later
// is never shown to a visitor.
type VisitorAgentView struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
	// Status is the agent's single display status: its activity while it
	// runs, otherwise its lifecycle phase (state.AgentState.DisplayStatus).
	Status string `json:"status"`
}

// ListVisitorAgentsResponse is a visitor's GET {projectId}/agents
// response.
type ListVisitorAgentsResponse struct {
	Agents []VisitorAgentView `json:"agents"`
}

// visitorAgentView returns the visitor view of a: the one place an agent
// is projected for a visitor.
func visitorAgentView(a *store.Agent) VisitorAgentView {
	st := state.AgentState{Phase: state.Phase(a.Phase), Activity: state.Activity(a.Activity)}
	return VisitorAgentView{
		Name:   a.Name,
		Slug:   a.Slug,
		Status: st.DisplayStatus(),
	}
}

// visitorAgentListPageSize is the page size listProjectAgentsForVisitor
// reads the project's agents with; it reads every page.
const visitorAgentListPageSize = 500

// visitorMessageableParam is the query parameter that limits a visitor's
// agent list to the agents the visitor may message now.
const visitorMessageableParam = "messageable"

// listProjectAgentsForVisitor answers a visitor's GET {projectId}/agents:
// every agent of the project that is not deleted, newest first, in the
// visitor view. With ?messageable=true the list holds only the agents the
// visitor may message now: ComputeMessageability, the same decision
// (authorizeAgentMessage) that admits a send, evaluated per agent; a
// store or authorization error in that decision leaves the agent out.
// Every other query parameter, and any other value of messageable, is
// ignored. admitVisitorSubRoute has already checked the grant and
// agent.list for this request.
func (s *Server) listProjectAgentsForVisitor(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	visitor, _ := GetIdentityFromContext(ctx).(*VisitorIdentity)
	messageableOnly := r.URL.Query().Get(visitorMessageableParam) == "true"
	views := []VisitorAgentView{}
	cursor := ""
	for {
		page, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: projectID},
			store.ListOptions{Limit: visitorAgentListPageSize, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		for i := range page.Items {
			if page.Items[i].ProjectID != projectID {
				continue
			}
			if messageableOnly && !s.visitorMayMessageAgent(ctx, visitor, &page.Items[i]) {
				continue
			}
			views = append(views, visitorAgentView(&page.Items[i]))
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	writeJSON(w, http.StatusOK, ListVisitorAgentsResponse{Agents: views})
}

// visitorMayMessageAgent reports whether the visitor may message agent
// now (ComputeMessageability). It is false for a nil visitor, and for any
// store or authorization error, which the decision treats as a denial.
func (s *Server) visitorMayMessageAgent(ctx context.Context, visitor *VisitorIdentity, agent *store.Agent) bool {
	if visitor == nil {
		return false
	}
	m := s.ComputeMessageability(ctx, visitor, agent)
	return m != nil && m.CanMessage
}

// getProjectAgentForVisitor answers a visitor's GET
// {projectId}/agents/{agentId} (an agent ID or slug) with the agent's
// visitor view. An agent that is not found in the project, or is deleted,
// gets the uniform refusal, by ID as by slug. admitVisitorSubRoute has
// already checked the grant and agent.read for this request.
func (s *Server) getProjectAgentForVisitor(w http.ResponseWriter, r *http.Request, projectID, agentID string) {
	agent, err := s.resolveProjectAgent(r.Context(), projectID, agentID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !agent.DeletedAt.IsZero()) {
		writeVisitorRefusal(w)
		return
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, visitorAgentView(agent))
}

// visitorMayListProjectAgents reports whether the visitor may list the
// agents of the project projectRef names ({uuid} or {uuid}__{slug}): the
// project exists, the visitor holds a current project-visitor grant on it
// (visitorHoldsActiveGrant), and agent.list on the project's agents. An
// unknown project is false with no error; any other store error is
// returned.
func (s *Server) visitorMayListProjectAgents(ctx context.Context, visitor *VisitorIdentity, projectRef string) (bool, error) {
	if s.store == nil || projectRef == "" {
		return false, nil
	}
	project, err := s.store.GetProject(ctx, resolveProjectID(projectRef))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	granted, err := s.visitorHoldsActiveGrant(ctx, visitor.ID(), project.ID)
	if err != nil || !granted {
		return false, err
	}
	decision := s.authzService.CheckAccess(ctx, visitor,
		Resource{Type: "agent", ParentType: "project", ParentID: project.ID}, ActionList)
	return decision.Allowed, nil
}

// visitorMayReadAgent reports whether the visitor may read agent: the
// agent is not deleted, and the visitor holds a current project-visitor
// grant on the agent's project (visitorHoldsActiveGrant) and agent.read on
// the agent. The agent's message mode does not matter.
func (s *Server) visitorMayReadAgent(ctx context.Context, visitor *VisitorIdentity, agent *store.Agent) (bool, error) {
	if !agent.DeletedAt.IsZero() {
		return false, nil
	}
	granted, err := s.visitorHoldsActiveGrant(ctx, visitor.ID(), agent.ProjectID)
	if err != nil || !granted {
		return false, err
	}
	decision := s.authzService.CheckAccess(ctx, visitor, agentResource(agent), ActionRead)
	return decision.Allowed, nil
}

// visitorHoldsActiveGrant reports whether the federated account visitorID
// holds a project-visitor grant on projectID that is active now
// (isBindingActive). It reads the bindings on every call.
func (s *Server) visitorHoldsActiveGrant(ctx context.Context, visitorID, projectID string) (bool, error) {
	if projectID == "" {
		return false, nil
	}
	grants, err := s.visitorGrantBindings(ctx, visitorID)
	if err != nil {
		return false, err
	}
	now := time.Now()
	for _, rb := range grants {
		if rb.ScopeID == projectID && isBindingActive(rb, now) {
			return true, nil
		}
	}
	return false, nil
}

// visitorGrantBindings returns the project-visitor bindings of the user
// visitorID at project scope, in any window. Bindings of any other role
// or scope are left out.
func (s *Server) visitorGrantBindings(ctx context.Context, visitorID string) ([]*store.RoleBinding, error) {
	if s.store == nil || visitorID == "" {
		return nil, nil
	}
	bindings, err := s.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, visitorID)
	if err != nil {
		return nil, err
	}
	roleNames := map[string]string{}
	var out []*store.RoleBinding
	for _, rb := range bindings {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID == "" || rb.PrincipalID != visitorID {
			continue
		}
		name, ok := roleNames[rb.RoleDefinitionID]
		if !ok {
			rd, err := s.store.GetRoleDefinition(ctx, rb.RoleDefinitionID)
			if errors.Is(err, store.ErrNotFound) {
				roleNames[rb.RoleDefinitionID] = ""
				continue
			}
			if err != nil {
				return nil, err
			}
			if rd != nil {
				name = rd.Name
			}
			roleNames[rb.RoleDefinitionID] = name
		}
		if name == store.ProjectRoleVisitor {
			out = append(out, rb)
		}
	}
	return out, nil
}

// Visitor grant statuses, as GET /api/v1/users/me/visitor-grants reports
// them. A revoked grant no longer exists and is not listed.
const (
	visitorGrantStatusActive    = "active"
	visitorGrantStatusScheduled = "scheduled"
	visitorGrantStatusExpired   = "expired"
)

// VisitorOwnGrant is one of the caller's own visitor grants: the project
// it is on (ID, slug and name, so the visitor can address the project's
// agents), its window and its status now.
type VisitorOwnGrant struct {
	ProjectID   string `json:"projectId"`
	ProjectSlug string `json:"projectSlug"`
	ProjectName string `json:"projectName"`
	// Status is "active" inside the window, "scheduled" before NotBefore
	// and "expired" from ExpiresAt on.
	Status    string     `json:"status"`
	NotBefore *time.Time `json:"notBefore,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ListVisitorOwnGrantsResponse is the GET /api/v1/users/me/visitor-grants
// response.
type ListVisitorOwnGrantsResponse struct {
	Grants []VisitorOwnGrant `json:"grants"`
}

// visitorGrantStatus returns the status of rb at now.
func visitorGrantStatus(rb *store.RoleBinding, now time.Time) string {
	switch {
	case isBindingActive(rb, now):
		return visitorGrantStatusActive
	case rb.ExpiresAt != nil && !now.Before(*rb.ExpiresAt):
		return visitorGrantStatusExpired
	default:
		return visitorGrantStatusScheduled
	}
}

// handleUserMeVisitorGrants serves GET /api/v1/users/me/visitor-grants:
// the caller's own project-visitor grants, read on every request. Only a
// project visitor (a *VisitorIdentity, which exists only while the
// federation.project_visitors experiment is on) is answered; any other
// identity gets 404 Not Found, and a visitor while the experiment is off
// gets the uniform refusal. routeGuard admits
// a visitor on GET only. A grant whose project no longer exists is left
// out.
func (s *Server) handleUserMeVisitorGrants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	visitor, ok := GetIdentityFromContext(ctx).(*VisitorIdentity)
	if !ok || visitor == nil {
		NotFound(w, "Route")
		return
	}
	if !s.experimentEnabled(experiments.FederationProjectVisitors) || r.Method != http.MethodGet {
		writeVisitorRefusal(w)
		return
	}
	bindings, err := s.visitorGrantBindings(ctx, visitor.ID())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	now := time.Now()
	grants := []VisitorOwnGrant{}
	for _, rb := range bindings {
		project, err := s.store.GetProject(ctx, rb.ScopeID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		grants = append(grants, VisitorOwnGrant{
			ProjectID:   project.ID,
			ProjectSlug: project.Slug,
			ProjectName: project.Name,
			Status:      visitorGrantStatus(rb, now),
			NotBefore:   rb.NotBefore,
			ExpiresAt:   rb.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, ListVisitorOwnGrantsResponse{Grants: grants})
}

// VisitorOwnDM is one of the caller's own DMs with an agent: the
// conversation ID, the DM key the visitor DM read routes take, the agent's
// project and slug, and the time of the last activity.
type VisitorOwnDM struct {
	ConversationID  string    `json:"conversationId"`
	ConversationKey string    `json:"conversationKey"`
	ProjectSlug     string    `json:"projectSlug"`
	AgentSlug       string    `json:"agentSlug"`
	LastActivityAt  time.Time `json:"lastActivityAt"`
}

// ListVisitorOwnDMsResponse is the GET /api/v1/users/me/visitor-dms
// response.
type ListVisitorOwnDMsResponse struct {
	DMs []VisitorOwnDM `json:"dms"`
}

// handleUserMeVisitorDMs serves GET /api/v1/users/me/visitor-dms: the
// caller's own DMs with agents, most recent activity first, read on every
// request. A conversation is listed only when all of these hold:
//   - the visitor participates in it (GetConversationsForPrincipal);
//   - it is a native conversation whose key is the canonical DM key of an
//     agent and this visitor (visitorDMAgentID), so another principal's DM
//     is never listed;
//   - the agent exists, is not deleted, and the visitor may message it now
//     (authorizeAgentMessage), the same check that admits the DM read
//     routes, so every listed DM is one the visitor can read.
//
// Only a project visitor is answered; any other identity gets 404 Not
// Found, and a visitor while the federation.project_visitors experiment
// is off gets the uniform refusal.
// routeGuard admits a visitor on GET only.
func (s *Server) handleUserMeVisitorDMs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	visitor, ok := GetIdentityFromContext(ctx).(*VisitorIdentity)
	if !ok || visitor == nil {
		NotFound(w, "Route")
		return
	}
	if !s.experimentEnabled(experiments.FederationProjectVisitors) || r.Method != http.MethodGet {
		writeVisitorRefusal(w)
		return
	}
	convs, err := s.store.GetConversationsForPrincipal(ctx, "user", visitor.ID())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	dms := []VisitorOwnDM{}
	projectSlugs := map[string]string{}
	for i := range convs {
		conv := &convs[i]
		if conv.Surface != "native" || conv.DeletedAt != nil {
			continue
		}
		agentID, ok := visitorDMAgentID(conv.ExternalRef, visitor.ID())
		if !ok {
			continue
		}
		agent, err := s.store.GetAgent(ctx, agentID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if !agent.DeletedAt.IsZero() {
			continue
		}
		if allowed, _, _ := s.authorizeAgentMessage(ctx, visitor, agent, false); !allowed {
			continue
		}
		slug, ok := projectSlugs[agent.ProjectID]
		if !ok {
			project, err := s.store.GetProject(ctx, agent.ProjectID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				writeErrorFromErr(w, err, "")
				return
			}
			if project != nil && err == nil {
				slug = project.Slug
			}
			projectSlugs[agent.ProjectID] = slug
		}
		dms = append(dms, VisitorOwnDM{
			ConversationID:  conv.ID,
			ConversationKey: conv.ExternalRef,
			ProjectSlug:     slug,
			AgentSlug:       agent.Slug,
			LastActivityAt:  conv.LastActivityAt,
		})
	}
	sort.SliceStable(dms, func(i, j int) bool { return dms[i].LastActivityAt.After(dms[j].LastActivityAt) })
	writeJSON(w, http.StatusOK, ListVisitorOwnDMsResponse{DMs: dms})
}
