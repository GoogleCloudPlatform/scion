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
	ProjectID     string `json:"projectId,omitempty"`
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

// handleMyGroups handles GET /api/v1/users/me/groups.
func (s *Server) handleMyGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	ctx := r.Context()

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

	if groups == nil {
		groups = []store.Group{}
	}

	writeJSON(w, http.StatusOK, GroupsResponse{Groups: groups})
}

// handleAgentGroups handles GET /api/v1/agents/{id}/groups.
func (s *Server) handleAgentGroups(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
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

	if groups == nil {
		groups = []store.Group{}
	}

	writeJSON(w, http.StatusOK, GroupsResponse{Groups: groups})
}

// handlePrincipalRoutes handles GET /api/v1/principals/{principalType}/{principalId}.
func (s *Server) handlePrincipalRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
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
		if effectiveGroupIDs == nil {
			effectiveGroupIDs = []string{}
		}

		writeJSON(w, http.StatusOK, PrincipalResolutionResponse{
			Principal: PrincipalInfo{
				Type:        "user",
				ID:          u.ID,
				DisplayName: u.DisplayName,
			},
			DirectGroups:    directGroupIDs,
			EffectiveGroups: effectiveGroupIDs,
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
		if effectiveGroupIDs == nil {
			effectiveGroupIDs = []string{}
		}

		resp := PrincipalResolutionResponse{
			Principal: PrincipalInfo{
				Type:        "agent",
				ID:          a.ID,
				DisplayName: a.Name,
				ProjectID:     a.ProjectID,
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
		g, err := s.store.GetGroup(ctx, principalID)
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
