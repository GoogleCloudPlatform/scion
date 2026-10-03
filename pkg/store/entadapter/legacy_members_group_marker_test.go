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
	"testing"
	"time"

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

	newGroup := func(slug string, annotations map[string]string) *store.Group {
		t.Helper()
		g := &store.Group{
			ID:          uuid.NewString(),
			Name:        slug,
			Slug:        slug,
			GroupType:   store.GroupTypeExplicit,
			ProjectID:   project.ID,
			Annotations: annotations,
		}
		require.NoError(t, cs.CreateGroup(ctx, g))
		return g
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
	legacy2 := newGroup("project:mm2:members", map[string]string{
		store.LegacyAnnotationProjectMembersGroup: "true",
	})
	userGroup := newGroup("my-team", map[string]string{"example.dev/x": "y"})
	noAnnotations := newGroup("plain", nil)
	nonMarking := newGroup("non-marking", map[string]string{
		store.LegacyAnnotationProjectMembersGroup: "false",
	})
	conflicting := newGroup("conflicting", map[string]string{
		store.LegacyAnnotationProjectMembersGroup: "true",
		store.AnnotationProjectMembersGroup:       "false",
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

	_, err := cs.GetHubSetting(ctx, legacyProjectMembersGroupMarkerMigrationSection)
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
