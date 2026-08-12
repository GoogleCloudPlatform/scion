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

	// 2. Seed hub-member-read-all policy
	seedPolicy(ctx, s, group.ID, &store.Policy{
		ID:           api.NewUUID(),
		Name:         "hub-member-read-all",
		Description:  "Allow hub members to read all resources",
		ScopeType:    "hub",
		ScopeID:      "",
		ResourceType: "*",
		Actions:      []string{"read", "list"},
		Effect:       "allow",
	})

	// 3. Seed hub-member-create-projects policy
	seedPolicy(ctx, s, group.ID, &store.Policy{
		ID:           api.NewUUID(),
		Name:         "hub-member-create-projects",
		Description:  "Allow hub members to create projects",
		ScopeType:    "hub",
		ScopeID:      "",
		ResourceType: "project",
		Actions:      []string{"create"},
		Effect:       "allow",
	})

	// The human half of the svc-accnt service-account assign baseline is
	// deliberately NOT seeded here. It is a PROJECT-scoped policy created per
	// project by createProjectMembersGroupAndPolicy, and backfilled onto
	// existing projects by backfillProjectAssignPolicies below. See
	// projectAssignPolicyName for why hub scope was rejected.
}

// projectAssignPolicyName returns the name of a project's service-account
// assign policy.
//
// ⚠️ Policy names are NOT unique hub-wide — there is no such constraint; see
// ensureProjectAssignPolicy. Idempotency here comes from that function looking
// the name up before creating, and from this being the only code that writes
// this name. It is a convention this package keeps, not an invariant the store
// enforces.
//
// ── Why this policy exists ────────────────────────────────────────────────
//
// Assigning a service account to an agent is gated today on ActionRead, which
// the hub-wide hub-member-read-all policy already allows for every hub member.
// svc-accnt P3 converts that gate to ActionAssign so the GCP actAs check has a
// resource-shaped place to hang. Without an assign grant somewhere, that
// conversion would deny every caller who is neither the account's creator nor
// a project owner or admin, since no other seeded policy grants assign. The
// security in that change comes from the actAs check, not from narrowing the
// Hub policy layer — a conversion that denies someone who can assign today is
// a regression, not a hardening.
//
// The population it must reproduce is exactly "members of this project":
//   - seed.go's hub-member-read-all grants read+list on "*" hub-wide, which is
//     what makes assign reachable for plain members today via the ActionRead
//     gate; and
//   - createProjectMembersGroupAndPolicy binds agent create as a PROJECT-scoped
//     policy to the members group, so only members of project P can create an
//     agent in P in the first place.
//
// Effective population today == project members. This policy reproduces it.
//
// ── Why project scope and not hub scope ───────────────────────────────────
//
// A hub-scoped assign policy on gcp_service_account cannot exclude parentless
// resources, so it would grant assign on every HUB-scoped service account to
// every hub member — which is the Goal 2 coupling arriving early, through the
// policy layer, before it has been ruled on.
//
// Project scope dissolves that structurally rather than managing it:
// matchesResource rejects a project-scoped policy against a resource that
// resolves to no project (pid == "" || pid != policy.ScopeID, fail closed
// rather than fall through — #595). gcpServiceAccountResource gives a project
// parent only to project-scoped accounts, so a hub-scoped account yields
// pid == "" and this policy CANNOT match it. No code-side guard is needed, and
// none should be added: a policy that presents as applied but is revoked
// elsewhere is the shape this change exists to avoid.
//
// ⚠️ WHAT CONFINES THIS POLICY is matchesResource alone: it refuses to match a
// project-scoped policy against a resource that resolves to no project (#595).
// That is a property of the authorization engine. It holds no matter what any
// handler does, and it is the only thing that needs to be true.
//
// Do NOT justify this policy by the scope check in createAgentInProject. An
// earlier version of this comment named `sa.ScopeID != projectID` there as an
// enforcing mechanism; a44b2950 replaced it with sa.ReachableFromProject,
// which admits hub-scoped accounts from every project. The justification went
// stale while the policy stayed correct for a reason the comment had not
// recorded. A confinement argument that names a call site can be invalidated
// by a commit to that call site, silently. Name engine properties only.
//
// The agent-side arm (step 3b of checkAccessForAgent) is confined by the SAME
// engine property read the other way: that arm by `pid != ""`, this policy by
// `pid == ""` in matchesResource. One discipline in two places, not two
// unrelated accidents.
//
// Goal 2 makes hub-scoped accounts assignable across projects. That does not
// breach this policy — a hub-scoped account stays parentless, so this policy
// still cannot match it, which is the fail-closed outcome §8.2 rules correct:
// hub-scoped accounts are assignable by hub admins and the account's creator
// and nobody else. If you are here to make hub-scoped accounts broadly
// assignable, that is task #19, and doing it by adding a hub-scoped assign
// policy would grant every hub member every service account on the hub.
//
// ⚠️ #19 CONSTRAINT: whatever implements that toggle must NOT do it by
// deleting or editing a grant by name. CreatePolicy enforces no name
// uniqueness, so a name identifies a SET of rows, and ListPolicies(Name:X,
// Limit:1) returns an arbitrary element of it. Additive operations are safe;
// revocation by name is not.
//
// ── Scope of the "preserves existing reach" claim ─────────────────────────
//
// It restores the reach of hub-member-read-all for project members and nothing
// else. The action-insensitive bypasses — hub admin, resource owner, project
// owner/admin — never consulted the action and are unaffected. A hand-authored
// policy granting read on service accounts to some other group is deliberately
// NOT mirrored, because a grant to read a service account is not a grant to
// assign one. Such an operator loses assign for that group and must grant it
// explicitly. Do not describe this policy as "reachability-preserving" without
// that qualification.
func projectAssignPolicyName(slug string) string {
	return "project:" + slug + ":member-assign-service-accounts"
}

