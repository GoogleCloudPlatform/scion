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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// provenanceEdgeColumns lists the provenance, effect-ceiling and deactivation
// columns on delegation_edges. A database that predates them has none.
var provenanceEdgeColumns = []string{
	"provenance_version", "source_principal_kind", "source_principal_id",
	"source_credential_kind", "source_credential_id", "source_event_id",
	"source_schedule_id", "source_authorization_revision",
	"initiator_principal_kind", "initiator_principal_id",
	"initiator_credential_kind", "initiator_credential_id",
	"ceiling_kind", "ceiling_version", "ceiling_permission_ids",
	"ceiling_boundary_kind", "ceiling_boundary_project_id",
	"ceiling_source_expires_at",
	"deactivation_cause", "deactivated_at", "deactivation_op_id",
}

const (
	legacyEdgeID     = "22222222-2222-2222-2222-222222222222"
	legacyEdgeAgent  = "agent-legacy"
	legacyEdgeParent = "user-legacy"
)

// assertLegacyEdgeUnrecorded reads the pre-existing edge back through the
// store and checks that every recorded field is the unrecorded zero value.
func assertLegacyEdgeUnrecorded(t *testing.T, client *ent.Client) {
	t.Helper()
	ctx := context.Background()
	edges, err := NewDelegationEdgeStore(client).GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, legacyEdgeAgent)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	e := edges[0]
	assert.Equal(t, legacyEdgeID, e.ID)
	assert.Equal(t, legacyEdgeParent, e.DelegatorID)
	assert.Equal(t, store.EffectCeilingUnrecorded, e.Kind)
	assert.Nil(t, e.PermissionIDs)
	assert.Equal(t, permissions.CeilingVersionUnspecified, e.Version)
	_, ok := e.Frozen()
	assert.False(t, ok, "an unrecorded edge must not yield a frozen ceiling")
	assert.Equal(t, store.AuthorityProvenance{}, e.AuthorityProvenance)
	assert.Equal(t, store.Deactivation{}, e.Deactivation)
}

// TestMigrationExistingEdgesUnrecorded upgrades a delegation_edges table
// that predates the provenance columns and checks that an existing edge
// reads back as unrecorded. The SQLite case always runs. The Postgres case
// runs under -tags integration with SCION_TEST_POSTGRES_URL set.
func TestMigrationExistingEdgesUnrecorded(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
		ctx := context.Background()

		raw, err := sql.Open("sqlite", dsn)
		require.NoError(t, err)
		// Frozen snapshot of the table as deployed before the provenance
		// columns. Do not regenerate it from the current schema.
		old := []string{
			"CREATE TABLE `delegation_edges` (" +
				"`id` uuid NOT NULL, " +
				"`delegator_type` text NOT NULL, " +
				"`delegator_id` text NOT NULL, " +
				"`delegate_type` text NOT NULL, " +
				"`delegate_id` text NOT NULL, " +
				"`scope_type` text NOT NULL, " +
				"`scope_id` text NOT NULL DEFAULT (''), " +
				"`role` text NOT NULL, " +
				"`active` bool NOT NULL DEFAULT (true), " +
				"`grandfathered` bool NOT NULL DEFAULT (false), " +
				"`created` datetime NOT NULL, " +
				"`updated` datetime NOT NULL, " +
				"PRIMARY KEY (`id`))",
			"CREATE INDEX `delegationedge_delegate_type_delegate_id` ON `delegation_edges` (`delegate_type`, `delegate_id`)",
			"CREATE INDEX `delegationedge_delegator_type_delegator_id` ON `delegation_edges` (`delegator_type`, `delegator_id`)",
			"CREATE UNIQUE INDEX `delegationedge_delegate_type_delegate_id_scope_type_scope_id` ON `delegation_edges` (`delegate_type`, `delegate_id`, `scope_type`, `scope_id`) WHERE active = true",
			"CREATE INDEX `delegationedge_delegator_type_delegator_id_active` ON `delegation_edges` (`delegator_type`, `delegator_id`, `active`)",
			"INSERT INTO delegation_edges (id, delegator_type, delegator_id, delegate_type, delegate_id, scope_type, scope_id, role, active, grandfathered, created, updated) " +
				"VALUES ('" + legacyEdgeID + "','user','" + legacyEdgeParent + "','agent','" + legacyEdgeAgent + "','project','proj-1','full',true,false,'2026-01-01 00:00:00+00:00','2026-01-01 00:00:00+00:00')",
		}
		for _, stmt := range old {
			_, err := raw.ExecContext(ctx, stmt)
			require.NoError(t, err, stmt)
		}
		require.NoError(t, raw.Close())

		client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, entc.AutoMigrate(ctx, client))

		assertLegacyEdgeUnrecorded(t, client)
	})

	t.Run("postgres", func(t *testing.T) {
		if !enttest.Active() {
			t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres migration case")
		}
		ctx := context.Background()
		dsn := enttest.NewSchemaURL(t)
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })

		// Return the table to its pre-provenance shape, then insert an
		// edge the way the earlier schema wrote it.
		drops := make([]string, len(provenanceEdgeColumns))
		for i, c := range provenanceEdgeColumns {
			drops[i] = "DROP COLUMN " + c
		}
		_, err = db.ExecContext(ctx, "ALTER TABLE delegation_edges "+strings.Join(drops, ", "))
		require.NoError(t, err)
		_, err = db.ExecContext(ctx,
			`INSERT INTO delegation_edges (id, delegator_type, delegator_id, delegate_type, delegate_id, scope_type, scope_id, role, active, grandfathered, created, updated)
			 VALUES ($1, 'user', $2, 'agent', $3, 'project', 'proj-1', 'full', true, false, now(), now())`,
			legacyEdgeID, legacyEdgeParent, legacyEdgeAgent)
		require.NoError(t, err)

		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, entc.AutoMigrate(ctx, client))

		assertLegacyEdgeUnrecorded(t, client)
	})
}

