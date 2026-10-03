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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// =============================================================================
// Built-in Role Definitions — Curated, Versioned Permission Lists (PG1)
// =============================================================================
//
// Each built-in role declares an explicit set of canonical permission IDs and a
// code-declared revision number. A new registry permission does NOT
// automatically enter any built-in role — the permission must be added to the
// list here and the revision bumped.
//
// Reconciliation at startup compares the code revision with the last-applied
// revision stored as a hub setting. If the code revision is higher, the role
// definition's permissions are updated to match the code-declared set.

// BuiltInRole declares the code-authoritative definition of a system role.
type BuiltInRole struct {
	Name        string
	Description string
	ScopeType   string
	Revision    int
	Permissions []string
}

// builtInRoleRevisionKey returns the hub-setting key used to track the last
// reconciled revision for a built-in role.
func builtInRoleRevisionKey(roleName string) string {
	return "builtin_role.revision." + roleName
}

// builtInRoleMarker stores both the code-declared revision and a hash of
// the resolved permission list. This ensures reconciliation fires when
// either the revision is bumped OR the dynamic permission list changes
// (e.g., a new permission is added to the registry, expanding the
// super-admin set).
type builtInRoleMarker struct {
	Revision int    `json:"revision"`
	PermHash string `json:"permHash"`
}

// permListHash returns a truncated SHA-256 hex digest of the sorted
// permission list. The sort ensures determinism regardless of input order.
func permListHash(perms []string) string {
	sorted := make([]string, len(perms))
	copy(sorted, perms)
	sort.Strings(sorted)
	h := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(h[:8]) // 16 hex chars — enough for change detection
}

// BuiltInRoles returns the authoritative list of built-in role definitions.
// Every permission ID must exist in the canonical permissions registry.
//
// Ordering: permissions are listed alphabetically within each role for
// readability and deterministic comparison.
func BuiltInRoles() []BuiltInRole {
	return []BuiltInRole{
		// ── System-scoped roles ──────────────────────────────────────────

		{
			Name:        store.SystemRoleSuperAdmin,
			Description: "Full platform administrator with all permissions",
			ScopeType:   store.RoleScopeSystem,
			Revision:    1,
			Permissions: allPermissionIDs(),
		},
		{
			Name:        store.SystemRoleHubAdmin,
			Description: "Hub administrator with scopeable admin permissions",
			ScopeType:   store.RoleScopeSystem,
			Revision:    4,
			Permissions: hubAdminPermissionIDs(),
		},
		{
			// global-catalog-author: Phase 1 (ptone/scion#1713) grants only
			// skill.create_global. Phase 2 extends this to update/delete for
			// skills and templates. Seeded as a role *definition only* — no
			// binding is created. An operator grants it deliberately.
			// See design doc §3.1 (roles).
			Name:        store.SystemRoleGlobalCatalogAuthor,
			Description: "Creates global (hub-scoped) skills",
			ScopeType:   store.RoleScopeSystem,
			Revision:    1,
			Permissions: globalCatalogAuthorPermissionIDs(),
		},
		{
			// hub-member: curated read permissions for directory/catalog resources.
			// CRITICAL: Does NOT include project.list, project.read, agent.list,
			// agent.read at system scope. Those are handled by project-scoped
			// role bindings to prevent cross-project visibility.
			// See: TestGolden_CrossProjectVisibilityRegression
			Name:        store.SystemRoleHubMember,
			Description: "Hub member with read access to directory resources and project creation",
			ScopeType:   store.RoleScopeSystem,
			Revision:    3, // R3: add broker.create (ptone/scion#2138) — explicit hub-member grant for broker registration
			Permissions: hubMemberPermissionIDs(),
		},
		{
			// hub-viewer: read-only permissions at system scope, carefully curated.
			// Same exclusions as hub-member (no cross-project visibility).
			Name:        store.SystemRoleHubViewer,
			Description: "Hub viewer with read-only access to directory resources",
			ScopeType:   store.RoleScopeSystem,
			Revision:    2,
			Permissions: hubViewerPermissionIDs(),
		},

		// ── Project-scoped roles ─────────────────────────────────────────

		{
			Name:        store.ProjectRoleOwner,
			Description: "Project owner with full project permissions",
			ScopeType:   store.RoleScopeProject,
			Revision:    5, // R5: agent.port_access for owners and admins; R4: add gcp_service_account.assign (ptone/scion#2147)
			Permissions: projectOwnerPermissionIDs(),
		},
		{
			Name:        store.ProjectRoleAdmin,
			Description: "Project admin with most project permissions (no delete, no set_message_mode)",
			ScopeType:   store.RoleScopeProject,
			Revision:    5, // R5: agent.port_access for owners and admins; R4: add gcp_service_account.assign (ptone/scion#2147)
			Permissions: projectAdminPermissionIDs(),
		},
		{
			Name:        store.ProjectRoleMember,
			Description: "Project member with basic project permissions",
			ScopeType:   store.RoleScopeProject,
			Revision:    4, // R4: add gcp_service_account.assign (ptone/scion#2147)
			Permissions: projectMemberCuratedPermissionIDs(),
		},

		// ── Agent roles (system-scoped, used for agent JWT scope mapping) ─

		{
			Name:        store.AgentRoleDefNone,
			Description: "No agent permissions",
			ScopeType:   store.RoleScopeSystem,
			Revision:    1,
			Permissions: nil,
		},
		{
			Name:        store.AgentRoleDefReadonly,
			Description: "Read-only agent permissions",
			ScopeType:   store.RoleScopeSystem,
			Revision:    1,
			Permissions: agentRolePermissionIDs(AgentRoleReadOnly),
		},
		{
			Name:        store.AgentRoleDefBaseline,
			Description: "Baseline agent permissions",
			ScopeType:   store.RoleScopeSystem,
			Revision:    1,
			Permissions: agentRolePermissionIDs(AgentRoleBaseline),
		},
		{
			Name:        store.AgentRoleDefFull,
			Description: "Full agent permissions",
			ScopeType:   store.RoleScopeSystem,
			Revision:    1,
			Permissions: agentRolePermissionIDs(AgentRoleFull),
		},
	}
}

// hubMemberPermissionIDs returns the curated permission set for the hub-member
// role. This is an explicit list — NOT derived from registry action classes.
//
// SECURITY: project.list, project.read, agent.list, agent.read are intentionally
// EXCLUDED. Including them would grant cross-project admin-view visibility to
// every hub member via the hasAdminView handler pattern.
func hubMemberPermissionIDs() []string {
	return []string{
		// User directory (read-only)
		"user.read",
		"user.list",
		// Group directory (read-only)
		"group.read",
		"group.list",
		// Template catalog (read-only)
		"template.read",
		"template.list",
		// Harness config catalog (read-only)
		"harness_config.read",
		"harness_config.list",
		// Broker catalog (read-only), plus registration (ptone/scion#2138):
		// merely being an authenticated user is not enough to register a
		// broker — it requires this explicit hub-member grant.
		"broker.read",
		"broker.list",
		"broker.create",
		// GCP service account catalog (read-only)
		"gcp_service_account.read",
		"gcp_service_account.list",
		// OBS-5: policy.read and policy.list removed — Policy API returns 410 Gone.
		// Skill catalog (read-only)
		"skill.read",
		"skill.list",
		// Quota definitions (read-only)
		"quota.read",
		// Role definitions (read-only)
		"role.read",
		// S1 fix: role_binding.read REMOVED. Hub members no longer need
		// system-scoped role_binding.read because the project members UI
		// uses the project-scoped /api/v1/projects/{id}/members endpoint,
		// which authorizes via project.read instead. Leaving role_binding.read
		// here let any hub member enumerate all role bindings hub-wide.
		// Hub metadata (read-only)
		"hub.settings.read",
		// Project creation — hub members may create projects
		"project.create",
	}
}

