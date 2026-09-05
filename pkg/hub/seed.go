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
			Revision:    2,
			Permissions: hubAdminPermissionIDs(),
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
			Revision:    1,
			Permissions: hubMemberPermissionIDs(),
		},
		{
			// hub-viewer: read-only permissions at system scope, carefully curated.
			// Same exclusions as hub-member (no cross-project visibility).
			Name:        store.SystemRoleHubViewer,
			Description: "Hub viewer with read-only access to directory resources",
			ScopeType:   store.RoleScopeSystem,
			Revision:    1,
			Permissions: hubViewerPermissionIDs(),
		},

		// ── Project-scoped roles ─────────────────────────────────────────

		{
			Name:        store.ProjectRoleOwner,
			Description: "Project owner with full project permissions",
			ScopeType:   store.RoleScopeProject,
			Revision:    1,
			Permissions: projectOwnerPermissionIDs(),
		},
		{
			Name:        store.ProjectRoleAdmin,
			Description: "Project admin with most project permissions (no delete, no set_message_mode)",
			ScopeType:   store.RoleScopeProject,
			Revision:    1,
			Permissions: projectAdminPermissionIDs(),
		},
		{
			Name:        store.ProjectRoleMember,
			Description: "Project member with basic project permissions",
			ScopeType:   store.RoleScopeProject,
			Revision:    1,
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
		// Broker catalog (read-only)
		"broker.read",
		"broker.list",
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
		// Role bindings (read-only)
		"role_binding.read",
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
		"role_binding.read",
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
		// Agent lifecycle and operations
		"agent.attach",
		"agent.create",
		"agent.delete",
		"agent.identity_token",
		"agent.list",
		"agent.log_append",
		"agent.message",
		"agent.notify",
		"agent.port_access",
		"agent.port_forward",
		"agent.read",
		"agent.set_message_mode",
		"agent.status_update",
		"agent.stop_all",
		"agent.token_refresh",
		"agent.update",
		// Harness config management
		"harness_config.create",
		"harness_config.delete",
		"harness_config.list",
		"harness_config.read",
		"harness_config.update",
		// Project management
		"project.clone",
		"project.create",
		"project.delete",
		"project.list",
		"project.manage",
		"project.read",
		"project.register",
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
// A new registry permission does NOT automatically enter this role; it must be
// added here and the role revision bumped.
func projectAdminPermissionIDs() []string {
	return []string{
		// Agent lifecycle and operations (no delete, no set_message_mode)
		"agent.attach",
		"agent.create",
		"agent.identity_token",
		"agent.list",
		"agent.log_append",
		"agent.message",
		"agent.notify",
		"agent.port_access",
		"agent.port_forward",
		"agent.read",
		"agent.status_update",
		"agent.stop_all",
		"agent.token_refresh",
		"agent.update",
		// Harness config management (no delete)
		"harness_config.create",
		"harness_config.list",
		"harness_config.read",
		"harness_config.update",
		// Project management (no delete)
		"project.clone",
		"project.create",
		"project.list",
		"project.manage",
		"project.read",
		"project.register",
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
// iteration. Members get create, read, list, and message actions on project-
// scoped resources, plus agent.stop_all.
//
// A new registry permission does NOT automatically enter this role; it must be
// added here and the role revision bumped.
func projectMemberCuratedPermissionIDs() []string {
	return []string{
		// Agent operations (create, read, list, message, stop_all)
		"agent.create",
		"agent.list",
		"agent.message",
		"agent.read",
		"agent.stop_all",
		// Harness config (create, read, list)
		"harness_config.create",
		"harness_config.list",
		"harness_config.read",
		// Project (create, read, list)
		"project.create",
		"project.list",
		"project.read",
		// Scheduled events (create, read, list)
		"scheduled_event.create",
		"scheduled_event.list",
		"scheduled_event.read",
		// Skills (create, read, list)
		"skill.create",
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
// This replaces the old create-if-missing seedRoleDefinitions behavior.
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
		"skill.read":     true,
		"skill.list":     true,
		"skill.create":   true,
		"skill.update":   true,
		"skill.delete":   true,
		"skill.register": true,
		// Access constraint read (admin does NOT get access_constraint.admin)
		"access_constraint.read": true,
	}

	var ids []string
	for _, p := range permissions.Registry {
		if included[p.ID] {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// permissionIDsByActions returns permission IDs for permissions whose action matches any given action.
func permissionIDsByActions(actions ...string) []string {
	actionSet := make(map[string]bool, len(actions))
	for _, a := range actions {
		actionSet[a] = true
	}
	var ids []string
	for _, p := range permissions.Registry {
		if actionSet[p.Action] {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// projectScopedPermissionIDs returns permission IDs for resources that are
// typically project-scoped (agents, templates, harness configs, skills, etc.)
// plus project.* itself.
func projectScopedPermissionIDs() []string {
	projectResources := map[string]bool{
		permissions.ResourceAgent:          true,
		permissions.ResourceProject:        true,
		permissions.ResourceTemplate:       true,
		permissions.ResourceHarnessConfig:  true,
		permissions.ResourceSkill:          true,
		permissions.ResourceScheduledEvent: true,
	}
	var ids []string
	for _, p := range permissions.Registry {
		if projectResources[p.Resource] {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// projectPermissionIDsExcluding returns project-scoped permission IDs
// excluding those with the given action.
func projectPermissionIDsExcluding(excludeAction string) []string {
	projectResources := map[string]bool{
		permissions.ResourceAgent:          true,
		permissions.ResourceProject:        true,
		permissions.ResourceTemplate:       true,
		permissions.ResourceHarnessConfig:  true,
		permissions.ResourceSkill:          true,
		permissions.ResourceScheduledEvent: true,
	}
	// Explicit permission IDs excluded from this role regardless of action.
	// agent.set_message_mode must NOT be held by project admins — only project
	// owners may unseal none-mode agents (D7).
	excludeIDs := map[string]bool{
		"agent.set_message_mode": true,
	}
	var ids []string
	for _, p := range permissions.Registry {
		if projectResources[p.Resource] && p.Action != excludeAction && !excludeIDs[p.ID] {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// projectMemberPermissionIDs returns the permission IDs that a regular
// project member gets: create agents, read/list most things.
func projectMemberPermissionIDs() []string {
	memberActions := map[string]bool{
		"create":  true,
		"read":    true,
		"list":    true,
		"message": true,
	}
	projectResources := map[string]bool{
		permissions.ResourceAgent:          true,
		permissions.ResourceProject:        true,
		permissions.ResourceTemplate:       true,
		permissions.ResourceHarnessConfig:  true,
		permissions.ResourceSkill:          true,
		permissions.ResourceScheduledEvent: true,
	}
	var ids []string
	for _, p := range permissions.Registry {
		if projectResources[p.Resource] && memberActions[p.Action] {
			ids = append(ids, p.ID)
		}
	}
	// Also include stop_all for project members (matching existing policy)
	ids = append(ids, "agent.stop_all")
	return ids
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

// BackfillRoleBindings creates role bindings from existing User.Role values and
// project group memberships. It is idempotent (skips if binding already exists)
// and called from the startup/migration path.
func BackfillRoleBindings(ctx context.Context, s store.Store) error {
	// Backfill system role bindings from User.Role
	if err := backfillUserRoleBindings(ctx, s); err != nil {
		return fmt.Errorf("backfill user role bindings: %w", err)
	}

	return nil
}

// backfillUserRoleBindings creates system-scoped role bindings from User.Role.
// It paginates through all users to avoid silent truncation by store defaults.
func backfillUserRoleBindings(ctx context.Context, s store.Store) error {
	userRoleMap := map[string]string{
		"admin":  store.SystemRoleSuperAdmin,
		"member": store.SystemRoleHubMember,
		"viewer": store.SystemRoleHubViewer,
	}

	var cursor string
	var created int
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
			roleName, ok := userRoleMap[u.Role]
			if !ok {
				continue
			}

			rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
			if err != nil {
				slog.Warn("role definition not found during backfill", "role", roleName, "error", err)
				continue
			}

			_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: rd.ID,
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      u.ID,
				ScopeType:        store.RoleScopeSystem,
				ScopeID:          "",
				CreatedBy:        "system-backfill",
			})
			if err != nil {
				if errors.Is(err, store.ErrAlreadyExists) {
					continue // already backfilled
				}
				slog.Warn("failed to create role binding during backfill",
					"user_id", u.ID, "role", roleName, "error", err)
				continue
			}
			created++
		}

		if users.NextCursor == "" {
			break
		}
		cursor = users.NextCursor
	}

	if created > 0 {
		slog.Info("backfilled user role bindings", "created", created)
	}
	return nil
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
//	    Role from "admin" to "member" (if needed) and delete any super-admin
//	    binding. Ordinary grants (non-super-admin) are NOT touched.
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
func ReconcileSuperAdminBindings(ctx context.Context, s store.Store, adminEmails []string) (demotionSafe bool, err error) {
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
		// Pre-scan: count existing users who match AdminEmails. This is the
		// intended admin set — the set of users who WILL be admin after
		// reconciliation completes.
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
			// match nobody. Proceeding would demote every current admin,
			// leaving zero administrators. Refuse.
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
				// Reverse: demote role and delete super-admin binding.
				if u.Role == "admin" {
					slog.Warn("demoting user: removed from AdminEmails",
						"user_id", u.ID, "email", u.Email, "old_role", "admin", "new_role", "member")
					u.Role = "member"
					if err := s.UpdateUser(ctx, u); err != nil {
						slog.Warn("failed to demote user during reconciliation",
							"user_id", u.ID, "error", err)
					} else {
						demoted++
					}
				}
				// Delete orphaned super-admin bindings for users NOT in adminEmails.
				// Only the super-admin binding is touched — ordinary grants are preserved.
				bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID)
				if err != nil {
					continue
				}
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
// already exist. Shipped with DefaultValue=0 (unlimited) for discoverability
// per sponsor decision OQ-2 Option B. It is called once during Hub
// initialization and is idempotent.
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

// =============================================================================
// Backward-compatible aliases (PG1)
// =============================================================================
//
// These functions are kept as aliases so that test files outside this package's
// ownership can continue to call them without modification. They delegate to
// the new reconciliation and seeding functions.

// seedRoleDefinitions is a backward-compatible alias for reconcileBuiltInRoles.
// Tests and external callers that need to seed role definitions should use this.
func seedRoleDefinitions(ctx context.Context, s store.Store) {
	reconcileBuiltInRoles(ctx, s)
}

// =============================================================================
// PG1 → RG1 Conversion Points: Progeny Policy to Relationship Grant
// =============================================================================
//
// The following progeny policy creation sites must be converted to named
// built-in relationship grants by the RG1 developer (af-rg1-dev). PG1
// documents the conversion points here; the actual conversion happens in RG1.
//
// Conversion site 1: handlers_env_secrets.go:ensureProgenyPolicy
//   Policy: progeny-secret-access:<secretID>
//   ResourceType: secret, Action: read
//   Condition: DelegatedFrom user (secret creator)
//   Target: lineage/progeny resolver checks agent ancestry against creator
//
// Conversion site 2: handlers_env_secrets.go:ensureEnvVarProgenyPolicy
//   Policy: progeny-envvar-access:<envVarID>
//   ResourceType: envvar, Action: read
//   Condition: DelegatedFrom user (env var creator)
//   Target: same lineage/progeny resolver
//
// Conversion site 3: handlers_skills_injection.go:ensureSkillProgenyPolicy
//   Policy: progeny-skill-access:<skillInjectionID>
//   ResourceType: skill_injection, Action: read
//   Condition: DelegatedFrom user (skill injection creator)
//   Target: same lineage/progeny resolver
//
// All three follow the same pattern: the policy grants read access to a
// specific resource (by ResourceID) to agents whose ancestry chain includes
// the resource creator. The DelegatedFrom condition is checked by
// checkDelegation (authz.go:781-847).
//
// The target relationship grant resolver must:
// 1. Accept (agentIdentity, resource) as inputs
// 2. Check if the agent's ancestry/delegation chain includes the resource creator
// 3. Return allow with "relationship grant: progeny access" provenance
// 4. NOT use the Policy or PolicyBinding tables
//
// Until RG1 is available, these policy creation sites remain active and the
// current evaluator's checkDelegation path handles them.
