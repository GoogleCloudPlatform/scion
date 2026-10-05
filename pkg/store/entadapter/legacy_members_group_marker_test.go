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
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateLegacyProjectMembersGroupMarkers covers the one-shot rewrite of
// the legacy members group marker key to the canonical key
// (ptone/scion#2556): legacy groups are rewritten across pages, other
// annotations and the owner are preserved, unrelated groups are untouched,
// the completion marker is written and a second run is a no-op.
func TestMigrateLegacyProjectMembersGroupMarkers(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))

	// Page size 2 with several groups forces more than one page.
	saved := legacyProjectMembersGroupMarkerPageSize
	legacyProjectMembersGroupMarkerPageSize = 2
	t.Cleanup(func() { legacyProjectMembersGroupMarkerPageSize = saved })

	project := &store.Project{
		ID: uuid.NewString(), Name: "mm", Slug: "mm",
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	// Fixed, ordered IDs make the page layout deterministic. The migration
	// walks groups in ascending ID order, two per page:
	//   page 1: legacy (…01), userGroup (…02)
	//   page 2: legacy2 (…03), noAnnotations (…04)
	//   page 3: nonMarking (…05), conflicting (…06)
	//   page 4: noProject (…07)
	// so the two legacy groups are on different pages and a pass that stops
	// after the first page leaves legacy2 unrewritten.
	var seq int
	nextID := func() string {
		seq++
		return fmt.Sprintf("00000000-0000-0000-0000-%012d", seq)
	}
	newGroupIn := func(projectID, slug string, annotations map[string]string) *store.Group {
		t.Helper()
		g := &store.Group{
			ID:          nextID(),
			Name:        slug,
			Slug:        slug,
			GroupType:   store.GroupTypeExplicit,
			ProjectID:   projectID,
			Annotations: annotations,
		}
		require.NoError(t, cs.CreateGroup(ctx, g))
		return g
	}
	newGroup := func(slug string, annotations map[string]string) *store.Group {
		t.Helper()
		return newGroupIn(project.ID, slug, annotations)
	}
	get := func(id string) *store.Group {
		t.Helper()
		g, err := cs.GetGroup(ctx, id)
		require.NoError(t, err)
		return g
	}

	legacy := newGroup("project:mm:members", map[string]string{
		store.LegacyAnnotationProjectMembersGroup: "true",
		"example.dev/other":                       "kept",
	})
	userGroup := newGroup("my-team", map[string]string{"example.dev/x": "y"})
	legacy2 := newGroup("project:mm2:members", map[string]string{
		store.LegacyAnnotationProjectMembersGroup: "true",
	})
	noAnnotations := newGroup("plain", nil)
	nonMarking := newGroup("non-marking", map[string]string{
		store.LegacyAnnotationProjectMembersGroup: "false",
	})
	conflicting := newGroup("conflicting", map[string]string{
		store.LegacyAnnotationProjectMembersGroup: "true",
		store.AnnotationProjectMembersGroup:       "false",
	})
	// A legacy marker on a group with no ProjectID is inert (every consumer
	// requires a ProjectID), so the migration leaves it alone.
	noProject := newGroupIn("", "no-project", map[string]string{
		store.LegacyAnnotationProjectMembersGroup: "true",
	})

	require.NoError(t, cs.MigrateLegacyProjectMembersGroupMarkers(ctx))

	for _, id := range []string{legacy.ID, legacy2.ID} {
		g := get(id)
		assert.Equal(t, "true", g.Annotations[store.AnnotationProjectMembersGroup], "group %s", g.Slug)
		assert.NotContains(t, g.Annotations, store.LegacyAnnotationProjectMembersGroup, "group %s", g.Slug)
		assert.Empty(t, g.OwnerID, "the migration must not set an owner")
	}
	assert.Equal(t, "kept", get(legacy.ID).Annotations["example.dev/other"])

	assert.Equal(t, map[string]string{"example.dev/x": "y"}, get(userGroup.ID).Annotations)
	assert.Empty(t, get(noAnnotations.ID).Annotations)
	assert.Equal(t, map[string]string{store.LegacyAnnotationProjectMembersGroup: "false"},
		get(nonMarking.ID).Annotations, "a non-marking legacy value is not a marker and is left alone")
	assert.Equal(t, map[string]string{
		store.LegacyAnnotationProjectMembersGroup: "true",
		store.AnnotationProjectMembersGroup:       "false",
	}, get(conflicting.ID).Annotations, "conflicting markers are left for an operator")
	assert.Equal(t, map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"},
		get(noProject.ID).Annotations, "a group with no ProjectID is not migrated")

	_, err := cs.GetHubSetting(ctx, LegacyProjectMembersGroupMarkerMigrationSection)
	require.NoError(t, err, "completion marker must be written")

	// Second run is a no-op: a group written with the legacy key afterwards
	// (as an older binary might) is not rewritten again.
	late := newGroup("project:late:members", map[string]string{
		store.LegacyAnnotationProjectMembersGroup: "true",
	})
	require.NoError(t, cs.MigrateLegacyProjectMembersGroupMarkers(ctx))
	assert.Equal(t, map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"},
		get(late.ID).Annotations)
	assert.Equal(t, "true", get(legacy.ID).Annotations[store.AnnotationProjectMembersGroup])
}

