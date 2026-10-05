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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestProjectRenameSlugMigratesSystemGroups checks that a slug rename
// re-slugs and renames this project's marked agents and members groups.
func TestProjectRenameSlugMigratesSystemGroups(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := createSlugMigrationProject(t, s, tid("project_slugmig_sys"), "sys-old", "Sys Project")

	agents := &store.Group{
		ID:          tid("group_slugmig_sys_agents"),
		Name:        "Sys Project Agents",
		Slug:        "project:sys-old:agents",
		GroupType:   store.GroupTypeProjectAgents,
		ProjectID:   project.ID,
		Annotations: map[string]string{store.AnnotationProjectAgentsGroup: "true"},
	}
	members := &store.Group{
		ID:          tid("group_slugmig_sys_members"),
		Name:        "Sys Project Members",
		Slug:        "project:sys-old:members",
		GroupType:   store.GroupTypeExplicit,
		ProjectID:   project.ID,
		Annotations: map[string]string{store.AnnotationProjectMembersGroup: "true"},
	}
	for _, g := range []*store.Group{agents, members} {
		if err := s.CreateGroup(ctx, g); err != nil {
			t.Fatalf("create group %s: %v", g.Slug, err)
		}
	}

	renameProjectSlug(t, srv, project.ID, "sys-new", "Sys Renamed")

	for _, tc := range []struct {
		id, slug, name string
	}{
		{agents.ID, "project:sys-new:agents", "Sys Renamed Agents"},
		{members.ID, "project:sys-new:members", "Sys Renamed Members"},
	} {
		got, err := s.GetGroup(ctx, tc.id)
		if err != nil {
			t.Fatalf("get group %s: %v", tc.id, err)
		}
		if got.Slug != tc.slug {
			t.Errorf("group %s slug = %q, want %q", tc.id, got.Slug, tc.slug)
		}
		if got.Name != tc.name {
			t.Errorf("group %s name = %q, want %q", tc.id, got.Name, tc.name)
		}
	}
}

// TestProjectRenameSlugMigratesLegacyMembersGroup checks that a members group
// of this project that still carries only the legacy marker key is re-slugged
// too, so it is not stranded at the old slug while createProjectMembersGroup
// creates a duplicate at the new one.
func TestProjectRenameSlugMigratesLegacyMembersGroup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := createSlugMigrationProject(t, s, tid("project_slugmig_legacy"), "legacy-old", "Legacy Project")
	members := &store.Group{
		ID:          tid("group_slugmig_legacy_members"),
		Name:        "Legacy Project Members",
		Slug:        "project:legacy-old:members",
		GroupType:   store.GroupTypeExplicit,
		ProjectID:   project.ID,
		Annotations: map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"},
	}
	if err := s.CreateGroup(ctx, members); err != nil {
		t.Fatalf("create group %s: %v", members.Slug, err)
	}

	renameProjectSlug(t, srv, project.ID, "legacy-new", "Legacy Renamed")

	got, err := s.GetGroup(ctx, members.ID)
	if err != nil {
		t.Fatalf("get group %s: %v", members.ID, err)
	}
	if got.Slug != "project:legacy-new:members" {
		t.Errorf("legacy members group slug = %q, want %q", got.Slug, "project:legacy-new:members")
	}
	if got.Name != "Legacy Renamed Members" {
		t.Errorf("legacy members group name = %q, want %q", got.Name, "Legacy Renamed Members")
	}
}

