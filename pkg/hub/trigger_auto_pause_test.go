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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTriggerAutoPause_CreatorAndTargetDeleted verifies Stage 1 §5.6:
//   - deleting a user pauses all active triggers whose recorded authority is
//     that user across projects with creator_deleted;
//   - deleting an agent pauses active triggers whose recorded authority is
//     that agent with creator_deleted;
//   - deleting a message target agent pauses active triggers (including
//     cross-project triggers) whose action_payload.agentId matches with
//     target_deleted;
//   - a self-targeted trigger authored by the deleted agent records
//     target_deleted (target pause runs before creator pause).
func TestTriggerAutoPause_CreatorAndTargetDeleted(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	mkEndpoint := func(projectID, ownerKind, ownerID, name string) *store.WebhookEndpoint {
		t.Helper()
		ep := &store.WebhookEndpoint{
			ID:                  api.NewUUID(),
			ProjectID:           projectID,
			Name:                name,
			OwnerPrincipalKind:  ownerKind,
			OwnerPrincipalID:    ownerID,
			OwnerCredentialKind: store.InitiatorCredentialKindSession,
			SecretVersion:       1,
			SignatureMode:       store.WebhookSignatureModeScionV1,
			Status:              store.WebhookEndpointStatusActive,
			CreatedBy:           ownerID,
		}
		require.NoError(t, f.store.CreateWebhookEndpoint(ctx, ep))
		return ep
	}

	mkTrigger := func(projectID, epID, name, authorKind, authorID, targetAgentID, targetSlug string) *store.Trigger {
		t.Helper()
		credKind := store.InitiatorCredentialKindSession
		if authorKind == store.DelegationPrincipalAgent {
			credKind = store.InitiatorCredentialKindAgent
		}
		tr := &store.Trigger{
			ID:             api.NewUUID(),
			ProjectID:      projectID,
			Name:           name,
			SourceKind:     store.TriggerSourceKindWebhook,
			SourceID:       epID,
			MatchTypes:     []string{"webhook.received"},
			MatchDialect:   store.TriggerDialectCEL,
			ActionType:     store.TriggerActionMessage,
			ActionPayload:  `{"agentId":"` + targetAgentID + `","agentName":"` + targetSlug + `","message":"hi"}`,
			SubjectDialect: store.TriggerDialectCEL,
			SubjectMode:    store.TriggerSubjectModeNew,
			Status:         store.TriggerStatusActive,
			CreatedBy:      authorID,
			InitiatorAttribution: store.InitiatorAttribution{
				AttributionVersion:      1,
				InitiatorPrincipalKind:  authorKind,
				InitiatorPrincipalID:    authorID,
				InitiatorCredentialKind: credKind,
				AuthorizationRevision:   1,
			},
			AuthorityCeiling: store.EffectCeiling{
				Version: 1,
				Kind:    store.EffectCeilingPrincipal,
			},
		}
		require.NoError(t, f.store.CreateTrigger(ctx, tr))
		return tr
	}

	epMain := mkEndpoint(f.projectID, store.DelegationPrincipalUser, f.owner.ID, "ep-main")
	epOther := mkEndpoint(f.otherProjID, store.DelegationPrincipalUser, f.owner.ID, "ep-other")

	// 1. User delete pauses authored triggers across both projects with creator_deleted.
	trUserMain := mkTrigger(f.projectID, epMain.ID, "tr-user-main", store.DelegationPrincipalUser, f.member.ID, f.targetAgent.ID, f.targetAgent.Slug)
	trUserOther := mkTrigger(f.otherProjID, epOther.ID, "tr-user-other", store.DelegationPrincipalUser, f.member.ID, f.targetAgent.ID, f.targetAgent.Slug)
	trOwnerUnaffected := mkTrigger(f.projectID, epMain.ID, "tr-owner-keep", store.DelegationPrincipalUser, f.owner.ID, f.targetAgent.ID, f.targetAgent.Slug)

	f.srv.pauseSchedulesAuthoredByUser(ctx, f.member.ID)

	for _, id := range []string{trUserMain.ID, trUserOther.ID} {
		got, err := f.store.GetTrigger(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, store.TriggerStatusPaused, got.Status)
		assert.Equal(t, store.TriggerStatusReasonCreatorDeleted, got.StatusReason)
	}
	gotOwner, err := f.store.GetTrigger(ctx, trOwnerUnaffected.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusActive, gotOwner.Status)

	// 2. Agent delete: target pause runs before creator pause, so:
	//    - a trigger targeting agentA (same-project and cross-project) -> target_deleted
	//    - a self-targeted trigger authored by agentA targeting agentA -> target_deleted
	//    - a trigger authored by agentA targeting another agent -> creator_deleted
	//    - an unrelated trigger -> stays active.
	trTargetSame := mkTrigger(f.projectID, epMain.ID, "tr-target-same", store.DelegationPrincipalUser, f.owner.ID, f.agentA.ID, f.agentA.Slug)
	trTargetCross := mkTrigger(f.otherProjID, epOther.ID, "tr-target-cross", store.DelegationPrincipalUser, f.owner.ID, f.agentA.ID, f.agentA.Slug)
	trSelfTargeted := mkTrigger(f.projectID, epMain.ID, "tr-self-targeted", store.DelegationPrincipalAgent, f.agentA.ID, f.agentA.ID, f.agentA.Slug)
	trAuthoredByA := mkTrigger(f.projectID, epMain.ID, "tr-authored-by-a", store.DelegationPrincipalAgent, f.agentA.ID, f.targetAgent.ID, f.targetAgent.Slug)
	trUnrelated := mkTrigger(f.projectID, epMain.ID, "tr-unrelated", store.DelegationPrincipalUser, f.owner.ID, f.agentB.ID, f.agentB.Slug)

	// Mirror agent_delete_engine.go tailStep order: target pause then creator pause.
	f.srv.pauseTriggersTargetingAgent(ctx, f.agentA)
	f.srv.pauseSchedulesAuthoredByAgent(ctx, f.agentA)

	for _, id := range []string{trTargetSame.ID, trTargetCross.ID, trSelfTargeted.ID} {
		got, err := f.store.GetTrigger(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, store.TriggerStatusPaused, got.Status, "trigger %s", id)
		assert.Equal(t, store.TriggerStatusReasonTargetDeleted, got.StatusReason, "trigger %s", id)
	}
	gotAuthored, err := f.store.GetTrigger(ctx, trAuthoredByA.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusPaused, gotAuthored.Status)
	assert.Equal(t, store.TriggerStatusReasonCreatorDeleted, gotAuthored.StatusReason)

	gotUnrelated, err := f.store.GetTrigger(ctx, trUnrelated.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusActive, gotUnrelated.Status)
}
