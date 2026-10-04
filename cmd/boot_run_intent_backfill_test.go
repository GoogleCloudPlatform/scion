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

package cmd

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunIntentBackfill_WritesMarkerAndSkipsOnNextBoot runs the backfill
// twice. The first pass sets intent from phase and writes the completion
// marker; the second pass sees the marker and leaves a row created in
// between untouched.
func TestRunIntentBackfill_WritesMarkerAndSkipsOnNextBoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	proj := &store.Project{ID: uuid.NewString(), Name: "p", Slug: uuid.NewString()}
	require.NoError(t, s.CreateProject(ctx, proj))
	newAgent := func(slug, phase string) *store.Agent {
		a := &store.Agent{ID: uuid.NewString(), Slug: slug, Name: slug, ProjectID: proj.ID, Phase: phase}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	running := newAgent("running", "running")
	stopped := newAgent("stopped", "stopped")

	buf, restore := captureSlog(t)
	defer restore()

	runRunIntentBackfill(ctx, s)

	assert.Contains(t, buf.String(), "Run intent backfill: pass completed")
	assert.NotContains(t, buf.String(), "failed to write completion marker")
	done, err := IsMigrationComplete(ctx, s, MigrationRunIntentBackfill)
	require.NoError(t, err)
	assert.True(t, done, "completion marker should be written")

	for a, want := range map[*store.Agent]store.RunIntent{running: store.RunIntentRunning, stopped: store.RunIntentStopped} {
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, want, got.RunIntent, a.Slug)
	}

	late := newAgent("late", "running")
	runRunIntentBackfill(ctx, s)
	got, err := s.GetAgent(ctx, late.ID)
	require.NoError(t, err)
	assert.Empty(t, got.RunIntent, "a completed backfill must not run again")
}

// TestMigrationNames_AllKnown checks that every MigrationName constant is
// accepted by isKnownMigration, so its completion marker can be written.
func TestMigrationNames_AllKnown(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "migration_markers.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	var names []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "MigrationName" {
				continue
			}
			for i, n := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				require.True(t, ok, "%s must be a string literal", n.Name)
				names = append(names, lit.Value[1:len(lit.Value)-1])
			}
		}
	}
	require.NotEmpty(t, names)
	for _, n := range names {
		assert.True(t, isKnownMigration(MigrationName(n)), "MigrationName %q is not in isKnownMigration", n)
	}
}