// ensureProjectAssignPolicy creates the project's service-account assign
// policy and binds it to the project's members group. It is idempotent, and an
// existing policy has its ScopeID repaired in case the project was recreated.
// Best-effort; failures are logged.
//
// ResourceType is the single literal "gcp_service_account" and not "*".
// hub-member-read-all uses the wildcard because read is broadly safe; assign
// is not, so this mirrors that policy's reach without inheriting its breadth.
// Actions is assign alone for the same reason.
//
// ⚠️ Idempotency is by name LOOKUP, the way seedPolicy does it — deliberately
// not by creating and catching store.ErrAlreadyExists. CreatePolicy does not
// enforce name uniqueness, so that error never arrives and the catch would be
// dead code: three project touches would produce three identical policies.
// This is a live defect on the neighbouring project:<slug>:member-create-agents
// policy above, which does use the catch pattern. Do not "simplify" this back
// to create-then-catch, and do not assume a uniqueness constraint exists.
func ensureProjectAssignPolicy(ctx context.Context, s store.Store, project *store.Project, groupID string) {
	name := projectAssignPolicyName(project.Slug)

	existing, err := s.ListPolicies(ctx, store.PolicyFilter{Name: name}, store.ListOptions{Limit: 1})
	if err != nil {
		slog.Warn("failed to check for existing project assign policy",
			"project_id", project.ID, "policy", name, "error", err.Error())
		return
	}

	var policy *store.Policy
	if len(existing.Items) > 0 {
		policy = &existing.Items[0]
		if policy.ScopeID != project.ID {
			policy.ScopeID = project.ID
			if updateErr := s.UpdatePolicy(ctx, policy); updateErr != nil {
				slog.Warn("failed to update existing project assign policy",
					"project_id", project.ID, "policy", name, "error", updateErr.Error())
			}
		}
	} else {
		policy = &store.Policy{
			ID:           api.NewUUID(),
			Name:         name,
			Description:  "Allow project members to assign GCP service accounts in this project",
			ScopeType:    "project",
			ScopeID:      project.ID,
			ResourceType: "gcp_service_account",
			Actions:      []string{"assign"},
			Effect:       "allow",
		}
		if createErr := s.CreatePolicy(ctx, policy); createErr != nil {
			slog.Warn("failed to create project assign policy",
				"project_id", project.ID, "policy", name, "error", createErr.Error())
			return
		}
		slog.Info("created project assign policy", "project_id", project.ID, "policy", name, "id", policy.ID)
	}

	if err := s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      policy.ID,
		PrincipalType: "group",
		PrincipalID:   groupID,
	}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Warn("failed to bind project assign policy",
			"project_id", project.ID, "policy", name, "error", err.Error())
	}
}

// backfillProjectAssignPolicies gives every existing project the assign policy
// described on projectAssignPolicyName. It runs once at startup and is
// idempotent by policy name, like seedPolicy.
//
// It is not redundant with createProjectMembersGroupAndPolicy, which already
// runs on several project touch paths. Most projects would pick the policy up
// on their next touch — but a project nobody touches never would, and its
// members would silently lose the ability to assign service accounts the
// moment the ActionAssign conversion lands. Without this, the conversion is
// not reachability-preserving on existing hubs. Do not remove it on the
// grounds that the create path covers it; the create path covers active
// projects only.
//
// A project with no members group is skipped: there is nothing to bind to, and
// createProjectMembersGroupAndPolicy will create both together on next touch.
func backfillProjectAssignPolicies(ctx context.Context, s store.Store) {
	res, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{})
	if err != nil {
		slog.Warn("failed to list projects for assign-policy backfill", "error", err)
		return
	}
	for i := range res.Items {
		project := &res.Items[i]
		group, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
		if err != nil {
			slog.Debug("skipping assign-policy backfill, no members group",
				"project_id", project.ID, "slug", project.Slug)
			continue
		}
		ensureProjectAssignPolicy(ctx, s, project, group.ID)
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
func seedDevUser(ctx context.Context, s store.Store, cfg DevUserConfig) {
	u := NewDevUser(cfg)
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
		Email:       u.Email(),
		DisplayName: u.DisplayName(),
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