// hubViewerPermissionIDs returns the curated permission set for the hub-viewer
// role. Read-only access to directory/catalog resources at system scope.
//
// Same cross-project exclusions as hub-member: no project.read/list or
// agent.read/list at system scope.
func hubViewerPermissionIDs() []string {
	return []string{
		"user.read",
		"user.list",
		"group.read",
		"group.list",
		"template.read",
		"template.list",
		"harness_config.read",
		"harness_config.list",
		"broker.read",
		"broker.list",
		"gcp_service_account.read",
		"gcp_service_account.list",
		// OBS-5: policy.read and policy.list removed — Policy API returns 410 Gone.
		"skill.read",
		"skill.list",
		"quota.read",
		"role.read",
		// S1 fix: role_binding.read REMOVED — same rationale as hub-member.
		"hub.settings.read",
	}
}

// projectOwnerPermissionIDs returns the curated permission set for the
// project-owner role: all project-scoped permissions. This is an explicit list
// — NOT derived from registry iteration. A new registry permission does NOT
// automatically enter this role; it must be added here and the role revision
// bumped.
func projectOwnerPermissionIDs() []string {
	return []string{
		// Agent lifecycle and operations — human control-plane permissions.
		// Agent-self credential permissions (status_update, log_append,
		// token_refresh, identity_token, port_forward, notify) are excluded:
		// those are intended for agent identities, not human project admins.
		//
		// agent.attach is excluded (R3): agents run with
		// their creator's user-scoped secrets, so a terminal on another
		// member's agent would expose that member's credentials. Owners reach
		// their own agents and progeny via the resource-owner and ancestor
		// relationship grants. agent.lifecycle (start/stop/suspend/restart/
		// restore) is retained so owners keep management oversight of
		// members' agents.
		//
		// agent.port_access is included (R5) so owners and admins can open
		// members' already-exposed ports for oversight. It does not grant
		// terminal, exec or env access (agent.attach), or port registration
		// (hub-level). project-member still does not carry it; grant it to
		// members through a custom role.
		"agent.create",
		"agent.delete",
		"agent.lifecycle",
		"agent.list",
		"agent.message",
		"agent.port_access",
		"agent.read",
		"agent.set_message_mode",
		"agent.stop_all",
		"agent.update",
		// GCP service account management (project-scoped). Lets a project
		// owner assign project-scoped service accounts in the project
		// (ptone/scion#2147). When gcpIamCheckMode is enforce, the immediate
		// creator's IAM actAs grant (iam.serviceAccounts.actAs) is also
		// checked; in the default off mode this permission alone authorizes
		// assignment of project-scoped service accounts.
		"gcp_service_account.assign",
		// Harness config management
		"harness_config.create",
		"harness_config.delete",
		"harness_config.list",
		"harness_config.read",
		"harness_config.update",
		// Project management — project.create, project.clone, and
		// project.register are hub-level operations (enforced against hub
		// scope, not the bound project) and are excluded from project-scoped
		// roles.
		"project.delete",
		"project.list",
		"project.manage",
		"project.read",
		"project.secret_read",
		"project.update",
		// Scheduled event management
		"scheduled_event.create",
		"scheduled_event.delete",
		"scheduled_event.list",
		"scheduled_event.read",
		"scheduled_event.update",
		// Skill management
		"skill.create",
		"skill.delete",
		"skill.list",
		"skill.read",
		"skill.register",
		"skill.update",
		// Template management
		"template.create",
		"template.delete",
		"template.list",
		"template.read",
		"template.update",
	}
}

// projectAdminPermissionIDs returns the curated permission set for the
// project-admin role. This is an explicit list — NOT derived from registry
// iteration. Compared to project-owner, this excludes:
//   - All *.delete permissions (admins cannot delete resources)
//   - agent.set_message_mode (D7: only project owners may unseal none-mode agents)
//
// Agent-self credential permissions (status_update, log_append, token_refresh,
// identity_token, port_forward, notify) and hub-level operations (project.create,
// project.clone, project.register) are also excluded — see projectOwnerPermissionIDs
// comments for rationale.
//
// A new registry permission does NOT automatically enter this role; it must be
// added here and the role revision bumped.
func projectAdminPermissionIDs() []string {
	return []string{
		// Agent lifecycle and operations (no delete, no set_message_mode,
		// no agent-self credential permissions, no attach — see
		// projectOwnerPermissionIDs for the rationale)
		"agent.create",
		"agent.lifecycle",
		"agent.list",
		"agent.message",
		"agent.port_access",
		"agent.read",
		"agent.stop_all",
		"agent.update",
		// GCP service account management (project-scoped). Lets a project
		// admin assign project-scoped service accounts in the project
		// (ptone/scion#2147). When gcpIamCheckMode is enforce, the immediate
		// creator's IAM actAs grant (iam.serviceAccounts.actAs) is also
		// checked; in the default off mode this permission alone authorizes
		// assignment of project-scoped service accounts.
		"gcp_service_account.assign",
		// Harness config management (no delete)
		"harness_config.create",
		"harness_config.list",
		"harness_config.read",
		"harness_config.update",
		// Project management (no delete, no hub-level create/clone/register)
		"project.list",
		"project.manage",
		"project.read",
		"project.secret_read",
		"project.update",
		// Scheduled event management (no delete)
		"scheduled_event.create",
		"scheduled_event.list",
		"scheduled_event.read",
		"scheduled_event.update",
		// Skill management (no delete)
		"skill.create",
		"skill.list",
		"skill.read",
		"skill.register",
		"skill.update",
		// Template management (no delete)
		"template.create",
		"template.list",
		"template.read",
		"template.update",
	}
}

// projectMemberCuratedPermissionIDs returns the curated permission set for the
// project-member role. This is an explicit list — NOT derived from registry
// iteration. Members get create, read, and list actions on project-
// scoped resources.
//
// Excluded from this role:
//   - agent.message: messaging requires owner/admin role or ancestry
//   - agent.stop_all: bulk stop is an administrative action, not basic membership
//   - skill.create: skill creation is an admin/owner action
//   - project.create: hub-level operation, meaningless in a project-scoped role
//
// A new registry permission does NOT automatically enter this role; it must be
// added here and the role revision bumped.
func projectMemberCuratedPermissionIDs() []string {
	return []string{
		// Agent operations (create, read, list)
		"agent.create",
		"agent.list",
		"agent.read",
		// GCP service account management (project-scoped). Lets a project
		// member assign project-scoped service accounts in the project
		// (ptone/scion#2147). When gcpIamCheckMode is enforce, the immediate
		// creator's IAM actAs grant (iam.serviceAccounts.actAs) is also
		// checked; in the default off mode this permission alone authorizes
		// assignment of project-scoped service accounts.
		"gcp_service_account.assign",
		// Harness config (create, read, list)
		"harness_config.create",
		"harness_config.list",
		"harness_config.read",
		// Project (read, list — no project.create, which is hub-level)
		"project.list",
		"project.read",
		// Scheduled events (create, read, list)
		"scheduled_event.create",
		"scheduled_event.list",
		"scheduled_event.read",
		// Skills (read, list — no skill.create)
		"skill.list",
		"skill.read",
		// Templates (create, read, list)
		"template.create",
		"template.list",
		"template.read",
	}
}

