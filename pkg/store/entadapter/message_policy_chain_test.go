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
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func TestChainStore_RoundTripAndRevision(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	mpStore := entadapter.NewMessagePolicyStore(client)
	projectID := uuid.NewString()

	chain := &store.MessagePolicyChain{
		Version: store.MessagePolicyChainVersion,
		Steps: []store.MessagePolicyStep{
			{
				ID:      "is_user",
				Role:    store.MessagePolicyStepRoleGuard,
				Dialect: store.MessagePolicyStepDialectCEL,
				Expr:    `surface == "agent_to_user"`,
			},
			{
				ID:          "block_secret",
				Role:        store.MessagePolicyStepRoleViolation,
				Dialect:     store.MessagePolicyStepDialectCEL,
				Expr:        `message.body.contains("SECRET")`,
				Reason:      "Secret detected",
				Remediation: "Remove secret",
			},
		},
		RejectReason:      "Chain default reason",
		RejectRemediation: "Chain default remediation",
	}

	pol := &store.MessagePolicy{
		ProjectID:     projectID,
		Name:          "chain-roundtrip",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: "true",
		RejectReason:  "Policy chain-roundtrip requires a newer Hub (rule chain); send blocked.",
		Chain:         chain,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}

	require.NoError(t, mpStore.CreateMessagePolicy(ctx, pol))
	assert.Equal(t, int64(1), pol.Revision)

	got, err := mpStore.GetMessagePolicy(ctx, pol.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Chain)
	assert.Equal(t, store.MessagePolicyChainVersion, got.Chain.Version)
	require.Len(t, got.Chain.Steps, 2)
	assert.Equal(t, "is_user", got.Chain.Steps[0].ID)
	assert.Equal(t, "block_secret", got.Chain.Steps[1].ID)
	assert.Equal(t, "Secret detected", got.Chain.Steps[1].Reason)
	assert.Equal(t, "Chain default reason", got.Chain.RejectReason)

	// Reorder steps and update -> revision bumps to 2 and order is persisted.
	got.Chain.Steps = []store.MessagePolicyStep{got.Chain.Steps[1], got.Chain.Steps[0]}
	require.NoError(t, mpStore.UpdateMessagePolicy(ctx, got))
	assert.Equal(t, int64(2), got.Revision)

	reloaded, err := mpStore.GetMessagePolicy(ctx, pol.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.Chain)
	assert.Equal(t, int64(2), reloaded.Revision)
	assert.Equal(t, "block_secret", reloaded.Chain.Steps[0].ID)
	assert.Equal(t, "is_user", reloaded.Chain.Steps[1].ID)

	// Explicit Clear on update removes chain column (NULL).
	reloaded.Chain = nil
	require.NoError(t, mpStore.UpdateMessagePolicy(ctx, reloaded))
	assert.Equal(t, int64(3), reloaded.Revision)

	cleared, err := mpStore.GetMessagePolicy(ctx, pol.ID)
	require.NoError(t, err)
	assert.Nil(t, cleared.Chain)

	// Decision row chain JSON round-trip.
	chainProv := json.RawMessage(`{"v":1,"source":"authored","through":1,"engine_class":"fail","consumer_handling":"skip_policy","verdict":"skip_policy","steps":[{"i":0,"id":"is_user","role":"guard","dialect":"cel","class":"fail","duration_ms":0}],"unexecuted":[1]}`)
	dec := &store.MessagePolicyDecision{
		ProjectID:      projectID,
		PolicyID:       pol.ID,
		PolicyName:     pol.Name,
		PolicyRevision: 2,
		Mode:           store.MessagePolicyModeEnforce,
		Surface:        store.MessagePolicySurfaceAgentToUser,
		Class:          store.MessagePolicyClassPass,
		Action:         store.MessagePolicyActionAdmitted,
		Chain:          chainProv,
	}
	require.NoError(t, mpStore.CreateMessagePolicyDecision(ctx, dec))

	list, err := mpStore.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: projectID,
		PolicyID:  pol.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.JSONEq(t, string(chainProv), string(list.Items[0].Chain))
}
