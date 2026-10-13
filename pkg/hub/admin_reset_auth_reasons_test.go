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
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetAuthAllBody is the bulk reset-auth response.
type resetAuthAllBody struct {
	Succeeded []struct {
		ID string `json:"id"`
	} `json:"succeeded"`
	Failed       []map[string]any `json:"failed"`
	Total        int              `json:"total"`
	FailedByCode map[string]int   `json:"failed_by_code"`
}

// postResetAuthAll runs the bulk reset-auth and decodes its response.
func postResetAuthAll(t *testing.T, srv *Server) (resetAuthAllBody, string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, adminResetAuthAllPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body resetAuthAllBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body, rec.Body.String()
}

// healthyResetAuthAgent stores a running agent with a user edge, which the
// bulk reset-auth resets successfully.
func healthyResetAuthAgent(t *testing.T, f *mintFixture, slug string) *store.Agent {
	t.Helper()
	a := f.agent(t, slug, AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceilPrincip, provSession)
	return a
}

// requireCreatorMissingEntry checks the one failed entry: the stable code,
// the deny cause, the named creator or ancestor, and the plain reason, with
// no raw error text.
func requireCreatorMissingEntry(t *testing.T, body resetAuthAllBody, raw string, agent *store.Agent, ancestorID string, depth int, reason string) {
	t.Helper()
	require.Len(t, body.Failed, 1, raw)
	assert.Equal(t, map[string]any{
		"id": agent.ID, "name": agent.Name,
		"code":           resetAuthCodeCreatorMissing,
		"cause":          string(DenyCauseCeilingOrphaned),
		"ancestor_id":    ancestorID,
		"ancestor_depth": float64(depth),
		"error":          reason,
	}, body.Failed[0])
	assert.NotContains(t, raw, "provenance:", "no raw error text in the response")
	assert.NotContains(t, raw, "agent token not issued", "no raw error text in the response")
	assert.Equal(t, map[string]int{resetAuthCodeCreatorMissing: 1}, body.FailedByCode)
}

// A running agent whose creator was hard-deleted is reported with
// delegation_creator_missing, the creator's ID named as the creator, and a
// failed_by_code count; a healthy agent in the same run still succeeds.
func TestAdminResetAuthAll_DeletedCreator(t *testing.T) {
	f := newMintFixture(t, "rar-deleted")
	setBackfillCompleted(t, f.store)
	ctx := context.Background()

	parent := f.agent(t, "rar-deleted-parent", AgentRoleFull, state.PhaseStopped)
	f.edge(t, store.DelegationPrincipalUser, f.userID, parent.ID, ceilPrincip, provSession)
	child := f.childAgent(t, "rar-deleted-child", parent, AgentRoleFull)
	ok := healthyResetAuthAgent(t, f, "rar-deleted-ok")

	require.NoError(t, f.store.WithTx(ctx, func(tx store.Store) error {
		if err := f.srv.hardDeleteAgentTx(ctx, tx, parent, AuditActor{}); err != nil {
			return err
		}
		return tx.DeleteAgent(ctx, parent.ID)
	}))

	body, raw := postResetAuthAll(t, f.srv)

	assert.Equal(t, 2, body.Total)
	require.Len(t, body.Succeeded, 1, raw)
	assert.Equal(t, ok.ID, body.Succeeded[0].ID)
	requireCreatorMissingEntry(t, body, raw, child, parent.ID, 1,
		fmt.Sprintf("creator agent %s has no active delegation in this project (deleted or moved); recreate this agent", parent.ID))
	assertIssueDeniedAudit(t, f.store, child.ID, mintSiteResetAuth, string(DenyCauseCeilingOrphaned))
}

// A running agent whose creator is in another project (its only edge is
// scoped there) is reported the same way.
func TestAdminResetAuthAll_CreatorInOtherProject(t *testing.T) {
	f := newMintFixture(t, "rar-moved")
	setBackfillCompleted(t, f.store)
	ctx := context.Background()
	other := setupProjectWithBroker(t, f.store, "rar-moved-other", "rar-moved-other")

	parent := &store.Agent{
		ID: tid("rar-moved-parent"), Slug: "rar-moved-parent", Name: "rar-moved-parent",
		ProjectID: other.ID, OwnerID: f.userID, Phase: string(state.PhaseStopped), StateVersion: 1,
		Ancestry:      []string{f.userID},
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(ctx, parent))
	require.NoError(t, f.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.userID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: parent.ID,
		ScopeType: store.RoleScopeProject, ScopeID: other.ID,
		Role: string(AgentRoleFull), Active: true,
		AuthorityProvenance: provSession, EffectCeiling: ceilPrincip,
	}))
	child := f.childAgent(t, "rar-moved-child", parent, AgentRoleFull)

	body, raw := postResetAuthAll(t, f.srv)

	assert.Equal(t, 1, body.Total)
	assert.Empty(t, body.Succeeded, raw)
	requireCreatorMissingEntry(t, body, raw, child, parent.ID, 1,
		fmt.Sprintf("creator agent %s has no active delegation in this project (deleted or moved); recreate this agent", parent.ID))
}

