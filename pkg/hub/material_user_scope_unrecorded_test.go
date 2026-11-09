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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestAgentSecretRead_UnrecordedChainAnswersAsMissing: a by-key read
// refused as chain_unrecorded answers exactly as a read of a key the owner
// does not have: same status, same body bytes, same content type. The
// caller cannot tell whether the owner's row exists.
func TestAgentSecretRead_UnrecordedChainAnswersAsMissing(t *testing.T) {
	f := newUserScopeChainFixture(t, "usu-missing")
	require.Equal(t, ReasonChainUnrecorded, f.userItem(t, f.AgentID, userScopeCeilingKey).Reason, "control: the row exists and is refused")

	get := func(key string) (int, []byte, string) {
		rec := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			"/api/v1/agents/"+f.AgentID+"/secrets/"+key+"?scope=user", nil, f.Token)
		return rec.Code, rec.Body.Bytes(), rec.Header().Get("Content-Type")
	}
	refusedCode, refusedBody, refusedType := get(userScopeCeilingKey)
	missingCode, missingBody, missingType := get("USU_ABSENT_KEY")

	assert.Equal(t, http.StatusNotFound, missingCode, "control: a missing key is not found")
	assert.Equal(t, missingCode, refusedCode, "the refusal has the missing-key status")
	assert.Equal(t, missingBody, refusedBody, "the refusal has the missing-key body, byte for byte")
	assert.Equal(t, missingType, refusedType)
	assert.NotContains(t, string(refusedBody), userScopeCeilingKey)
}

// TestAgentSecretList_UnrecordedChainItemCarriesNoRowFacts: the
// request-level chain_unrecorded item of the agent secret list names no row
// and carries no count of the owner's rows: the item, and the list the
// agent receives, are the same whether the owner has one shared secret or
// three.
func TestAgentSecretList_UnrecordedChainItemCarriesNoRowFacts(t *testing.T) {
	f := newUserScopeChainFixture(t, "usu-list")
	ctx := context.Background()
	listItems := func() []MaterialSelectionEventItem {
		ident, facts := f.itemFacts(t, f.AgentID)
		secrets, items, ok := f.Server.agentListUserSecrets(ctx, f.AgentID, ident, facts, nil, nil)
		require.True(t, ok)
		assert.Empty(t, secrets, "no user key is listed")
		return items
	}
	listBody := func() []byte {
		rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets?scope=user", nil, f.Token)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec.Body.Bytes()
	}

	one := listItems()
	require.Len(t, one, 1, "one request-level item")
	item := one[0]
	assert.Equal(t, ReasonChainUnrecorded, item.Reason)
	assert.Empty(t, item.Key, "the item names no row")
	assert.Nil(t, item.SharingSource, "the item names no row author")
	assert.NotContains(t, item.Detail, userScopeCeilingKey)
	bodyOne := listBody()

	extra := []string{"USU_EXTRA_ONE", "USU_EXTRA_TWO"}
	for _, key := range extra {
		_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name: key, Value: "v", SecretType: store.SecretTypeEnvironment, Target: key,
			Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)
	}
	three := listItems()
	assert.Equal(t, one, three, "the item is the same for one row or three: it carries no row count")
	for _, key := range extra {
		assert.NotContains(t, three[0].Detail, key)
		assert.NotContains(t, string(listBody()), key)
	}
	assert.Equal(t, bodyOne, listBody(), "the list the agent receives is the same for one row or three")
}

// TestRuntimeReadCensus: the delivery census lists every live agent with
// ancestry whose chain is not recorded (cohort runtime_read_refused), with
// the shape and depth of the stop, the ancestor agent above the agent, the
// root user, the names (never values) of the root user's shared secrets
// and the remedy; an agent whose chain is recorded is not listed.
func TestRuntimeReadCensus(t *testing.T) {
	f := newUserScopeChainFixture(t, "usu-census")
	ctx := context.Background()
	child, _ := f.mixedChain(t, "usu-census-mixed", store.EffectCeiling{Kind: store.EffectCeilingPrincipal})
	recorded, _ := f.childAgent(t, "usu-census-recorded", []string{f.UserID}, AgentRoleFull)
	f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, recorded, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)

	rep, err := f.Server.authzService.LaunchDeliveryCensus(ctx, false)
	require.NoError(t, err)
	rows := map[string]RuntimeReadCensusRow{}
	for _, r := range rep.RuntimeRead.Rows {
		rows[r.AgentID] = r
	}
	assert.NotContains(t, rows, recorded, "a recorded chain is not listed")

	own, ok := rows[f.AgentID]
	require.True(t, ok, "the agent without an own edge is listed: %+v", rep.RuntimeRead.Rows)
	assert.Equal(t, runtimeReadCohortRefused, own.Cohort)
	assert.Equal(t, unrecordedShapeNoEdge, own.Shape)
	assert.Zero(t, own.Depth)
	assert.Empty(t, own.AncestorAgentID)
	assert.Equal(t, f.UserID, own.RootUserID)
	assert.Equal(t, []string{userScopeCeilingKey}, own.SharedSecrets)
	assert.Equal(t, ownerMaterialChainRemedy, own.Remedy)

	above, ok := rows[child]
	require.True(t, ok, "the agent with an unrecorded hop above is listed")
	assert.Equal(t, unrecordedShapeUnrecordedAbove, above.Shape)
	assert.Equal(t, 1, above.Depth)
	assert.Equal(t, f.mixedParent, above.AncestorAgentID)

	assert.GreaterOrEqual(t, rep.RuntimeRead.ByCohort[runtimeReadCohortRefused], 2)
	assert.GreaterOrEqual(t, rep.RuntimeRead.ByShape[unrecordedShapeNoEdge], 1)
	assert.GreaterOrEqual(t, rep.RuntimeRead.ByShape[unrecordedShapeUnrecordedAbove], 1)
}

// TestRuntimeReadReRecordNotice: a re-record whose new delegator is an agent
// whose own chain is not recorded leaves the agent's chain unrecorded above
// its own edge, so the plan warns that runtime reads stay refused; a user
// delegator or a recorded agent delegator gives no notice.
func TestRuntimeReadReRecordNotice(t *testing.T) {
	f := newUserScopeChainFixture(t, "usu-notice")
	ctx := context.Background()
	child, _ := f.mixedChain(t, "usu-notice-mixed", store.EffectCeiling{Kind: store.EffectCeilingPrincipal})
	recordedParent, _ := f.childAgent(t, "usu-notice-recorded", []string{f.UserID}, AgentRoleFull)
	f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, recordedParent, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
	agent, err := f.Store.GetAgent(ctx, child)
	require.NoError(t, err)

	assert.Equal(t, runtimeReadReRecordNoticeText,
		f.Server.runtimeReadReRecordNotice(ctx, agent, store.DelegationPrincipalAgent, f.mixedParent), "an unrecorded agent delegator")
	assert.Empty(t, f.Server.runtimeReadReRecordNotice(ctx, agent, store.DelegationPrincipalUser, f.UserID), "a user delegator")
	assert.Empty(t, f.Server.runtimeReadReRecordNotice(ctx, agent, store.DelegationPrincipalAgent, recordedParent), "a recorded agent delegator")
}