// seedDefaultGroupsAndBindings creates the default hub-members group and
// associated role binding. This is called once during Hub initialization
// and is idempotent.
//
// The hub-members group gets a single system-scoped RoleBinding of the
// hub-member role, which contains a curated permission set. All
// authorization decisions route through the AK1 kernel using role
// bindings — no legacy policy seeding is needed.
func seedDefaultGroupsAndBindings(ctx context.Context, s store.Store) {
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

	// 2. Create a system-scoped RoleBinding of hub-member role to the
	// hub-members group. This single binding is the positive authority
	// source for hub member permissions.
	seedHubMemberRoleBinding(ctx, s, group.ID)
}

// seedHubMemberRoleBinding creates a system-scoped RoleBinding of the
// hub-member role to the hub-members group. Idempotent — skips if the
// binding already exists.
func seedHubMemberRoleBinding(ctx context.Context, s store.Store, hubMembersGroupID string) {
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	if err != nil {
		slog.Warn("hub-member role definition not found; cannot seed binding", "error", err)
		return
	}

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      hubMembersGroupID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return // already seeded
		}
		slog.Warn("failed to seed hub-member role binding for hub-members group",
			"group_id", hubMembersGroupID, "error", err)
		return
	}
	slog.Info("seeded hub-member role binding for hub-members group",
		"group_id", hubMembersGroupID, "role_definition_id", rd.ID)
}

// seedDevUser ensures the development pseudo-user exists in the store.
// This is needed because Ent enforces foreign key constraints on owner_id,
// and the dev user must exist as a User record for project group creation to
// succeed in workstation/dev-auth mode.
func seedDevUser(ctx context.Context, s store.Store, cfg DevUserConfig) {
	u := NewDevUser(cfg)
	_, err := s.GetUser(ctx, DevUserID)
	if err == nil {
		// User exists — ensure super-admin role binding (CO1: AK1 kernel requires
		// role bindings, not just the User.Role field).
		ensureDevUserRoleBinding(ctx, s)
		return
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
		return
	}
	ensureDevUserRoleBinding(ctx, s)
}

// ensureDevUserRoleBinding creates a super-admin role binding for the dev user
// if one does not already exist. CO1: The AK1 kernel requires role bindings
// for authorization — the User.Role field alone is not sufficient.
func ensureDevUserRoleBinding(ctx context.Context, s store.Store) {
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		slog.Warn("failed to find super-admin role definition for dev user", "error", err)
		return
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      DevUserID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Warn("failed to create super-admin role binding for dev user", "error", err)
	}
}

// reconcileBuiltInRoles performs deterministic reconciliation of built-in role
// definitions. For each code-declared built-in role:
//
//  1. If the role does not exist in the store, create it.
//  2. If it exists and the code revision is higher than the last-applied
//     revision (tracked via hub settings), update the permissions to match
//     the code-declared set exactly.
//  3. If the code revision equals the stored revision, no update is needed.
//
// This replaces the old create-if-missing role seeding behavior.
// A new registry permission does NOT automatically enter a built-in role —
// it must be explicitly added to the role's permission list and the revision
// bumped.
//
// Reconciliation is idempotent: running twice with the same code produces the
// same result.
func reconcileBuiltInRoles(ctx context.Context, s store.Store) {
	for _, role := range BuiltInRoles() {
		reconcileBuiltInRole(ctx, s, role)
	}
}

// reconcileBuiltInRole creates or updates a single built-in role definition.
// R-6: The applied-revision marker now includes a hash of the resolved
// permission list. This ensures reconciliation fires when the dynamic
// permission list changes (e.g., a new permission added to the registry
// expands the super-admin set), even if the code revision is unchanged.
func reconcileBuiltInRole(ctx context.Context, s store.Store, role BuiltInRole) {
	codeMarker := builtInRoleMarker{
		Revision: role.Revision,
		PermHash: permListHash(role.Permissions),
	}

	existing, err := s.GetRoleDefinitionByName(ctx, role.Name, role.ScopeType)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("failed to check for existing role definition",
				"name", role.Name, "error", err)
			return
		}
		// Role does not exist — create it.
		rd := &store.RoleDefinition{
			Name:        role.Name,
			Description: role.Description,
			ScopeType:   role.ScopeType,
			Permissions: role.Permissions,
			System:      true,
		}
		if _, err := s.CreateRoleDefinition(ctx, rd); err != nil {
			slog.Warn("failed to seed role definition",
				"name", role.Name, "error", err)
			return
		}
		// Record the applied revision marker.
		recordBuiltInRoleMarker(ctx, s, role.Name, codeMarker)
		slog.Info("seeded role definition",
			"name", role.Name, "scope_type", role.ScopeType,
			"revision", role.Revision, "perm_hash", codeMarker.PermHash)
		return
	}

	// Role exists — check if reconciliation is needed.
	applied := getAppliedBuiltInRoleMarker(ctx, s, role.Name)
	if applied.Revision > role.Revision {
		return // stored revision is higher — operator override, do not downgrade
	}
	if applied.Revision == role.Revision && applied.PermHash == codeMarker.PermHash {
		return // same revision with matching permission hash — no changes needed
	}
	// Reconcile: either the code revision is higher, or the permission list
	// changed at the same revision (R-6: dynamic lists like allPermissionIDs).

	// Code revision is higher OR permission list changed — update permissions.
	if err := s.UpdateSystemRoleDefinitionPermissions(ctx, existing.ID, role.Permissions); err != nil {
		slog.Warn("failed to reconcile role definition permissions",
			"name", role.Name, "from_revision", applied.Revision,
			"to_revision", role.Revision, "error", err)
		return
	}
	recordBuiltInRoleMarker(ctx, s, role.Name, codeMarker)
	slog.Info("reconciled role definition permissions",
		"name", role.Name, "from_revision", applied.Revision,
		"to_revision", role.Revision,
		"perm_hash", codeMarker.PermHash,
		"permissions_count", len(role.Permissions))
}

// getAppliedBuiltInRoleMarker reads the last-applied revision marker for a
// built-in role from hub settings. Returns a zero marker if no revision has
// been recorded. Backward-compatible: legacy integer-only markers are parsed
// as revision-only (empty PermHash), which always triggers reconciliation
// on the first startup after the R-6 fix.
func getAppliedBuiltInRoleMarker(ctx context.Context, s store.Store, roleName string) builtInRoleMarker {
	setting, err := s.GetHubSetting(ctx, builtInRoleRevisionKey(roleName))
	if err != nil {
		return builtInRoleMarker{} // not yet recorded
	}

	// Try new marker format first.
	var marker builtInRoleMarker
	if err := json.Unmarshal(setting.Value, &marker); err == nil && marker.PermHash != "" {
		return marker
	}

	// Backward compat: parse legacy integer revision.
	var rev int
	if err := json.Unmarshal(setting.Value, &rev); err == nil {
		return builtInRoleMarker{Revision: rev}
	}
	// Try string-encoded integer (legacy quoted format).
	var revStr string
	if err := json.Unmarshal(setting.Value, &revStr); err == nil {
		if parsed, err2 := strconv.Atoi(revStr); err2 == nil {
			return builtInRoleMarker{Revision: parsed}
		}
	}
	return builtInRoleMarker{}
}

// recordBuiltInRoleMarker writes the applied revision marker for a built-in
// role to hub settings. Best-effort; failures are logged.
func recordBuiltInRoleMarker(ctx context.Context, s store.Store, roleName string, marker builtInRoleMarker) {
	markerJSON, _ := json.Marshal(marker)
	if _, err := s.UpsertHubSetting(ctx, builtInRoleRevisionKey(roleName),
		markerJSON, "system", -1, "seeded"); err != nil {
		slog.Warn("failed to record built-in role revision marker",
			"role", roleName, "revision", marker.Revision,
			"perm_hash", marker.PermHash, "error", err)
	}
}

