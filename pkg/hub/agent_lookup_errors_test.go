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

// Tests that a failed agent lookup during @mention and target resolution is
// reported as a failure, not as "no such agent", and that the targeted
// slug/ID lookup used by the two resolve endpoints keeps the matching rules
// of the list scan it replaced (project scope, soft-delete exclusion, exact
// case).
//
// Run: go test ./pkg/hub/ -run 'TestMentionLookupError|TestMessagingTargetsLookup|TestConversationResolveLookup' -count=1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

var errLookupOutage = errors.New("simulated agent store outage: secret-detail")

// failingAgentLookupStore wraps a store.Store and fails selected agent
// lookups. All other calls delegate to the embedded store.
type failingAgentLookupStore struct {
	store.Store
	// listProjectID, when set, fails every ListAgents call for that project
	// after the first page (i.e. any call carrying a cursor).
	listProjectID string
	// slugRef, when set, fails GetAgentBySlug for that slug.
	slugRef string
	// idRef, when set, fails GetAgent for that ID.
	idRef string
}

func (f *failingAgentLookupStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	if f.listProjectID != "" && filter.ProjectID == f.listProjectID && opts.Cursor != "" {
		return nil, errLookupOutage
	}
	return f.Store.ListAgents(ctx, filter, opts)
}

func (f *failingAgentLookupStore) GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error) {
	if f.slugRef != "" && slug == f.slugRef {
		return nil, errLookupOutage
	}
	return f.Store.GetAgentBySlug(ctx, projectID, slug)
}

func (f *failingAgentLookupStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if f.idRef != "" && id == f.idRef {
		return nil, errLookupOutage
	}
	return f.Store.GetAgent(ctx, id)
}

// requireInternalError asserts a static 500 that does not leak the cause.
func requireInternalError(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusInternalServerError, rr.Code, "body: %s", rr.Body.String())
	var body apiErrorBody
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Equal(t, ErrCodeInternalError, body.Error.Code)
	require.NotContains(t, rr.Body.String(), "secret-detail")
}

// A list failure part-way through the walk must surface as an "error"
// result for every mention, never as an empty result list or "not_found".
func TestMentionLookupError_ProcessMentions_ReportsErrorPerSlug(t *testing.T) {
	srv, s, project, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	// Enough agents that the walk needs a second page, which then fails.
	createFillerAgents(t, s, project.ID, sender.Ancestry, 210)
	srv.store = &failingAgentLookupStore{Store: s, listProjectID: project.ID}

	owner, err := s.GetUser(ctx, project.OwnerID)
	require.NoError(t, err)

	const missingSlug = "no-such-agent"
	mentions := []string{mentionBystanderSlug, missingSlug, target.Slug}
	sm := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      messages.TypeInstruction,
		Recipient: "agent:" + target.Slug,
		Msg:       "hey @" + mentionBystanderSlug + " and @" + missingSlug,
	}
	reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm, Mentions: mentions})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(owner.ID, owner.Email, "Owner", "member", "cli")))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	// The primary recipient is skipped, as on the success path.
	require.Equal(t, []messages.MentionResult{
		{Slug: mentionBystanderSlug, Status: "error", Error: "mention resolution unavailable"},
		{Slug: missingSlug, Status: "error", Error: "mention resolution unavailable"},
	}, resp.MentionResults)
	require.Empty(t, dispatchesTo(dispatcher, bystander.ID), "no mention may be dispatched when resolution failed")
}