// A missing edge two hops up names that agent as an ancestor, with its
// depth.
func TestAdminResetAuthAll_AncestorMissing(t *testing.T) {
	f := newMintFixture(t, "rar-ancestor")
	setBackfillCompleted(t, f.store)

	root := f.agent(t, "rar-ancestor-root", AgentRoleFull, state.PhaseStopped)
	f.edge(t, store.DelegationPrincipalUser, f.userID, root.ID, ceilPrincip, provSession)
	parent := f.childAgent(t, "rar-ancestor-parent", root, AgentRoleFull)
	parent.Phase = string(state.PhaseStopped)
	require.NoError(t, f.store.UpdateAgent(context.Background(), parent))
	child := f.childAgent(t, "rar-ancestor-child", parent, AgentRoleFull)
	revokeDelegateEdges(t, f.store, root.ID)

	body, raw := postResetAuthAll(t, f.srv)

	requireCreatorMissingEntry(t, body, raw, child, root.ID, 2,
		fmt.Sprintf("ancestor agent %s (2 levels up the creator chain) has no active delegation in this project (deleted or moved); recreate this agent", root.ID))
}

// The agent's own missing edge and any other structural chain outcome (here
// a cycle in the chain) each get their own code and count.
func TestAdminResetAuthAll_OtherDelegationCategories(t *testing.T) {
	f := newMintFixture(t, "rar-other")
	setBackfillCompleted(t, f.store)

	own := f.agent(t, "rar-other-own", AgentRoleFull, state.PhaseRunning)
	cyclic := f.agent(t, "rar-other-cyclic", AgentRoleFull, state.PhaseRunning)
	partner := f.agent(t, "rar-other-partner", AgentRoleFull, state.PhaseStopped)
	f.edge(t, store.DelegationPrincipalAgent, partner.ID, cyclic.ID, ceilPrincip, agentProv(partner.ID))
	f.edge(t, store.DelegationPrincipalAgent, cyclic.ID, partner.ID, ceilPrincip, agentProv(cyclic.ID))

	body, raw := postResetAuthAll(t, f.srv)

	assert.Equal(t, 2, body.Total)
	assert.Empty(t, body.Succeeded, raw)
	byID := map[string]map[string]any{}
	for _, e := range body.Failed {
		byID[e["id"].(string)] = e
	}
	assert.Equal(t, map[string]any{
		"id": own.ID, "name": own.Name,
		"code":  resetAuthCodeOwnMissing,
		"cause": string(DenyCauseCeilingOrphaned),
		"error": "this agent has no active delegation of its own in this project; reincarnate or recreate this agent",
	}, byID[own.ID])
	assert.Equal(t, map[string]any{
		"id": cyclic.ID, "name": cyclic.Name,
		"code":  resetAuthCodeDelegationCeiling,
		"cause": string(DenyCauseCeilingOrphaned),
		"error": "this agent's delegation chain does not authorize a new credential (ceiling_orphaned); see the hub log for details",
	}, byID[cyclic.ID])
	assert.NotContains(t, raw, "provenance:", "no raw error text in the response")
	assert.Equal(t, map[string]int{resetAuthCodeOwnMissing: 1, resetAuthCodeDelegationCeiling: 1}, body.FailedByCode)
}

// The success path carries an empty failed_by_code and unchanged entries.
func TestAdminResetAuthAll_SuccessShape(t *testing.T) {
	f := newMintFixture(t, "rar-success")
	setBackfillCompleted(t, f.store)
	ok := healthyResetAuthAgent(t, f, "rar-success-ok")

	rec := doRequest(t, f.srv, http.MethodPost, adminResetAuthAllPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, fmt.Sprintf(`{"succeeded":[{"id":%q,"name":%q}],"failed":null,"total":1,"failed_by_code":{}}`, ok.ID, ok.Name),
		rec.Body.String())
	assert.True(t, f.client.resetAuthCalled)
}

// resetAuthDelegationFailure classifies only structural chain outcomes: a
// lookup fault, an agent not in good standing, a credential record failure
// and a broker error are not delegation failures.
func TestResetAuthDelegationFailure_NotStructural(t *testing.T) {
	cases := map[string]error{
		"lookup":   &agentTokenIssueError{Site: mintSiteResetAuth, Lookup: true, Err: errMintLookup},
		"standing": &agentTokenIssueError{Site: mintSiteResetAuth, Standing: true, Err: errAgentNotInStanding},
		"record":   fmt.Errorf("x: %w", errAgentTokenRecord),
		"broker":   errors.New("broker unreachable"),
		"nil":      nil,
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			_, ok := resetAuthDelegationFailure(err)
			assert.False(t, ok)
		})
	}
}
