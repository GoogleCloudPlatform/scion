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

package entadapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/delegationedge"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	entgroup "github.com/GoogleCloudPlatform/scion/pkg/ent/group"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/notification"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/notificationsubscription"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const emptyAgentRoleBackfillMarkerSection = "migration_empty_agent_roles_backfilled"
const delegationEdgeBackfillMarkerSection = "migration_delegation_edge_backfill_v1"
const projectMembersGroupMarkerBackfillSection = "backfill_project_group_markers_done"
const systemProjectMembersGroupAnnotation = "scion.io/system-project-members-group"
const projectAgentsGroupMarkerBackfillSection = "migration_project_agents_group_markers_backfilled"
const systemProjectAgentsGroupAnnotation = "scion.io/project-agents-group"
const adoptionReviewRequiredAnnotation = "scion.io/adoption-review-required"

// CompositeStore is a fully Ent-backed implementation of store.Store. Every
// domain is served by a dedicated Ent sub-store; CompositeStore embeds them so
// their methods are promoted to satisfy the store.Store interface, while the
// store-level Close/Ping/Migrate operations act on the shared Ent client.
//
// There is no longer a separate raw-SQL store: all Hub state lives in a single
// Ent database.
type CompositeStore struct {
	*AgentStore
	*ProjectStore
	*UserStore
	*SecretStore
	*TemplateStore
	*NotificationStore
	*ScheduleStore
	*MaintenanceStore
	*MessageStore
	*ExternalStore
	*BrokerSecretStore
	*AllowListStore
	*GroupStore
	*BrokerDispatchStore
	*LifecycleHookStore
	*SkillStore
	*SkillRegistryStore
	*HubSettingStore
	*SkillInjectionStore
	*ProjectPreStartHookStore
	*AgentSessionMetricsStore
	*ConversationStore
	*RoleStore
	*DelegationEdgeStore
	*AgentCredentialStore
	*DecisionAuditStore
	*MutationAuditStore
	*QuotaStore
	*AccessConstraintStore

	client *ent.Client
}

// Compile-time assertion that CompositeStore satisfies the full store.Store
// interface purely through its embedded Ent-backed sub-stores.
var _ store.Store = (*CompositeStore)(nil)

// NewCompositeStore creates a store.Store backed entirely by the given Ent
// client. Each domain sub-store shares the same client and therefore the same
// underlying database, so cross-domain foreign keys (e.g. group -> project,
// agent -> project) resolve natively without any shadow synchronization.
func NewCompositeStore(client *ent.Client) *CompositeStore {
	return &CompositeStore{
		AgentStore:               NewAgentStore(client),
		ProjectStore:             NewProjectStore(client),
		UserStore:                NewUserStore(client),
		SecretStore:              NewSecretStore(client),
		TemplateStore:            NewTemplateStore(client),
		NotificationStore:        NewNotificationStore(client),
		ScheduleStore:            NewScheduleStore(client),
		MaintenanceStore:         NewMaintenanceStore(client),
		MessageStore:             NewMessageStore(client),
		ExternalStore:            NewExternalStore(client),
		BrokerSecretStore:        NewBrokerSecretStore(client),
		AllowListStore:           NewAllowListStore(client),
		GroupStore:               NewGroupStore(client),
		BrokerDispatchStore:      NewBrokerDispatchStore(client),
		LifecycleHookStore:       NewLifecycleHookStore(client),
		SkillStore:               NewSkillStore(client),
		SkillRegistryStore:       NewSkillRegistryStore(client),
		HubSettingStore:          NewHubSettingStore(client),
		SkillInjectionStore:      NewSkillInjectionStore(client),
		ProjectPreStartHookStore: NewProjectPreStartHookStore(client),
		AgentSessionMetricsStore: NewAgentSessionMetricsStore(client),
		ConversationStore:        NewConversationStore(client),
		RoleStore:                NewRoleStore(client),
		DelegationEdgeStore:      NewDelegationEdgeStore(client),
		AgentCredentialStore:     NewAgentCredentialStore(client),
		DecisionAuditStore:       NewDecisionAuditStore(client),
		MutationAuditStore:       NewMutationAuditStore(client),
		QuotaStore:               NewQuotaStore(client),
		AccessConstraintStore:    NewAccessConstraintStore(client),
		client:                   client,
	}
}

