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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Through the router, on the top-level message route and on the project
// route by ID: a caller who cannot read the target agent's project gets,
// for a soft-deleted agent, exactly the answer the same route gives for an
// unknown agent (status, headers and body, the requested ID aside). A
// caller who can read the project (a member, the hub admin) keeps the
// answer it gets for the agent today.

// routeAnswer is one answer through the router, with every occurrence of
// the requested ID in the body replaced by a placeholder, so answers for
// different IDs can be compared byte for byte.
type routeAnswer struct {
	status int
	header http.Header
	body   string
}

func routeAnswerOf(rec *httptest.ResponseRecorder, id string) routeAnswer {
	return routeAnswer{status: rec.Code, header: rec.Header().Clone(), body: strings.ReplaceAll(rec.Body.String(), id, "<agent>")}
}

// requireSameRouteAnswer asserts byte parity of two answers: status,
// headers and body.
func requireSameRouteAnswer(t *testing.T, want, got routeAnswer, label string) {
	t.Helper()
	assert.Equal(t, want.status, got.status, "%s: status must match:\n  want %d %s\n  got  %d %s", label, want.status, want.body, got.status, got.body)
	assert.Equal(t, want.header, got.header, "%s: headers must match", label)
	assert.Equal(t, want.body, got.body, "%s: body must match", label)
}

// routeMessageBody is a plain message.
func routeMessageBody() map[string]interface{} {
	return map[string]interface{}{"message": "hello"}
}

// routeMessagePaths are the two message routes for an agent ID in project.
func routeMessagePaths(projectID string) map[string]func(id string) string {
	return map[string]func(id string) string{
		"top-level":     func(id string) string { return "/api/v1/agents/" + id + "/message" },
		"project by ID": func(id string) string { return "/api/v1/projects/" + projectID + "/agents/" + id + "/message" },
	}
}

// postAsAgentToken posts body to path through the router with an agent
// token.
func postAsAgentToken(t *testing.T, srv *Server, token, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestAgentMessageRoute_NonReaderSoftDeletedMatchesUnknown: a user who may
// message agents in project B but cannot read the project gets, for a
// soft-deleted agent, byte for byte the answer for an unknown agent ID on
// both routes, and no message row is written.
func TestAgentMessageRoute_NonReaderSoftDeletedMatchesUnknown(t *testing.T) {
	f := acceptanceSetup(t)
	ctx := context.Background()
	msgAuthzGrantAgentMessage(t, f.store, f.ownerA.ID, f.projectB)
	userA := userIdentityOf(f.ownerA)
	require.False(t, f.srv.canReadProject(ctx, userA, f.projectB), "precondition: the sender cannot read project B")
	deleted := softDeletedAgentB(t, f, "route-nr-deleted")
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, userA, f.hubAgentB, false)
	require.True(t, allowed, "precondition: the sender may message agents in project B: %s", reason)
	unknownID := tid("route-nr-unknown")

	for name, path := range routeMessagePaths(f.projectB) {
		unknown := routeAnswerOf(doRequestAsUser(t, f.srv, f.ownerA, http.MethodPost, path(unknownID), routeMessageBody()), unknownID)
		assert.Equal(t, http.StatusNotFound, unknown.status, "%s: precondition: an unknown agent is not found: %s", name, unknown.body)
		got := routeAnswerOf(doRequestAsUser(t, f.srv, f.ownerA, http.MethodPost, path(deleted.ID), routeMessageBody()), deleted.ID)
		requireSameRouteAnswer(t, unknown, got, name+": route check: a non-reader's answer for a soft-deleted agent equals the unknown-agent answer")
	}
	assert.Equal(t, 0, agentMessageCount(t, f.store, deleted.ID), "no message row is written")
}

// TestAgentMessageRoute_MemberSendToSoftDeletedUnchanged: the project owner
// (a reader) keeps the answer it gets today for a soft-deleted agent on
// both routes: the send is accepted, as upstream pins it.
func TestAgentMessageRoute_MemberSendToSoftDeletedUnchanged(t *testing.T) {
	f := acceptanceSetup(t)
	require.True(t, f.srv.canReadProject(context.Background(), userIdentityOf(f.ownerB), f.projectB), "precondition: the owner reads project B")
	deleted := softDeletedAgentB(t, f, "route-member-deleted")

	for name, path := range routeMessagePaths(f.projectB) {
		rec := doRequestAsUser(t, f.srv, f.ownerB, http.MethodPost, path(deleted.ID), routeMessageBody())
		assert.Equal(t, http.StatusOK, rec.Code, "%s: a member's send to a soft-deleted agent is unchanged: %s", name, rec.Body.String())
	}
}

