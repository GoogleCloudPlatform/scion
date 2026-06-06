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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// seedDefaultPoliciesAndGroups creates the default hub-members group and
// associated policies if they don't already exist. This is called once
// during Hub initialization and is idempotent.
func seedDefaultPoliciesAndGroups(ctx context.Context, s store.Store) {
	// 1. Create hub-members group (skip if already exists)
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("failed to check for hub-members group", "error", err)
			return
		}
		group = &store.Group{
			ID:        api.NewUUID(),
			Name:      "Hub Members",
			Slug:      "hub-members",
			GroupType: store.GroupTypeExplicit,
		}
		if err := s.CreateGroup(ctx, group); err != nil {
			slog.Warn("failed to create hub-members group", "error", err)
			return
		}
		slog.Info("seeded hub-members group", "id", group.ID)
	}

	// 2. Migrate: remove legacy hub-member-read-all wildcard policy if present.
	// This policy granted read/list on ResourceType:"*" which is too broad —
	// project, agent, broker visibility is now membership-gated.
	retirePolicy(ctx, s, group.ID, "hub-member-read-all")

	// 3. Seed explicit per-type read/list policies for hub-readable types only.
	// GATE (membership-derived, NOT seeded here): project, agent, broker.
	// SENSITIVE (NOT seeded here): policy, gcp_service_account, secret, environment, variable.
	for _, rt := range []struct{ name, desc, resourceType string }{
		{"hub-member-read-user", "Allow hub members to read users", "user"},
		{"hub-member-read-group", "Allow hub members to read groups", "group"},
		{"hub-member-read-template", "Allow hub members to read templates", "template"},
		{"hub-member-read-harness-config", "Allow hub members to read harness configs", "harness_config"},
	} {
		seedPolicy(ctx, s, group.ID, &store.Policy{
			ID:           api.NewUUID(),
			Name:         rt.name,
			Description:  rt.desc,
			ScopeType:    "hub",
			ResourceType: rt.resourceType,
			Actions:      []string{"read", "list"},
			Effect:       "allow",
		})
	}

	// 4. Seed hub-member-create-projects policy
	seedPolicy(ctx, s, group.ID, &store.Policy{
		ID:           api.NewUUID(),
		Name:         "hub-member-create-projects",
		Description:  "Allow hub members to create projects",
		ScopeType:    "hub",
		ResourceType: "project",
		Actions:      []string{"create"},
		Effect:       "allow",
	})
}

// retirePolicy removes a policy by name and its binding to the given group.
// No-op if the policy does not exist.
func retirePolicy(ctx context.Context, s store.Store, groupID, policyName string) {
	existing, err := s.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
	if err != nil || existing.TotalCount == 0 {
		return
	}
	p := existing.Items[0]
	if err := s.RemovePolicyBinding(ctx, p.ID, "group", groupID); err != nil {
		slog.Warn("failed to remove binding for retired policy", "name", policyName, "error", err)
	}
	if err := s.DeletePolicy(ctx, p.ID); err != nil {
		slog.Warn("failed to delete retired policy", "name", policyName, "error", err)
	} else {
		slog.Info("retired legacy policy", "name", policyName, "id", p.ID)
	}
}

// seedPolicy creates a policy and binds it to the given group, skipping
// if a policy with the same name already exists.
func seedPolicy(ctx context.Context, s store.Store, groupID string, policy *store.Policy) {
	// Check if policy already exists by name
	existing, err := s.ListPolicies(ctx, store.PolicyFilter{Name: policy.Name}, store.ListOptions{Limit: 1})
	if err != nil {
		slog.Warn("failed to check for existing policy", "name", policy.Name, "error", err)
		return
	}
	if existing.TotalCount > 0 {
		return
	}

	if err := s.CreatePolicy(ctx, policy); err != nil {
		slog.Warn("failed to create seed policy", "name", policy.Name, "error", err)
		return
	}
	slog.Info("seeded policy", "name", policy.Name, "id", policy.ID)

	// Bind policy to the group
	binding := &store.PolicyBinding{
		PolicyID:      policy.ID,
		PrincipalType: "group",
		PrincipalID:   groupID,
	}
	if err := s.AddPolicyBinding(ctx, binding); err != nil {
		slog.Warn("failed to bind seed policy to hub-members group",
			"policy", policy.Name, "error", err)
	}
}

// seedDevUser ensures the development pseudo-user exists in the store.
// This is needed because Ent enforces foreign key constraints on owner_id,
// and the dev user must exist as a User record for project group creation to
// succeed in workstation/dev-auth mode.
func seedDevUser(ctx context.Context, s store.Store) {
	_, err := s.GetUser(ctx, DevUserID)
	if err == nil {
		return // already exists
	}
	if !errors.Is(err, store.ErrNotFound) {
		slog.Warn("failed to check for dev user", "error", err)
		return
	}
	if err := s.CreateUser(ctx, &store.User{
		ID:          DevUserID,
		Email:       "dev@localhost",
		DisplayName: "Development User",
		Role:        "admin",
		Status:      "active",
	}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Warn("failed to seed dev user", "error", err)
	}
}

// ensureHubMembership adds the given user to the hub-members group.
// This is best-effort; errors are logged at debug level and ignored.
func ensureHubMembership(ctx context.Context, s store.Store, userID string) {
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	if err != nil {
		slog.Debug("hub-members group not found, skipping membership", "error", err)
		return
	}

	err = s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleMember,
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Debug("failed to add user to hub-members group", "userID", userID, "error", err)
	}
}