// The same walk failure on the agent-authored fan-out path must surface as
// an "error" result per agent mention while the primary still delivers.
// Raw-email and human display-name mentions address a human and are
// dropped, as on the success path.
func TestMentionLookupError_AgentFanout_ReportsErrorPerSlug(t *testing.T) {
	srv, s, project, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	human := &store.User{
		ID: tid("amf-human"), Email: "jane-doe@test.example", DisplayName: "Jane Doe",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, human))
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      human.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	createFillerAgents(t, s, project.ID, sender.Ancestry, 210)
	srv.store = &failingAgentLookupStore{Store: s, listProjectID: project.ID}

	const missingSlug = "no-such-agent"
	rr := sendViaStructured(t, srv, sender, target,
		"hey @"+mentionBystanderSlug+" @"+missingSlug+" @"+target.Slug+" @"+mentionBystanderSlug+" @someone@example.com @jane-doe")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.Len(t, dispatchesTo(dispatcher, target.ID), 1, "the primary recipient must still be dispatched")

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	// The primary is skipped and duplicates collapse, as on the success path.
	require.Equal(t, []messages.MentionResult{
		{Slug: mentionBystanderSlug, Status: "error", Error: "mention resolution unavailable"},
		{Slug: missingSlug, Status: "error", Error: "mention resolution unavailable"},
	}, resp.MentionResults)
	require.Empty(t, dispatchesTo(dispatcher, bystander.ID), "no mention may be dispatched when resolution failed")
}

func targetsResolveAsAgentA(srv *Server, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/messaging/targets/resolve?"+query, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		tid("cpm-agent-a"), tid("cpm-project-a"), []string{tid("cpm-owner-a")})))
	rr := httptest.NewRecorder()
	srv.handleMessagingTargetsResolve(rr, req)
	return rr
}

func conversationResolveAsAgentA(srv *Server, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/resolve?"+query, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		tid("cpm-agent-a"), tid("cpm-project-a"), []string{tid("cpm-owner-a")})))
	rr := httptest.NewRecorder()
	srv.handleConversationResolve(rr, req)
	return rr
}

func TestMessagingTargetsLookup_SlugLookupError_Returns500(t *testing.T) {
	srv, s, _, _, _, _, _, _ := cpmSetup(t)
	srv.store = &failingAgentLookupStore{Store: s, slugRef: "agent-beta"}

	requireInternalError(t, targetsResolveAsAgentA(srv, "project=project-b&agent=agent-beta"))
}

func TestMessagingTargetsLookup_IDLookupError_Returns500(t *testing.T) {
	srv, s, _, _, _, _, _, _ := cpmSetup(t)
	ref := tid("cpm-unknown-agent")
	srv.store = &failingAgentLookupStore{Store: s, idRef: ref}

	requireInternalError(t, targetsResolveAsAgentA(srv, "project=project-b&agent="+ref))
}

func TestConversationResolveLookup_SlugLookupError_Returns500(t *testing.T) {
	srv, s, _, projectB, _, _, _, _ := cpmSetup(t)
	srv.store = &failingAgentLookupStore{Store: s, slugRef: "agent-beta"}

	requireInternalError(t, conversationResolveAsAgentA(srv, "reference=@agent-beta&project_id="+projectB))
}

func TestConversationResolveLookup_IDLookupError_Returns500(t *testing.T) {
	srv, s, _, projectB, _, _, _, _ := cpmSetup(t)
	ref := tid("cpm-unknown-agent")
	srv.store = &failingAgentLookupStore{Store: s, idRef: ref}

	requireInternalError(t, conversationResolveAsAgentA(srv, "reference=@"+ref+"&project_id="+projectB))
}

// A malformed project ID is caller input, not a lookup failure: it keeps
// its 404 rather than becoming a 500.
func TestConversationResolveLookup_MalformedProjectID_Returns404(t *testing.T) {
	srv, _, _, _, _, _, _, _ := cpmSetup(t)

	rr := conversationResolveAsAgentA(srv, "reference=@agent-beta&project_id=not-a-uuid")
	require.Equal(t, http.StatusNotFound, rr.Code, "body: %s", rr.Body.String())
}