// TestAgentMessageRoute_HubAdminSendToSoftDeletedUnchanged: the hub admin
// reads every project, so it takes the reader path on both routes.
func TestAgentMessageRoute_HubAdminSendToSoftDeletedUnchanged(t *testing.T) {
	f := acceptanceSetup(t)
	adminID := tid("route-hub-admin")
	createTestUserWithRole(t, f.store, adminID, "route-hub-admin@test.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	admin, err := f.store.GetUser(context.Background(), adminID)
	require.NoError(t, err)
	require.True(t, f.srv.canReadProject(context.Background(), userIdentityOf(admin), f.projectB), "precondition: the hub admin reads project B")
	deleted := softDeletedAgentB(t, f, "route-admin-deleted")

	for name, path := range routeMessagePaths(f.projectB) {
		rec := doRequestAsUser(t, f.srv, admin, http.MethodPost, path(deleted.ID), routeMessageBody())
		assert.Equal(t, http.StatusOK, rec.Code, "%s: the hub admin's send to a soft-deleted agent is unchanged: %s", name, rec.Body.String())
	}
}

// TestAgentMessageRoute_CrossProjectAgentSoftDeletedMatchesUnknown: an
// agent of project A cannot read project B (strict agent-project rule), so
// on the top-level route its answer for a soft-deleted agent of project B
// equals its answer for an unknown agent, and no message row is written.
func TestAgentMessageRoute_CrossProjectAgentSoftDeletedMatchesUnknown(t *testing.T) {
	f := acceptanceSetup(t)
	ctx := context.Background()
	sender := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	require.False(t, f.srv.canReadProject(ctx, sender, f.projectB), "precondition: an agent of project A cannot read project B")
	token, err := f.srv.GetAgentTokenService().GenerateAgentToken(f.hubAgentA.ID, f.projectA, nil, f.hubAgentA.Ancestry)
	require.NoError(t, err)
	deleted := softDeletedAgentB(t, f, "route-xp-deleted")
	unknownID := tid("route-xp-unknown")
	path := func(id string) string { return "/api/v1/agents/" + id + "/message" }

	unknown := routeAnswerOf(postAsAgentToken(t, f.srv, token, path(unknownID), routeMessageBody()), unknownID)
	got := routeAnswerOf(postAsAgentToken(t, f.srv, token, path(deleted.ID), routeMessageBody()), deleted.ID)
	requireSameRouteAnswer(t, unknown, got, "route check: a cross-project agent's answer for a soft-deleted agent equals the unknown-agent answer")
	assert.NotEqual(t, http.StatusOK, got.status, "the send is not accepted: %s", got.body)
	assert.Equal(t, 0, agentMessageCount(t, f.store, deleted.ID), "no message row is written")
}

// TestAgentMessageRoute_VisitorSoftDeletedMatchesUnknown: a federation
// visitor gets the one visitor refusal for a soft-deleted agent and for an
// unknown one, byte for byte, on both routes.
func TestAgentMessageRoute_VisitorSoftDeletedMatchesUnknown(t *testing.T) {
	vf := newVisitorMessagingFixture(t)
	ctx := context.Background()
	_, token := vf.visitor(t, "sub-route-deleted", "route-deleted@corp.example")
	deleted := &store.Agent{
		ID: tid(t.Name() + "-deleted"), Name: "route-visitor-deleted", Slug: "route-visitor-deleted",
		ProjectID: vf.projectID, RuntimeBrokerID: vf.agent.RuntimeBrokerID, Phase: "running",
		MessageMode: store.MessageModeProject, DeletedAt: time.Now().UTC().Add(-time.Minute),
	}
	require.NoError(t, vf.st.CreateAgent(ctx, deleted))
	unknownID := tid(t.Name() + "-unknown")

	for name, path := range map[string]func(id string) string{
		"top-level":     agentMessagePath,
		"project by ID": vf.projectMessagePath,
	} {
		unknown := vf.do(t, token, http.MethodPost, path(unknownID), visitorTextBody("x"))
		assertVisitorRefusal(t, unknown, name+": unknown agent")
		got := vf.do(t, token, http.MethodPost, path(deleted.ID), visitorTextBody("x"))
		requireSameRouteAnswer(t, routeAnswerOf(unknown, unknownID), routeAnswerOf(got, deleted.ID), name+": visitor refusal parity")
	}
}

// errRouteProjectFault is the project lookup fault of routeProjectFaultStore.
var errRouteProjectFault = errors.New("route test: project lookup fault")

// routeProjectFaultStore fails GetProject for one project while its switch
// is armed. passNext lets the next n lookups of that project through first,
// and failed counts the lookups that failed since then.
type routeProjectFaultStore struct {
	store.Store
	fault     *storeFaultSwitch
	projectID string

	mu     sync.Mutex
	pass   int
	failed int
}

func (p *routeProjectFaultStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if id == p.projectID && p.fault.Active() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.pass > 0 {
			p.pass--
		} else {
			p.failed++
			return nil, errRouteProjectFault
		}
	}
	return p.Store.GetProject(ctx, id)
}

