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
}
