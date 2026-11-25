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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	entagent "github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMessageMode_ParityAndExhaustiveness pins every switch site and enum list
// to store.MessageModes() so adding a mode fails here if any site falls to
// default.
func TestMessageMode_ParityAndExhaustiveness(t *testing.T) {
	modes := store.MessageModes()

	t.Run("Ent enum matches store.MessageModes", func(t *testing.T) {
		for _, m := range modes {
			assert.NoError(t, entagent.MessageModeValidator(entagent.MessageMode(m)), m)
		}
		for _, bad := range []string{"", "lineage", "branch", "bogus"} {
			assert.Error(t, entagent.MessageModeValidator(entagent.MessageMode(bad)), bad)
		}
	})

	t.Run("api.ValidMessageModes matches store.MessageModes", func(t *testing.T) {
		assert.Equal(t, modes, api.ValidMessageModes())
	})

	t.Run("GET /api/v1/messaging/capabilities supportedModes matches store.MessageModes", func(t *testing.T) {
		srv, _, owner, _, _ := msgAuthzSetup(t)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/messaging/capabilities", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), msgAuthzUserIdentity(owner.ID)))
		rec := httptest.NewRecorder()
		srv.handleMessagingCapabilities(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp messagingCapabilitiesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, modes, resp.SupportedModes)
	})

	t.Run("every switch site handles every valid mode without falling to default", func(t *testing.T) {
		srv, s, owner, _, projectID := msgAuthzSetup(t)
		ctx := context.Background()
		ownerIdent := msgAuthzUserIdentity(owner.ID)
		conv := groupModeConversation(t, s, projectID, "")

		for _, mode := range modes {
			t.Run(mode, func(t *testing.T) {
				ag := msgAuthzAgent(t, s, "exhaust-"+mode, projectID, mode, []string{owner.ID})

				// 1. agentGroupPostDecision must not return unknown mode
				dec, err := srv.agentGroupPostDecision(ctx, ag, conv.ID)
				require.NoError(t, err)
				assert.NotEqual(t, groupModeReasonUnknownMode, dec.Reason)

				// 2. groupAgentJoinAllowed must not return unknown mode
				_, joinReason := srv.groupAgentJoinAllowed(ctx, ownerIdent, ag)
				assert.NotEqual(t, groupModeReasonUnknownMode, joinReason)

				// 3. precheckThreadGroupPost must not return unknown mode
				rec := httptest.NewRecorder()
				_ = srv.precheckThreadGroupPost(ctx, rec, ag, projectID, "nonexistent-ext", "nonexistent-thread", "slack")
				assert.NotContains(t, rec.Body.String(), groupModeReasonUnknownMode)

				// 4. agentThreadPostPolicy with empty conversationID returns errChatThreadUnresolved for narrow modes
				_, policyErr := srv.agentThreadPostPolicy(ctx, ag, "")
				if store.IsNarrowMessageMode(mode) {
					assert.ErrorIs(t, policyErr, errChatThreadUnresolved)
				} else {
					assert.NoError(t, policyErr)
				}

				// 5. groupParticipantCleanupReason must not return unknown_mode
				cleanupReason := groupParticipantCleanupReason(ag)
				assert.NotEqual(t, groupCleanupReasonUnknown, cleanupReason)
				if store.IsNarrowMessageMode(mode) || mode == store.MessageModeNone {
					assert.Equal(t, mode, cleanupReason)
				} else {
					assert.Empty(t, cleanupReason)
				}

				// 6. computeCanReachViewer & countReachableUsers
				canReachOwner := computeCanReachViewer(ownerIdent, ag)
				userCount := countReachableUsers(ag)
				if mode == store.MessageModeNone {
					assert.False(t, canReachOwner)
					assert.Equal(t, 0, userCount)
				} else {
					assert.True(t, canReachOwner)
					assert.GreaterOrEqual(t, userCount, 1)
				}
			})
		}
	})

	t.Run("hold redrive classifies tree denial as permanent (not lookup fault)", func(t *testing.T) {
		srv, s, owner, member, projectID := msgAuthzSetup(t)
		ctx := context.Background()
		treeTarget := msgAuthzAgent(t, s, "redrive-tree-target", projectID, store.MessageModeTree, []string{owner.ID})
		memberIdent := msgAuthzUserIdentity(member.ID)

		// Verify member is denied on tree target
		allowed, reason, _ := srv.authorizeAgentMessage(ctx, memberIdent, treeTarget, false)
		require.False(t, allowed)
		assert.Equal(t, ReasonModeOwnerOnly, mapReasonToCode(reason))

		// Verify hold redrive diagnoses this as a permanent denial (no error, false)
		allowed2, err := srv.revalidateIdentityToAgent(ctx, memberIdent, treeTarget)
		require.NoError(t, err)
		assert.False(t, allowed2)
	})
}