// passNext lets the next n lookups of the project through and resets the
// failed count.
func (p *routeProjectFaultStore) passNext(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pass, p.failed = n, 0
}

// failedLookups is the number of lookups that failed since passNext.
func (p *routeProjectFaultStore) failedLookups() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failed
}

// TestAgentMessageRoute_ProjectLookupFaultFailsClosed: when the project
// lookup behind canReadProject fails, the caller is taken as a non-reader:
// even the project owner gets, for a soft-deleted agent on both routes,
// exactly the unknown-agent answer, not a distinguishing error.
func TestAgentMessageRoute_ProjectLookupFaultFailsClosed(t *testing.T) {
	srv, s, owner, _, projectID, faultStore, fault := msgAuthzSetupWithFault(t, func(inner store.Store, fault *storeFaultSwitch) *routeProjectFaultStore {
		return &routeProjectFaultStore{Store: inner, fault: fault, projectID: tid(msgAuthzProjectID)}
	})
	ctx := context.Background()
	deleted := &store.Agent{
		ID: tid("route-fault-deleted"), Name: "route-fault-deleted", Slug: "route-fault-deleted",
		ProjectID: projectID, MessageMode: store.MessageModeProject, Phase: "running",
		Ancestry: []string{owner.ID}, Created: time.Now(), Updated: time.Now(),
		DeletedAt: time.Now().UTC().Add(-time.Minute),
	}
	require.NoError(t, s.CreateAgent(ctx, deleted))
	unknownID := tid("route-fault-unknown")
	path := func(id string) string { return "/api/v1/agents/" + id + "/message" }

	fault.Arm()
	require.False(t, srv.canReadProject(ctx, userIdentityOf(owner), projectID), "precondition: a project lookup fault counts as no read")
	unknown := routeAnswerOf(doRequestAsUser(t, srv, owner, http.MethodPost, path(unknownID), routeMessageBody()), unknownID)
	assert.Equal(t, http.StatusNotFound, unknown.status, "precondition: an unknown agent is not found: %s", unknown.body)
	got := routeAnswerOf(doRequestAsUser(t, srv, owner, http.MethodPost, path(deleted.ID), routeMessageBody()), deleted.ID)
	requireSameRouteAnswer(t, unknown, got, "fail-closed check: under a project lookup fault the answer for a soft-deleted agent equals the unknown-agent answer")

	// The project route reads the project in its path before it resolves
	// the agent, so that first lookup is let through and the next one, the
	// lookup behind canReadProject, fails.
	projectPath := routeMessagePaths(projectID)["project by ID"]
	faultStore.passNext(1)
	unknown = routeAnswerOf(doRequestAsUser(t, srv, owner, http.MethodPost, projectPath(unknownID), routeMessageBody()), unknownID)
	assert.Equal(t, http.StatusNotFound, unknown.status, "project route: precondition: an unknown agent is not found: %s", unknown.body)
	require.Zero(t, faultStore.failedLookups(), "project route: precondition: no project lookup fails on the unknown-agent path")
	faultStore.passNext(1)
	got = routeAnswerOf(doRequestAsUser(t, srv, owner, http.MethodPost, projectPath(deleted.ID), routeMessageBody()), deleted.ID)
	require.Equal(t, 1, faultStore.failedLookups(), "project route: precondition: the second project lookup, behind canReadProject, is the one that fails")
	requireSameRouteAnswer(t, unknown, got, "project route: fail-closed check: under a project lookup fault the answer for a soft-deleted agent equals the unknown-agent answer")
	assert.Equal(t, 0, agentMessageCount(t, s, deleted.ID), "no message row is written")
}
