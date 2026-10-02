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
	"bytes"
	"context"
	"database/sql"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestUTCTimestampNormalizeJSON_ tests run on SQLite by default and on
// Postgres in the CI job that sets SCION_TEST_POSTGRES_URL (Makefile target
// test-launch-store-postgres selects them by name), because the
// JSON-embedded rewrite is the part of utc-timestamp-normalize that runs on
// both backends.

type jsonNormalizeFixture struct {
	db       *sql.DB
	dialect  string
	sqlite   bool
	agents   *AgentStore
	client   *ent.Client
	agentID  string
	cleanID  string
	policyID string
	badID    string
}

func newJSONNormalizeFixture(t *testing.T) *jsonNormalizeFixture {
	t.Helper()
	ctx := context.Background()
	client := enttest.NewClient(t)
	drv, ok := client.Driver().(*entsql.Driver)
	require.True(t, ok, "test client is not backed by database/sql")
	f := &jsonNormalizeFixture{db: drv.DB(), client: client, agents: NewAgentStore(client)}
	f.dialect = drv.Dialect()
	var err error
	f.sqlite, err = isSQLiteDialect(f.db, f.dialect)
	require.NoError(t, err)
	require.Equal(t, !enttest.Active(), f.sqlite, "dialect disagrees with enttest")

	_, err = client.Project.Create().SetID(agentTestProjectUID).SetName("p").SetSlug("p").Save(ctx)
	require.NoError(t, err)
	projectID := agentTestProjectUID.String()

	a := makeAgent(projectID, "json-legacy")
	require.NoError(t, f.agents.CreateAgent(ctx, a))
	f.agentID = a.ID
	clean := makeAgent(projectID, "json-clean")
	clean.ExposedPorts = []store.ExposedPort{{Port: 7000, ExposedAt: time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC), ExposedBy: "u"}}
	require.NoError(t, f.agents.CreateAgent(ctx, clean))
	f.cleanID = clean.ID

	for _, name := range []string{"legacy", "bad"} {
		pol, err := client.AccessPolicy.Create().
			SetName(name).SetScopeType("hub").SetResourceType("*").
			SetActions([]string{"read"}).SetEffect("allow").Save(ctx)
		require.NoError(t, err)
		if name == "legacy" {
			f.policyID = pol.ID.String()
		} else {
			f.badID = pol.ID.String()
		}
	}

	// Legacy rows, written as raw JSON with local offsets, as a writer
	// without .UTC() stored them.
	f.setJSON(t, "agents", "exposed_ports", f.agentID,
		`[{"port":8080,"label":"web","exposedAt":"2026-10-01T18:00:00.5+09:00","exposedBy":"u"},`+
			`{"port":9090,"exposedAt":"2026-10-01T09:00:00Z","exposedBy":"u"}]`)
	f.setJSON(t, "access_policies", "conditions", f.policyID,
		`{"labels":{"team":"a"},"validFrom":"2026-10-01T09:45:00+05:45","validUntil":"2026-10-02T02:00:00+02:00","sourceIps":["10.0.0.0/8"]}`)
	f.setJSON(t, "access_policies", "conditions", f.badID, `{"validFrom":"not-a-time-7c1e"}`)
	return f
}

