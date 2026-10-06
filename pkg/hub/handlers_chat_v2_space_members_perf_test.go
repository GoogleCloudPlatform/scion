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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The members list must return the same agents, in the same order, with the
// same canAttach values as per-agent decisions evaluated without the input
// memo, while making one audited attach decision per agent and loading the
// caller's groups once for the whole attach phase instead of once per agent.
func TestSpaceMembers_MemoizedAttachMatchesUnmemoized(t *testing.T) {
	srv, s, owner, member, projectID, wrapped, fault := msgAuthzSetupWithFault(t, newSpaceMembersStore)
	ctx := context.Background()

	ownerAgents := createSpaceMembersAgents(t, s, projectID, owner.ID, "memo-owner", 3)
	time.Sleep(5 * time.Millisecond)
	memberAgents := createSpaceMembersAgents(t, s, projectID, member.ID, "memo-member", 4)
	nAgents := len(ownerAgents) + len(memberAgents)

	// Reference: the walk order and, per agent, the attach decision made
	// with the memo masked, i.e. as the handler decided before.
	agents, _, err := walkProjectAgentPages(ctx, s, projectID, spaceMembersMaxAgents)
	require.NoError(t, err)
	require.Len(t, agents, nAgents)
	identity := identityOf(member)
	type want struct {
		id        string
		canAttach bool
	}
	var wants []want
	sawAllow, sawDeny := false, false
	legacyGroupLoads := 0
	refStore := &spaceMembersStore{Store: s, onEffectiveGroups: func() { legacyGroupLoads++ }}
	srv.authzService.store = refStore
	for i := range agents {
		allowed := srv.authzService.CheckAccess(maskAllAuthzMemo(ctx), identity, agentResource(&agents[i]), ActionAttach).Allowed
		sawAllow = sawAllow || allowed
		sawDeny = sawDeny || !allowed
		wants = append(wants, want{agents[i].ID, allowed})
	}
	require.True(t, sawAllow && sawDeny, "fixture: need both attachable and non-attachable agents")
	require.Equal(t, nAgents, legacyGroupLoads, "unmemoized: one group load per attach decision")

	// Count group loads made after the agent walk, i.e. by the attach phase.
	walkDone := false
	attachGroupLoads := 0
	wrapped.onAgentPage = func(_ int, last bool) { walkDone = walkDone || last }
	wrapped.onEffectiveGroups = func() {
		if walkDone {
			attachGroupLoads++
		}
	}
	fault.Arm()
	srv.authzService.store = wrapped
	emitter := &parityRecordingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	srv.authzService.DecisionAuditSampleRate = 1.0

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	resp := decodeSpaceMembers(t, rec.Code, rec.Body.Bytes())

	require.Len(t, resp.Agents, nAgents)
	for i, a := range resp.Agents {
		assert.Equal(t, wants[i].id, a.ID, "agent %d order", i)
		assert.Equal(t, wants[i].canAttach, a.CanAttach, "agent %d (%s) canAttach", i, a.Slug)
	}

	var attachRecords []*store.DecisionAuditRecord
	for _, r := range emitter.snapshot() {
		if r.Permission == string(ActionAttach) {
			attachRecords = append(attachRecords, r)
		}
	}
	require.Len(t, attachRecords, nAgents, "one audited attach decision per agent")
	for i, r := range attachRecords {
		assert.Equal(t, wants[i].id, r.ResourceID, "attach decision %d order", i)
	}

	t.Logf("attach-phase GetEffectiveGroups: unmemoized=%d memoized=%d", legacyGroupLoads, attachGroupLoads)
	assert.Equal(t, 1, attachGroupLoads, "groups load once for the attach phase, not once per agent")
}
