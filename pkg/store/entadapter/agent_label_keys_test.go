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

package entadapter_test

import (
	"context"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentFilter_LabelKeys(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	s := entadapter.NewCompositeStore(client)

	proj := &store.Project{
		ID:   api.NewUUID(),
		Name: "label-keys-proj",
		Slug: "label-keys-proj",
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	seedAgent := func(slug string, labels map[string]string) string {
		id := api.NewUUID()
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID:        id,
			Slug:      slug,
			Name:      slug,
			ProjectID: proj.ID,
			Phase:     "running",
			Labels:    labels,
		}))
		return slug
	}

	seedAgent("ag-trigger-1", map[string]string{"trigger": "t1", "scion.dev/role": "worker"})
	seedAgent("ag-trigger-empty-val", map[string]string{"trigger": ""})
	seedAgent("ag-other-labels", map[string]string{"env": "prod", "scion.dev/role": "lead"})
	seedAgent("ag-no-labels", nil)

	listSlugs := func(filter store.AgentFilter) []string {
		res, err := s.ListAgents(ctx, filter, store.ListOptions{Limit: 50})
		require.NoError(t, err)
		count, err := s.CountAgents(ctx, filter)
		require.NoError(t, err)
		assert.Equal(t, len(res.Items), count)
		slugs := make([]string, len(res.Items))
		for i, ag := range res.Items {
			slugs[i] = ag.Slug
		}
		sort.Strings(slugs)
		return slugs
	}

	t.Run("single_key_matches_any_value_including_empty", func(t *testing.T) {
		got := listSlugs(store.AgentFilter{
			ProjectID: proj.ID,
			LabelKeys: []string{"trigger"},
		})
		assert.Equal(t, []string{"ag-trigger-1", "ag-trigger-empty-val"}, got)
	})

	t.Run("dotted_key", func(t *testing.T) {
		got := listSlugs(store.AgentFilter{
			ProjectID: proj.ID,
			LabelKeys: []string{"scion.dev/role"},
		})
		assert.Equal(t, []string{"ag-other-labels", "ag-trigger-1"}, got)
	})

	t.Run("multiple_keys_AND_semantics", func(t *testing.T) {
		got := listSlugs(store.AgentFilter{
			ProjectID: proj.ID,
			LabelKeys: []string{"trigger", "scion.dev/role"},
		})
		assert.Equal(t, []string{"ag-trigger-1"}, got)
	})

	t.Run("combined_with_Labels_value_filter", func(t *testing.T) {
		got := listSlugs(store.AgentFilter{
			ProjectID: proj.ID,
			LabelKeys: []string{"scion.dev/role"},
			Labels:    map[string]string{"env": "prod"},
		})
		assert.Equal(t, []string{"ag-other-labels"}, got)
	})

	t.Run("nonexistent_key_returns_empty", func(t *testing.T) {
		got := listSlugs(store.AgentFilter{
			ProjectID: proj.ID,
			LabelKeys: []string{"does-not-exist"},
		})
		assert.Empty(t, got)
	})
}
