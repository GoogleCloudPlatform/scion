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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// targetResolveResponse is the response for GET /api/v1/messaging/targets/resolve.
type targetResolveResponse struct {
	Agent          *targetAgentInfo    `json:"agent"`
	Messageability *messageabilityInfo `json:"messageability"`
}

// targetAgentInfo contains minimal identity for a resolved target.
type targetAgentInfo struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	ProjectID   string `json:"projectId"`
	ProjectSlug string `json:"projectSlug"`
}

// messageabilityInfo contains directional reachability for a resolved target.
type messageabilityInfo struct {
	CanMessage     bool   `json:"canMessage"`
	CanReachViewer bool   `json:"canReachViewer"`
	ReplyReason    string `json:"replyReason,omitempty"`
}

// handleMessagingTargetsResolve handles GET /api/v1/messaging/targets/resolve.
// Read-only exact target lookup using messaging authorization.
// Returns minimal identity and directional reachability.
// Privacy-preserving: deny/nonexistent responses are indistinguishable.
func (s *Server) handleMessagingTargetsResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return
	}

	// Check Hub feature enabled first, before target lookup.
	ops := s.GetOperationalSettings()
	if ops == nil || !ops.CrossProjectMessagingEnabled() {
		writeError(w, http.StatusNotFound, "cross_project_disabled",
			"Cross-project messaging is not enabled", nil)
		return
	}

	ctx := r.Context()
	q := r.URL.Query()
	projectRef := q.Get("project")
	agentRef := q.Get("agent")

	if projectRef == "" || agentRef == "" {
		BadRequest(w, "Both 'project' and 'agent' query parameters are required")
		return
	}

	// Resolve the target project by ID or slug.
	var targetProject *store.Project
	targetProject, err := s.store.GetProject(ctx, projectRef)
	if err != nil {
		// Try by slug using the dedicated store method.
		targetProject, err = s.store.GetProjectBySlug(ctx, projectRef)
		if err != nil {
			targetProject = nil
		}
	}
	if targetProject == nil {
		// Privacy-preserving: indistinguishable from nonexistent.
		NotFound(w, "Target")
		return
	}

	// Resolve the agent within the target project by ID or slug.
	var targetAgent *store.Agent
	agentResult, err := s.store.ListAgents(ctx, store.AgentFilter{
		ProjectID: targetProject.ID,
	}, store.ListOptions{})
	if err != nil {
		NotFound(w, "Target")
		return
	}

	for i := range agentResult.Items {
		if agentResult.Items[i].ID == agentRef || agentResult.Items[i].Slug == agentRef {
			targetAgent = &agentResult.Items[i]
			break
		}
	}
	if targetAgent == nil {
		// Privacy-preserving: indistinguishable from nonexistent.
		NotFound(w, "Target")
		return
	}

	// Evaluate directional reachability.
	canMessage := false
	canReachViewer := false
	replyReason := ""

	// Forward direction: caller → target
	forwardAllowed, _, _ := s.authorizeAgentMessage(ctx, identity, targetAgent, false)
	canMessage = forwardAllowed

	// Reverse direction: target → caller (for reply capability).
	// Only evaluate if the caller is an agent.
	if agentIdent, ok := identity.(AgentIdentity); ok {
		callerAgent, err := s.store.GetAgent(ctx, agentIdent.ID())
		if err == nil && callerAgent != nil {
			reverseAllowed, reverseReason, _ := s.authorizeAgentMessage(ctx,
				&peerAgentIdentity{agent: targetAgent}, callerAgent, false)
			canReachViewer = reverseAllowed
			if !reverseAllowed {
				replyReason = reverseReason
			}
		}
	}

	// Privacy-preserving: if the caller cannot message the target in either
	// direction, return the same NotFound as for nonexistent targets so that
	// existence cannot be distinguished from non-existence.
	if !canMessage && !canReachViewer {
		NotFound(w, "Target")
		return
	}

	resp := targetResolveResponse{
		Agent: &targetAgentInfo{
			ID:          targetAgent.ID,
			Slug:        targetAgent.Slug,
			ProjectID:   targetProject.ID,
			ProjectSlug: targetProject.Slug,
		},
		Messageability: &messageabilityInfo{
			CanMessage:     canMessage,
			CanReachViewer: canReachViewer,
			ReplyReason:    replyReason,
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

// peerAgentIdentity wraps a store.Agent as an AgentIdentity for
// authorization checks where the peer agent is not the authenticated caller.
type peerAgentIdentity struct {
	agent *store.Agent
}

func (w *peerAgentIdentity) Type() string             { return "agent" }
func (w *peerAgentIdentity) ID() string               { return w.agent.ID }
func (w *peerAgentIdentity) ProjectID() string         { return w.agent.ProjectID }
func (w *peerAgentIdentity) Scopes() []AgentTokenScope { return nil }
func (w *peerAgentIdentity) HasScope(_ AgentTokenScope) bool { return false }
func (w *peerAgentIdentity) Ancestry() []string {
	return w.agent.Ancestry
}
func (w *peerAgentIdentity) OriginUserID() string {
	if len(w.agent.Ancestry) > 0 {
		return w.agent.Ancestry[0]
	}
	return ""
}
func (w *peerAgentIdentity) TokenID() string { return "" }
