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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// chatMemberProjectIDs returns the IDs of the projects userID is an explicit
// member of, for chat's unread state: the badge, the spaces rollup and the
// thread list's unread dots count only these projects.
//
// For every project p, chatMemberProjectIDs(ctx, u)[p] equals
// CheckEffectiveMembership(ctx, u, p).IsMember, computed for all projects at
// once: an active project-scoped role binding held directly, or through an
// effective group (a group-bound owner role confers nothing). Hub-admin
// status, public visibility and generic read grants are not membership, so
// an admin's unread state covers only the projects they belong to, not all
// the projects they can read.
//
// As in CheckEffectiveMembership, a project with an active binding whose
// role definition is missing is not a member: that check fails for the
// project, and chat treats a failed check as no membership. Other store
// errors are returned.
func (s *Server) chatMemberProjectIDs(ctx context.Context, userID string) (map[string]bool, error) {
	now := time.Now()
	members := make(map[string]bool)
	// Projects whose check CheckEffectiveMembership would fail: a binding
	// scoped to them names a missing role definition.
	failed := make(map[string]bool)
	roleNames := make(map[string]string)
	roleMissing := make(map[string]bool)
	// roleName resolves a role definition's name, reporting whether the
	// definition exists.
	roleName := func(id string) (string, bool, error) {
		if roleMissing[id] {
			return "", false, nil
		}
		if name, ok := roleNames[id]; ok {
			return name, true, nil
		}
		rd, err := s.store.GetRoleDefinition(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && rd == nil) {
			roleMissing[id] = true
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("get role definition %s: %w", id, err)
		}
		roleNames[id] = rd.Name
		return rd.Name, true, nil
	}

	direct, err := s.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return nil, fmt.Errorf("list direct bindings: %w", err)
	}
	for _, rb := range direct {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID == "" || !isBindingActive(rb, now) {
			continue
		}
		_, ok, err := roleName(rb.RoleDefinitionID)
		if err != nil {
			return nil, err
		}
		if !ok {
			failed[rb.ScopeID] = true
			continue
		}
		members[rb.ScopeID] = true
	}

	groupIDs, err := s.store.GetEffectiveGroups(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get effective groups: %w", err)
	}
	if len(groupIDs) > 0 {
		principals := make([]store.PrincipalRef, 0, len(groupIDs))
		for _, gid := range groupIDs {
			principals = append(principals, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: gid})
		}
		bindings, err := s.store.ListRoleBindingsForPrincipals(ctx, principals, []string{store.RoleScopeProject}, nil)
		if err != nil {
			return nil, fmt.Errorf("list group bindings: %w", err)
		}
		for _, rb := range bindings {
			if rb.ScopeType != store.RoleScopeProject || rb.ScopeID == "" || !isBindingActive(rb, now) {
				continue
			}
			name, ok, err := roleName(rb.RoleDefinitionID)
			if err != nil {
				return nil, err
			}
			if !ok {
				failed[rb.ScopeID] = true
				continue
			}
			// Groups never confer owner; such a binding is ignored.
			if name == store.ProjectRoleOwner {
				continue
			}
			members[rb.ScopeID] = true
		}
	}

	for id := range failed {
		delete(members, id)
	}
	return members, nil
}

// chatDeletedAgentPeers returns the agent peers of dms that are deleted:
// soft-deleted, or with no agent row at all. A DM with a deleted agent is
// not counted on the unread badge nor listed as unread in the rail; its
// data is kept. A failed lookup is returned as an error.
func (s *Server) chatDeletedAgentPeers(ctx context.Context, dms []WebChatDM) (map[string]bool, error) {
	var ids []string
	for _, dm := range dms {
		if dm.PeerKind == "agent" && dm.PeerID != "" {
			ids = append(ids, dm.PeerID)
		}
	}
	out := make(map[string]bool)
	if len(ids) == 0 {
		return out, nil
	}
	agents, err := s.store.GetAgentsByIDsIncludingDeleted(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("agent peers: %w", err)
	}
	for _, id := range ids {
		if agentPeerDeleted(agents[id]) {
			out[id] = true
		}
	}
	return out, nil
}

// agentPeerDeleted reports whether a DM's agent peer, as looked up including
// soft-deleted agents, is deleted: absent, or soft-deleted.
func agentPeerDeleted(a *store.Agent) bool {
	return a == nil || !a.DeletedAt.IsZero()
}
