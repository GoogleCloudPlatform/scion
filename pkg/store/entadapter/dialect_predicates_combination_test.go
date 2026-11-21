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

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDialectPredicates_CombinedWithRestrictiveFilter verifies that every
// dialect-specific raw-SQL predicate in pkg/store/entadapter composes safely
// when ANDed with a restrictive predicate (such as AuthorizedProjectIDs,
// ProjectID, or Status). An unparenthesized top-level OR inside a raw predicate
// breaks WHERE precedence and leaks rows outside the restrictive scope
// (ptone/scion#8531, ptone/scion#8533).
//
// This test uses enttest.NewClient with no SkipOnPostgres so it runs against
// both SQLite and Postgres (launch-store-postgres-entadapter in CI).
func TestDialectPredicates_CombinedWithRestrictiveFilter(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	ps := NewProjectStore(client)
	as := NewAgentStore(client)

	t.Run("projectLabelNotContains_and_projectLabelContains_with_AuthorizedProjectIDs", func(t *testing.T) {
		mkProj := func(name string, labels map[string]string) *store.Project {
			id := uuid.NewString()
			p := &store.Project{
				ID:     id,
				Name:   name + "-" + id[:8],
				Slug:   name + "-" + id[:8],
				Labels: labels,
			}
			require.NoError(t, ps.CreateProject(ctx, p))
			return p
		}

		allowedNullLabels := mkProj("allowed-null", nil)
		allowedLabeled := mkProj("allowed-labeled", map[string]string{"env": "prod"})
		allowedTemplate := mkProj("allowed-tmpl", map[string]string{store.LabelTemplate: "true"})
		_ = mkProj("foreign-null", nil)
		_ = mkProj("foreign-labeled", map[string]string{"env": "dev"})
		_ = mkProj("foreign-tmpl", map[string]string{store.LabelTemplate: "true"})

		allowedIDs := []string{allowedNullLabels.ID, allowedLabeled.ID, allowedTemplate.ID}

		// IsTemplate=false exercises projectLabelNotContains. Foreign rows with
		// NULL labels or non-template labels must NOT leak through.
		isFalse := false
		res, err := ps.ListProjects(ctx, store.ProjectFilter{
			IsTemplate:           &isFalse,
			AuthorizedProjectIDs: allowedIDs,
		}, store.ListOptions{})
		require.NoError(t, err)
		gotIDs := make([]string, 0, len(res.Items))
		for _, item := range res.Items {
			gotIDs = append(gotIDs, item.ID)
		}
		assert.ElementsMatch(t, []string{allowedNullLabels.ID, allowedLabeled.ID}, gotIDs,
			"projectLabelNotContains must stay within AuthorizedProjectIDs")
		assert.Equal(t, 2, res.TotalCount)

		// Empty AuthorizedProjectIDs must return zero rows even with IsTemplate=false.
		emptyRes, err := ps.ListProjects(ctx, store.ProjectFilter{
			IsTemplate:           &isFalse,
			AuthorizedProjectIDs: []string{},
		}, store.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, emptyRes.Items, "empty AuthorizedProjectIDs must remain empty with IsTemplate=false")
		assert.Equal(t, 0, emptyRes.TotalCount)

		// IsTemplate=true exercises projectLabelContains.
		isTrue := true
		tmplRes, err := ps.ListProjects(ctx, store.ProjectFilter{
			IsTemplate:           &isTrue,
			AuthorizedProjectIDs: allowedIDs,
		}, store.ListOptions{})
		require.NoError(t, err)
		tmplIDs := make([]string, 0, len(tmplRes.Items))
		for _, item := range tmplRes.Items {
			tmplIDs = append(tmplIDs, item.ID)
		}
		assert.ElementsMatch(t, []string{allowedTemplate.ID}, tmplIDs,
			"projectLabelContains must stay within AuthorizedProjectIDs")
		assert.Equal(t, 1, tmplRes.TotalCount)
	})

	// Shared projects for the agent-level dialect predicate subtests.
	allowedProj := newProject(1)
	require.NoError(t, ps.CreateProject(ctx, allowedProj))
	foreignProj := newProject(2)
	require.NoError(t, ps.CreateProject(ctx, foreignProj))

	t.Run("labelContains_with_AuthorizedProjectIDs", func(t *testing.T) {
		mkAgent := func(projectID, slug string, labels map[string]string) *store.Agent {
			a := makeAgent(projectID, slug)
			a.Labels = labels
			require.NoError(t, as.CreateAgent(ctx, a))
			return a
		}

		allowedMatch := mkAgent(allowedProj.ID, "lbl-allowed-match", map[string]string{"scion.dev/role": "worker"})
		_ = mkAgent(allowedProj.ID, "lbl-allowed-other", map[string]string{"scion.dev/role": "lead"})
		_ = mkAgent(foreignProj.ID, "lbl-foreign-match", map[string]string{"scion.dev/role": "worker"})
		_ = mkAgent(foreignProj.ID, "lbl-foreign-nil", nil)

		res, err := as.ListAgents(ctx, store.AgentFilter{
			Labels:               map[string]string{"scion.dev/role": "worker"},
			AuthorizedProjectIDs: []string{allowedProj.ID},
		}, store.ListOptions{})
		require.NoError(t, err)
		require.Len(t, res.Items, 1)
		assert.Equal(t, allowedMatch.ID, res.Items[0].ID)
	})

	t.Run("ancestryContains_with_AuthorizedProjectIDs", func(t *testing.T) {
		rootAgent := makeAgent(allowedProj.ID, "anc-root")
		require.NoError(t, as.CreateAgent(ctx, rootAgent))

		mkDesc := func(projectID, slug string, ancestry []string) *store.Agent {
			a := makeAgent(projectID, slug)
			a.Ancestry = ancestry
			require.NoError(t, as.CreateAgent(ctx, a))
			return a
		}

		allowedDesc := mkDesc(allowedProj.ID, "anc-allowed-desc", []string{rootAgent.ID})
		_ = mkDesc(allowedProj.ID, "anc-allowed-other", []string{uuid.NewString()})
		_ = mkDesc(foreignProj.ID, "anc-foreign-desc", []string{rootAgent.ID})
		_ = mkDesc(foreignProj.ID, "anc-foreign-nil", nil)

		// AncestorID exercises ancestryContains directly.
		res, err := as.ListAgents(ctx, store.AgentFilter{
			AncestorID:           rootAgent.ID,
			AuthorizedProjectIDs: []string{allowedProj.ID},
		}, store.ListOptions{})
		require.NoError(t, err)
		require.Len(t, res.Items, 1)
		assert.Equal(t, allowedDesc.ID, res.Items[0].ID)

		// LineageRootID exercises agent.Or(agent.IDEQ(root), ancestryContains(root))
		// ANDed with AuthorizedProjectIDs.
		linRes, err := as.ListAgents(ctx, store.AgentFilter{
			LineageRootID:        rootAgent.ID,
			AuthorizedProjectIDs: []string{allowedProj.ID},
		}, store.ListOptions{})
		require.NoError(t, err)
		linIDs := make([]string, 0, len(linRes.Items))
		for _, item := range linRes.Items {
			linIDs = append(linIDs, item.ID)
		}
		assert.ElementsMatch(t, []string{rootAgent.ID, allowedDesc.ID}, linIDs)
	})

	t.Run("appliedGCPServiceAccountIDEQ_with_AuthorizedProjectIDs", func(t *testing.T) {
		mkSAAgent := func(projectID, slug, saID string) *store.Agent {
			a := makeAgent(projectID, slug)
			if saID != "" {
				a.AppliedConfig = &store.AgentAppliedConfig{
					GCPIdentity: &store.GCPIdentityConfig{
						MetadataMode:     store.GCPMetadataModeAssign,
						ServiceAccountID: saID,
					},
				}
			}
			require.NoError(t, as.CreateAgent(ctx, a))
			return a
		}

		allowedSA := mkSAAgent(allowedProj.ID, "sa-allowed-match", "sa-target")
		_ = mkSAAgent(allowedProj.ID, "sa-allowed-other", "sa-other")
		_ = mkSAAgent(foreignProj.ID, "sa-foreign-match", "sa-target")
		_ = mkSAAgent(foreignProj.ID, "sa-foreign-nil", "")

		res, err := as.ListAgents(ctx, store.AgentFilter{
			GCPServiceAccountID:  "sa-target",
			AuthorizedProjectIDs: []string{allowedProj.ID},
		}, store.ListOptions{})
		require.NoError(t, err)
		require.Len(t, res.Items, 1)
		assert.Equal(t, allowedSA.ID, res.Items[0].ID)
	})

	t.Run("appliedNativeCloudRunIs_with_ProjectIDEQ", func(t *testing.T) {
		mkCRAgent := func(projectID, slug string, setNative *bool) *store.Agent {
			a := makeAgent(projectID, slug)
			if setNative != nil {
				a.AppliedConfig = &store.AgentAppliedConfig{Profile: "cr"}
			}
			require.NoError(t, as.CreateAgent(ctx, a))
			if setNative != nil && *setNative {
				require.NoError(t, as.SetAgentNativeCloudRun(ctx, a.ID, true))
			}
			return a
		}

		yes, no := true, false
		allowedNative := mkCRAgent(allowedProj.ID, "cr-allowed-true", &yes)
		allowedNotNative := mkCRAgent(allowedProj.ID, "cr-allowed-false", &no)
		allowedNilCfg := mkCRAgent(allowedProj.ID, "cr-allowed-nil", nil)
		_ = mkCRAgent(foreignProj.ID, "cr-foreign-true", &yes)
		_ = mkCRAgent(foreignProj.ID, "cr-foreign-false", &no)
		_ = mkCRAgent(foreignProj.ID, "cr-foreign-nil", nil)

		allowedUID, err := parseUUID(allowedProj.ID)
		require.NoError(t, err)

		trueRows, err := client.Agent.Query().
			Where(agent.ProjectIDEQ(allowedUID), appliedNativeCloudRunIs(true)).
			All(ctx)
		require.NoError(t, err)
		trueIDs := make([]string, 0, len(trueRows))
		for _, row := range trueRows {
			trueIDs = append(trueIDs, row.ID.String())
		}
		assert.Equal(t, []string{allowedNative.ID}, trueIDs,
			"appliedNativeCloudRunIs(true) must stay within ProjectIDEQ")

		falseRows, err := client.Agent.Query().
			Where(agent.ProjectIDEQ(allowedUID), appliedNativeCloudRunIs(false)).
			All(ctx)
		require.NoError(t, err)
		falseIDs := make([]string, 0, len(falseRows))
		for _, row := range falseRows {
			falseIDs = append(falseIDs, row.ID.String())
		}
		assert.Contains(t, falseIDs, allowedNotNative.ID)
		assert.Contains(t, falseIDs, allowedNilCfg.ID)
		for _, row := range falseRows {
			assert.Equal(t, allowedUID, row.ProjectID,
				"appliedNativeCloudRunIs(false) must not leak rows from foreign projects")
		}
	})

	t.Run("brokerLabelContains_with_Status", func(t *testing.T) {
		mkBroker := func(name, status string, labels map[string]string) *store.RuntimeBroker {
			id := uuid.NewString()
			b := &store.RuntimeBroker{
				ID:     id,
				Name:   name + "-" + id[:8],
				Slug:   name + "-" + id[:8],
				Status: status,
				Labels: labels,
			}
			require.NoError(t, ps.CreateRuntimeBroker(ctx, b))
			return b
		}

		onlineMatch := mkBroker("brk-online-match", store.BrokerStatusOnline, map[string]string{"region": "us-central1"})
		_ = mkBroker("brk-offline-match", store.BrokerStatusOffline, map[string]string{"region": "us-central1"})
		_ = mkBroker("brk-online-other", store.BrokerStatusOnline, map[string]string{"region": "europe-west1"})
		_ = mkBroker("brk-offline-nil", store.BrokerStatusOffline, nil)

		rows, err := client.RuntimeBroker.Query().
			Where(
				runtimebroker.StatusEQ(store.BrokerStatusOnline),
				brokerLabelContains("region", "us-central1"),
			).
			All(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, onlineMatch.ID, rows[0].ID.String())
	})
}
