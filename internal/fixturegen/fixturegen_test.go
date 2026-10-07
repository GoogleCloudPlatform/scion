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

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectedTableCount is the number of domain tables in the hub schema
// (excluding the schema_migrations bookkeeping table). The fixture must cover
// every one of them.
const expectedTableCount = 73

// TestFixtureCoverage is the CI coverage gate: it generates the fixture and
// fails if any domain table has zero rows.
func TestFixtureCoverage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	report, err := Generate(context.Background(), path)
	require.NoError(t, err)

	t.Logf("fixture covers %d domain tables", report.TotalTables())
	for _, c := range report.Counts {
		t.Logf("  %-32s %d row(s)", c.Table, c.Count)
	}

	assert.Equal(t, expectedTableCount, report.TotalTables(),
		"fixture should cover exactly the %d domain tables", expectedTableCount)
	assert.Empty(t, report.Missing,
		"every domain table must have at least one fixture row; missing: %v", report.Missing)
}

// TestFixtureLoadable verifies the generated database is a valid, openable
// SQLite store with the seeded data intact.
func TestFixtureLoadable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fixture.db")
	_, err := Generate(ctx, path)
	require.NoError(t, err)

	// Reopen as a fresh Ent client and confirm connectivity + seeded rows.
	client, err := entc.OpenSQLite("file:"+path, entc.PoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	db := client.Driver().(*entsql.Driver).DB()
	require.NoError(t, db.PingContext(ctx))

	var users int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&users))
	assert.Positive(t, users, "users table should have seeded rows")

	// The soft-deleted agent edge case must be present.
	var deletedAgents int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agents WHERE deleted_at IS NOT NULL").Scan(&deletedAgents))
	assert.Positive(t, deletedAgents, "fixture should include a soft-deleted agent")

	// The broker_settings row must be readable through the real store
	// adapter, not just present as a row (ptone/scion#2061 P2 review round
	// 3, F5): a value that satisfies the coverage count but fails to parse
	// against its own schema's column type (field.UUID("id", ...)) would
	// defeat the fixture's purpose as a representative, application-usable
	// hub database.
	settings, err := entadapter.NewBrokerSettingStore(client).GetBrokerSettings(ctx, brokerID)
	require.NoError(t, err, "broker_settings fixture row must be readable via BrokerSettingStore")
	require.NotNil(t, settings.Settings.MaxAgents)
	assert.EqualValues(t, 5, *settings.Settings.MaxAgents)

	// The agent_identity_keys and external_identities rows must likewise be
	// readable through their real store adapters, not just present as rows:
	// the same non-hex-id pitfall as above applies to these two tables'
	// field.UUID("id", ...) columns.
	keys, err := entadapter.NewAgentIdentityKeyStore(client).ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err, "agent_identity_keys fixture row must be readable via AgentIdentityKeyStore")
	require.Len(t, keys, 1)
	assert.Equal(t, "worker", keys[0].Key)

	identity, err := entadapter.NewExternalIdentityStore(client).GetExternalIdentity(
		ctx, "fixture-provider", "https://issuer.fixture.example", "fixture-subject-001")
	require.NoError(t, err, "external_identities fixture row must be readable via ExternalIdentityStore")
	assert.Equal(t, userID, identity.UserID)

	// The user_terminal_workspaces row must likewise be readable through its
	// real store adapter: the same non-hex-id pitfall as above applies to its
	// field.UUID("id", ...) column.
	workspace, err := entadapter.NewUserTerminalWorkspaceStore(client).GetUserTerminalWorkspace(ctx, userID)
	require.NoError(t, err, "user_terminal_workspaces fixture row must be readable via UserTerminalWorkspaceStore")
	assert.Equal(t, []string{agentID}, workspace.AgentIDs)
	assert.Equal(t, agentID, workspace.FrontmostAgentID)

	// The conduit session rows must decode through the real registry store
	// (capabilities JSON, NULL project/exec_scope, relay join).
	conduit := entadapter.NewConduitRegistryStore(client)
	ps, found, err := conduit.ListPrincipalSessionsBySession(ctx, "cs000000-0000-0000-0000-000000000001")
	require.NoError(t, err, "conduit_sessions fixture row must be readable via ConduitRegistryStore")
	require.True(t, found)
	require.Len(t, ps.Sessions, 1)
	assert.Equal(t, "launch_id", ps.Sessions[0].Session.Capabilities.IncarnationSource)
	assert.Equal(t, int64(1), ps.CurrentEpoch)
	require.NotNil(t, ps.Sessions[0].Relay)
	ps, found, err = conduit.ListPrincipalSessionsBySession(ctx, "cs000000-0000-0000-0000-000000000002")
	require.NoError(t, err)
	require.True(t, found)
	assert.Empty(t, ps.Sessions[0].Session.ProjectID)
	assert.Empty(t, ps.Sessions[0].Session.ExecScope)
}

// TestFixtureDeterministic verifies the spec produces a stable set of row
// counts across runs (no time.Now()/random values leaking in).
func TestFixtureDeterministic(t *testing.T) {
	ctx := context.Background()
	r1, err := Generate(ctx, filepath.Join(t.TempDir(), "a.db"))
	require.NoError(t, err)
	r2, err := Generate(ctx, filepath.Join(t.TempDir(), "b.db"))
	require.NoError(t, err)
	assert.Equal(t, r1.Counts, r2.Counts, "row counts should be identical across runs")
}

func TestAccessConstraintHistoryEventIDIsCanonical(t *testing.T) {
	t.Parallel()

	var history row
	for _, fixture := range Spec() {
		if fixture.Table == "access_constraint_history" {
			require.Len(t, fixture.Rows, 1)
			history = fixture.Rows[0]
			break
		}
	}
	require.NotNil(t, history, "fixture must include access_constraint_history")

	event := auditevent.EnvelopeV1{
		SchemaVersion: auditevent.SchemaVersion,
		EventID:       history["event_id"].(string),
		OccurredAt:    history["occurred_at"].(time.Time),
		Family:        "access_boundary",
		Action:        history["operation"].(string),
		Phase:         auditevent.PhaseCommit,
		Outcome:       auditevent.OutcomeSucceeded,
		Severity:      auditevent.SeverityInfo,
		CorrelationID: history["correlation_id"].(string),
		Principal: &auditevent.IdentityRef{
			Kind: auditevent.IdentityKind(history["actor_kind"].(string)),
			ID:   history["actor_id"].(string),
		},
		Resource: &auditevent.ResourceRef{
			Kind:  "access_constraint",
			ID:    history["constraint_id"].(string),
			Scope: auditevent.ResourceScopeSystem,
		},
		Payload: auditevent.AccessBoundaryPayload{
			AfterRevision:  pointer(history["after_revision"].(int64)),
			Classification: auditevent.BoundaryClassification(history["classification"].(string)),
			ImpactCounts:   &auditevent.ImpactCounts{Agents: 1, Users: 1},
			ChangedFields:  []string{"maximum_permissions"},
		},
	}
	require.NoError(t, auditevent.Validate(event))
}

func pointer[T any](value T) *T {
	return &value
}
