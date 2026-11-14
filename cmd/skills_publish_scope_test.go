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

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// publishLookupHub serves GET /api/v1/skills from skills, applying the name,
// scope and scopeId filters the real hub applies unless ignoreFilters is set.
// It records every list query it receives.
func publishLookupHub(t *testing.T, skills []hubclient.Skill, ignoreFilters bool) (hubclient.SkillService, func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/skills" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		out := []hubclient.Skill{}
		for _, sk := range skills {
			if !ignoreFilters {
				if v := q.Get("name"); v != "" && sk.Name != v {
					continue
				}
				if v := q.Get("scope"); v != "" && sk.Scope != v {
					continue
				}
				if v := q.Get("scopeId"); v != "" && sk.ScopeID != v {
					continue
				}
			}
			out = append(out, sk)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"skills": out})
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return client.Skills(), func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return append([]url.Values(nil), queries...)
	}
}

// sameNamedSkills holds one skill named "lint" in each scope; the project
// skill is listed first, as an unscoped name lookup could return it.
var sameNamedSkills = []hubclient.Skill{
	{ID: "proj-skill", Name: "lint", Scope: "project", ScopeID: "proj-1"},
	{ID: "other-proj-skill", Name: "lint", Scope: "project", ScopeID: "proj-2"},
	{ID: "user-skill", Name: "lint", Scope: "user", ScopeID: "user-1"},
	{ID: "global-skill", Name: "lint", Scope: "global"},
}

// TestSkillsPublish_LookupFiltersByScope pins Q6.3 (ptone/scion#1713): a
// --scope global publish asks the hub for global skills only and never
// reuses a same-named project or user skill.
func TestSkillsPublish_LookupFiltersByScope(t *testing.T) {
	ctx := context.Background()

	t.Run("global reuses the global skill", func(t *testing.T) {
		svc, queries := publishLookupHub(t, sameNamedSkills, false)
		got, err := findPublishTargetSkill(ctx, svc, "lint", "global", "")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "global-skill", got.ID)
		qs := queries()
		require.Len(t, qs, 1)
		assert.Equal(t, "lint", qs[0].Get("name"))
		assert.Equal(t, "global", qs[0].Get("scope"))
		assert.Empty(t, qs[0].Get("scopeId"))
	})

	t.Run("global with only a same-named project skill finds nothing", func(t *testing.T) {
		svc, _ := publishLookupHub(t, sameNamedSkills[:3], false)
		got, err := findPublishTargetSkill(ctx, svc, "lint", "global", "")
		require.NoError(t, err)
		assert.Nil(t, got, "a same-named project/user skill must not receive a --scope global publish")
	})

	t.Run("global ignores other scopes even if the hub does not filter", func(t *testing.T) {
		svc, _ := publishLookupHub(t, sameNamedSkills[:3], true)
		got, err := findPublishTargetSkill(ctx, svc, "lint", "global", "")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("unset scope keeps the name-only lookup", func(t *testing.T) {
		svc, queries := publishLookupHub(t, sameNamedSkills, false)
		got, err := findPublishTargetSkill(ctx, svc, "lint", "", "")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "proj-skill", got.ID)
		assert.Empty(t, queries()[0].Get("scope"))
	})
}

// TestSkillsPublish_ProjectLookupFiltersByScopeAndProject is the inverse:
// a --scope project publish reuses only the same-named skill in this
// project, never a global, user or other project's skill.
func TestSkillsPublish_ProjectLookupFiltersByScopeAndProject(t *testing.T) {
	ctx := context.Background()

	t.Run("project reuses this project's skill", func(t *testing.T) {
		svc, queries := publishLookupHub(t, sameNamedSkills, false)
		got, err := findPublishTargetSkill(ctx, svc, "lint", "project", "proj-2")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "other-proj-skill", got.ID)
		qs := queries()
		require.Len(t, qs, 1)
		assert.Equal(t, "project", qs[0].Get("scope"))
		assert.Equal(t, "proj-2", qs[0].Get("scopeId"))
	})

	t.Run("project with only global and user skills finds nothing", func(t *testing.T) {
		svc, _ := publishLookupHub(t, sameNamedSkills[2:], false)
		got, err := findPublishTargetSkill(ctx, svc, "lint", "project", "proj-1")
		require.NoError(t, err)
		assert.Nil(t, got, "a same-named global/user skill must not receive a --scope project publish")
	})

	t.Run("project ignores other projects even if the hub does not filter", func(t *testing.T) {
		svc, _ := publishLookupHub(t, sameNamedSkills, true)
		got, err := findPublishTargetSkill(ctx, svc, "lint", "project", "proj-3")
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}
