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
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func enableFiltersQueryDialect(t *testing.T, srv *Server) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(`{"overrides":{"`+experiments.FiltersQueryDialect+`":true}}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

func TestListAgents_FilterQuery_ExperimentOffIgnoresQ(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: uuid.NewString(), Name: "fq-off", Slug: "fq-off"}
	require.NoError(t, s.CreateProject(ctx, proj))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: uuid.NewString(), Name: "a1", Slug: "a1", ProjectID: proj.ID, Phase: "running",
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: uuid.NewString(), Name: "a2", Slug: "a2", ProjectID: proj.ID, Phase: "stopped",
	}))

	// Experiment is off by default: even invalid q is ignored and all agents are returned.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape("phase:stopped"), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp.Agents, 2)

	recBad := doRequest(t, srv, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape("phase:"), nil)
	require.Equal(t, http.StatusOK, recBad.Code)
}

func TestListAgents_FilterQuery_StructuredAndErrorsAndPagination(t *testing.T) {
	srv, s := testServer(t)
	enableFiltersQueryDialect(t, srv)
	ctx := context.Background()

	proj := &store.Project{ID: uuid.NewString(), Name: "fq-on", Slug: "fq-on"}
	require.NoError(t, s.CreateProject(ctx, proj))

	seed := []store.Agent{
		{ID: uuid.NewString(), Name: "foo-run-1", Slug: "foo-run-1", ProjectID: proj.ID, Phase: "running", Labels: map[string]string{"team": "ui"}},
		{ID: uuid.NewString(), Name: "foo-err-2", Slug: "foo-err-2", ProjectID: proj.ID, Phase: "error", Labels: map[string]string{"team": "ui", "tmp": "1"}},
		{ID: uuid.NewString(), Name: "foo-err-3", Slug: "foo-err-3", ProjectID: proj.ID, Phase: "error"},
		{ID: uuid.NewString(), Name: "bar-run-4", Slug: "bar-run-4", ProjectID: proj.ID, Phase: "running", Labels: map[string]string{"team": "ui"}},
		{ID: uuid.NewString(), Name: "foo-stop-5", Slug: "foo-stop-5", ProjectID: proj.ID, Phase: "stopped"},
	}
	for i := range seed {
		require.NoError(t, s.CreateAgent(ctx, &seed[i]))
	}

	// Combined: phase:running,error -label:tmp foo -> foo-run-1 and foo-err-3.
	q := "phase:running,error -label:tmp foo"
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape(q), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	var names []string
	for _, a := range resp.Agents {
		names = append(names, a.Name)
	}
	slices.Sort(names)
	assert.Equal(t, []string{"foo-err-3", "foo-run-1"}, names)
	assert.Equal(t, 2, resp.TotalCount)

	// Legacy params ANDed with q: phase=running AND q=foo -> only foo-run-1.
	recAnd := doRequest(t, srv, http.MethodGet, "/api/v1/agents?phase=running&q="+url.QueryEscape("foo"), nil)
	require.Equal(t, http.StatusOK, recAnd.Code)
	var respAnd ListAgentsResponse
	require.NoError(t, json.Unmarshal(recAnd.Body.Bytes(), &respAnd))
	require.Len(t, respAnd.Agents, 1)
	assert.Equal(t, "foo-run-1", respAnd.Agents[0].Name)

	// Pagination with q (limit=1 across 2 pages) in sorted mode.
	recP1 := doRequest(t, srv, http.MethodGet, "/api/v1/agents?sort=created&dir=desc&limit=1&q="+url.QueryEscape(q), nil)
	require.Equal(t, http.StatusOK, recP1.Code)
	var p1 ListAgentsResponse
	require.NoError(t, json.Unmarshal(recP1.Body.Bytes(), &p1))
	require.Len(t, p1.Agents, 1)
	assert.Equal(t, 2, p1.TotalCount)
	require.NotEmpty(t, p1.NextCursor)

	recP2 := doRequest(t, srv, http.MethodGet, "/api/v1/agents?sort=created&dir=desc&limit=1&q="+url.QueryEscape(q)+"&cursor="+url.QueryEscape(p1.NextCursor), nil)
	require.Equal(t, http.StatusOK, recP2.Code)
	var p2 ListAgentsResponse
	require.NoError(t, json.Unmarshal(recP2.Body.Bytes(), &p2))
	require.Len(t, p2.Agents, 1)
	assert.NotEqual(t, p1.Agents[0].ID, p2.Agents[0].ID)

	// Syntax error -> 400 invalid_filter_query with offset and length.
	recErr := doRequest(t, srv, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape("phase:running OR phase:error"), nil)
	require.Equal(t, http.StatusBadRequest, recErr.Code)
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(recErr.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeInvalidFilterQuery, errResp.Error.Code)
	assert.Equal(t, float64(14), errResp.Error.Details["offset"])
	assert.Equal(t, float64(2), errResp.Error.Details["length"])

	// Invalid enum value -> 400 invalid_filter_query with suggestion.
	recEnum := doRequest(t, srv, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape("phase:runing"), nil)
	require.Equal(t, http.StatusBadRequest, recEnum.Code)
	var enumResp ErrorResponse
	require.NoError(t, json.Unmarshal(recEnum.Body.Bytes(), &enumResp))
	assert.Equal(t, ErrCodeInvalidFilterQuery, enumResp.Error.Code)
	assert.Contains(t, enumResp.Error.Message, "running")
	assert.Equal(t, float64(0), enumResp.Error.Details["offset"])
	assert.Equal(t, float64(12), enumResp.Error.Details["length"])

	// Client-only key (mode:) -> 400 invalid_filter_query.
	recMode := doRequest(t, srv, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape("mode:lineage"), nil)
	require.Equal(t, http.StatusBadRequest, recMode.Code)
	var modeResp ErrorResponse
	require.NoError(t, json.Unmarshal(recMode.Body.Bytes(), &modeResp))
	assert.Equal(t, ErrCodeInvalidFilterQuery, modeResp.Error.Code)
	assert.Contains(t, modeResp.Error.Message, "mode")
}

func TestListAgents_FilterQuery_AllServerKeysAndReferents(t *testing.T) {
	srv, s := testServer(t)
	enableFiltersQueryDialect(t, srv)
	ctx := context.Background()

	aliceID := uuid.NewString()
	bobID := uuid.NewString()
	createTestUserWithRole(t, s, aliceID, "alice@example.com", string(store.UserRoleAdmin), store.SystemRoleSuperAdmin)
	aliceUser, err := s.GetUser(ctx, aliceID)
	require.NoError(t, err)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: bobID, Email: "bob@example.com", DisplayName: "Bob", Role: store.UserRoleMember, Status: "active",
	}))

	projA := &store.Project{ID: uuid.NewString(), Name: "Alpha", Slug: "alpha-slug", OwnerID: aliceID}
	projB := &store.Project{ID: uuid.NewString(), Name: "Beta", Slug: "beta-slug", OwnerID: bobID}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))

	ownerRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		RoleDefinitionID: ownerRole.ID,
		PrincipalType:    "user",
		PrincipalID:      aliceID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projA.ID,
	})
	require.NoError(t, err)

	brokerEast := &store.RuntimeBroker{ID: uuid.NewString(), Name: "broker-east", Slug: "broker-east", Status: store.BrokerStatusOnline}
	brokerWest := &store.RuntimeBroker{ID: uuid.NewString(), Name: "broker-west", Slug: "broker-west", Status: store.BrokerStatusOnline}
	brokerDup1 := &store.RuntimeBroker{ID: uuid.NewString(), Name: "shared-name", Slug: "shared-name-1", Status: store.BrokerStatusOnline}
	brokerDup2 := &store.RuntimeBroker{ID: uuid.NewString(), Name: "shared-name", Slug: "shared-name-2", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, brokerEast))
	require.NoError(t, s.CreateRuntimeBroker(ctx, brokerWest))
	require.NoError(t, s.CreateRuntimeBroker(ctx, brokerDup1))
	require.NoError(t, s.CreateRuntimeBroker(ctx, brokerDup2))

	agents := []store.Agent{
		{
			ID: uuid.NewString(), Name: "a1", Slug: "a1", ProjectID: projA.ID,
			Phase: "running", Activity: "working", Template: "developer",
			RuntimeBrokerID: brokerEast.ID, OwnerID: aliceID,
			AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
		},
		{
			ID: uuid.NewString(), Name: "a2", Slug: "a2", ProjectID: projB.ID,
			Phase: "running", Activity: "idle", Template: "reviewer",
			RuntimeBrokerID: brokerWest.ID, OwnerID: bobID,
			AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "antigravity"},
		},
	}
	for i := range agents {
		require.NoError(t, s.CreateAgent(ctx, &agents[i]))
	}

	queryNames := func(t *testing.T, rawQ string) []string {
		t.Helper()
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape(rawQ), nil)
		require.Equal(t, http.StatusOK, rec.Code, "q=%q body=%s", rawQ, rec.Body.String())
		var resp ListAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		out := make([]string, 0, len(resp.Agents))
		for _, a := range resp.Agents {
			out = append(out, a.Name)
		}
		slices.Sort(out)
		assert.Equal(t, len(out), resp.TotalCount)
		return out
	}

	assert.Equal(t, []string{"a1"}, queryNames(t, "activity:working"))
	assert.Equal(t, []string{"a2"}, queryNames(t, "-activity:working"))
	assert.Equal(t, []string{"a1"}, queryNames(t, "project:alpha-slug"))
	assert.Equal(t, []string{"a1"}, queryNames(t, "project:Alpha-Slug"))
	assert.Equal(t, []string{"a1"}, queryNames(t, "project:Alpha-Slug,alpha-slug"))
	assert.Equal(t, []string{"a1"}, queryNames(t, "project:alpha-slug,Alpha-Slug"))
	assert.Equal(t, []string{"a2"}, queryNames(t, "project:"+projB.ID))
	assert.Equal(t, []string{"a2"}, queryNames(t, "project:"+strings.ToUpper(projB.ID)))
	assert.Equal(t, []string{}, queryNames(t, "project:no-such-slug"))
	assert.Equal(t, []string{"a1"}, queryNames(t, "harness:CLAUDE"))
	assert.Equal(t, []string{"a2"}, queryNames(t, "broker:broker-west"))
	assert.Equal(t, []string{"a1"}, queryNames(t, "broker:"+brokerEast.ID))
	assert.Equal(t, []string{}, queryNames(t, "broker:no-such-broker"))
	assert.Equal(t, []string{"a2"}, queryNames(t, "template:reviewer"))
	assert.Equal(t, []string{"a1"}, queryNames(t, "owner:alice@example.com"))
	assert.Equal(t, []string{"a2"}, queryNames(t, "owner:"+bobID))
	assert.Equal(t, []string{}, queryNames(t, "owner:nobody@example.com"))

	// owner:me resolves to the authenticated user's ID.
	recMe := doRequestAsUser(t, srv, aliceUser, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape("owner:me"), nil)
	require.Equal(t, http.StatusOK, recMe.Code)
	var respMe ListAgentsResponse
	require.NoError(t, json.Unmarshal(recMe.Body.Bytes(), &respMe))
	require.Len(t, respMe.Agents, 1)
	assert.Equal(t, "a1", respMe.Agents[0].Name)

	// is:mine and is:shared map to project ownership classification.
	recMine := doRequestAsUser(t, srv, aliceUser, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape("is:mine"), nil)
	require.Equal(t, http.StatusOK, recMine.Code)
	var respMine ListAgentsResponse
	require.NoError(t, json.Unmarshal(recMine.Body.Bytes(), &respMine))
	require.Len(t, respMine.Agents, 1)
	assert.Equal(t, "a1", respMine.Agents[0].Name)

	recShared := doRequestAsUser(t, srv, aliceUser, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape("is:shared"), nil)
	require.Equal(t, http.StatusOK, recShared.Code)
	var respShared ListAgentsResponse
	require.NoError(t, json.Unmarshal(recShared.Body.Bytes(), &respShared))
	require.Len(t, respShared.Agents, 1)
	assert.Equal(t, "a2", respShared.Agents[0].Name)

	// Conflicting is:mine + legacy scope=shared returns empty 200.
	recConflict := doRequestAsUser(t, srv, aliceUser, http.MethodGet, "/api/v1/agents?scope=shared&q="+url.QueryEscape("is:mine"), nil)
	require.Equal(t, http.StatusOK, recConflict.Code)
	var respConflict ListAgentsResponse
	require.NoError(t, json.Unmarshal(recConflict.Body.Bytes(), &respConflict))
	assert.Empty(t, respConflict.Agents)

	// Out-of-scope project slug or uppercase UUID returns empty 200 (never 403/400, never widens authz scope).
	carolID := uuid.NewString()
	carolUser := &store.User{
		ID: carolID, Email: "carol@example.com", DisplayName: "Carol", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, carolUser))
	memberRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		RoleDefinitionID: memberRole.ID,
		PrincipalType:    "user",
		PrincipalID:      carolID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projA.ID,
	})
	require.NoError(t, err)
	for _, q := range []string{"project:beta-slug", "project:" + strings.ToUpper(projB.ID)} {
		recOutOfScope := doRequestAsUser(t, srv, carolUser, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape(q), nil)
		require.Equal(t, http.StatusOK, recOutOfScope.Code)
		var respOutOfScope ListAgentsResponse
		require.NoError(t, json.Unmarshal(recOutOfScope.Body.Bytes(), &respOutOfScope))
		assert.Empty(t, respOutOfScope.Agents)
	}
	// Explicit-scope non-admin caller can match in-scope project via uppercase UUID or mixed-case slug.
	for _, q := range []string{"project:" + strings.ToUpper(projA.ID), "project:Alpha-Slug"} {
		recInScope := doRequestAsUser(t, srv, carolUser, http.MethodGet, "/api/v1/agents?q="+url.QueryEscape(q), nil)
		require.Equal(t, http.StatusOK, recInScope.Code)
		var respInScope ListAgentsResponse
		require.NoError(t, json.Unmarshal(recInScope.Body.Bytes(), &respInScope))
		require.Len(t, respInScope.Agents, 1)
		assert.Equal(t, "a1", respInScope.Agents[0].Name)
	}

	// Two brokers sharing the same non-unique name: broker:<name> resolves to BOTH deterministically.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: uuid.NewString(), Name: "dup-1", Slug: "dup-1", ProjectID: projA.ID,
		Phase: "running", RuntimeBrokerID: brokerDup1.ID,
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: uuid.NewString(), Name: "dup-2", Slug: "dup-2", ProjectID: projA.ID,
		Phase: "running", RuntimeBrokerID: brokerDup2.ID,
	}))
	assert.Equal(t, []string{"dup-1", "dup-2"}, queryNames(t, "broker:shared-name"))
}

func TestListProjectAgents_FilterQuery(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projA := &store.Project{ID: uuid.NewString(), Name: "ProjA", Slug: "proj-a"}
	projB := &store.Project{ID: uuid.NewString(), Name: "ProjB", Slug: "proj-b"}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: uuid.NewString(), Name: "pa-run", Slug: "pa-run", ProjectID: projA.ID,
		Phase: "running", Activity: "working", Template: "developer",
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: uuid.NewString(), Name: "pa-stop", Slug: "pa-stop", ProjectID: projA.ID,
		Phase: "stopped", Template: "reviewer",
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: uuid.NewString(), Name: "pb-run", Slug: "pb-run", ProjectID: projB.ID,
		Phase: "running", Activity: "working", Template: "developer",
	}))

	// Experiment off: ignores q.
	recOff := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projA.ID+"/agents?q="+url.QueryEscape("phase:running"), nil)
	require.Equal(t, http.StatusOK, recOff.Code)
	var respOff ListAgentsResponse
	require.NoError(t, json.Unmarshal(recOff.Body.Bytes(), &respOff))
	assert.Len(t, respOff.Agents, 2)

	// Enable experiment.
	enableFiltersQueryDialect(t, srv)

	// Filters within project A only.
	recOn := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projA.ID+"/agents?q="+url.QueryEscape("phase:running harness:claude"), nil)
	require.Equal(t, http.StatusOK, recOn.Code)
	var respOn ListAgentsResponse
	require.NoError(t, json.Unmarshal(recOn.Body.Bytes(), &respOn))
	require.Len(t, respOn.Agents, 1)
	assert.Equal(t, "pa-run", respOn.Agents[0].Name)
	assert.Equal(t, 1, respOn.TotalCount)

	// Client-only key mode: is rejected with 400.
	recMode := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projA.ID+"/agents?q="+url.QueryEscape("mode:lineage"), nil)
	require.Equal(t, http.StatusBadRequest, recMode.Code)

	// project: is not a known key on project_agents, so it searches as text and never widens to projB.
	recProj := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projA.ID+"/agents?q="+url.QueryEscape("project:proj-b"), nil)
	require.Equal(t, http.StatusOK, recProj.Code)
	var respProj ListAgentsResponse
	require.NoError(t, json.Unmarshal(recProj.Body.Bytes(), &respProj))
	assert.Empty(t, respProj.Agents)
}
