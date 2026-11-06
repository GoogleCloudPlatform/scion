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
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sortedSkillIDs(items []store.Skill) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	sort.Strings(out)
	return out
}

func sortedTemplateIDs(items []store.Template) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(in ...string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestSkillStore_AccessScopeAllProjects: AllProjects matches every
// project-scoped skill, in any project, and adds no hub-scoped or
// user-scoped skill. With AllProjects false (the default) the predicate is
// unchanged: the same scope lists exactly what it listed before.
func TestSkillStore_AccessScopeAllProjects(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	mk := func(name, scope, scopeID string) string {
		s := &store.Skill{ID: uuid.New().String(), Name: name, Slug: name, Scope: scope, ScopeID: scopeID, Status: "active"}
		require.NoError(t, cs.CreateSkill(ctx, s))
		return s.ID
	}
	pA := mk("all-projects-a", store.SkillScopeProject, "project-a")
	pB := mk("all-projects-b", store.SkillScopeProject, "project-b")
	mk("all-projects-global", store.SkillScopeGlobal, "")
	mk("all-projects-core", store.SkillScopeCore, "")
	user := mk("all-projects-user", store.SkillScopeUser, "user-1")

	all, err := cs.ListSkills(ctx, store.SkillFilter{Status: "active", AccessScope: &store.SkillAccessScope{AllProjects: true}}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, sortedStrings(pA, pB), sortedSkillIDs(all.Items), "every project-scoped skill and nothing else")
	assert.Equal(t, 2, all.TotalCount)

	// AllProjects ignores ProjectIDs: it already matches every project.
	withIDs, err := cs.ListSkills(ctx, store.SkillFilter{Status: "active", AccessScope: &store.SkillAccessScope{AllProjects: true, ProjectIDs: []string{"project-a"}}}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, sortedStrings(pA, pB), sortedSkillIDs(withIDs.Items))

	// Default false: an existing scope is unchanged.
	explicit, err := cs.ListSkills(ctx, store.SkillFilter{Status: "active", AccessScope: &store.SkillAccessScope{CallerID: "user-1", ProjectIDs: []string{"project-a"}}}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, sortedStrings(pA, user), sortedSkillIDs(explicit.Items))
	assert.Equal(t, 2, explicit.TotalCount)

	empty, err := cs.ListSkills(ctx, store.SkillFilter{Status: "active", AccessScope: &store.SkillAccessScope{}}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, empty.Items, "an empty scope still matches nothing")
	assert.Zero(t, empty.TotalCount)
}

// TestTemplateStore_ProjectIDsFilter: a non-nil ProjectIDs narrows the list
// to project-scoped templates whose scope ID or deprecated project ID is in
// the set, before COUNT and LIMIT; a non-nil empty set matches nothing and
// never falls back to the unfiltered list; nil leaves every query as it was.
func TestTemplateStore_ProjectIDsFilter(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()
	mk := func(name, scope, scopeID, projectID string) string {
		tp := &store.Template{ID: uuid.New().String(), Name: name, Slug: name, Harness: "claude", Scope: scope, ScopeID: scopeID, ProjectID: projectID, Status: store.TemplateStatusActive}
		require.NoError(t, ts.CreateTemplate(ctx, tp))
		return tp.ID
	}
	pA := mk("pids-a", store.TemplateScopeProject, "project-a", "")
	pB := mk("pids-b", store.TemplateScopeProject, "project-b", "")
	legacyA := mk("pids-legacy-a", store.TemplateScopeProject, "project-c", "project-a")
	global := mk("pids-global", store.TemplateScopeGlobal, "", "")
	user := mk("pids-user", store.TemplateScopeUser, "user-1", "")
	userInA := mk("pids-user-a", store.TemplateScopeUser, "project-a", "")

	list := func(f store.TemplateFilter, opts store.ListOptions) *store.ListResult[store.Template] {
		t.Helper()
		f.Status = store.TemplateStatusActive
		res, err := ts.ListTemplates(ctx, f, opts)
		require.NoError(t, err)
		return res
	}

	a := list(store.TemplateFilter{ProjectIDs: []string{"project-a"}}, store.ListOptions{})
	assert.Equal(t, sortedStrings(pA, legacyA), sortedTemplateIDs(a.Items), "project-scoped rows matched by scope ID or deprecated project ID")
	assert.Equal(t, 2, a.TotalCount)
	assert.NotContains(t, sortedTemplateIDs(a.Items), userInA, "a user-scoped row whose scope ID matches is not a project template")

	ab := list(store.TemplateFilter{ProjectIDs: []string{"project-a", "project-b"}}, store.ListOptions{Limit: 1})
	assert.Len(t, ab.Items, 1, "the limit applies after the filter")
	assert.Equal(t, 3, ab.TotalCount, "the count applies after the filter")

	none := list(store.TemplateFilter{ProjectIDs: []string{}}, store.ListOptions{})
	assert.Empty(t, none.Items, "a non-nil empty set matches nothing")
	assert.Zero(t, none.TotalCount)
	noneProject := list(store.TemplateFilter{ProjectIDs: []string{}, Scope: store.TemplateScopeProject}, store.ListOptions{})
	assert.Empty(t, noneProject.Items)

	unfiltered := list(store.TemplateFilter{}, store.ListOptions{})
	assert.Equal(t, sortedStrings(pA, pB, legacyA, global, user, userInA), sortedTemplateIDs(unfiltered.Items), "nil ProjectIDs leaves the query unchanged")
	byProject := list(store.TemplateFilter{ProjectID: "project-b"}, store.ListOptions{})
	assert.Equal(t, sortedStrings(pB, global), sortedTemplateIDs(byProject.Items), "the project-without-scope query is unchanged")
}
