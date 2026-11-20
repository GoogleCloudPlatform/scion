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
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ============================================================================
// Principal Query Endpoints (Phase 4)
// ============================================================================

// PrincipalInfo describes a resolved principal identity.
type PrincipalInfo struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	ProjectID   string `json:"projectId,omitempty"`
}

// PrincipalResolutionResponse is the response for principal resolution.
type PrincipalResolutionResponse struct {
	Principal       PrincipalInfo  `json:"principal"`
	DirectGroups    []string       `json:"directGroups"`
	EffectiveGroups []string       `json:"effectiveGroups"`
	DelegatesFrom   *PrincipalInfo `json:"delegatesFrom,omitempty"`
}

// GroupsResponse is the response for group listing endpoints.
type GroupsResponse struct {
	Groups []store.Group `json:"groups"`
}

// handleMyGroups handles GET /api/v1/users/me/groups. A project-boundary
// user access token lists the caller's groups that belong to no project and
// the groups of its boundary project, the latter only while the caller is a
// current member of that project. Only the credentials the
// user.self.groups catalog operation declares are admitted.
func (s *Server) handleMyGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	ctx := r.Context()
	token, ok := selfReadCredential(ctx, user, "user.self.groups")
	if !ok {
		refuseSelfReadCredential(w, r, user, "group")
		return
	}

	groupIDs, err := s.store.GetEffectiveGroups(ctx, user.ID())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	groups, err := s.store.GetGroupsByIDs(ctx, groupIDs)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if token != nil && token.Boundary().Kind == BoundaryKindProject {
		groups = s.boundaryProjectGroups(ctx, token, groups)
	}

	groups, err = s.filterVisibleGroups(ctx, user, groups)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, GroupsResponse{Groups: groups})
}

// boundaryProjectGroups keeps the groups a project-boundary token may list:
// groups that belong to no project, and groups of the token's boundary
// project while its holder is a current member of that project.
func (s *Server) boundaryProjectGroups(ctx context.Context, token *ScopedUserIdentity, groups []store.Group) []store.Group {
	boundaryProject := token.Boundary().ProjectID
	memberChecked, member := false, false
	out := make([]store.Group, 0, len(groups))
	for _, g := range groups {
		if g.ProjectID == "" {
			out = append(out, g)
			continue
		}
		if g.ProjectID != boundaryProject {
			continue
		}
		if !memberChecked {
			member = s.tokenBoundaryProjectMember(ctx, token)
			memberChecked = true
		}
		if member {
			out = append(out, g)
		}
	}
	return out
}

// handleAgentGroups handles GET /api/v1/agents/{id}/groups.
func (s *Server) handleAgentGroups(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()

	// Verify the agent exists
	_, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	groupIDs, err := s.store.GetEffectiveGroupsForAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	groups, err := s.store.GetGroupsByIDs(ctx, groupIDs)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	groups, err = s.filterVisibleGroups(ctx, GetIdentityFromContext(ctx), groups)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, GroupsResponse{Groups: groups})
}

// handlePrincipalRoutes handles GET /api/v1/principals/{principalType}/{principalId}.
func (s *Server) handlePrincipalRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	// Parse /api/v1/principals/{principalType}/{principalId}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/principals/")
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		BadRequest(w, "Path must be /api/v1/principals/{principalType}/{principalId}")
		return
	}

	principalType := parts[0]
	principalID := parts[1]

	ctx := r.Context()

	switch principalType {
	case "user":
		u, err := s.store.GetUser(ctx, principalID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		directMemberships, err := s.store.GetUserGroups(ctx, principalID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		directGroupIDs := make([]string, 0, len(directMemberships))
		for _, m := range directMemberships {
			directGroupIDs = append(directGroupIDs, m.GroupID)
		}

		effectiveGroupIDs, err := s.store.GetEffectiveGroups(ctx, principalID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Filter both direct and effective group IDs through the central
		// group visibility decision in a single batch lookup.
		allIDs := make([]string, 0, len(directGroupIDs)+len(effectiveGroupIDs))
		seenIDs := make(map[string]struct{}, len(directGroupIDs)+len(effectiveGroupIDs))
		for _, gid := range directGroupIDs {
			if _, ok := seenIDs[gid]; !ok {
				seenIDs[gid] = struct{}{}
				allIDs = append(allIDs, gid)
			}
		}
		for _, gid := range effectiveGroupIDs {
			if _, ok := seenIDs[gid]; !ok {
				seenIDs[gid] = struct{}{}
				allIDs = append(allIDs, gid)
			}
		}
		visibleIDs, err := s.filterVisibleGroupIDs(ctx, GetIdentityFromContext(ctx), allIDs)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		visibleSet := make(map[string]struct{}, len(visibleIDs))
		for _, gid := range visibleIDs {
			visibleSet[gid] = struct{}{}
		}
		filteredDirect := make([]string, 0, len(directGroupIDs))
		for _, gid := range directGroupIDs {
			if _, ok := visibleSet[gid]; ok {
				filteredDirect = append(filteredDirect, gid)
			}
		}
		filteredEffective := make([]string, 0, len(effectiveGroupIDs))
		for _, gid := range effectiveGroupIDs {
			if _, ok := visibleSet[gid]; ok {
				filteredEffective = append(filteredEffective, gid)
			}
		}

		writeJSON(w, http.StatusOK, PrincipalResolutionResponse{
			Principal: PrincipalInfo{
				Type:        "user",
				ID:          u.ID,
				DisplayName: u.DisplayName,
			},
			DirectGroups:    filteredDirect,
			EffectiveGroups: filteredEffective,
		})

	case "agent":
		a, err := s.store.GetAgent(ctx, principalID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		effectiveGroupIDs, err := s.store.GetEffectiveGroupsForAgent(ctx, principalID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		effectiveGroupIDs, err = s.filterVisibleGroupIDs(ctx, GetIdentityFromContext(ctx), effectiveGroupIDs)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		resp := PrincipalResolutionResponse{
			Principal: PrincipalInfo{
				Type:        "agent",
				ID:          a.ID,
				DisplayName: a.Name,
				ProjectID:   a.ProjectID,
			},
			DirectGroups:    effectiveGroupIDs,
			EffectiveGroups: effectiveGroupIDs,
		}

		// Include delegation info if creator is set
		if a.CreatedBy != "" {
			creator, err := s.store.GetUser(ctx, a.CreatedBy)
			if err == nil {
				resp.DelegatesFrom = &PrincipalInfo{
					Type:        "user",
					ID:          creator.ID,
					DisplayName: creator.DisplayName,
				}
			}
		}

		writeJSON(w, http.StatusOK, resp)

	case "group":
		g, err := s.resolveVisibleGroupByID(ctx, GetIdentityFromContext(ctx), principalID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		writeJSON(w, http.StatusOK, PrincipalResolutionResponse{
			Principal: PrincipalInfo{
				Type:        "group",
				ID:          g.ID,
				DisplayName: g.Name,
			},
			DirectGroups:    []string{},
			EffectiveGroups: []string{},
		})

	default:
		BadRequest(w, "Invalid principal type: must be user, agent, or group")
	}
}
