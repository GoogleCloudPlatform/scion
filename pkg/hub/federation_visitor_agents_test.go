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
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project visitors listing and reading agents, and reading their own
// grants and DMs (ptone/scion#4603).

// visitorAgentViewFields is the complete set of JSON fields a visitor sees
// of an agent.
var visitorAgentViewFields = []string{"name", "slug", "status"}

func (f *visitorMessagingFixture) projectAgentsPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/agents"
}

// populatedAgent creates an agent in the fixture's project with every
// field a visitor must not see set.
func (f *visitorMessagingFixture) populatedAgent(t *testing.T) *store.Agent {
	t.Helper()
	exit := 3
	a := &store.Agent{
		ID: tid(t.Name() + "-populated"), Name: "Populated Agent", Slug: "populated", ProjectID: f.projectID,
		Template:    "private-template",
		Labels:      map[string]string{"team": "private-label"},
		Annotations: map[string]string{"note": "private-annotation"},
		Phase:       "running", Activity: "thinking", ToolName: "private-tool",
		ConnectionState: "connected", ContainerStatus: "private-container-status", RuntimeState: "private-runtime-state",
		ExitCode: &exit, ExitReason: "private-exit-reason",
		CurrentTurns: 7, CurrentModelCalls: 9, StartedAt: time.Now().Add(-time.Hour).UTC(),
		Image: "private-image", Detached: true, Runtime: "docker", RuntimeBrokerID: f.agent.RuntimeBrokerID,
		WebPTYEnabled: true, TaskSummary: "private-task-summary", Message: "private-message",
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.st.CreateAgent(context.Background(), a))
	return a
}

// objectFields decodes a JSON object and returns its field names.
func objectFields(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &obj))
	out := make([]string, 0, len(obj))
	for k := range obj {
		out = append(out, k)
	}
	return out
}

// The visitor view is built field by field: the struct has exactly the
// three allowed fields, so a field added to store.Agent cannot reach a
// visitor.
func TestVisitorAgentView_FieldAllowList(t *testing.T) {
	rt := reflect.TypeOf(VisitorAgentView{})
	var got []string
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		got = append(got, name)
	}
	assert.ElementsMatch(t, visitorAgentViewFields, got)

	a := &store.Agent{ID: tid("view-agent"), Name: "N", Slug: "n", Template: "t", Phase: "running", Activity: "thinking"}
	assert.Equal(t, VisitorAgentView{Name: "N", Slug: "n", Status: "thinking"}, visitorAgentView(a))
	a.Activity = ""
	a.Phase = "stopped"
	assert.Equal(t, "stopped", visitorAgentView(a).Status)
}