// allPermissionIDs returns IDs for all permissions in the registry.
func allPermissionIDs() []string {
	ids := make([]string, len(permissions.Registry))
	for i, p := range permissions.Registry {
		ids[i] = p.ID
	}
	return ids
}

// hubAdminPermissionIDs returns the curated permission set for the hub-admin
// role. This is a subset of all permissions — it excludes super-admin-only
// operations (maintenance, auth reset, diagnostics, admin mode, policies,
// user suspend/promote) and non-route internal permissions.
func hubAdminPermissionIDs() []string {
	// Explicit set of permission IDs included in the hub-admin role.
	// This set is a product decision; changes require architect or sponsor review.
	included := map[string]bool{
		// User management (not suspend/promote — those remain super-admin-only)
		"user.read":   true,
		"user.list":   true,
		"user.update": true,
		"user.invite": true,
		// Group management
		"group.read":         true,
		"group.list":         true,
		"group.create":       true,
		"group.update":       true,
		"group.delete":       true,
		"group.addMember":    true,
		"group.removeMember": true,
		// Hub settings (read + update, but not maintenance/reset)
		"hub.settings.read":           true,
		"hub.settings.update":         true,
		"hub.config.read":             true,
		"hub.config.update":           true,
		"hub.health.read":             true,
		"hub.integrations.read":       true,
		"hub.integrations.update":     true,
		"hub.lifecycle_hooks.read":    true,
		"hub.lifecycle_hooks.update":  true,
		"hub.allow_list.read":         true,
		"hub.allow_list.update":       true,
		"hub.project_defaults.read":   true,
		"hub.project_defaults.update": true,
		"hub.scheduler.read":          true,
		"hub.scheduler.update":        true,
		// Scheduled event management (hub-wide visibility and control)
		"scheduled_event.read":      true,
		"scheduled_event.list":      true,
		"scheduled_event.create":    true,
		"scheduled_event.delete":    true,
		"scheduled_event.update":    true,
		"hub.federation.read":       true,
		"hub.federation.update":     true,
		"hub.teams_manifest.read":   true,
		"hub.teams_manifest.update": true,
		"hub.github_app.read":       true,
		"hub.github_app.update":     true,
		"hub.metrics.read":          true,
		"hub.validate.execute":      true,
		// Quota management
		"quota.read":   true,
		"quota.create": true,
		"quota.update": true,
		"quota.delete": true,
		// Role and binding management (PR-C1)
		"role.read":           true,
		"role.create":         true,
		"role.update":         true,
		"role.delete":         true,
		"role_binding.read":   true,
		"role_binding.create": true,
		"role_binding.delete": true,
		// Project oversight
		"project.read":   true,
		"project.list":   true,
		"project.update": true,
		// Skill registries
		"skill.read":          true,
		"skill.list":          true,
		"skill.create":        true,
		"skill.update":        true,
		"skill.delete":        true,
		"skill.register":      true,
		"skill.create_global": true,
		// Access constraints — full operator control.
		// hub-admin can read and administer access constraints so that
		// operators who are not super-admins can manage them via the web UI.
		"access_constraint.read":  true,
		"access_constraint.admin": true,
	}

	var ids []string
	for _, p := range permissions.Registry {
		if included[p.ID] {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// globalCatalogAuthorPermissionIDs returns the exact permission set for the
// global-catalog-author role. This role is system-scoped, so its safety relies
// on the permission set containing ONLY global-catalog IDs — a careless
// addition would silently grant hub-wide authority. Pin with a test.
func globalCatalogAuthorPermissionIDs() []string {
	return []string{
		"skill.create_global",
	}
}

// agentRolePermissionIDs maps an AgentRole to permission IDs by examining
// what scopes that role grants.
func agentRolePermissionIDs(role AgentRole) []string {
	scopes := ScopesForRole(role)
	if scopes == nil {
		return nil
	}
	// Build a set of scope strings
	scopeSet := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		scopeSet[string(s)] = true
	}
	// Map scopes back to permission IDs
	var ids []string
	for _, p := range permissions.Registry {
		for _, s := range p.AgentScopes {
			if scopeSet[s] {
				ids = append(ids, p.ID)
				break
			}
		}
	}
	return ids
}

// BackfillRoleBindings runs the startup role-binding backfills:
//   - system role bindings from User.Role;
//   - project-owner role bindings from Project.CreatedBy (only when the
//     User.Role step succeeded, preserving the original ordering);
//   - clearing the legacy Group.OwnerID on project members groups
//     (ptone/scion#2599), which always runs regardless of earlier failures.
//
// Every step is idempotent. Steps do not stop at the first failure: their
// errors are combined with errors.Join and returned together, and the
// startup caller logs them as a warning.
func BackfillRoleBindings(ctx context.Context, s store.Store) error {
	var errs []error

	// Backfill system role bindings from User.Role
	if err := backfillUserRoleBindings(ctx, s); err != nil {
		errs = append(errs, fmt.Errorf("backfill user role bindings: %w", err))
	} else if err := backfillProjectOwnerRoleBindings(ctx, s); err != nil {
		// Backfill project-owner role bindings from Project.CreatedBy.
		// Pre-existing projects (created before project-scoped RoleBindings were
		// introduced) have a legacy CreatedBy/OwnerID but no project-owner
		// RoleBinding. This causes the project members view to show "no members"
		// and the "my projects" filter to miss RoleBinding-based membership.
		errs = append(errs, fmt.Errorf("backfill project owner role bindings: %w", err))
	}

	// Clear the legacy Group.OwnerID copied from Project.OwnerID onto
	// project members groups (ptone/scion#2599). This is security-relevant
	// (it removes a stale group.* grant) and independent of the steps above,
	// so it runs even when they fail, and its error is joined with theirs
	// rather than hiding them.
	if err := backfillClearProjectMembersGroupOwners(ctx, s); err != nil {
		errs = append(errs, fmt.Errorf("clear project members group owners: %w", err))
	}

	return errors.Join(errs...)
}

// projectMembersGroupOwnerBackfillPageSize is the ListGroups page size for
// backfillClearProjectMembersGroupOwners. It is a package variable, not a
// const, so tests can shrink it to exercise the pagination loop.
var projectMembersGroupOwnerBackfillPageSize = 200

// backfillClearProjectMembersGroupOwners clears Group.OwnerID on every
// project members group (ptone/scion#2599). createProjectMembersGroup used to
// copy Project.OwnerID into Group.OwnerID, and the owner/user/group
// relationship row grants group.* to Group.OwnerID, so a creator removed
// from the project without an ownership transfer kept managing the members
// group. Project.OwnerID confers no authority (ptone/scion#2586), and the
// members group is now created without an owner.
//
// Groups are identified by the project-members-group marker annotation
// (either key, see store.LegacyAnnotationProjectMembersGroup), never by
// slug, so a user-created group with a look-alike slug is left untouched. The pass runs
// on every startup and is idempotent: a group whose OwnerID is already empty
// is skipped, so a second run changes nothing. Per-group update errors are
// logged and skipped.
func backfillClearProjectMembersGroupOwners(ctx context.Context, s store.Store) error {
	// All groups are scanned rather than filtering by GroupType: the scan is
	// paginated and cheap, and a type filter could miss legacy group shapes.
	var cursor string
	var cleared int
	for {
		groups, err := s.ListGroups(ctx, store.GroupFilter{}, store.ListOptions{
			Limit:          projectMembersGroupOwnerBackfillPageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return fmt.Errorf("list groups for members group owner backfill: %w", err)
		}

		for i := range groups.Items {
			g := &groups.Items[i]
			if g.OwnerID == "" || !hasProjectMembersGroupMarker(g) {
				continue
			}
			prevOwner := g.OwnerID
			g.OwnerID = ""
			if err := s.UpdateGroup(ctx, g); err != nil {
				slog.Warn("failed to clear project members group owner during backfill",
					"group_id", g.ID, "project_id", g.ProjectID, "error", err)
				continue
			}
			slog.Info("cleared project members group owner",
				"group_id", g.ID, "project_id", g.ProjectID, "previous_owner_id", prevOwner)
			cleared++
		}

		if groups.NextCursor == "" {
			break
		}
		cursor = groups.NextCursor
	}

	if cleared > 0 {
		slog.Info("cleared project members group owners", "cleared", cleared)
	}
	return nil
}

// backfillUserRoleBindings brings hub-level grants in line with User.Role for
// every non-invited user (active and suspended) at startup:
//   - admin → system-scoped super-admin role binding (created here; the
//     hub role grant helper does not own super-admin).
//   - every role → syncHubRoleGrants, which ensures hub-members membership for
//     members, removes it from viewers and ensures their hub-viewer binding,
//     and removes stale hub-viewer bindings from members and admins.
//
// Invited users are skipped: the role stored on an invited row is a
// placeholder (the real role is assigned at first sign-in), so they get no
// grants until they sign in. The loop is idempotent, logs and continues on
// per-user errors, and paginates through all users to avoid silent
// truncation by store defaults.
func backfillUserRoleBindings(ctx context.Context, s store.Store) error {
	var cursor string
	var createdBindings int
	var synced int
	for {
		users, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{
			Limit:  200,
			Cursor: cursor,
		})
		if err != nil {
			return err
		}

		for i := range users.Items {
			u := &users.Items[i]

			if u.Status == store.UserStatusInvited {
				continue
			}

			if u.Role == store.UserRoleAdmin {
				created, err := backfillSuperAdminBinding(ctx, s, u.ID)
				if err != nil {
					slog.Warn("failed to backfill super-admin role binding",
						"user_id", u.ID, "error", err)
				} else if created {
					createdBindings++
				}
			}

			if err := syncHubRoleGrants(ctx, s, u.ID, u.Role, store.SystemBackfillCreatedBy); err != nil {
				slog.Warn("failed to sync hub role grants during backfill",
					"user_id", u.ID, "role", u.Role, "error", err)
				continue
			}
			synced++
		}

		if users.NextCursor == "" {
			break
		}
		cursor = users.NextCursor
	}

	if createdBindings > 0 {
		slog.Info("backfilled super-admin role bindings", "created", createdBindings)
	}
	if synced > 0 {
		slog.Info("synced hub role grants", "users", synced)
	}
	return nil
}

// backfillSuperAdminBinding creates the system-scoped super-admin binding for
// an admin user if it does not exist. It reports whether a binding was created.
func backfillSuperAdminBinding(ctx context.Context, s store.Store, userID string) (bool, error) {
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		return false, fmt.Errorf("super-admin role definition lookup: %w", err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemBackfillCreatedBy,
	})
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return false, nil // already backfilled
		}
		return false, err
	}
	return true, nil
}