// DeleteAgent hard-deletes an agent and cascade-deletes its notification
// subscriptions and notifications. The former raw-SQL store enforced this via
// ON DELETE CASCADE foreign keys (notification_subscriptions.agent_id ->
// agents(id), notifications.subscription_id -> notification_subscriptions(id)).
// In the Ent schema agent_id is a plain field with no edge, so the cascade is
// performed explicitly here to preserve store parity. Soft delete goes through
// UpdateAgent and is unaffected, so subscriptions are retained for soft-deleted
// agents.
func (c *CompositeStore) DeleteAgent(ctx context.Context, id string) error {
	if err := c.AgentStore.DeleteAgent(ctx, id); err != nil {
		return err
	}
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if _, err := c.client.Notification.Delete().
		Where(notification.AgentIDEQ(uid)).Exec(ctx); err != nil {
		return err
	}
	if _, err := c.client.NotificationSubscription.Delete().
		Where(notificationsubscription.AgentIDEQ(uid)).Exec(ctx); err != nil {
		return err
	}
	return nil
}

// DeleteProject deletes a project and cascade-deletes its agents (and each
// agent's notification subscriptions/notifications). The former raw-SQL store
// enforced this via agents.grove_id -> groves(id) ON DELETE CASCADE; the Ent
// project->agents edge has no DB-level cascade, so deleting a project while
// agents still reference it would fail with a foreign-key violation. The bulk
// agent delete is a hard delete, so it also removes soft-deleted agents.
func (c *CompositeStore) DeleteProject(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	agentIDs, err := c.client.Agent.Query().Where(agent.ProjectIDEQ(uid)).IDs(ctx)
	if err != nil {
		return err
	}
	if len(agentIDs) > 0 {
		if _, err := c.client.Notification.Delete().
			Where(notification.AgentIDIn(agentIDs...)).Exec(ctx); err != nil {
			return err
		}
		if _, err := c.client.NotificationSubscription.Delete().
			Where(notificationsubscription.AgentIDIn(agentIDs...)).Exec(ctx); err != nil {
			return err
		}
		if _, err := c.client.Agent.Delete().
			Where(agent.ProjectIDEQ(uid)).Exec(ctx); err != nil {
			return err
		}
	}
	return c.ProjectStore.DeleteProject(ctx, id)
}

// Close closes the underlying Ent client.
func (c *CompositeStore) Close() error {
	return c.client.Close()
}

// Ping verifies connectivity to the underlying database.
func (c *CompositeStore) Ping(ctx context.Context) error {
	drv, ok := c.client.Driver().(*entsql.Driver)
	if !ok {
		return fmt.Errorf("ent client driver does not expose a *sql.DB for ping")
	}
	return drv.DB().PingContext(ctx)
}

