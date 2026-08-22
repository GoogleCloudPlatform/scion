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
	"fmt"
	"log/slog"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/notification"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/notificationsubscription"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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
	*PolicyStore
	*BrokerDispatchStore
	*LifecycleHookStore
	*SkillStore
	*SkillRegistryStore
	*HubSettingStore
	*SkillInjectionStore
	*ProjectPreStartHookStore
	*AgentSessionMetricsStore

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
		PolicyStore:              NewPolicyStore(client),
		BrokerDispatchStore:      NewBrokerDispatchStore(client),
		LifecycleHookStore:       NewLifecycleHookStore(client),
		SkillStore:               NewSkillStore(client),
		SkillRegistryStore:       NewSkillRegistryStore(client),
		HubSettingStore:          NewHubSettingStore(client),
		SkillInjectionStore:      NewSkillInjectionStore(client),
		ProjectPreStartHookStore: NewProjectPreStartHookStore(client),
		AgentSessionMetricsStore: NewAgentSessionMetricsStore(client),
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

	if err := entc.AutoMigrate(ctx, c.client); err != nil {
		return err
	}

	if err := c.BackfillEmptyAgentRoles(ctx); err != nil {
		return fmt.Errorf("empty agent role backfill: %w", err)
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
	return nil
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