// A visitor lists the granted project's agents and reads one by slug or
// ID; every response carries name, slug and status only, against a fully
// populated agent. The agent in message mode none is listed and readable
// (its mode limits messaging only); the agent of another project is not
// listed.
func TestVisitorAgents_ListAndReadProjected(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	populated := f.populatedAgent(t)
	_, token := f.visitor(t, "sub-alice", "alice@corp.example")

	rec := f.do(t, token, http.MethodGet, f.projectAgentsPath(f.projectID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &top))
	assert.ElementsMatch(t, []string{"agents"}, objectFields(t, rec.Body.Bytes()))
	var items []json.RawMessage
	require.NoError(t, json.Unmarshal(top["agents"], &items))
	for _, item := range items {
		assert.ElementsMatch(t, visitorAgentViewFields, objectFields(t, item), string(item))
	}
	var list ListVisitorAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.ElementsMatch(t, []VisitorAgentView{
		{Name: "helper", Slug: "helper", Status: "running"},
		{Name: "sealed", Slug: "sealed", Status: "running"},
		{Name: "Populated Agent", Slug: "populated", Status: "thinking"},
	}, list.Agents, "only the granted project's agents, the sealed one included")
	for _, private := range []string{populated.ID, f.foreign.Slug, "private-", f.projectID, "messageMode", "template"} {
		assert.NotContains(t, rec.Body.String(), private)
	}

	for _, ref := range []string{populated.Slug, populated.ID} {
		rec := f.do(t, token, http.MethodGet, f.projectAgentsPath(f.projectID)+"/"+ref, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", ref, rec.Body.String())
		assert.ElementsMatch(t, visitorAgentViewFields, objectFields(t, rec.Body.Bytes()), ref)
		assert.JSONEq(t, `{"name":"Populated Agent","slug":"populated","status":"thinking"}`, rec.Body.String(), ref)
	}
	rec = f.do(t, token, http.MethodGet, f.projectAgentsPath(f.projectID)+"/"+f.sealed.Slug, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"name":"sealed","slug":"sealed","status":"running"}`, rec.Body.String())

	// Query parameters change nothing for a visitor.
	rec = f.do(t, token, http.MethodGet, f.projectAgentsPath(f.projectID)+"?includeDeleted=true&view=full&limit=1", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var again ListVisitorAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &again))
	assert.ElementsMatch(t, list.Agents, again.Agents)
}

// A member's list and read responses are unchanged: the full agent record,
// not the visitor view. An owner's send response carries no conversation
// fields.
func TestVisitorAgents_MemberResponsesUnchanged(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	populated := f.populatedAgent(t)

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, f.projectAgentsPath(f.projectID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"id":"`+populated.ID+`"`, "the member list carries the agent record")
	assert.Contains(t, rec.Body.String(), `"projectId":"`+f.projectID+`"`)

	rec = doRequestAsUser(t, f.srv, f.member, http.MethodGet, f.projectAgentsPath(f.projectID)+"/"+populated.Slug, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"id":"`+populated.ID+`"`)
	assert.Contains(t, rec.Body.String(), "private-template")

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("owner"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var sent map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sent))
	assert.NotContains(t, sent, "conversation_id")
	assert.NotContains(t, sent, "conversation_key")

	// The visitor-only routes answer 404 for a member.
	for _, path := range []string{"/api/v1/users/me/visitor-grants", "/api/v1/users/me/visitor-dms"} {
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", path, rec.Body.String())
	}
}

// Unknown project, a project without a grant, an agent of another project,
// an unknown agent, no current grant (not started, expired, revoked),
// other routes and methods, and the issuer no longer accepting visitors
// all give 5A's refusal, byte for byte.
func TestVisitorAgents_UniformRefusal(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	ctx := context.Background()
	_, alice := f.visitor(t, "sub-alice", "alice@corp.example")

	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "email": "carol@corp.example", "notBefore": future})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	carol := f.token(t, grantTestIssuer, "sub-carol", "carol@corp.example", boolPtr(true))

	daveGrant := f.grantOK(t, grantTestIssuer, "dave@corp.example")
	dave := f.token(t, grantTestIssuer, "sub-dave", "dave@corp.example", boolPtr(true))
	past := time.Now().Add(-time.Minute)
	_, err := f.st.UpdateVisitorRoleBindingWindow(ctx, daveGrant.BindingID, nil, &past)
	require.NoError(t, err)

	erinGrant := f.grantOK(t, grantTestIssuer, "erin@corp.example")
	erin := f.token(t, grantTestIssuer, "sub-erin", "erin@corp.example", boolPtr(true))
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+erinGrant.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// 5A's refusal, the reference response.
	reference := f.do(t, alice, http.MethodPost, f.projectMessagePath("no-such-agent"), visitorTextBody("x"))
	assertVisitorRefusal(t, reference, "5A refusal")

	list := f.projectAgentsPath(f.projectID)
	other := f.foreign.ProjectID
	type refusal struct{ label, token, method, path string }
	cases := []refusal{
		{"list: unknown project", alice, http.MethodGet, f.projectAgentsPath(tid("no-such-project"))},
		{"list: project without a grant", alice, http.MethodGet, f.projectAgentsPath(other)},
		{"read: unknown project", alice, http.MethodGet, f.projectAgentsPath(tid("no-such-project")) + "/" + f.agent.Slug},
		{"read: unknown agent", alice, http.MethodGet, list + "/no-such-agent"},
		{"read: unknown agent ID", alice, http.MethodGet, list + "/" + tid("no-such-agent")},
		{"read: agent of another project by ID", alice, http.MethodGet, list + "/" + f.foreign.ID},
		{"read: agent in a project without a grant", alice, http.MethodGet, f.projectAgentsPath(other) + "/" + f.foreign.Slug},
		{"list: grant not started", carol, http.MethodGet, list},
		{"read: grant not started", carol, http.MethodGet, list + "/" + f.agent.Slug},
		{"list: grant expired", dave, http.MethodGet, list},
		{"read: grant expired", dave, http.MethodGet, list + "/" + f.agent.Slug},
		{"list: grant revoked", erin, http.MethodGet, list},
		{"read: grant revoked", erin, http.MethodGet, list + "/" + f.agent.ID},
		{"HEAD list", alice, http.MethodHead, list},
		{"HEAD read", alice, http.MethodHead, list + "/" + f.agent.Slug},
		{"create agent", alice, http.MethodPost, list},
		{"update agent", alice, http.MethodPatch, list + "/" + f.agent.Slug},
		{"delete agent", alice, http.MethodDelete, list + "/" + f.agent.Slug},
		{"agent logs", alice, http.MethodGet, list + "/" + f.agent.Slug + "/logs"},
		{"agent status", alice, http.MethodGet, list + "/" + f.agent.Slug + "/status"},
		{"agent read by ID form", alice, http.MethodGet, "/api/v1/agents/" + f.agent.ID},
		{"global agent list", alice, http.MethodGet, "/api/v1/agents"},
		{"project read", alice, http.MethodGet, "/api/v1/projects/" + f.projectID},
		{"project members", alice, http.MethodGet, "/api/v1/projects/" + f.projectID + "/members"},
		{"project visitors", alice, http.MethodGet, f.visitorsPath()},
		{"own grants: POST", alice, http.MethodPost, "/api/v1/users/me/visitor-grants"},
		{"own DMs: DELETE", alice, http.MethodDelete, "/api/v1/users/me/visitor-dms"},
	}
	for _, c := range cases {
		rec := f.do(t, c.token, c.method, c.path, nil)
		if c.method == http.MethodHead {
			// A HEAD response has no body; the status and headers match.
			assert.Equal(t, reference.Code, rec.Code, c.label)
			assert.Equal(t, reference.Header(), rec.Header(), c.label)
			continue
		}
		assertVisitorRefusal(t, rec, c.label)
		assertSameResponse(t, reference, rec, c.label)
	}

	f.setIssuerAcceptsVisitors(t, grantTestIssuer, false)
	for _, path := range []string{list, list + "/" + f.agent.Slug, "/api/v1/users/me/visitor-grants", "/api/v1/users/me/visitor-dms"} {
		assertSameResponse(t, reference, f.do(t, alice, http.MethodGet, path, nil), "issuer off: "+path)
	}
	f.setIssuerAcceptsVisitors(t, grantTestIssuer, true)
	rec = f.do(t, alice, http.MethodGet, list, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "accepting the issuer again restores access: %s", rec.Body.String())
}

// With the experiment off the route guard refuses a visitor identity on
// the agent list and read sub-paths, with the uniform refusal, and the
// own-grants and own-DM routes refuse it too.
func TestVisitorAgents_ExperimentOff(t *testing.T) {
	srv, _ := testServer(t)
	visitorSetExperiment(t, srv, false)
	visitor := NewVisitorIdentity(tid("off-visitor"), visitorTestUserIssuer, "sub", "v@corp.com", "V")
	pid := tid("off-project")
	for _, path := range []string{"/api/v1/projects/" + pid + "/agents", "/api/v1/projects/" + pid + "/agents/helper"} {
		rec, called := serveGuardedPathAs(srv, routeMetadataTable["/api/v1/projects/"], http.MethodGet, path, visitor)
		assert.False(t, called, path)
		assertVisitorRefusal(t, rec, "experiment off: "+path)
	}
	for _, h := range []http.HandlerFunc{srv.handleUserMeVisitorGrants, srv.handleUserMeVisitorDMs} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), visitor))
		rec := httptest.NewRecorder()
		h(rec, req)
		assertVisitorRefusal(t, rec, "experiment off: own route")
	}
}

// Revoking a grant refuses the visitor's next list and read, and the own
// grants route no longer lists it: nothing caches the grant.
func TestVisitorAgents_RevocationTakesEffectOnNextRequest(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	visitorID, token := f.visitor(t, "sub-gus", "gus@corp.example")
	list := f.projectAgentsPath(f.projectID)

	require.Equal(t, http.StatusOK, f.do(t, token, http.MethodGet, list, nil).Code)
	require.Equal(t, http.StatusOK, f.do(t, token, http.MethodGet, list+"/"+f.agent.Slug, nil).Code)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+visitorID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assertVisitorRefusal(t, f.do(t, token, http.MethodGet, list, nil), "list after revocation")
	assertVisitorRefusal(t, f.do(t, token, http.MethodGet, list+"/"+f.agent.Slug, nil), "read after revocation")
	rec = f.do(t, token, http.MethodGet, "/api/v1/users/me/visitor-grants", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var grants ListVisitorOwnGrantsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grants))
	assert.Empty(t, grants.Grants)
}

// The own-grants route lists the caller's grants only (project, window and
// status), never another visitor's.
func TestVisitorAgents_OwnGrantsOnly(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	ctx := context.Background()
	aliceID, alice := f.visitor(t, "sub-alice", "alice@corp.example")
	_, bob := f.visitor(t, "sub-bob", "bob@corp.example")

	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "email": "carol@corp.example", "notBefore": future})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	carol := f.token(t, grantTestIssuer, "sub-carol", "carol@corp.example", boolPtr(true))

	daveGrant := f.grantOK(t, grantTestIssuer, "dave@corp.example")
	dave := f.token(t, grantTestIssuer, "sub-dave", "dave@corp.example", boolPtr(true))
	past := time.Now().Add(-time.Minute)
	_, err := f.st.UpdateVisitorRoleBindingWindow(ctx, daveGrant.BindingID, nil, &past)
	require.NoError(t, err)

	// Alice also holds a grant on the other project, with an expiry.
	expires := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	_, err = f.st.CreateVisitorRoleBinding(ctx, &store.RoleBinding{
		PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: aliceID,
		ScopeType: store.RoleScopeProject, ScopeID: f.foreign.ProjectID, ExpiresAt: &expires,
	})
	require.NoError(t, err)
	other, err := f.st.GetProject(ctx, f.foreign.ProjectID)
	require.NoError(t, err)
	project, err := f.st.GetProject(ctx, f.projectID)
	require.NoError(t, err)

	grantsOf := func(token string) []VisitorOwnGrant {
		t.Helper()
		rec := f.do(t, token, http.MethodGet, "/api/v1/users/me/visitor-grants", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListVisitorOwnGrantsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.Grants
	}

	got := grantsOf(alice)
	require.Len(t, got, 2, "alice's two grants, not bob's, carol's or dave's")
	byProject := map[string]VisitorOwnGrant{}
	for _, g := range got {
		byProject[g.ProjectID] = g
	}
	assert.Equal(t, VisitorOwnGrant{ProjectID: project.ID, ProjectSlug: project.Slug, ProjectName: project.Name,
		Status: visitorGrantStatusActive}, byProject[project.ID])
	og := byProject[other.ID]
	assert.Equal(t, other.Slug, og.ProjectSlug)
	assert.Equal(t, visitorGrantStatusActive, og.Status)
	require.NotNil(t, og.ExpiresAt)
	assert.True(t, og.ExpiresAt.Equal(expires))

	require.Len(t, grantsOf(bob), 1)
	assert.Equal(t, project.ID, grantsOf(bob)[0].ProjectID)
	cg := grantsOf(carol)
	require.Len(t, cg, 1)
	assert.Equal(t, visitorGrantStatusScheduled, cg[0].Status)
	require.NotNil(t, cg[0].NotBefore)
	dg := grantsOf(dave)
	require.Len(t, dg, 1)
	assert.Equal(t, visitorGrantStatusExpired, dg[0].Status)

	// The second grant admits alice on the other project's agents too.
	rec = f.do(t, alice, http.MethodGet, f.projectAgentsPath(other.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"slug":"foreign"`)
}

