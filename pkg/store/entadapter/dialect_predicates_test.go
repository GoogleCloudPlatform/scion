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

// NO BUILD TAG — runs under default (SQLite), -tags no_sqlite, and -tags integration.

package entadapter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/project"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// sqlCaptureDriver is a minimal dialect.Driver (no build tags) that records
// generated SQL statements and fails them before execution.
type sqlCaptureDriver struct {
	dialectName string
	mu          sync.Mutex
	stmts       []string
}

var errSQLCaptured = errors.New("sql captured, not executed")

func (d *sqlCaptureDriver) record(query string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = append(d.stmts, query)
	return errSQLCaptured
}

func (d *sqlCaptureDriver) Exec(_ context.Context, query string, _, _ any) error {
	return d.record(query)
}

func (d *sqlCaptureDriver) Query(_ context.Context, query string, _, _ any) error {
	return d.record(query)
}

func (d *sqlCaptureDriver) Tx(context.Context) (dialect.Tx, error) { return dialect.NopTx(d), nil }
func (d *sqlCaptureDriver) Close() error                           { return nil }
func (d *sqlCaptureDriver) Dialect() string                        { return d.dialectName }

// TestProjectLabelNotContains_OuterParentheses verifies that
// projectLabelNotContains wraps its OR expression in outer parentheses on both
// Postgres and SQLite so combining it with AND predicates (such as
// AuthorizedProjectIDs) cannot let the OR swallow subsequent WHERE clauses
// (ptone/scion#8531, GoogleCloudPlatform/scion#5105).
func TestProjectLabelNotContains_OuterParentheses(t *testing.T) {
	idA, idB := uuid.New(), uuid.New()

	t.Run("postgres", func(t *testing.T) {
		sel := entsql.Dialect(dialect.Postgres).Select("*").From(entsql.Table(project.Table))
		projectLabelNotContains(store.LabelTemplate, "true")(sel)
		project.IDIn(idA, idB)(sel)

		query, args := sel.Query()
		assert.Contains(t, query,
			`WHERE (NOT ("projects"."labels" @> $1::jsonb) OR "projects"."labels" IS NULL) AND "projects"."id" IN ($2, $3)`,
			"Postgres projectLabelNotContains must wrap its OR in outer parentheses")
		require.Len(t, args, 3)
		assert.Equal(t, `{"scion.io/template":"true"}`, args[0])
	})

	t.Run("sqlite", func(t *testing.T) {
		sel := entsql.Dialect(dialect.SQLite).Select("*").From(entsql.Table(project.Table))
		projectLabelNotContains(store.LabelTemplate, "true")(sel)
		project.IDIn(idA, idB)(sel)

		query, args := sel.Query()
		assert.Contains(t, query,
			`WHERE (json_extract(`+"`projects`.`labels`"+`, ?) IS NULL OR json_extract(`+"`projects`.`labels`"+`, ?) != ?) AND `+"`projects`.`id`"+` IN (?, ?)`,
			"SQLite projectLabelNotContains must wrap its OR in outer parentheses")
		require.Len(t, args, 5)
	})
}

// TestListProjects_PostgresTemplateExclusion_ParenthesizedWithAuthorizedScope
// exercises the full ProjectStore.ListProjects query builder against a Postgres
// dialect driver with IsTemplate=false and AuthorizedProjectIDs set.
func TestListProjects_PostgresTemplateExclusion_ParenthesizedWithAuthorizedScope(t *testing.T) {
	drv := &sqlCaptureDriver{dialectName: dialect.Postgres}
	ps := NewProjectStore(ent.NewClient(ent.Driver(drv)))

	isTemplateFalse := false
	projID := uuid.NewString()
	_, err := ps.ListProjects(context.Background(), store.ProjectFilter{
		IsTemplate:           &isTemplateFalse,
		AuthorizedProjectIDs: []string{projID},
	}, store.ListOptions{SkipTotalCount: true})
	require.ErrorIs(t, err, errSQLCaptured)

	drv.mu.Lock()
	defer drv.mu.Unlock()
	require.NotEmpty(t, drv.stmts)

	q := drv.stmts[0]
	assert.Contains(t, q, `(NOT ("projects"."labels" @> $1::jsonb) OR "projects"."labels" IS NULL) AND "projects"."id"`,
		"ListProjects on Postgres must parenthesize template exclusion before ANDing AuthorizedProjectIDs; got: %s", q)
}

// TestAncestryContains_OuterParentheses verifies that ancestryContains wraps
// its compound predicate (col IS NOT NULL AND EXISTS (...)) in outer
// parentheses on both Postgres and SQLite so it composes safely in any boolean
// context (ptone/scion#8531, GoogleCloudPlatform/scion#5105).
func TestAncestryContains_OuterParentheses(t *testing.T) {
	rootID := uuid.New()

	for _, tc := range []struct {
		name      string
		dialect   string
		wantSubsq string
	}{
		{
			name:      "postgres",
			dialect:   dialect.Postgres,
			wantSubsq: `("agents"."ancestry" IS NOT NULL AND EXISTS (SELECT 1 FROM jsonb_array_elements_text("agents"."ancestry") AS elem WHERE elem = $2))`,
		},
		{
			name:      "sqlite",
			dialect:   dialect.SQLite,
			wantSubsq: "(`agents`.`ancestry` IS NOT NULL AND EXISTS (SELECT 1 FROM json_each(`agents`.`ancestry`) WHERE json_each.value = ?))",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := entsql.Dialect(tc.dialect).Select("*").From(entsql.Table(agent.Table))
			agent.Or(agent.IDEQ(rootID), ancestryContains("user-123"))(sel)

			query, args := sel.Query()
			assert.True(t, strings.Contains(query, tc.wantSubsq),
				"%s ancestryContains must wrap compound predicate in outer parentheses; got:\n%s", tc.name, query)
			require.Len(t, args, 2)
			assert.Equal(t, "user-123", args[1])
		})
	}
}
