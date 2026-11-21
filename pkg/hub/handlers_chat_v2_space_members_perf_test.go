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
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	// Unmemoized, each decision loads groups once for the kernel and once
	// more for the project-access check of an owner or ancestor
	// relationship (the member's own agents).
	require.Equal(t, nAgents+len(memberAgents), legacyGroupLoads,
		"unmemoized: one group load per attach decision, plus one per relationship project-access check")

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

// ?include=humans returns only the humans section, omitting the agents key,
// walking no agent store pages, and auditing zero attach decisions.
func TestSpaceMembers_IncludeHumansSkipsAgentWalk(t *testing.T) {
	srv, s, owner, member, projectID, counting, fault := msgAuthzSetupWithFault(t, newSpaceMembersStore)
	createSpaceMembersAgents(t, s, projectID, owner.ID, "inc-humans", 4)
	audits := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(audits)
	srv.authzService.DecisionAuditSampleRate = 1.0
	fault.Arm()

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members?include=humans", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	_, hasHumans := raw["humans"]
	_, hasAgents := raw["agents"]
	assert.True(t, hasHumans, "humans key must be present")
	assert.False(t, hasAgents, "agents key must be omitted under ?include=humans")
	assert.Zero(t, counting.listAgentsCalls, "ListAgents must not run under ?include=humans")
	assert.Zero(t, countSpaceMembersAttachChecks(audits), "attach decisions must not run under ?include=humans")

	// Unrecognised ?include values still return the full response.
	recFull := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members?include=other", nil)
	fullResp := decodeSpaceMembers(t, recFull.Code, recFull.Body.Bytes())
	assert.Len(t, fullResp.Agents, 4)
}

// Human members are resolved in one GetUsersByIDs batch in ListProjectMembers
// order, with zero per-member GetUser calls, deduplicating repeated role
// bindings and skipping missing user records.
func TestSpaceMembers_HumansBatched(t *testing.T) {
	srv, s, _, member, projectID, counting, fault := msgAuthzSetupWithFault(t, newSpaceMembersStore)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	rdCustom, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "batch-extra-project-role",
		Description: "secondary custom role for deduplication check",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
	})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		u := &store.User{
			ID:          tid(fmt.Sprintf("batch-human-%d", i)),
			Email:       fmt.Sprintf("batch-human-%d@test.com", i),
			DisplayName: fmt.Sprintf("Batch Human %d", i),
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, u))
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      u.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}
	// Duplicate role binding (custom project role) for member + dangling binding for a missing user.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rdCustom.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      member.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	dangling := &store.User{
		ID:          tid("missing-human-user"),
		Email:       "missing-human-user@test.com",
		DisplayName: "Missing Human",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, dangling))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      dangling.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	require.NoError(t, s.DeleteUser(ctx, dangling.ID))

	fault.Arm()
	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	resp := decodeSpaceMembers(t, rec.Code, rec.Body.Bytes())

	var wantOrder []string
	seen := map[string]bool{}
	for _, m := range counting.lastProjectMembers {
		if seen[m.UserID] {
			continue
		}
		seen[m.UserID] = true
		if m.UserID != dangling.ID {
			wantOrder = append(wantOrder, m.UserID)
		}
	}
	require.GreaterOrEqual(t, len(wantOrder), 5)

	assert.Equal(t, 1, counting.getUsersByIDsCalls, "humans must be fetched in one GetUsersByIDs batch")
	assert.Zero(t, counting.getUserCalls, "per-member GetUser must not be called")
	gotOrder := make([]string, 0, len(resp.Humans))
	for _, h := range resp.Humans {
		gotOrder = append(gotOrder, h.ID)
	}
	assert.Equal(t, wantOrder, gotOrder, "ListProjectMembers order and deduplication must hold")

	// A GetUsersByIDs failure surfaces as a 500 rather than silently returning empty humans.
	counting.failGetUsersByIDs = true
	recErr := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	assert.Equal(t, http.StatusInternalServerError, recErr.Code, recErr.Body.String())
}