// A visitor finds its DM with the agent from the send response alone, and
// from its own DM list, and reads the history with the key it got. The DM
// list never shows another visitor's or a local user's DM.
func TestVisitorAgents_FindAndReadOwnDM(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	aliceID, alice := f.visitor(t, "sub-alice", "alice@corp.example")
	bobID, bob := f.visitor(t, "sub-bob", "bob@corp.example")
	aliceDM, err := messages.DMConversationKey("agent", f.agent.ID, "user", aliceID)
	require.NoError(t, err)

	// From the send response.
	rec := f.do(t, alice, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("alice to agent"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var sent MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sent))
	assert.Equal(t, aliceDM, sent.ConversationKey)
	require.NotEmpty(t, sent.ConversationID)
	conv, err := f.st.GetConversationByExternalRef(context.Background(), "native", aliceDM)
	require.NoError(t, err)
	assert.Equal(t, conv.ID, sent.ConversationID)

	rec = f.do(t, alice, http.MethodGet, chatDMPath(sent.ConversationKey, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var history chatHistoryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &history))
	require.Len(t, history.Messages, 1)
	assert.Equal(t, "alice to agent", history.Messages[0].Msg)

	// Bob and the owner write to the same agent; their DMs are not alice's.
	rec = f.do(t, bob, http.MethodPost, agentMessagePath(f.agent.ID), visitorTextBody("bob to agent"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("owner to agent"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	dmsOf := func(token string) []VisitorOwnDM {
		t.Helper()
		rec := f.do(t, token, http.MethodGet, "/api/v1/users/me/visitor-dms", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListVisitorOwnDMsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.DMs
	}

	// From the DM list.
	dms := dmsOf(alice)
	require.Len(t, dms, 1, "alice's DM only")
	project, err := f.st.GetProject(context.Background(), f.projectID)
	require.NoError(t, err)
	assert.Equal(t, conv.ID, dms[0].ConversationID)
	assert.Equal(t, aliceDM, dms[0].ConversationKey)
	assert.Equal(t, project.Slug, dms[0].ProjectSlug)
	assert.Equal(t, f.agent.Slug, dms[0].AgentSlug)
	rec = f.do(t, alice, http.MethodGet, chatDMPath(dms[0].ConversationKey, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listed chatHistoryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	require.Len(t, listed.Messages, 1)
	assert.Equal(t, "alice to agent", listed.Messages[0].Msg)

	bobDMs := dmsOf(bob)
	require.Len(t, bobDMs, 1)
	assert.Equal(t, "dm:agent:"+f.agent.ID+":user:"+bobID, bobDMs[0].ConversationKey)
	for _, d := range append(dms, bobDMs...) {
		assert.NotContains(t, d.ConversationKey, f.owner.ID, "a local user's DM is never listed")
	}

	// A new visitor with no DM gets an empty list.
	_, carl := f.visitor(t, "sub-carl", "carl@corp.example")
	assert.Empty(t, dmsOf(carl))

	// After revocation the DM is no longer listed (it is no longer readable).
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+aliceID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, dmsOf(alice))
}

// The agent list and read sub-paths are admitted on GET with these exact
// shapes only.
func TestVisitorSubRoutes_AgentListAndReadShapes(t *testing.T) {
	pid, aid := tid("shape-project"), tid("shape-agent")
	base := "/api/v1/projects/" + pid + "/agents"
	for _, tc := range []struct {
		method, path string
		want         bool
		access       visitorAccess
	}{
		{http.MethodGet, base, true, visitorAccessAgentList},
		{http.MethodGet, base + "/", true, visitorAccessAgentList},
		{http.MethodGet, "/api/v1/projects/" + pid + "__my-project/agents", true, visitorAccessAgentList},
		{http.MethodGet, base + "/" + aid, true, visitorAccessAgentRead},
		{http.MethodGet, base + "/my-agent", true, visitorAccessAgentRead},
		{http.MethodGet, base + "/my-agent/", true, visitorAccessAgentRead},

		{http.MethodHead, base, false, visitorAccessNone},
		{http.MethodPost, base, false, visitorAccessNone},
		{http.MethodPatch, base + "/" + aid, false, visitorAccessNone},
		{http.MethodDelete, base + "/" + aid, false, visitorAccessNone},
		{http.MethodGet, base + "/stop-all", false, visitorAccessNone},
		{http.MethodGet, base + "/" + aid + "/logs", false, visitorAccessNone},
		{http.MethodGet, base + "/" + aid + "/status", false, visitorAccessNone},
		{http.MethodGet, base + "/" + aid + "/message", false, visitorAccessNone},
		{http.MethodGet, base + "/" + aid + "/extra/x", false, visitorAccessNone},
		{http.MethodGet, "/api/v1/projects/" + pid, false, visitorAccessNone},
		{http.MethodGet, "/api/v1/projects/" + pid + "/members", false, visitorAccessNone},
		{http.MethodGet, "/api/v1/projects/" + pid + "/visitors", false, visitorAccessNone},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		ref, ok := matchVisitorSubRoute("/api/v1/projects/", r)
		assert.Equal(t, tc.want, ok, "%s %s", tc.method, tc.path)
		assert.Equal(t, tc.access, ref.Access, "%s %s", tc.method, tc.path)
	}
	// Under the by-ID pattern GET is not open to visitors at all.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+aid, nil)
	_, ok := matchVisitorSubRoute("/api/v1/agents/", r)
	assert.False(t, ok)

	// Every admitted sub-route names a check.
	for _, sub := range visitorAdmittedSubRoutes {
		assert.NotEqual(t, visitorAccessNone, sub.Access, "%s %s%s", sub.Method, sub.Pattern, sub.SubPath)
	}
}

// Only a visitor sender gets the conversation fields in the send response.
func TestVisitorDeliveryConversation_VisitorOnly(t *testing.T) {
	agent := &store.Agent{ID: tid("delivery-agent")}
	visitor := NewVisitorIdentity(tid("delivery-visitor"), visitorTestUserIssuer, "sub", "v@corp.com", "V")
	id, key := visitorDeliveryConversation(visitor, agent, "conv-1")
	assert.Equal(t, "conv-1", id)
	assert.Equal(t, "dm:agent:"+agent.ID+":user:"+visitor.ID(), key)

	for name, identity := range map[string]Identity{
		"local user":        NewDevUser(DevUserConfig{}),
		"federated user":    NewFederatedUserIdentity(visitorTestUserIssuer, "sub", "u@corp.com", "U", "viewer", nil),
		"no identity":       nil,
		"typed nil visitor": (*VisitorIdentity)(nil),
	} {
		id, key := visitorDeliveryConversation(identity, agent, "conv-1")
		assert.Empty(t, id, name)
		assert.Empty(t, key, name)
	}
}

// A deleted agent is gone for a visitor: absent from the list, and its
// read by ID and by slug both give 5A's refusal, byte for byte. Admission
// and the handler each refuse it.
func TestVisitorAgents_DeletedAgentRefused(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	ctx := context.Background()
	aliceID, alice := f.visitor(t, "sub-alice", "alice@corp.example")
	list := f.projectAgentsPath(f.projectID)

	gone := &store.Agent{
		ID: tid(t.Name() + "-gone"), Name: "gone", Slug: "gone", ProjectID: f.projectID,
		RuntimeBrokerID: f.agent.RuntimeBrokerID, Phase: "running", MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.st.CreateAgent(ctx, gone))
	rec := f.do(t, alice, http.MethodGet, list+"/"+gone.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "readable before deletion: %s", rec.Body.String())

	row, err := f.st.GetAgent(ctx, gone.ID)
	require.NoError(t, err)
	row.DeletedAt = time.Now()
	require.NoError(t, f.st.UpdateAgent(ctx, row))

	reference := f.do(t, alice, http.MethodPost, f.projectMessagePath("no-such-agent"), visitorTextBody("x"))
	assertVisitorRefusal(t, reference, "5A refusal")

	rec = f.do(t, alice, http.MethodGet, list, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listed ListVisitorAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	assert.NotEmpty(t, listed.Agents)
	for _, v := range listed.Agents {
		assert.NotEqual(t, gone.Slug, v.Slug, "a deleted agent is not listed")
	}

	for _, ref := range []string{gone.ID, gone.Slug} {
		rec := f.do(t, alice, http.MethodGet, list+"/"+ref, nil)
		assertVisitorRefusal(t, rec, "read deleted agent: "+ref)
		assertSameResponse(t, reference, rec, "read deleted agent: "+ref)
	}

	// Each check alone refuses the deleted agent.
	deleted, err := f.st.GetAgent(ctx, gone.ID)
	require.NoError(t, err)
	require.False(t, deleted.DeletedAt.IsZero())
	// The identity holds alice's grant, so only the deletion refuses.
	visitor := NewVisitorIdentity(aliceID, grantTestIssuer, "sub-alice", "alice@corp.example", "Alice")
	allowed, err := f.srv.visitorMayReadAgent(ctx, visitor, f.agent)
	require.NoError(t, err)
	require.True(t, allowed, "control: the granted visitor may read a live agent")
	allowed, err = f.srv.visitorMayReadAgent(ctx, visitor, deleted)
	require.NoError(t, err)
	assert.False(t, allowed, "admission refuses a deleted agent")

	direct := httptest.NewRecorder()
	f.srv.getProjectAgentForVisitor(direct, httptest.NewRequest(http.MethodGet, list+"/"+gone.ID, nil), f.projectID, gone.ID)
	assertVisitorRefusal(t, direct, "handler: deleted agent by ID")
}

// A deleted agent is refused to a visitor like an unknown agent on every
// messaging path: the send (project form by ID and by slug, and the by-ID
// form), the DM history read and the read marker (GET and POST); the
// visitor's own DM list leaves the DM out. A member's send to the same
// agent is unchanged.
func TestVisitorMessaging_DeletedAgentRefused(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	ctx := context.Background()
	_, alice := f.visitor(t, "sub-alice", "alice@corp.example")

	gone := &store.Agent{
		ID: tid(t.Name() + "-gone"), Name: "gone", Slug: "gone", ProjectID: f.projectID,
		RuntimeBrokerID: f.agent.RuntimeBrokerID, Phase: "running", MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.st.CreateAgent(ctx, gone))

	// Before deletion: alice has a DM with gone and one with helper, and
	// both are listed.
	rec := f.do(t, alice, http.MethodPost, f.projectMessagePath(gone.Slug), visitorTextBody("alice to gone"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var sent MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sent))
	goneDM := sent.ConversationKey
	require.NotEmpty(t, goneDM)
	rec = f.do(t, alice, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("alice to helper"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	dmsOfAlice := func() []VisitorOwnDM {
		t.Helper()
		rec := f.do(t, alice, http.MethodGet, "/api/v1/users/me/visitor-dms", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListVisitorOwnDMsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.DMs
	}
	require.Len(t, dmsOfAlice(), 2, "control: both DMs listed before deletion")
	rec = f.do(t, alice, http.MethodGet, chatDMPath(goneDM, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, "control: DM readable before deletion: %s", rec.Body.String())

	row, err := f.st.GetAgent(ctx, gone.ID)
	require.NoError(t, err)
	row.DeletedAt = time.Now()
	require.NoError(t, f.st.UpdateAgent(ctx, row))

	reference := f.do(t, alice, http.MethodPost, f.projectMessagePath("no-such-agent"), visitorTextBody("x"))
	assertVisitorRefusal(t, reference, "5A refusal")

	for _, c := range []struct {
		label, method, path string
		body                interface{}
	}{
		{"send, project form by ID", http.MethodPost, f.projectMessagePath(gone.ID), visitorTextBody("x")},
		{"send, project form by slug", http.MethodPost, f.projectMessagePath(gone.Slug), visitorTextBody("x")},
		{"send, by-ID form", http.MethodPost, agentMessagePath(gone.ID), visitorTextBody("x")},
		{"DM history", http.MethodGet, chatDMPath(goneDM, "messages"), nil},
		{"read marker, GET", http.MethodGet, chatDMPath(goneDM, "read"), nil},
		{"read marker, POST", http.MethodPost, chatDMPath(goneDM, "read"), map[string]string{"messageId": sent.MessageID}},
	} {
		rec := f.do(t, alice, c.method, c.path, c.body)
		assertVisitorRefusal(t, rec, c.label)
		assertSameResponse(t, reference, rec, c.label)
	}

	dms := dmsOfAlice()
	require.Len(t, dms, 1, "the DM with the deleted agent is not listed")
	assert.Equal(t, f.agent.Slug, dms[0].AgentSlug)
	assert.NotEqual(t, goneDM, dms[0].ConversationKey)

	// A member's send to the deleted agent is unchanged.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPost, f.projectMessagePath(gone.ID), visitorTextBody("owner to gone"))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// With ?messageable=true a visitor's list holds only the agents the
// visitor may message now: not the agent in message mode none, nor the
// lineage agent a visitor cannot reach. Without the flag, or with any
// other value, the list is unchanged.
func TestVisitorAgents_MessageableFilter(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	ctx := context.Background()
	_, alice := f.visitor(t, "sub-alice", "alice@corp.example")
	list := f.projectAgentsPath(f.projectID)

	lineal := &store.Agent{
		ID: tid(t.Name() + "-lineal"), Name: "lineal", Slug: "lineal", ProjectID: f.projectID,
		RuntimeBrokerID: f.agent.RuntimeBrokerID, Phase: "running", MessageMode: store.MessageModeLineage,
	}
	require.NoError(t, f.st.CreateAgent(ctx, lineal))

	slugs := func(path string) []string {
		t.Helper()
		rec := f.do(t, alice, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		var top map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &top))
		var items []json.RawMessage
		require.NoError(t, json.Unmarshal(top["agents"], &items))
		for _, item := range items {
			assert.ElementsMatch(t, visitorAgentViewFields, objectFields(t, item), string(item))
		}
		var resp ListVisitorAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		out := []string{}
		for _, v := range resp.Agents {
			out = append(out, v.Slug)
		}
		return out
	}

	all := []string{f.agent.Slug, f.sealed.Slug, lineal.Slug}
	assert.ElementsMatch(t, all, slugs(list), "unfiltered")
	assert.ElementsMatch(t, []string{f.agent.Slug}, slugs(list+"?messageable=true"), "filtered")
	assert.ElementsMatch(t, all, slugs(list+"?messageable=false"), "other value")
	assert.ElementsMatch(t, all, slugs(list+"?messageable=1"), "other value")

	// The filter agrees with the send: the listed agent accepts a message,
	// the others are refused.
	rec := f.do(t, alice, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("hi"))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, slug := range []string{f.sealed.Slug, lineal.Slug} {
		assertVisitorRefusal(t, f.do(t, alice, http.MethodPost, f.projectMessagePath(slug), visitorTextBody("hi")), "send to "+slug)
	}
}

// errVisitorMessageableFault is returned by visitorMessageableFaultStore's
// role binding loads while it is failing.
var errVisitorMessageableFault = errors.New("injected role binding load fault")

// visitorMessageableFaultStore fails every role binding load once an
// agent list has been read while its fault switch is armed, so the
// request's admission succeeds and only the per-agent messaging decisions
// after the list read see the fault.
type visitorMessageableFaultStore struct {
	store.Store
	fault   *storeFaultSwitch
	failing atomic.Bool
	failed  atomic.Int32
}

func (s *visitorMessageableFaultStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	res, err := s.Store.ListAgents(ctx, filter, opts)
	if s.fault.Active() {
		s.failing.Store(true)
	}
	return res, err
}

func (s *visitorMessageableFaultStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	if s.failing.Load() {
		s.failed.Add(1)
		return nil, errVisitorMessageableFault
	}
	return s.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
}

func (s *visitorMessageableFaultStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if s.failing.Load() {
		s.failed.Add(1)
		return nil, errVisitorMessageableFault
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

// A store error in an agent's messaging decision leaves that agent out of
// the filtered list (deny on error); the request still succeeds, and the
// unfiltered list is unaffected.
func TestVisitorAgents_MessageableFilterDeniesOnError(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	_, alice := f.visitor(t, "sub-alice", "alice@corp.example")
	list := f.projectAgentsPath(f.projectID)

	faulty, fault := installStoreFault(t, f.srv, func(inner store.Store, fault *storeFaultSwitch) *visitorMessageableFaultStore {
		return &visitorMessageableFaultStore{Store: inner, fault: fault}
	})
	// The messaging decision reads role bindings through the authorization
	// service's store as well.
	origAuthzStore := f.srv.authzService.store
	f.srv.authzService.store = faulty
	t.Cleanup(func() { f.srv.authzService.store = origAuthzStore })

	get := func(path string) ListVisitorAgentsResponse {
		t.Helper()
		faulty.failing.Store(false)
		rec := f.do(t, alice, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		var resp ListVisitorAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	clean := get(list + "?messageable=true")
	require.Len(t, clean.Agents, 1, "without the fault the messageable agent is listed")
	assert.Equal(t, f.agent.Slug, clean.Agents[0].Slug)

	fault.Arm()
	faulted := get(list + "?messageable=true")
	assert.Positive(t, faulty.failed.Load(), "the messaging decision observed the fault")
	assert.Empty(t, faulted.Agents, "an agent whose messaging decision errored is left out")

	unfiltered := get(list)
	assert.Len(t, unfiltered.Agents, 2, "the unfiltered list makes no messaging decision")
}

// visitorOwnGrants returns the caller's own grants from GET
// /api/v1/users/me/visitor-grants.
func (f *visitorMessagingFixture) visitorOwnGrants(t *testing.T, token string) []VisitorOwnGrant {
	t.Helper()
	rec := f.do(t, token, http.MethodGet, "/api/v1/users/me/visitor-grants", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListVisitorOwnGrantsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Grants
}

// visitorOwnDMs returns the caller's own DMs from GET
// /api/v1/users/me/visitor-dms.
func (f *visitorMessagingFixture) visitorOwnDMs(t *testing.T, token string) []VisitorOwnDM {
	t.Helper()
	rec := f.do(t, token, http.MethodGet, "/api/v1/users/me/visitor-dms", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListVisitorOwnDMsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.DMs
}

// ruleVisitorAgentsStart makes a visitor through a domain rule
// (ruleVisitor) and checks that it lists the project's agents and reads
// one with the visitor view, sees the rule's grant as active in its own
// grants, and after one send sees that DM in its own DM list. It returns
// the rule, the visitor's account ID and its issuer credential.
func (f *visitorMessagingFixture) ruleVisitorAgentsStart(t *testing.T, subject, email string) (ProjectVisitorRuleView, string, string) {
	t.Helper()
	rule, visitorID, token := f.ruleVisitor(t, subject, email)
	list := f.projectAgentsPath(f.projectID)

	rec := f.do(t, token, http.MethodGet, list, nil)
	require.Equal(t, http.StatusOK, rec.Code, "list with a rule grant: %s", rec.Body.String())
	var agents ListVisitorAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &agents))
	assert.ElementsMatch(t, []VisitorAgentView{
		{Name: "helper", Slug: "helper", Status: "running"},
		{Name: "sealed", Slug: "sealed", Status: "running"},
	}, agents.Agents)
	rec = f.do(t, token, http.MethodGet, list+"/"+f.agent.Slug, nil)
	require.Equal(t, http.StatusOK, rec.Code, "read with a rule grant: %s", rec.Body.String())
	assert.JSONEq(t, `{"name":"helper","slug":"helper","status":"running"}`, rec.Body.String())

	project, err := f.st.GetProject(context.Background(), f.projectID)
	require.NoError(t, err)
	assert.Equal(t, []VisitorOwnGrant{{ProjectID: project.ID, ProjectSlug: project.Slug, ProjectName: project.Name,
		Status: visitorGrantStatusActive}}, f.visitorOwnGrants(t, token), "the rule's grant, active")

	rec = f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("from a rule visitor"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", visitorID)
	require.NoError(t, err)
	dms := f.visitorOwnDMs(t, token)
	require.Len(t, dms, 1, "the DM of the send")
	assert.Equal(t, dmKey, dms[0].ConversationKey)
	assert.Equal(t, f.agent.Slug, dms[0].AgentSlug)
	return rule, visitorID, token
}

// A visitor created by a domain rule lists and reads the project's agents
// and sees its grant and its DM like a visitor granted by email; removing
// the rule refuses list and read on the next request, and its own grants
// and DM lists are empty.
func TestVisitorAgents_RuleVisitorAdmittedUntilRuleRemoved(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	rule, _, token := f.ruleVisitorAgentsStart(t, "sub-rua", "rua@any.example")
	list := f.projectAgentsPath(f.projectID)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.rulesPath()+"/"+rule.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assertVisitorRefusal(t, f.do(t, token, http.MethodGet, list, nil), "list after rule removal")
	assertVisitorRefusal(t, f.do(t, token, http.MethodGet, list+"/"+f.agent.Slug, nil), "read after rule removal")
	assert.Empty(t, f.visitorOwnGrants(t, token), "own grants after rule removal")
	assert.Empty(t, f.visitorOwnDMs(t, token), "own DMs after rule removal")
}

// Revoking one rule visitor refuses its next list and read, empties its
// own grants and DM lists, and the rule, applied again on the account's
// next requests, does not bind it again.
func TestVisitorAgents_RuleVisitorRevokedStaysRefused(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	rule, visitorID, token := f.ruleVisitorAgentsStart(t, "sub-rho", "rho@any.example")
	list := f.projectAgentsPath(f.projectID)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+visitorID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assertVisitorRefusal(t, f.do(t, token, http.MethodGet, list, nil), "list after revocation")
	assertVisitorRefusal(t, f.do(t, token, http.MethodGet, list+"/"+f.agent.Slug, nil), "read after revocation")
	assert.Empty(t, f.visitorOwnGrants(t, token), "own grants after revocation")
	assert.Empty(t, f.visitorOwnDMs(t, token), "own DMs after revocation")

	// The account is still a visitor, so the rule is applied again on this
	// request; the refusal holds and no binding is created.
	require.NotNil(t, f.visitorThrough(t, token), "the revoked account still resolves as a visitor")
	assertVisitorRefusal(t, f.do(t, token, http.MethodGet, list, nil), "list after the rule is applied again")
	assert.Empty(t, f.ruleBindings(t, f.projectID, rule.ID), "the rule does not bind the revoked visitor again")
}