// The targeted lookup must match exactly what the list scan matched: slug
// or ID, exact case, within the project, live agents only.
func TestMessagingTargetsLookup_MatchingRules(t *testing.T) {
	srv, s, _, _, _, _, agentA, agentB := cpmSetup(t)
	require.NotEqual(t, agentB.ID, strings.ToUpper(agentB.ID), "sanity: the uppercase row must differ from the canonical id")

	cases := []struct {
		name     string
		agentRef string
		wantCode int
	}{
		{"slug", "agent-beta", http.StatusOK},
		{"id", agentB.ID, http.StatusOK},
		{"id is parsed as a uuid, so uppercase resolves", strings.ToUpper(agentB.ID), http.StatusOK},
		{"slug is case-sensitive", "Agent-Beta", http.StatusNotFound},
		{"id of an agent in another project", agentA.ID, http.StatusNotFound},
		{"slug of an agent in another project", agentA.Slug, http.StatusNotFound},
		{"unknown non-uuid ref", "nobody", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := targetsResolveAsAgentA(srv, "project=project-b&agent="+tc.agentRef)
			require.Equal(t, tc.wantCode, rr.Code, "body: %s", rr.Body.String())
			if tc.wantCode == http.StatusOK {
				var resp targetResolveResponse
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
				require.Equal(t, agentB.ID, resp.Agent.ID)
			}
		})
	}

	t.Run("soft-deleted agent by id", func(t *testing.T) {
		markAgentSoftDeleted(t, s, agentB.ID)
		rr := targetsResolveAsAgentA(srv, "project=project-b&agent="+agentB.ID)
		require.Equal(t, http.StatusNotFound, rr.Code, "body: %s", rr.Body.String())
	})
}

func TestConversationResolveLookup_MatchingRules(t *testing.T) {
	srv, s, _, projectB, _, _, agentA, agentB := cpmSetup(t)

	cases := []struct {
		name     string
		ref      string
		wantPeer bool
	}{
		{"slug", "agent-beta", true},
		{"id", agentB.ID, true},
		{"id is parsed as a uuid, so uppercase resolves", strings.ToUpper(agentB.ID), true},
		{"slug is case-sensitive", "Agent-Beta", false},
		{"id of an agent in another project", agentA.ID, false},
		{"slug of an agent in another project", agentA.Slug, false},
		{"unknown non-uuid ref", "nobody", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := conversationResolveAsAgentA(srv, "reference=@"+tc.ref+"&project_id="+projectB)
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
			var resp conversationResolveResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			if tc.wantPeer {
				require.NotNil(t, resp.PeerAgent)
				require.Equal(t, agentB.ID, resp.PeerAgent.ID)
			} else {
				require.False(t, resp.Exists)
				require.Nil(t, resp.PeerAgent)
			}
		})
	}

	t.Run("soft-deleted agent by id", func(t *testing.T) {
		markAgentSoftDeleted(t, s, agentB.ID)
		rr := conversationResolveAsAgentA(srv, "reference=@"+agentB.ID+"&project_id="+projectB)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		var resp conversationResolveResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		require.False(t, resp.Exists)
		require.Nil(t, resp.PeerAgent, "a soft-deleted agent must not resolve")
	})
}

func markAgentSoftDeleted(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	ctx := context.Background()
	a, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	a.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, a))
}

// nilAgentLookupStore returns (nil, nil) from GetAgentBySlug for slugRef,
// simulating a store that reports "no row" without ErrNotFound.
type nilAgentLookupStore struct {
	store.Store
	slugRef string
}

func (n *nilAgentLookupStore) GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error) {
	if slug == n.slugRef {
		return nil, nil
	}
	return n.Store.GetAgentBySlug(ctx, projectID, slug)
}

// A (nil, nil) lookup result must be treated as not found, not dereferenced.
func TestMessagingTargetsLookup_NilAgentResult_Returns404(t *testing.T) {
	srv, s, _, _, _, _, _, _ := cpmSetup(t)
	srv.store = &nilAgentLookupStore{Store: s, slugRef: "agent-beta"}

	rr := targetsResolveAsAgentA(srv, "project=project-b&agent=agent-beta")
	require.Equal(t, http.StatusNotFound, rr.Code, "body: %s", rr.Body.String())
}

func TestConversationResolveLookup_NilAgentResult_NotExists(t *testing.T) {
	srv, s, _, projectB, _, _, _, _ := cpmSetup(t)
	srv.store = &nilAgentLookupStore{Store: s, slugRef: "agent-beta"}

	rr := conversationResolveAsAgentA(srv, "reference=@agent-beta&project_id="+projectB)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp conversationResolveResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.False(t, resp.Exists)
}