// Migrate runs Ent's automatic schema migration against the shared client and
// seeds the built-in maintenance operations, matching the behavior of the
// former raw-SQL store (which seeded these as part of its migrations).
func (c *CompositeStore) Migrate(ctx context.Context) error {
	// Backfill null scope_id to empty string before dedup and schema migration.
	// Must run BEFORE dedup: in SQL NULL != NULL, so dedup won't detect
	// duplicate rows where scope_id IS NULL. Converting to '' first lets
	// dedup catch all real duplicates. Also prevents SQLSTATE 23502 when
	// the schema migration applies the NOT NULL constraint.
	if db := c.DB(); db != nil {
		exists, err := c.accessPoliciesTableExists(ctx, db)
		if err != nil {
			return fmt.Errorf("pre-migration null scope_id check: %w", err)
		}
		if exists {
			result, err := db.ExecContext(ctx,
				"UPDATE access_policies SET scope_id = '' WHERE scope_id IS NULL")
			if err != nil {
				return fmt.Errorf("pre-migration null scope_id backfill: %w", err)
			}
			if n, _ := result.RowsAffected(); n > 0 {
				slog.Info("backfilled null scope_id before migration", "rows_updated", n)
			}
		}
	}

	// Deduplicate access_policies before migration adds a unique index.
	// Existing databases may have duplicate (name, scope_type, scope_id) rows
	// (including former NULL scope_id rows now normalized to '') which would
	// cause the UNIQUE constraint migration to fail.
	if err := c.deduplicateAccessPolicies(ctx); err != nil {
		return fmt.Errorf("pre-migration dedup: %w", err)
	}

	// Deduplicate delegation_edges before migration adds a partial unique index.
	// Existing databases that ran the initial backfill and were interrupted may
	// have duplicate active edges that would violate the new constraint.
	if err := c.deduplicateDelegationEdges(ctx); err != nil {
		return fmt.Errorf("pre-migration delegation edge dedup: %w", err)
	}

	if err := entc.AutoMigrate(ctx, c.client); err != nil {
		return err
	}

	if err := c.BackfillEmptyAgentRoles(ctx); err != nil {
		return fmt.Errorf("empty agent role backfill: %w", err)
	}
	if err := c.BackfillDelegationEdges(ctx); err != nil {
		return fmt.Errorf("delegation edge backfill: %w", err)
	}
	if err := c.BackfillProjectMembersGroupMarkers(ctx); err != nil {
		return fmt.Errorf("project members group marker backfill: %w", err)
	}
	if err := c.BackfillProjectAgentsGroupMarkers(ctx); err != nil {
		return fmt.Errorf("project agents group marker backfill: %w", err)
	}

	// Migrate AllowListEntry records to User(status=invited) records.
	// Runs after schema migration (which adds the "invited" status enum value)
	// so the new status is available. Idempotent — safe to run on every startup.
	if err := c.MigrateAllowListToInvitedUsers(ctx); err != nil {
		slog.Error("allowlist→invited migration failed (non-fatal)", "error", err)
	}

	// Data backfills belong here rather than at the call site: on Postgres this
	// runs under the schema-migration advisory lock, so replicas booting
	// together do not race, and the columns being read are guaranteed to exist.
	if err := c.BackfillGCPVerificationStatus(ctx); err != nil {
		return err
	}
	return c.SeedMaintenanceOperations(ctx)
}