func (f *jsonNormalizeFixture) setJSON(t *testing.T, table, column, id, raw string) {
	t.Helper()
	q := "UPDATE " + table + " SET " + column + " = ? WHERE id = ?"
	if !f.sqlite {
		q = "UPDATE " + table + " SET " + column + " = $1::jsonb WHERE id = $2"
	}
	res, err := f.db.ExecContext(context.Background(), q, raw, id)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

func (f *jsonNormalizeFixture) rawJSON(t *testing.T, table, column, id string) string {
	t.Helper()
	q := "SELECT CAST(" + column + " AS TEXT) FROM " + table + " WHERE id = ?"
	if !f.sqlite {
		q = "SELECT " + column + "::text FROM " + table + " WHERE id = $1"
	}
	var s string
	require.NoError(t, f.db.QueryRowContext(context.Background(), q, id).Scan(&s))
	return s
}

func (f *jsonNormalizeFixture) snapshot(t *testing.T) []string {
	return []string{
		f.rawJSON(t, "agents", "exposed_ports", f.agentID),
		f.rawJSON(t, "agents", "exposed_ports", f.cleanID),
		f.rawJSON(t, "access_policies", "conditions", f.policyID),
		f.rawJSON(t, "access_policies", "conditions", f.badID),
	}
}

func TestUTCTimestampNormalizeJSON_RewritesEmbeddedTimes(t *testing.T) {
	ctx := context.Background()
	f := newJSONNormalizeFixture(t)
	cleanBefore := f.rawJSON(t, "agents", "exposed_ports", f.cleanID)
	badBefore := f.rawJSON(t, "access_policies", "conditions", f.badID)

	var log bytes.Buffer
	rep, err := NormalizeUTCTimestamps(ctx, f.db, f.dialect, &log, TimestampNormalizeOptions{BatchSize: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Unparseable, log.String())
	assert.NotContains(t, log.String(), "not-a-time-7c1e", "a stored value reached the log")
	assert.Contains(t, log.String(), "table=access_policies column=conditions")

	ports := f.rawJSON(t, "agents", "exposed_ports", f.agentID)
	assert.Contains(t, ports, `"2026-10-01T09:00:00.5Z"`)
	assert.NotContains(t, ports, "+09:00")
	assert.Contains(t, ports, `"2026-10-01T09:00:00Z"`)
	cond := f.rawJSON(t, "access_policies", "conditions", f.policyID)
	assert.Contains(t, cond, `"2026-10-01T04:00:00Z"`)
	assert.Contains(t, cond, `"2026-10-02T00:00:00Z"`)
	assert.Contains(t, cond, "10.0.0.0/8", "untouched keys must survive")
	assert.Equal(t, cleanBefore, f.rawJSON(t, "agents", "exposed_ports", f.cleanID), "a canonical row was rewritten")
	assert.Equal(t, badBefore, f.rawJSON(t, "access_policies", "conditions", f.badID), "an unparseable row was rewritten")

	// The rewritten rows read back through the store as UTC instants.
	got, err := f.agents.GetAgent(ctx, f.agentID)
	require.NoError(t, err)
	require.Len(t, got.ExposedPorts, 2)
	assert.True(t, got.ExposedPorts[0].ExposedAt.Equal(time.Date(2026, 10, 1, 9, 0, 0, 500000000, time.UTC)))
	assert.Equal(t, 8080, got.ExposedPorts[0].Port)
	assert.Equal(t, "web", got.ExposedPorts[0].Label)
	pol, err := f.client.AccessPolicy.Get(ctx, uuid.MustParse(f.policyID))
	require.NoError(t, err)
	require.NotNil(t, pol.Conditions.ValidFrom)
	assert.Equal(t, time.UTC, pol.Conditions.ValidFrom.Location())
	assert.Equal(t, "a", pol.Conditions.Labels["team"])

	// The startup check runs without error on both backends; on Postgres it
	// has nothing to report (scalar columns are timestamptz).
	chk, err := CheckStoredTimestamps(ctx, f.db, f.dialect)
	require.NoError(t, err)
	if !f.sqlite {
		assert.Equal(t, TimestampCheck{}, chk)
	}

	// A second run changes nothing.
	before := f.snapshot(t)
	rep, err = NormalizeUTCTimestamps(ctx, f.db, f.dialect, nil, TimestampNormalizeOptions{})
	require.NoError(t, err)
	assert.Zero(t, rep.Rewritten)
	assert.Equal(t, before, f.snapshot(t))
}

func TestUTCTimestampNormalizeJSON_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	f := newJSONNormalizeFixture(t)
	before := f.snapshot(t)
	var log bytes.Buffer
	rep, err := NormalizeUTCTimestamps(ctx, f.db, f.dialect, &log, TimestampNormalizeOptions{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, 2, rep.Rewritten, "one exposed_ports row and one conditions row would change")
	assert.Equal(t, before, f.snapshot(t))
	assert.Contains(t, log.String(), "dry run")
}