// TestProjectRenameSlugSkipsLookAlikeGroups checks that a slug rename leaves
// a group at the old agents or members slug untouched when it is not this
// project's system group, and that the rename itself still succeeds
// (ptone/scion#2683).
func TestProjectRenameSlugSkipsLookAlikeGroups(t *testing.T) {
	agentsMarker := map[string]string{store.AnnotationProjectAgentsGroup: "true"}
	membersMarker := map[string]string{store.AnnotationProjectMembersGroup: "true"}

	cases := []struct {
		name string
		// annotations returns the annotations for the "agents" or
		// "members" look-alike group.
		annotations func(kind string) map[string]string
		// groupType overrides the GroupType of the look-alike group;
		// empty means the type the real system group would have.
		groupType  func(kind string) string
		otherOwner bool
		// kinds limits which look-alike groups are created; nil means both.
		kinds []string
	}{
		{
			name:        "nil annotations",
			annotations: func(string) map[string]string { return nil },
		},
		{
			name:        "unrelated annotation only",
			annotations: func(string) map[string]string { return map[string]string{"team": "platform"} },
		},
		{
			name: "marker set to false",
			annotations: func(kind string) map[string]string {
				if kind == "agents" {
					return map[string]string{store.AnnotationProjectAgentsGroup: "false"}
				}
				return map[string]string{
					store.AnnotationProjectMembersGroup:       "false",
					store.LegacyAnnotationProjectMembersGroup: "false",
				}
			},
		},
		{
			name: "other kind's marker",
			annotations: func(kind string) map[string]string {
				if kind == "agents" {
					return membersMarker
				}
				return agentsMarker
			},
		},
		{
			name: "marker for another project",
			annotations: func(kind string) map[string]string {
				if kind == "agents" {
					return agentsMarker
				}
				return membersMarker
			},
			otherOwner: true,
		},
		{
			name:        "marked agents group with another group type",
			annotations: func(string) map[string]string { return agentsMarker },
			groupType:   func(string) string { return store.GroupTypeExplicit },
			kinds:       []string{"agents"},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()

			oldSlug := fmt.Sprintf("look-old-%d", i)
			newSlug := fmt.Sprintf("look-new-%d", i)
			project := createSlugMigrationProject(t, s, tid(fmt.Sprintf("project_slugmig_look%d", i)), oldSlug, "Look Project")
			ownerID := project.ID
			if tc.otherOwner {
				other := createSlugMigrationProject(t, s, tid(fmt.Sprintf("project_slugmig_other%d", i)), fmt.Sprintf("other-%d", i), "Other Project")
				ownerID = other.ID
			}

			kinds := tc.kinds
			if kinds == nil {
				kinds = []string{"agents", "members"}
			}
			var groups []*store.Group
			for _, kind := range kinds {
				groupType := store.GroupTypeExplicit
				if kind == "agents" {
					groupType = store.GroupTypeProjectAgents
				}
				if tc.groupType != nil {
					groupType = tc.groupType(kind)
				}
				g := &store.Group{
					ID:          tid(fmt.Sprintf("group_slugmig_look%d_%s", i, kind)),
					Name:        "Look-alike " + kind,
					Slug:        "project:" + oldSlug + ":" + kind,
					GroupType:   groupType,
					ProjectID:   ownerID,
					Annotations: tc.annotations(kind),
				}
				if err := s.CreateGroup(ctx, g); err != nil {
					t.Fatalf("create group %s: %v", g.Slug, err)
				}
				groups = append(groups, g)
			}

			renameProjectSlug(t, srv, project.ID, newSlug, "Look Renamed")

			for _, want := range groups {
				got, err := s.GetGroup(ctx, want.ID)
				if err != nil {
					t.Fatalf("get group %s: %v", want.ID, err)
				}
				if got.Slug != want.Slug {
					t.Errorf("look-alike group %s slug = %q, want unchanged %q", want.ID, got.Slug, want.Slug)
				}
				if got.Name != want.Name {
					t.Errorf("look-alike group %s name = %q, want unchanged %q", want.ID, got.Name, want.Name)
				}
			}
		})
	}
}

func createSlugMigrationProject(t *testing.T, s store.Store, id, slug, name string) *store.Project {
	t.Helper()
	project := &store.Project{
		ID:      id,
		Slug:    slug,
		Name:    name,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(context.Background(), project); err != nil {
		t.Fatalf("create project %s: %v", slug, err)
	}
	return project
}

func renameProjectSlug(t *testing.T, srv *Server, projectID, newSlug, newName string) {
	t.Helper()
	body := map[string]interface{}{"name": newName, "slug": newSlug}
	rec := doRequest(t, srv, http.MethodPatch, fmt.Sprintf("/api/v1/projects/%s", projectID), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp store.Project
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode rename response: %v", err)
	}
	if resp.Slug != newSlug {
		t.Errorf("rename: slug = %q, want %q", resp.Slug, newSlug)
	}
}