// reconcileSyncHubRoleGrants syncs hub role grants after the startup
// reconciler changed a user's role. Invited users are skipped (placeholder
// role). Best-effort: errors are logged.
func reconcileSyncHubRoleGrants(ctx context.Context, s store.Store, u *store.User) {
	if u.Status == store.UserStatusInvited {
		return
	}
	if err := syncHubRoleGrants(ctx, s, u.ID, u.Role, store.SystemReconcileCreatedBy); err != nil {
		slog.Warn("failed to sync hub role grants during super-admin reconciliation",
			"user_id", u.ID, "role", u.Role, "error", err)
	}
}

// backfillProjectOwnerRoleBindings creates project-scoped project-owner role
// bindings from legacy Project.CreatedBy values. Pre-existing projects
// (created before the RoleBinding-based membership model) have a CreatedBy
// user but no corresponding project-owner RoleBinding, which causes the
// project members view to show "no members". This function is idempotent:
// it skips projects that already have the binding.
//
// The backfill only runs for a project that has ZERO project-owner bindings
// (for any principal). It runs on every startup, so without that gate a
// creator who was later removed, or who transferred ownership and was then
// removed, would be re-made owner on each restart (ptone/scion#2554). A
// project that has an owner is never a legacy pre-RoleBinding project, so
// skipping it loses nothing.
func backfillProjectOwnerRoleBindings(ctx context.Context, s store.Store) error {
	ownerRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil {
		slog.Warn("project-owner role definition not found during backfill; skipping", "error", err)
		return nil // not fatal — role definitions may not be seeded yet
	}

	var cursor string
	var created int
	for {
		projects, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit:          200,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return fmt.Errorf("list projects for owner backfill: %w", err)
		}

		for i := range projects.Items {
			p := &projects.Items[i]
			warnOwnerOnlyLegacyProject(ctx, s, p, ownerRoleDef.ID)
			if p.CreatedBy == "" {
				continue
			}

			hasOwner, err := projectHasOwnerBinding(ctx, s, p.ID, ownerRoleDef.ID)
			if err != nil {
				slog.Warn("failed to check project owner bindings during backfill; skipping",
					"project_id", p.ID, "error", err)
				continue
			}
			if hasOwner {
				continue
			}

			_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: ownerRoleDef.ID,
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      p.CreatedBy,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          p.ID,
				CreatedBy:        "system-backfill",
			})
			if err != nil {
				if errors.Is(err, store.ErrAlreadyExists) {
					continue // already has binding — idempotent
				}
				slog.Warn("failed to create project-owner role binding during backfill",
					"project_id", p.ID, "user_id", p.CreatedBy, "error", err)
				continue
			}
			created++
		}

		if projects.NextCursor == "" {
			break
		}
		cursor = projects.NextCursor
	}

	if created > 0 {
		slog.Info("backfilled project-owner role bindings", "created", created)
	}
	return nil
}

// projectHasOwnerBinding reports whether any principal holds a project-owner
// role binding on the given project.
func projectHasOwnerBinding(ctx context.Context, s store.Store, projectID, ownerRoleDefID string) (bool, error) {
	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	if err != nil {
		return false, err
	}
	for _, b := range bindings {
		if b != nil && b.RoleDefinitionID == ownerRoleDefID {
			return true, nil
		}
	}
	return false, nil
}

// warnOwnerOnlyLegacyProject logs, once per project per startup, a project
// whose OwnerID is not backed by a project-owner binding. Project.OwnerID is
// not an authorization source (ptone/scion#2586), so it grants nothing; this
// only logs and never grants. Two shapes warn:
//   - OwnerID set, CreatedBy empty, and no project-owner binding at all: the
//     project has no owner until an admin grants one.
//   - OwnerID set, CreatedBy set but different, and OwnerID itself holds no
//     project-owner binding: the named owner has no access through OwnerID.
//
// A project where OwnerID equals CreatedBy never warns; the backfill grants
// CreatedBy.
func warnOwnerOnlyLegacyProject(ctx context.Context, s store.Store, p *store.Project, ownerRoleDefID string) {
	if p.OwnerID == "" || p.OwnerID == p.CreatedBy {
		return
	}
	if p.CreatedBy == "" {
		hasOwner, err := projectHasOwnerBinding(ctx, s, p.ID, ownerRoleDefID)
		if err != nil {
			slog.Warn("failed to check project owner bindings for owner-only legacy project; skipping",
				"project_id", p.ID, "error", err)
			return
		}
		if hasOwner {
			return // any owner binding: the project has an owner
		}
		slog.Warn("project has OwnerID but no CreatedBy and no project-owner binding; OwnerID grants no access, an admin must add an owner",
			"project_id", p.ID, "owner_id", p.OwnerID)
		return
	}
	// OwnerID differs from a non-empty CreatedBy. projectHasOwnerBinding
	// answers "does anyone own the project", which is not this question:
	// the backfill grants CreatedBy, so check that OwnerID itself holds a
	// project-owner binding.
	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, p.ID)
	if err != nil {
		slog.Warn("failed to check project owner bindings for owner-only legacy project; skipping",
			"project_id", p.ID, "error", err)
		return
	}
	for _, b := range bindings {
		if b != nil && b.RoleDefinitionID == ownerRoleDefID &&
			b.PrincipalType == store.RoleBindingPrincipalUser && b.PrincipalID == p.OwnerID {
			return
		}
	}
	slog.Warn("project OwnerID differs from CreatedBy and holds no project-owner binding; OwnerID grants no access",
		"project_id", p.ID, "owner_id", p.OwnerID, "created_by", p.CreatedBy)
}