// Human entries omit kind and canAttach; agent entries omit kind, projectId
// and lastSeen, keep canAttach:false, and truncate message to 280 runes.
func TestSpaceMembers_SlimmedWireShapeAndMessageTruncation(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	longDetail := strings.Repeat("語", 320)
	now := time.Now().UTC().Truncate(time.Second)
	a := &store.Agent{
		ID:                tid("slim-agent"),
		ProjectID:         projectID,
		Name:              "Slim Agent",
		Slug:              "slim-agent",
		Phase:             "running",
		Activity:          "working",
		Message:           longDetail,
		LastSeen:          now,
		LastActivityEvent: now,
		OwnerID:           owner.ID,
		CreatedBy:         owner.ID,
		Ancestry:          []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, a))

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw struct {
		Humans []map[string]any `json:"humans"`
		Agents []map[string]any `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.NotEmpty(t, raw.Humans)
	require.Len(t, raw.Agents, 1)

	for _, h := range raw.Humans {
		assert.NotContains(t, h, "kind")
		assert.NotContains(t, h, "canAttach")
		assert.NotContains(t, h, "projectId")
		assert.NotContains(t, h, "lastSeen")
	}

	gotAgent := raw.Agents[0]
	assert.NotContains(t, gotAgent, "kind")
	assert.NotContains(t, gotAgent, "projectId")
	assert.NotContains(t, gotAgent, "lastSeen")
	assert.NotContains(t, gotAgent, "deletion", "idle deletion state must omit deletion key")
	canAttachVal, hasCanAttach := gotAgent["canAttach"]
	assert.True(t, hasCanAttach, "canAttach:false must still be serialised")
	assert.Equal(t, false, canAttachVal)

	msgStr, _ := gotAgent["message"].(string)
	assert.True(t, utf8.ValidString(msgStr), "truncated message must be valid UTF-8")
	assert.Equal(t, truncatePreview(longDetail, 280), msgStr)
}

// Active and failed async deletes surface on agent entries via the redacted
// deletion view, matching GET /api/v1/agents.
func TestSpaceMembers_DeletionVisibilityAndRedaction(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	ids := createSpaceMembersAgents(t, s, projectID, owner.ID, "del-view", 2)
	deletingID, failedID := ids[0], ids[1]

	started := time.Now().Add(-10 * time.Second).UTC()
	lease := time.Now().Add(time.Minute).UTC()
	stDeleting := store.DeletionStateDeleting
	claimOne := int64(1)
	n, err := s.UpdateAgentDeletion(ctx, deletingID,
		store.DeletionPredicate{States: []string{""}, DeletedAtNull: true},
		store.DeletionFields{State: &stDeleting, StartedAt: &started, LeaseAt: &lease, Claim: &claimOne})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	failedAt := time.Now().Add(-5 * time.Second).UTC()
	stFailed := store.DeletionStateFailed
	codeErr := store.DeletionCodeRuntimeError
	errMsg := "broker internal secret detail"
	claimTwo := int64(2)
	n, err = s.UpdateAgentDeletion(ctx, failedID,
		store.DeletionPredicate{States: []string{""}, DeletedAtNull: true},
		store.DeletionFields{
			State: &stFailed, Code: &codeErr, Error: &errMsg,
			StartedAt: &started, FailedAt: &failedAt, LeaseAt: &failedAt, Claim: &claimTwo,
		})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Non-admin project member gets generic deletion views (code/error/claim redacted).
	recMember := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	require.Equal(t, http.StatusOK, recMember.Code, recMember.Body.String())
	assert.NotContains(t, recMember.Body.String(), errMsg)
	memberResp := decodeSpaceMembers(t, recMember.Code, recMember.Body.Bytes())
	byID := map[string]store.DeletionInfo{}
	for _, a := range memberResp.Agents {
		if a.Deletion != nil {
			byID[a.ID] = *a.Deletion
		}
	}
	require.Contains(t, byID, deletingID)
	assert.Equal(t, store.DeletionStateDeleting, byID[deletingID].State)
	assert.Zero(t, byID[deletingID].Claim)

	require.Contains(t, byID, failedID)
	assert.Equal(t, store.DeletionStateFailed, byID[failedID].State)
	assert.Empty(t, byID[failedID].Code)
	assert.Empty(t, byID[failedID].Error)
	assert.Zero(t, byID[failedID].Claim)

	// Unscoped local platform admin sees full failure details.
	recAdmin := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	adminResp := decodeSpaceMembers(t, recAdmin.Code, recAdmin.Body.Bytes())
	adminByID := map[string]store.DeletionInfo{}
	for _, a := range adminResp.Agents {
		if a.Deletion != nil {
			adminByID[a.ID] = *a.Deletion
		}
	}
	require.Contains(t, adminByID, failedID)
	assert.Equal(t, store.DeletionCodeRuntimeError, adminByID[failedID].Code)
	assert.Equal(t, errMsg, adminByID[failedID].Error)
	assert.Equal(t, int64(2), adminByID[failedID].Claim)
}