// TestDelegationEdgeStoreRoundTripsProvenance checks that every recorded
// field survives a write and a read, and that the empty bounded list is
// kept distinct from "no list".
func TestDelegationEdgeStoreRoundTripsProvenance(t *testing.T) {
	ctx := context.Background()
	s := NewDelegationEdgeStore(enttest.NewClient(t))
	expires := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	bounded := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: "user-1",
		DelegateType: store.DelegationPrincipalAgent, DelegateID: "agent-bounded",
		ScopeType: "project", ScopeID: "proj-1", Role: "baseline", Active: true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:       store.ProvenanceVersionV1,
			SourcePrincipalKind:     "user",
			SourcePrincipalID:       "user-1",
			SourceCredentialKind:    store.SourceCredentialUAT,
			SourceCredentialID:      "uat-1",
			InitiatorPrincipalKind:  "user",
			InitiatorPrincipalID:    "user-1",
			InitiatorCredentialKind: store.InitiatorCredentialKindUAT,
			InitiatorCredentialID:   "uat-1",
		},
		EffectCeiling: store.EffectCeiling{
			Kind:              store.EffectCeilingBounded,
			Version:           permissions.CeilingVersionV1,
			PermissionIDs:     []string{"agent.create", "project.read"},
			BoundaryKind:      "project",
			BoundaryProjectID: "proj-1",
			SourceExpiresAt:   &expires,
		},
	}
	require.NoError(t, s.CreateDelegationEdge(ctx, bounded))

	emptyBounded := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: "user-1",
		DelegateType: store.DelegationPrincipalAgent, DelegateID: "agent-empty",
		ScopeType: "project", ScopeID: "proj-1", Role: "none", Active: true,
		AuthorityProvenance: store.AuthorityProvenance{ProvenanceVersion: store.ProvenanceVersionV1},
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1},
	}
	require.NoError(t, s.CreateDelegationEdge(ctx, emptyBounded))

	principal := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: "dev-user",
		DelegateType: store.DelegationPrincipalAgent, DelegateID: "agent-principal",
		ScopeType: "project", ScopeID: "proj-1", Role: "full", Active: true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  "user",
			SourcePrincipalID:    "dev-user",
			SourceCredentialKind: store.SourceCredentialDevLocal,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, s.CreateDelegationEdge(ctx, principal))

	read := func(id string) *store.DelegationEdge {
		edges, err := s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, id)
		require.NoError(t, err)
		require.Len(t, edges, 1)
		return edges[0]
	}

	got := read("agent-bounded")
	assert.Equal(t, bounded.AuthorityProvenance, got.AuthorityProvenance)
	assert.Equal(t, store.EffectCeilingBounded, got.Kind)
	assert.Equal(t, permissions.CeilingVersionV1, got.Version)
	assert.Equal(t, []string{"agent.create", "project.read"}, got.PermissionIDs)
	assert.Equal(t, "project", got.BoundaryKind)
	assert.Equal(t, "proj-1", got.BoundaryProjectID)
	require.NotNil(t, got.SourceExpiresAt)
	assert.True(t, expires.Equal(*got.SourceExpiresAt))

	got = read("agent-empty")
	require.NotNil(t, got.PermissionIDs, "a bounded ceiling with no IDs must read back as an empty list")
	assert.Empty(t, got.PermissionIDs)
	frozen, ok := got.Frozen()
	require.True(t, ok)
	assert.False(t, frozen.Allows("project.read"))

	got = read("agent-principal")
	assert.Equal(t, store.EffectCeilingPrincipal, got.Kind)
	assert.Nil(t, got.PermissionIDs)
	assert.Equal(t, store.SourceCredentialDevLocal, got.SourceCredentialKind)
}

// TestDelegationEdgeStoreRejectsMalformedCeiling checks that the store
// refuses a ceiling it cannot persist faithfully.
func TestDelegationEdgeStoreRejectsMalformedCeiling(t *testing.T) {
	ctx := context.Background()
	s := NewDelegationEdgeStore(enttest.NewClient(t))
	base := func(id string, c store.EffectCeiling) *store.DelegationEdge {
		return &store.DelegationEdge{
			DelegatorType: store.DelegationPrincipalUser, DelegatorID: "user-1",
			DelegateType: store.DelegationPrincipalAgent, DelegateID: id,
			ScopeType: "project", ScopeID: "proj-1", Role: "full", Active: true,
			EffectCeiling: c,
		}
	}
	err := s.CreateDelegationEdge(ctx, base("a1", store.EffectCeiling{PermissionIDs: []string{"project.read"}}))
	assert.ErrorIs(t, err, store.ErrInvalidInput, "unrecorded ceiling with IDs")
	err = s.CreateDelegationEdge(ctx, base("a2", store.EffectCeiling{Kind: store.EffectCeilingPrincipal, PermissionIDs: []string{}}))
	assert.ErrorIs(t, err, store.ErrInvalidInput, "principal ceiling with IDs")
	err = s.CreateDelegationEdge(ctx, base("a3", store.EffectCeiling{Kind: "unbounded"}))
	assert.ErrorIs(t, err, store.ErrInvalidInput, "unknown kind")
}