// ReconcileSuperAdminBindings ensures bidirectional consistency between
// User.Role == "admin", the AdminEmails config list, and system-scoped
// super-admin role bindings (Phase 1F, D11 revocability fix). On startup:
//
// Single-pass reconciliation (D11-fix2):
//
//	For each user, in one pass:
//	  - If the user is in adminEmails: promote Role to "admin" (if needed) and
//	    ensure a super-admin binding exists.
//	  - If the user is NOT in adminEmails AND adminEmails is non-empty: demote
//	    Role from "admin" to defaultRole (normalized: "viewer" or "member") if
//	    needed and delete any super-admin binding.
//	  - After a successful role change, syncHubRoleGrants brings the
//	    hub-members group and hub-viewer binding in line with the new role
//	    (this runs after BackfillRoleBindings, so grants would otherwise be
//	    stale until the next restart). Invited users are not synced: their
//	    stored role is a placeholder until first sign-in.
//
// Empty-list safety guard:
//   - When adminEmails is nil or empty, no demotions occur and a warning is
//     logged. An empty list is almost always a config load failure, not an
//     instruction to remove every administrator. Both nil and len==0 take the
//     same branch.
//
// Revocation latency: super-admin revocation takes effect on next hub restart.
//
// This is called after BackfillRoleBindings and is idempotent.
func ReconcileSuperAdminBindings(ctx context.Context, s store.Store, adminEmails []string, defaultRole string) (demotionSafe bool, err error) {
	// Demotion lands on the configured default role, exactly like login-time
	// demotion in determineUserRole.
	demoteTo := normalizedDefaultRole(defaultRole)

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			slog.Warn("super-admin role definition not found; skipping reconciliation")
			return false, nil
		}
		return false, fmt.Errorf("lookup super-admin role definition: %w", err)
	}

	// Build a lowercase set of admin emails for O(1) lookup.
	adminSet := make(map[string]bool, len(adminEmails))
	for _, e := range adminEmails {
		adminSet[strings.ToLower(strings.TrimSpace(e))] = true
	}

	// ---- Effect guard (D11-fix3): two-pass classify-then-apply ----
	//
	// Pass 1: scan ALL existing users to build the intended final admin set
	// (existing users whose normalized email appears in AdminEmails). If the
	// intended admin set is empty, refuse all demotions — regardless of the
	// reason (empty config, whitespace corruption, encoding issues).
	//
	// This MUST be computed before any mutation; evaluating inside the
	// mutation loop would be row-order dependent (same bug class as R1).

	canDemote := len(adminEmails) > 0

	if canDemote {
		// Pre-scan: count existing users who match AdminEmails OR who have
		// UI-promoted (AdminAPICreatedBy) super-admin bindings. Both groups
		// will remain admin after reconciliation, so they form the intended
		// admin set.
		var intendedAdminCount int
		var preCursor string
		for {
			users, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{
				Limit:  200,
				Cursor: preCursor,
			})
			if err != nil {
				return false, err
			}
			for i := range users.Items {
				if adminSet[strings.ToLower(users.Items[i].Email)] {
					intendedAdminCount++
					continue
				}
				// Also count UI-promoted admins who are not in AdminEmails —
				// they will NOT be demoted, so they count toward intended admins.
				if users.Items[i].Role == "admin" {
					bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, users.Items[i].ID)
					if err != nil {
						continue
					}
					for _, b := range bindings {
						if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID && b.CreatedBy == store.AdminAPICreatedBy {
							intendedAdminCount++
							break
						}
					}
				}
			}
			if users.NextCursor == "" {
				break
			}
			preCursor = users.NextCursor
		}

		if intendedAdminCount == 0 {
			// Every email in AdminEmails belongs to a user who has never
			// logged in, or AdminEmails contains only whitespace/typos that
			// match nobody, and no UI-promoted admins exist. Proceeding would
			// demote every current admin, leaving zero administrators. Refuse.
			slog.Error("effect guard: reconciliation would leave ZERO administrators — "+
				"AdminEmails matches no existing users; refusing all demotions. "+
				"Verify AdminEmails entries match real user emails",
				"admin_emails_count", len(adminEmails))
			canDemote = false
		}
	}

	if !canDemote && len(adminEmails) == 0 {
		// When AdminEmails is empty, BOTH directions are disabled: inAdminList is
		// false for every user (disabling forward promotion/binding creation) and
		// canDemote is false (disabling reverse demotion/binding deletion). This is
		// fail-closed: a config load failure cannot cause new promotions OR
		// demotions. Operators seeing this warning should verify their AdminEmails
		// config is loading correctly — until it is non-empty, reconciliation will
		// not repair any admin state.
		slog.Warn("AdminEmails is nil or empty; all reconciliation disabled to prevent hub-wide lockout — verify config load")
	}

	// ---- Pass 2: apply promotions and (if safe) demotions ----

	var created, demoted, deleted int
	var cursor string
	for {
		users, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{
			Limit:  200,
			Cursor: cursor,
		})
		if err != nil {
			return false, err
		}
		for i := range users.Items {
			u := &users.Items[i]
			inAdminList := adminSet[strings.ToLower(u.Email)]

			if inAdminList {
				// Forward: ensure admin role and super-admin binding.
				if u.Role != "admin" {
					u.Role = "admin"
					if err := s.UpdateUser(ctx, u); err != nil {
						slog.Warn("failed to promote user during reconciliation",
							"user_id", u.ID, "error", err)
					} else {
						reconcileSyncHubRoleGrants(ctx, s, u)
					}
				}
				_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: rd.ID,
					PrincipalType:    store.RoleBindingPrincipalUser,
					PrincipalID:      u.ID,
					ScopeType:        store.RoleScopeSystem,
					ScopeID:          "",
					CreatedBy:        store.SystemReconcileCreatedBy,
				})
				if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
					slog.Warn("failed to create super-admin binding during reconciliation",
						"user_id", u.ID, "error", err)
				} else if err == nil {
					created++
				}
			} else if canDemote {
				// Check if this admin was promoted via UI/API (protected from demotion).
				bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID)
				if err != nil {
					continue
				}

				isUIPromoted := false
				for _, b := range bindings {
					if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID && b.CreatedBy == store.AdminAPICreatedBy {
						isUIPromoted = true
						break
					}
				}

				if isUIPromoted {
					slog.Info("skipping demotion for UI-promoted admin",
						"user_id", u.ID, "email", u.Email)
					continue
				}

				// Reverse: demote role and delete super-admin binding (config-granted only).
				if u.Role == "admin" {
					slog.Warn("demoting user: removed from AdminEmails",
						"user_id", u.ID, "email", u.Email, "old_role", "admin", "new_role", demoteTo)
					u.Role = demoteTo
					if err := s.UpdateUser(ctx, u); err != nil {
						slog.Warn("failed to demote user during reconciliation",
							"user_id", u.ID, "error", err)
					} else {
						demoted++
						reconcileSyncHubRoleGrants(ctx, s, u)
					}
				}
				// Delete orphaned super-admin bindings for users NOT in adminEmails.
				for _, b := range bindings {
					if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
						slog.Warn("deleting orphaned super-admin binding",
							"user_id", u.ID, "email", u.Email, "binding_id", b.ID)
						if err := s.DeleteRoleBinding(ctx, b.ID); err != nil {
							slog.Warn("failed to delete orphaned super-admin binding",
								"binding_id", b.ID, "error", err)
						} else {
							deleted++
						}
					}
				}
			}
		}
		if users.NextCursor == "" {
			break
		}
		cursor = users.NextCursor
	}

	if created > 0 {
		slog.Info("reconciled super-admin bindings (forward)", "created", created)
	}
	if demoted > 0 || deleted > 0 {
		slog.Info("reconciled super-admin bindings (reverse/D11)",
			"users_demoted", demoted, "bindings_deleted", deleted)
	}

	return canDemote, nil
}