// TestMigrateLegacyProjectMembersGroupMarkers_MarkerOnlyOnFullSuccess pins
// that the completion marker is written only when every legacy group was
// rewritten: one failed group update makes the migration return an error
// and leaves the marker unwritten, while the other groups are still
// rewritten. A clean re-run then finishes the job and writes the marker.
func TestMigrateLegacyProjectMembersGroupMarkers_MarkerOnlyOnFullSuccess(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))

	project := &store.Project{
		ID: uuid.NewString(), Name: "ff", Slug: "ff",
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	newLegacy := func(slug string) *store.Group {
		t.Helper()
		g := &store.Group{
			ID:          uuid.NewString(),
			Name:        slug,
			Slug:        slug,
			GroupType:   store.GroupTypeExplicit,
			ProjectID:   project.ID,
			Annotations: map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"},
		}
		require.NoError(t, cs.CreateGroup(ctx, g))
		return g
	}
	ok1 := newLegacy("project:ff1:members")
	bad := newLegacy("project:ff2:members")
	ok2 := newLegacy("project:ff3:members")
	badID := uuid.MustParse(bad.ID)

	// Fail the UpdateOne of one group while injection is on. The hook is on
	// this test's own client, so it cannot affect any other test.
	var inject atomic.Bool
	inject.Store(true)
	cs.client.Group.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if gm, ok := m.(*ent.GroupMutation); ok && gm.Op() == ent.OpUpdateOne && inject.Load() {
				if id, ok := gm.ID(); ok && id == badID {
					return nil, errors.New("injected group update failure")
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	get := func(id string) *store.Group {
		t.Helper()
		g, err := cs.GetGroup(ctx, id)
		require.NoError(t, err)
		return g
	}
	canonical := map[string]string{store.AnnotationProjectMembersGroup: "true"}
	legacyOnly := map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"}

	err := cs.MigrateLegacyProjectMembersGroupMarkers(ctx)
	require.Error(t, err, "a failed group update must fail the migration")
	_, err = cs.GetHubSetting(ctx, LegacyProjectMembersGroupMarkerMigrationSection)
	require.ErrorIs(t, err, store.ErrNotFound, "the completion marker must not be written after a failure")
	assert.Equal(t, canonical, get(ok1.ID).Annotations, "other groups are still rewritten")
	assert.Equal(t, canonical, get(ok2.ID).Annotations, "other groups are still rewritten")
	assert.Equal(t, legacyOnly, get(bad.ID).Annotations, "the failed group keeps the legacy key")

	inject.Store(false)
	require.NoError(t, cs.MigrateLegacyProjectMembersGroupMarkers(ctx))
	_, err = cs.GetHubSetting(ctx, LegacyProjectMembersGroupMarkerMigrationSection)
	require.NoError(t, err, "a clean re-run must write the completion marker")
	assert.Equal(t, canonical, get(bad.ID).Annotations, "the re-run rewrites the previously failed group")
}

// TestMigrateRewritesLegacyProjectMembersGroupMarkers checks that the full
// startup Migrate runs the rewrite, so an existing database whose marker
// backfill already ran with the legacy key ends up with the canonical key.
func TestMigrateRewritesLegacyProjectMembersGroupMarkers(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))

	project := &store.Project{
		ID: uuid.NewString(), Name: "old", Slug: "old",
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))
	g := &store.Group{
		ID:          uuid.NewString(),
		Name:        "Old Members",
		Slug:        "project:old:members",
		GroupType:   store.GroupTypeExplicit,
		ProjectID:   project.ID,
		Annotations: map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"},
	}
	require.NoError(t, cs.CreateGroup(ctx, g))

	require.NoError(t, cs.Migrate(ctx))

	got, err := cs.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{store.AnnotationProjectMembersGroup: "true"}, got.Annotations)
}
