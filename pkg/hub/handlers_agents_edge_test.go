// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// edgeWriteErrStore fails CreateDelegationEdge, including inside WithTx.
type edgeWriteErrStore struct {
	store.Store
}

func (s *edgeWriteErrStore) CreateDelegationEdge(context.Context, *store.DelegationEdge) error {
	return errors.New("injected delegation edge write fault")
}

func (s *edgeWriteErrStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&edgeWriteErrStore{Store: tx})
	})
}

// activeEdgesFor returns the active project edges for an agent.
func activeEdgesFor(t *testing.T, s store.Store, agentID string) []*store.DelegationEdge {
	t.Helper()
	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	var active []*store.DelegationEdge
	for _, e := range edges {
		if e.Active {
			active = append(active, e)
		}
	}
	return active
}

// An edge write failure rolls back the whole create: no agent row, no
// identity key, no edge.
func TestCreateEdgeFailureRollsBack(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := setupProjectWithBroker(t, s, "edge-rollback", "Edge Rollback")

	srv.store = &edgeWriteErrStore{Store: s}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents",
		CreateAgentRequest{Name: "rollback-agent"})
	assert.GreaterOrEqual(t, rec.Code, 500, "body: %s", rec.Body.String())

	_, err := s.GetAgentBySlug(ctx, project.ID, "rollback-agent")
	assert.ErrorIs(t, err, store.ErrNotFound, "no agent row survives a failed edge write")
	agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, agents.Items, "no agent row in the project")

	// Control: the same create without the fault succeeds and writes the
	// edge in the same transaction.
	srv.store = s
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents",
		CreateAgentRequest{Name: "rollback-agent"})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted,
		"control create: %d %s", rec.Code, rec.Body.String())
	agent, err := s.GetAgentBySlug(ctx, project.ID, "rollback-agent")
	require.NoError(t, err)
	assert.Len(t, activeEdgesFor(t, s, agent.ID), 1)
}

// A create through DevAuthMiddleware records a principal edge with dev_local
// provenance and delegator user:<DevUserID>; the edge role equals the stored
// role.
func TestDevAuthCreateRecordsPrincipalCeiling(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := setupProjectWithBroker(t, s, "dev-edge", "Dev Edge")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents",
		CreateAgentRequest{Name: "dev-child"})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted,
		"create: %d %s", rec.Code, rec.Body.String())
	agent, err := s.GetAgentBySlug(ctx, project.ID, "dev-child")
	require.NoError(t, err)

	edges := activeEdgesFor(t, s, agent.ID)
	require.Len(t, edges, 1)
	e := edges[0]
	assert.Equal(t, store.DelegationPrincipalUser, e.DelegatorType)
	assert.Equal(t, DevUserID, e.DelegatorID)
	assert.Equal(t, store.EffectCeilingPrincipal, e.Kind)
	assert.Nil(t, e.PermissionIDs)
	assert.Equal(t, 1, e.ProvenanceVersion)
	assert.Equal(t, store.SourceCredentialDevLocal, e.SourceCredentialKind)
	assert.Equal(t, "user", e.SourcePrincipalKind)
	assert.Equal(t, DevUserID, e.SourcePrincipalID)
	assert.Empty(t, e.SourceCredentialID)
	assert.Equal(t, agent.AppliedConfig.AgentRole, e.Role, "edge role equals the stored role")
	assertEdgeDelegatorIsSourcePrincipal(t, e)
}

// assertEdgeDelegatorIsSourcePrincipal asserts that the edge's delegator is
// the principal its recorded provenance names as the source.
func assertEdgeDelegatorIsSourcePrincipal(t *testing.T, e *store.DelegationEdge) {
	t.Helper()
	require.NotEmpty(t, e.DelegatorID)
	assert.Equal(t, e.SourcePrincipalKind, e.DelegatorType, "delegator type is the source principal kind")
	assert.Equal(t, e.SourcePrincipalID, e.DelegatorID, "delegator ID is the source principal ID")
}
