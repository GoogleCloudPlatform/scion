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

package entc

import (
	"context"
	"testing"

	atlasmigrate "ariga.io/atlas/sql/migrate"
	"entgo.io/ent/dialect"
	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubExecQuerier is a no-op ExecQuerier used in tests — the UPDATE
// statements inside normalizeBrokerLabels are expected to fail when
// there is no real runtime_brokers table, but the hook swallows those
// errors with slog.Debug.
type stubExecQuerier struct{}

func (stubExecQuerier) Exec(_ context.Context, _ string, _, _ any) error {
	return nil // swallow; no real DB
}

func (stubExecQuerier) Query(_ context.Context, _ string, _, _ any) error {
	return nil
}

// TestNormalizeBrokerLabels_USINGClause verifies that the
// normalizeBrokerLabels hook injects "USING <col>::jsonb" into ALTER
// COLUMN statements that cast varchar to jsonb without an existing
// USING clause. This prevents SQLSTATE 42804 on fresh deployments.
func TestNormalizeBrokerLabels_USINGClause(t *testing.T) {
	tests := []struct {
		name   string
		cmd    string
		expect string
	}{
		{
			name:   "labels gets USING",
			cmd:    `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb`,
			expect: `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb USING "labels"::jsonb`,
		},
		{
			name:   "annotations gets USING",
			cmd:    `ALTER TABLE "runtime_brokers" ALTER COLUMN "annotations" TYPE jsonb`,
			expect: `ALTER TABLE "runtime_brokers" ALTER COLUMN "annotations" TYPE jsonb USING "annotations"::jsonb`,
		},
		{
			name:   "both columns in single statement",
			cmd:    `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb, ALTER COLUMN "annotations" TYPE jsonb`,
			expect: `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb USING "labels"::jsonb, ALTER COLUMN "annotations" TYPE jsonb USING "annotations"::jsonb`,
		},
		{
			name:   "already has USING — no double injection",
			cmd:    `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb USING "labels"::jsonb`,
			expect: `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb USING "labels"::jsonb`,
		},
		{
			name:   "unrelated statement unchanged",
			cmd:    `ALTER TABLE "runtime_brokers" ADD COLUMN "foo" text`,
			expect: `ALTER TABLE "runtime_brokers" ADD COLUMN "foo" text`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := &atlasmigrate.Plan{
				Changes: []*atlasmigrate.Change{{Cmd: tt.cmd}},
			}

			// terminal is a no-op applier — we only care about what the
			// hook does to the plan before calling next.
			var captured *atlasmigrate.Plan
			terminal := entschema.ApplyFunc(func(_ context.Context, _ dialect.ExecQuerier, p *atlasmigrate.Plan) error {
				captured = p
				return nil
			})

			hook := normalizeBrokerLabels(terminal)
			err := hook.Apply(context.Background(), stubExecQuerier{}, plan)
			require.NoError(t, err)
			require.NotNil(t, captured)
			assert.Equal(t, tt.expect, captured.Changes[0].Cmd)
		})
	}
}
