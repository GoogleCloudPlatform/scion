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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// seedFullAgentEdge records an active, recorded, principal-ceiling project
// edge from delegator to agent with the full role, and returns it.
func seedFullAgentEdge(t *testing.T, s store.Store, delegatorType, delegatorID string, agent *store.Agent) *store.DelegationEdge {
	t.Helper()
	kind := store.SourceCredentialSession
	if delegatorType == store.DelegationPrincipalAgent {
		kind = store.SourceCredentialAgent
	}
	e := &store.DelegationEdge{
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agent.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       agent.ProjectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  delegatorType,
			SourcePrincipalID:    delegatorID,
			SourceCredentialKind: kind,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, s.CreateDelegationEdge(context.Background(), e))
	return e
}

// fullRequesterFor is an agent identity that may reincarnate a full-role
// agent: the lifecycle scope plus every scope of the full role.
func fullRequesterFor(requesterID, projectID string) AgentIdentity {
	return agentIdentityFor(requesterID, projectID, append(ScopesForRole(AgentRoleFull), ScopeAgentLifecycle)...)
}

// patchTestSA persists a GCP service account scoped to projectID.
func patchTestSA(t *testing.T, s store.Store, projectID string, verified bool, createdBy string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        uuid.New().String(),
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Email:     fmt.Sprintf("sa-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
		ProjectID: "gcp-proj",
		CreatedBy: createdBy,
		Verified:  verified,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// reincarnateAsDev runs a reincarnate request as the dev user through the
// full HTTP stack.
func reincarnateAsDev(t *testing.T, srv *Server, agentID string, body ReincarnateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/reincarnate", body)
}

func snapshotAgent(t *testing.T, s store.Store, agentID string) agentSnapshot {
	t.Helper()
	a, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	applied, err := json.Marshal(a.AppliedConfig.ResponseView(true))
	require.NoError(t, err)
	return agentSnapshot{
		stateVersion: a.StateVersion,
		phase:        a.Phase,
		generation:   a.Generation,
		brokerID:     a.RuntimeBrokerID,
		reincState:   a.ReincarnationState,
		applied:      applied,
	}
}

// agentSnapshot is what a refused reincarnation must leave unchanged.
type agentSnapshot struct {
	stateVersion int64
	phase        string
	generation   int
	brokerID     string
	reincState   string
	applied      []byte
}