// seedLimitDefinitions creates the system limit definitions if they don't
// already exist. Most are shipped with DefaultValue=0 (unlimited) for
// discoverability per sponsor decision OQ-2 Option B — opt-in fairness
// quotas that do nothing until an operator sets them. It is called once
// during Hub initialization and is idempotent.
//
// max_agents_per_broker is the deliberate exception: it is an infra-scoped
// crash-prevention gate, not a per-user/per-project fairness quota
// (ptone/scion#1303 — exceeding a runtime broker's agent capacity destroys
// the whole Instance it runs on, including the control plane in the
// single-node tier, after already returning HTTP 201). Seeding it at 0 would
// leave every new deployment exposed to that crash until an operator
// discovers and sets the limit, which defeats the fix.
//
// The default below is ptone's ruling (2026-09-29): keep the global default
// at 100 for now, matching what scion-next already runs, rather than the
// old 12. No per-broker tuning until there is a proper UI (ptone/scion#2177,
// folded into ptone/scion#2061 P2); until then this is one global value for
// every broker on the hub.
//
// 100 is above the observed crash point of single-node Cloud Run (~19-20
// idle agents on 4 CPU/8 GiB, ~51 idle on 8 CPU/32 GiB — see
// .design/hosted/cloud-run-single-node.md §9.1), so a fresh single-node
// Cloud Run deployment is effectively unguarded by this default alone; the
// cap still stops an unbounded runaway loop. Operators deploying single-node
// Cloud Run should lower this value right after deploying (about 16 is
// recommended) via Admin → Quotas, or PUT /api/v1/admin/limits/{id} for the
// max_agents_per_broker system limit definition (ptone/scion#2061 P1a,
// ptone/scion#2063). Seeding is insert-only: it never overwrites an existing
// row, so a hub that already has 12, 30, or any other deliberately-set value
// keeps it across upgrades.
func seedLimitDefinitions(ctx context.Context, s store.Store) {
	systemLimits := []struct {
		name         string
		resourceType string
		unit         string
		description  string
		defaultValue int64
	}{
		{store.LimitMaxAgentsPerProject, "agent", "count", "Maximum agents per project", 0},
		{store.LimitMaxProjectsPerUser, "project", "count", "Maximum projects per user", 0},
		{store.LimitMaxMembersPerGroup, "group", "count", "Maximum members per group", 0},
		{store.LimitMaxAgentsPerBroker, "agent", "count", "Maximum concurrently live agents per runtime broker (crash-prevention ceiling, ptone/scion#1303)", 100},
	}

	for _, lim := range systemLimits {
		seedLimitDefinition(ctx, s, lim.name, lim.resourceType, lim.unit, lim.description, lim.defaultValue)
	}
}

// seedLimitDefinition creates a single system limit definition if it doesn't
// already exist.
func seedLimitDefinition(ctx context.Context, s store.Store, name, resourceType, unit, description string, defaultValue int64) {
	_, err := s.GetLimitDefinitionByName(ctx, name)
	if err == nil {
		return // already exists
	}
	if !errors.Is(err, store.ErrNotFound) {
		slog.Warn("failed to check for existing limit definition", "name", name, "error", err)
		return
	}
	ld := &store.LimitDefinition{
		Name:         name,
		ResourceType: resourceType,
		Unit:         unit,
		Description:  description,
		DefaultValue: defaultValue,
		System:       true,
	}
	if _, err := s.CreateLimitDefinition(ctx, ld); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return // Another instance already seeded this — expected in multi-node
		}
		slog.Warn("failed to seed limit definition", "name", name, "error", err)
		return
	}
	slog.Info("seeded limit definition", "name", name, "resource_type", resourceType)
}