// BackfillEmptyAgentRoles preserves access for pre-role agents before missing
// roles start resolving to the least-privileged role at runtime.
func (c *CompositeStore) BackfillEmptyAgentRoles(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, emptyAgentRoleBackfillMarkerSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	agents, err := c.client.Agent.Query().All(ctx)
	if err != nil {
		return err
	}

	var updated int
	for _, a := range agents {
		cfg := &store.AgentAppliedConfig{}
		if a.AppliedConfig != "" {
			parsed, err := parseAppliedConfig(a.AppliedConfig)
			if err != nil {
				return fmt.Errorf("parse applied_config for agent %s: %w", a.ID, err)
			}
			if parsed != nil {
				cfg = parsed
			}
		}
		if cfg.AgentRole != "" {
			continue
		}
		cfg.AgentRole = "full"
		cfg.AgentRoleGrandfathered = true
		if err := c.client.Agent.UpdateOneID(a.ID).
			SetAppliedConfig(marshalAppliedConfig(cfg)).
			Exec(ctx); err != nil {
			return err
		}
		updated++
	}
	if updated > 0 {
		slog.Info("backfilled empty agent roles before role default change", "rows_updated", updated)
	}
	_, err = c.UpsertHubSetting(ctx, emptyAgentRoleBackfillMarkerSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// BackfillDelegationEdges creates delegation edges for all existing agents,
// preserving their current authority as the baseline. This must run AFTER
// BackfillEmptyAgentRoles so that role values are populated.
//
// Sponsor decision: OPTION A — grandfather all existing agents with current
// authority. The live delegation ceiling applies going forward. No permanent
// exemption. If an ancestor loses authority after migration, grandfathered
// agents lose it too.
//
// Edge construction logic:
//   - Delegator is determined from agent provenance (CreatedBy → user, Ancestry → parent agent)
//   - For ambiguous provenance, a "system/migration" delegator is used
//   - All edges are marked grandfathered=true
//   - scope_type=project, scope_id=agent.ProjectID, role=current effective role
func (c *CompositeStore) BackfillDelegationEdges(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, delegationEdgeBackfillMarkerSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	const pageSize = 500
	var offset int
	var totalCreated int

	for {
		agents, err := c.client.Agent.Query().
			Order(ent.Asc(agent.FieldID)).
			Limit(pageSize).
			Offset(offset).
			All(ctx)
		if err != nil {
			return fmt.Errorf("query agents for delegation edge backfill: %w", err)
		}

		for _, a := range agents {
			// Parse the agent's applied config to get the effective role.
			// Default to "none" (floor), NOT "full" — this is a minting
			// operation that creates durable authority records. Fail closed.
			role := "none"
			if a.AppliedConfig != "" {
				parsed, err := parseAppliedConfig(a.AppliedConfig)
				if err != nil {
					// Parse failure: skip this agent entirely (fail closed).
					// Do not create a delegation edge from unparseable data.
					slog.Error("delegation edge backfill: failed to parse applied_config, skipping agent (fail closed)",
						"agent_id", a.ID, "error", err)
					continue
				} else if parsed != nil && parsed.AgentRole != "" {
					role = parsed.AgentRole
				} else {
					slog.Info("delegation edge backfill: empty/no AgentRole in config, using role=none",
						"agent_id", a.ID)
				}
			} else {
				slog.Info("delegation edge backfill: empty AppliedConfig, using role=none",
					"agent_id", a.ID)
			}

			// Determine the delegator from agent provenance.
			delegatorType, delegatorID := determineDelegator(a)

			// Idempotency: check for an existing active edge before
			// creating. If the migration was interrupted before writing
			// the completion marker, a restart re-processes agents from
			// offset 0. Without this check, duplicates would be created.
			existing, qErr := c.client.DelegationEdge.Query().
				Where(
					delegationedge.DelegateTypeEQ(delegationedge.DelegateTypeAgent),
					delegationedge.DelegateIDEQ(a.ID.String()),
					delegationedge.ScopeTypeEQ(delegationedge.ScopeTypeProject),
					delegationedge.ScopeIDEQ(a.ProjectID.String()),
					delegationedge.ActiveEQ(true),
				).
				First(ctx)
			if qErr == nil && existing != nil {
				// Active edge already exists — skip.
				slog.Debug("delegation edge backfill: active edge already exists, skipping",
					"agent_id", a.ID, "edge_id", existing.ID)
				continue
			}

			// Create the delegation edge. Fail the entire migration on
			// write errors — a partial backfill leaves agents without
			// edges, which is a security gap.
			_, err := c.client.DelegationEdge.Create().
				SetDelegatorType(delegationedge.DelegatorType(delegatorType)).
				SetDelegatorID(delegatorID).
				SetDelegateType(delegationedge.DelegateTypeAgent).
				SetDelegateID(a.ID.String()).
				SetScopeType(delegationedge.ScopeTypeProject).
				SetScopeID(a.ProjectID.String()).
				SetRole(role).
				SetActive(true).
				SetGrandfathered(true).
				Save(ctx)
			if err != nil {
				return fmt.Errorf("delegation edge backfill: failed to create edge for agent %s: %w", a.ID, err)
			}
			totalCreated++
		}

		if len(agents) < pageSize {
			break
		}
		offset += pageSize
	}

	if totalCreated > 0 {
		slog.Info("backfilled delegation edges for existing agents", "edges_created", totalCreated)
	}

	_, err := c.UpsertHubSetting(ctx, delegationEdgeBackfillMarkerSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// determineDelegator determines the delegator type and ID for a delegation edge
// based on agent provenance:
//  1. If CreatedBy is set → delegator_type="user", delegator_id=CreatedBy
//  2. If Ancestry has a parent agent (len >= 2, second-to-last entry) and
//     CreatedBy differs from Ancestry[0] → delegator_type="agent", delegator_id=parent
//  3. For ambiguous provenance → delegator_type="user", delegator_id="system/migration"
func determineDelegator(a *ent.Agent) (string, string) {
	const systemMigrationPrincipal = "system/migration"

	// If CreatedBy is set, that's the primary provenance signal.
	if a.CreatedBy != nil {
		createdByStr := a.CreatedBy.String()

		// Check if this was created by another agent (ancestry shows it).
		// Ancestry is [root_user, ..., parent_agent] — the parent is the
		// second-to-last element (the last element is the agent itself in
		// some conventions, but typically the parent is the last in the ancestry).
		if len(a.Ancestry) >= 2 {
			// If CreatedBy matches an element that's not the root user,
			// it was likely created by an agent.
			parentCandidate := a.Ancestry[len(a.Ancestry)-1]
			if parentCandidate == createdByStr && parentCandidate != a.Ancestry[0] {
				return store.DelegationPrincipalAgent, parentCandidate
			}
		}

		// CreatedBy is a user
		return store.DelegationPrincipalUser, createdByStr
	}

	// No CreatedBy — check if we can infer from Ancestry.
	if len(a.Ancestry) > 0 {
		// Use root user from ancestry as the delegator.
		return store.DelegationPrincipalUser, a.Ancestry[0]
	}

	// Ambiguous provenance — use system/migration principal.
	// This ensures the agent participates in the delegation model and is
	// NOT silently unbounded (absent edge = no authority for new lookups).
	return store.DelegationPrincipalUser, systemMigrationPrincipal
}

// BackfillProjectMembersGroupMarkers marks legitimate pre-upgrade project
// members groups so project registration can safely reuse them.
func (c *CompositeStore) BackfillProjectMembersGroupMarkers(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, projectMembersGroupMarkerBackfillSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	groups, err := c.client.Group.Query().
		Where(
			entgroup.SlugHasPrefix("project:"),
			entgroup.SlugHasSuffix(":members"),
		).
		All(ctx)
	if err != nil {
		return err
	}

	var updated, skipped int
	for _, g := range groups {
		if g.ProjectID == nil {
			skipped++
			slog.Warn("skipping unowned project members group marker backfill",
				"group", g.ID, "slug", g.Slug)
			continue
		}
		project, err := c.GetProject(ctx, g.ProjectID.String())
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				skipped++
				slog.Warn("skipping project members group marker backfill for missing project",
					"group", g.ID, "slug", g.Slug, "project_id", g.ProjectID.String())
				continue
			}
			return err
		}
		if expectedSlug := "project:" + project.Slug + ":members"; g.Slug != expectedSlug {
			skipped++
			slog.Warn("skipping mismatched project members group marker backfill",
				"group", g.ID, "slug", g.Slug, "project_id", project.ID, "expected_slug", expectedSlug)
			continue
		}
		if g.Annotations[systemProjectMembersGroupAnnotation] == "true" {
			continue
		}
		annotations := make(map[string]string, len(g.Annotations)+1)
		for k, v := range g.Annotations {
			annotations[k] = v
		}
		annotations[systemProjectMembersGroupAnnotation] = "true"
		if err := c.client.Group.UpdateOneID(g.ID).
			SetAnnotations(annotations).
			Exec(ctx); err != nil {
			return err
		}
		updated++
	}
	slog.Info("backfilled project members group system markers",
		"rows_updated", updated, "rows_skipped", skipped)

	_, err = c.UpsertHubSetting(ctx, projectMembersGroupMarkerBackfillSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// BackfillProjectAgentsGroupMarkers marks legitimate pre-upgrade project
// agents groups with the system annotation so the hardened adoption logic
// accepts them. Uses its own marker section, independent of the members
// group backfill.
func (c *CompositeStore) BackfillProjectAgentsGroupMarkers(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, projectAgentsGroupMarkerBackfillSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	groups, err := c.client.Group.Query().
		Where(
			entgroup.SlugHasPrefix("project:"),
			entgroup.SlugHasSuffix(":agents"),
		).
		All(ctx)
	if err != nil {
		return err
	}

	var updated, skipped int
	for _, g := range groups {
		if g.ProjectID == nil {
			skipped++
			slog.Warn("skipping unowned project agents group marker backfill",
				"group_id", g.ID, "slug", g.Slug)
			continue
		}
		project, err := c.GetProject(ctx, g.ProjectID.String())
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				skipped++
				slog.Warn("skipping project agents group marker backfill for missing project",
					"group_id", g.ID, "slug", g.Slug, "project_id", g.ProjectID.String())
				continue
			}
			return err
		}
		if expectedSlug := "project:" + project.Slug + ":agents"; g.Slug != expectedSlug {
			skipped++
			slog.Warn("skipping mismatched project agents group marker backfill",
				"group_id", g.ID, "slug", g.Slug, "project_id", project.ID, "expected_slug", expectedSlug)
			continue
		}

		// Check for owner mismatch: the signature of a previously-adopted squat.
		groupOwner := ""
		if g.OwnerID != nil {
			groupOwner = g.OwnerID.String()
		}
		ownerMismatch := groupOwner != "" && project.OwnerID != "" && groupOwner != project.OwnerID
		if ownerMismatch {
			slog.Warn("project agents group owner differs from project owner - manual review recommended",
				"group_id", g.ID, "slug", g.Slug, "project_id", project.ID,
				"group_owner", groupOwner, "project_owner", project.OwnerID)
		}

		if g.Annotations[systemProjectAgentsGroupAnnotation] == "true" {
			// D8-fix: even if the system annotation already exists, ensure the
			// adoption-review annotation is set on mismatch (handles interrupted
			// backfills or later ownership changes).
			if ownerMismatch && g.Annotations[adoptionReviewRequiredAnnotation] != "true" {
				annotations := make(map[string]string, len(g.Annotations)+1)
				for k, v := range g.Annotations {
					annotations[k] = v
				}
				annotations[adoptionReviewRequiredAnnotation] = "true"
				if err := c.client.Group.UpdateOneID(g.ID).
					SetAnnotations(annotations).
					Exec(ctx); err != nil {
					return err
				}
				slog.Warn("added adoption-review-required annotation to already-marked group",
					"group_id", g.ID, "slug", g.Slug, "project_id", project.ID,
					"group_owner", groupOwner, "project_owner", project.OwnerID)
				updated++
			} else {
				slog.Info("project agents group already marked",
					"group_id", g.ID, "slug", g.Slug, "project_id", project.ID, "owner_id", groupOwner)
			}
			continue
		}
		annotations := make(map[string]string, len(g.Annotations)+2)
		for k, v := range g.Annotations {
			annotations[k] = v
		}
		annotations[systemProjectAgentsGroupAnnotation] = "true"
		// D8-fix: when an owner mismatch is detected, add a durable annotation
		// so operators can query for suspect groups after the fact instead of
		// grepping startup logs. The group is still marked (not refused) because
		// project ownership transfers legitimately change the project owner
		// without changing the group owner.
		if ownerMismatch {
			annotations[adoptionReviewRequiredAnnotation] = "true"
		}
		if err := c.client.Group.UpdateOneID(g.ID).
			SetAnnotations(annotations).
			Exec(ctx); err != nil {
			return err
		}
		slog.Info("backfilled project agents group system marker",
			"group_id", g.ID, "slug", g.Slug, "project_id", project.ID, "owner_id", groupOwner)
		updated++
	}
	slog.Info("backfilled project agents group system markers",
		"rows_updated", updated, "rows_skipped", skipped)

	_, err = c.UpsertHubSetting(ctx, projectAgentsGroupMarkerBackfillSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// DB returns the underlying *sql.DB, or nil if the client is not backed by a
// database/sql driver. It is an escape hatch for diagnostics and tests that
// need raw SQL access; production code should use the typed store methods.
func (c *CompositeStore) DB() *sql.DB {
	if drv, ok := c.client.Driver().(*entsql.Driver); ok {
		return drv.DB()
	}
	return nil
}