// CleanupRedundantHubMemberBindings removes system-created direct user→hub-member
// role bindings that are redundant with the canonical Hub Members group membership.
//
// Safety invariants:
//  1. Before deleting anything, verifies the canonical Hub Members group has at
//     least one currently active group-principal hub-member role binding (exact
//     canonical role definition ID, principalType=group, principalId=group.ID,
//     scopeType=system, scopeId="", NotBefore absent or ≤ now, ExpiresAt absent
//     or > now). If this binding is missing, inactive, or lookup fails, cleanup
//     deletes nothing and returns an error (fail-closed).
//  2. Only deletes direct bindings that match ALL of: hub-member role definition,
//     principalType=user, scopeType=system, scopeID="", CreatedBy is an exact
//     trusted sentinel ("system-backfill" or "system-reconcile"), and both
//     NotBefore and ExpiresAt are absent (unconditional legacy duplicates only).
//     Time-windowed (scheduled or expiring) bindings are semantically distinct
//     and are preserved.
//  3. All validation and deletes execute in a single WithTx transaction. A delete
//     failure rolls back all deletes atomically — no partial cleanup is committed.
//
// This function is idempotent and safe to run on every startup.
func CleanupRedundantHubMemberBindings(ctx context.Context, s store.Store) error {
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	if err != nil {
		// Group doesn't exist yet — nothing to clean up.
		slog.Debug("hub-members group not found, skipping redundant binding cleanup", "error", err)
		return nil
	}

	hubMemberRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	if err != nil {
		slog.Debug("hub-member role definition not found, skipping cleanup", "error", err)
		return nil
	}

	// C1: Verify the canonical group-principal hub-member binding exists and is
	// currently active before deleting any direct bindings. Without this check,
	// cleanup could strip users of their only effective hub-member grant.
	groupBindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalGroup, group.ID)
	if err != nil {
		return fmt.Errorf("verify canonical group binding: list group bindings: %w", err)
	}
	now := time.Now()
	var hasActiveCanonicalBinding bool
	for _, gb := range groupBindings {
		if gb.RoleDefinitionID != hubMemberRD.ID {
			continue
		}
		if gb.PrincipalType != store.RoleBindingPrincipalGroup {
			continue
		}
		if gb.PrincipalID != group.ID {
			continue
		}
		if gb.ScopeType != store.RoleScopeSystem {
			continue
		}
		if gb.ScopeID != "" {
			continue
		}
		// Time-window check: binding must be currently active.
		if gb.NotBefore != nil && gb.NotBefore.After(now) {
			continue // scheduled, not yet active
		}
		if gb.ExpiresAt != nil && !gb.ExpiresAt.After(now) {
			continue // expired
		}
		hasActiveCanonicalBinding = true
		break
	}
	if !hasActiveCanonicalBinding {
		slog.Error("hub-members group lacks an active canonical hub-member binding; skipping cleanup to prevent privilege loss",
			"group_id", group.ID, "role_definition_id", hubMemberRD.ID)
		return fmt.Errorf("hub-members group has no active canonical hub-member binding: cleanup aborted to prevent privilege loss")
	}

	members, err := s.GetGroupMembers(ctx, group.ID)
	if err != nil {
		return fmt.Errorf("list hub-members group members: %w", err)
	}

	// Collect all binding IDs to delete, then execute inside a single
	// transaction so that a failure mid-cleanup rolls back all deletes.
	type deleteTarget struct {
		bindingID string
		userID    string
	}
	var targets []deleteTarget

	for _, m := range members {
		if m.MemberType != store.GroupMemberTypeUser {
			continue
		}

		bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, m.MemberID)
		if err != nil {
			return fmt.Errorf("list bindings for user %s during cleanup: %w", m.MemberID, err)
		}

		for _, rb := range bindings {
			// Only delete direct user→hub-member, system-scope bindings
			// that were created by the system (backfill or reconcile sentinels).
			if rb.RoleDefinitionID != hubMemberRD.ID {
				continue
			}
			if rb.ScopeType != store.RoleScopeSystem {
				continue
			}
			// R1: Require empty ScopeID — a corrupt or future binding with
			// a non-empty ScopeID is semantically distinct and must be preserved.
			if rb.ScopeID != "" {
				continue
			}
			if rb.PrincipalType != store.RoleBindingPrincipalUser {
				continue
			}
			if !store.IsSystemCreatedBinding(rb.CreatedBy) {
				continue // preserve admin-created direct bindings
			}
			// Preserve time-windowed direct bindings (scheduled or expiring) —
			// these are semantically distinct from unconditional legacy duplicates.
			if rb.NotBefore != nil || rb.ExpiresAt != nil {
				continue
			}

			targets = append(targets, deleteTarget{bindingID: rb.ID, userID: m.MemberID})
		}
	}

	if len(targets) == 0 {
		return nil
	}

	// Execute all deletes in a single transaction for atomicity.
	err = s.WithTx(ctx, func(tx store.Store) error {
		for _, t := range targets {
			if err := tx.DeleteRoleBinding(ctx, t.bindingID); err != nil {
				return fmt.Errorf("delete redundant hub-member binding %s for user %s: %w",
					t.bindingID, t.userID, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("transactional cleanup of redundant hub-member bindings: %w", err)
	}

	slog.Info("cleaned up redundant direct hub-member bindings", "deleted", len(targets))
	return nil
}

// ensureHubMembershipTx idempotently adds the given user to the canonical
// Hub Members group using the provided store (which may be a transaction).
// This is the single implementation of hub-members membership grants.
// Returns an error if the group cannot be found (fail-closed).
func ensureHubMembershipTx(ctx context.Context, tx store.Store, userID string) error {
	group, err := tx.GetGroupBySlug(ctx, hubMembersSlug)
	if err != nil {
		return fmt.Errorf("hub-members group lookup: %w", err)
	}

	err = tx.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleMember,
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("add user to hub-members group: %w", err)
	}
	return nil
}

// removeHubMembershipTx removes the given user from the canonical Hub Members
// group using the provided store (which may be a transaction). This is the
// counterpart to ensureHubMembershipTx, used when a user's role changes to
// viewer so they no longer carry hub-member permissions.
// A missing group or membership is not an error (idempotent).
func removeHubMembershipTx(ctx context.Context, tx store.Store, userID string) error {
	group, err := tx.GetGroupBySlug(ctx, hubMembersSlug)
	if err != nil {
		// Group doesn't exist yet — nothing to remove.
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("hub-members group lookup: %w", err)
	}

	err = tx.RemoveGroupMember(ctx, group.ID, store.GroupMemberTypeUser, userID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("remove user from hub-members group: %w", err)
	}
	return nil
}

// syncHubRoleGrants makes the hub-level grants for a user match their hub
// role (User.Role). It is the single place that reconciles the hub-members
// group membership and the system-scoped hub-viewer role binding with the
// role, and is idempotent. Call it after (or in the same transaction as)
// persisting User.Role. st may be a transaction store.
//
//	member → ensure hub-members membership; delete hub-viewer binding(s)
//	viewer → remove hub-members membership; ensure an unconditional hub-viewer binding
//	admin  → delete hub-viewer binding(s); hub-members membership is left as-is
//
// The super-admin binding is NOT handled here; the existing super-admin
// ensure/delete paths own it. createdBy records provenance on a newly created
// hub-viewer binding (store.AdminAPICreatedBy for the admin API,
// store.SystemReconcileCreatedBy for login paths).
//
// Any other role value is an error. Errors are returned to the caller, which
// decides whether to fail closed (transactions) or log and continue (login).
func syncHubRoleGrants(ctx context.Context, st store.Store, userID, role, createdBy string) error {
	switch role {
	case store.UserRoleMember:
		if err := ensureHubMembershipTx(ctx, st, userID); err != nil {
			return err
		}
		return deleteHubViewerBindingsTx(ctx, st, userID)
	case store.UserRoleViewer:
		if err := removeHubMembershipTx(ctx, st, userID); err != nil {
			return err
		}
		return ensureHubViewerBindingTx(ctx, st, userID, createdBy)
	case store.UserRoleAdmin:
		return deleteHubViewerBindingsTx(ctx, st, userID)
	default:
		return fmt.Errorf("sync hub role grants: unsupported role %q", role)
	}
}

// hubViewerBindingsForUser returns the user's system-scoped hub-viewer role
// bindings in every lifecycle state, and whether one of them is
// unconditional (no NotBefore and no ExpiresAt).
func hubViewerBindingsForUser(ctx context.Context, st store.Store, userID string) (*store.RoleDefinition, []*store.RoleBinding, bool, error) {
	rd, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	if err != nil {
		return nil, nil, false, fmt.Errorf("hub-viewer role definition lookup: %w", err)
	}
	if rd == nil {
		return nil, nil, false, fmt.Errorf("hub-viewer role definition not found")
	}
	bindings, err := st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("list role bindings for user: %w", err)
	}
	var matched []*store.RoleBinding
	unconditional := false
	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeSystem || b.RoleDefinitionID != rd.ID {
			continue
		}
		matched = append(matched, b)
		if b.NotBefore == nil && b.ExpiresAt == nil {
			unconditional = true
		}
	}
	return rd, matched, unconditional, nil
}

// ensureHubViewerBindingTx ensures the user has an unconditional
// system-scoped hub-viewer role binding. The viewer role is permanent, so the
// grant is too: a time-limited binding (expired, scheduled, or active with a
// future ExpiresAt) does not satisfy it and is replaced. The replacement is
// delete-then-create because the (role, principal, scope) tuple is unique
// regardless of lifecycle.
func ensureHubViewerBindingTx(ctx context.Context, st store.Store, userID, createdBy string) error {
	rd, existing, unconditional, err := hubViewerBindingsForUser(ctx, st, userID)
	if err != nil {
		return err
	}
	if unconditional {
		return nil
	}
	for _, b := range existing {
		if err := st.DeleteRoleBinding(ctx, b.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete time-limited hub-viewer binding %s: %w", b.ID, err)
		}
	}
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        createdBy,
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("create hub-viewer binding: %w", err)
	}
	return nil
}

// deleteHubViewerBindingsTx removes all system-scoped hub-viewer role
// bindings for the user (any lifecycle state). Idempotent.
func deleteHubViewerBindingsTx(ctx context.Context, st store.Store, userID string) error {
	_, existing, _, err := hubViewerBindingsForUser(ctx, st, userID)
	if err != nil {
		return err
	}
	for _, b := range existing {
		if err := st.DeleteRoleBinding(ctx, b.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete hub-viewer binding %s: %w", b.ID, err)
		}
	}
	return nil
}
