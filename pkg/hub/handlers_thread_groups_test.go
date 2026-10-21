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
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Handler tests for /api/v1/thread-groups and threadGroupId on
// POST /api/v1/conversations (ptone/scion#4424, design section 8 phase 2).

// threadGroupEventSpy records PublishChatThreadGroupEvent calls.
type threadGroupEventSpy struct {
	noopEventPublisher
	mu     sync.Mutex
	events []threadGroupSpyEvent
}

type threadGroupSpyEvent struct {
	projectID string
	action    string
	group     ThreadGroupResponse
}

func (p *threadGroupEventSpy) PublishChatThreadGroupEvent(_ context.Context, projectID, action string, group ThreadGroupResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, threadGroupSpyEvent{projectID: projectID, action: action, group: group})
}

// take returns the recorded events and clears them.
func (p *threadGroupEventSpy) take() []threadGroupSpyEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.events
	p.events = nil
	return out
}

// threadGroupFixture is the inbox fixture (projects A and B, an agent in
// each, a super-admin, an owner and a member of A) with the webchat store
// wired and an event spy installed. No experiment is set: thread groups are
// always on (ptone/scion#5546).
type threadGroupFixture struct {
	*inboxFixture
	wcs WebChatStore
	spy *threadGroupEventSpy
}

// setThreadGroupsExperimentsDoc installs operational settings whose stored
// experiments row is raw.
func setThreadGroupsExperimentsDoc(t *testing.T, srv *Server, raw string) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(raw))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

func newThreadGroupFixture(t *testing.T) *threadGroupFixture {
	t.Helper()
	f := &threadGroupFixture{inboxFixture: newInboxFixture(t)}
	dbProvider, ok := f.s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	f.wcs = NewWebChatStore(dbProvider.DB(), "sqlite3")
	require.NoError(t, f.wcs.Init())
	f.srv.SetWebChatStore(f.wcs)
	grantAgentProjectAccess(t, f.s, f.agentA.ID, f.projA)
	grantAgentProjectAccess(t, f.s, f.agentB.ID, f.projB)
	f.spy = &threadGroupEventSpy{}
	f.srv.events = f.spy
	return f
}

// thread creates a native, topic-backed group conversation in projectID
// and returns the conversation and its topic ID.
func (f *threadGroupFixture) thread(t *testing.T, projectID, name string) (*store.Conversation, string) {
	t.Helper()
	conv, apiErr := f.srv.createGroupConversation(context.Background(), "native", createGroupParams{
		ProjectID: projectID, DisplayName: name, CreatedBy: f.admin,
	})
	require.Nil(t, apiErr)
	topics, err := f.wcs.ListTopics(context.Background(), projectID)
	require.NoError(t, err)
	for _, tp := range topics {
		if tp.ConversationID == conv.ID {
			return conv, tp.ID
		}
	}
	t.Fatalf("no topic for conversation %s", conv.ID)
	return nil, ""
}

// mkGroup creates a thread group in projectID through the store.
func (f *threadGroupFixture) mkGroup(t *testing.T, projectID, name string) *store.ThreadGroup {
	t.Helper()
	g := &store.ThreadGroup{ProjectID: projectID, Name: name, CreatedBy: f.admin, CreatedByKind: store.ThreadGroupCreatorUser}
	require.NoError(t, f.s.CreateThreadGroup(context.Background(), g))
	return g
}

// agentCall sends a request as agent (an agent token of projectID with
// project:read) to the thread group handlers or to handleCreateConversation.
func (f *threadGroupFixture) agentCall(t *testing.T, agentID, projectID, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agentID, projectID, []AgentTokenScope{ScopeProjectRead}))
	rec := httptest.NewRecorder()
	switch {
	case strings.HasPrefix(path, "/api/v1/conversations"):
		f.srv.handleCreateConversation(rec, req)
	case strings.HasPrefix(path, "/api/v1/thread-groups/"):
		f.srv.handleThreadGroupRoutes(rec, req)
	default:
		f.srv.handleThreadGroups(rec, req)
	}
	return rec
}

func decodeThreadGroup(t *testing.T, rec *httptest.ResponseRecorder) ThreadGroupResponse {
	t.Helper()
	var g ThreadGroupResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g), rec.Body.String())
	return g
}

func threadGroupMemberCount(t *testing.T, s store.Store, conversationID string) int {
	t.Helper()
	db := s.(interface{ DB() *sql.DB }).DB()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM thread_group_members WHERE conversation_id = ?`, conversationID).Scan(&n))
	return n
}

// threadGroupOps is one call of every thread group operation against group
// g, with thread conv as the add/remove target. It returns the status of
// each, keyed by operation name. do sends one request.
func threadGroupOps(t *testing.T, projectID string, g *store.ThreadGroup, conv *store.Conversation,
	do func(method, path string, body interface{}) *httptest.ResponseRecorder) map[string]int {
	t.Helper()
	base := "/api/v1/thread-groups/" + g.ID
	return map[string]int{
		"list":   do(http.MethodGet, "/api/v1/thread-groups?projectId="+projectID, nil).Code,
		"get":    do(http.MethodGet, base, nil).Code,
		"create": do(http.MethodPost, "/api/v1/thread-groups", map[string]string{"projectId": projectID, "name": "ops-" + uuid.NewString()[:8]}).Code,
		"rename": do(http.MethodPatch, base, map[string]string{"name": "renamed-" + uuid.NewString()[:8]}).Code,
		"add":    do(http.MethodPost, base+"/threads", map[string]string{"conversationId": conv.ID}).Code,
		"remove": do(http.MethodDelete, base+"/threads/"+conv.ID, nil).Code,
		"delete": do(http.MethodDelete, base, nil).Code,
	}
}

// Thread groups are always on (ptone/scion#5546): a hub whose experiments
// row still holds an override for a retired name (for example one that
// turned the former thread groups experiment off before the upgrade) serves
// every route and files a new conversation into a group.
func TestThreadGroups_RetiredExperimentOverrideIgnored(t *testing.T) {
	for _, stored := range []bool{false, true} {
		t.Run(fmt.Sprintf("stored_%t", stored), func(t *testing.T) {
			f := newThreadGroupFixture(t)
			setThreadGroupsExperimentsDoc(t, f.srv, retiredOverridesDoc(t, stored))
			conv, _ := f.thread(t, f.projA, "override-thread")
			g := f.mkGroup(t, f.projA, "Override")

			got := threadGroupOps(t, f.projA, g, conv, func(method, path string, body interface{}) *httptest.ResponseRecorder {
				return f.agentCall(t, f.agentA.ID, f.projA, method, path, body)
			})
			assert.Equal(t, map[string]int{
				"list": http.StatusOK, "get": http.StatusOK, "create": http.StatusCreated, "rename": http.StatusOK,
				"add": http.StatusOK, "remove": http.StatusNoContent, "delete": http.StatusForbidden,
			}, got)

			rec := f.session(t, f.member, http.MethodPost, "/api/v1/conversations",
				map[string]string{"displayName": "override-create", "projectId": f.projA, "threadGroupId": g.ID})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			grp, err := f.s.GetThreadGroup(context.Background(), g.ID)
			require.NoError(t, err)
			require.Len(t, grp.Members, 1)
		})
	}
}

// retiredOverridesDoc returns a stored experiments row that sets every
// retired experiment name to value.
func retiredOverridesDoc(t *testing.T, value bool) string {
	t.Helper()
	retired := experiments.Default().Retired()
	require.NotEmpty(t, retired)
	overrides := make(map[string]bool, len(retired))
	for _, name := range retired {
		overrides[name] = value
	}
	raw, err := json.Marshal(map[string]any{"overrides": overrides})
	require.NoError(t, err)
	return string(raw)
}

// A hub built with an injected registry that has no thread groups entry
// (as the experiments API tests build one) starts, and the thread group
// routes reach their handlers: the collection answers a user's missing
// projectId with 400, and an unknown group ID is the handler's 404, not the
// "route not found" of a gate.
func TestThreadGroups_RegistryWithoutExperimentServesRoutes(t *testing.T) {
	srv, _ := testServerWithOps(t, testRegistry(t))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/thread-groups", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "projectId is required")

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/thread-groups/"+uuid.NewString(), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Thread group not found")
	assert.NotContains(t, rec.Body.String(), "route not found")
}

func assertNoConversationNamed(t *testing.T, s store.Store, name string) {
	t.Helper()
	db := s.(interface{ DB() *sql.DB }).DB()
	var convs, topics int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE display_name = ?`, name).Scan(&convs))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM webchat_topic WHERE name = ?`, name).Scan(&topics))
	assert.Zero(t, convs, "no conversation may be minted for %q", name)
	assert.Zero(t, topics, "no topic may be minted for %q", name)
}

// A project member user can do every operation, and each mutation
// publishes chat.thread_group with the full snapshot.
func TestThreadGroups_MemberUserFullLifecycle(t *testing.T) {
	f := newThreadGroupFixture(t)
	conv, topicID := f.thread(t, f.projA, "release-notes")
	conv2, topic2 := f.thread(t, f.projA, "changelog")

	// An empty project lists [], never null.
	rec := f.session(t, f.member, http.MethodGet, "/api/v1/thread-groups?projectId="+f.projA, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"threadGroups":[]}`, rec.Body.String())

	rec = f.session(t, f.member, http.MethodPost, "/api/v1/thread-groups", map[string]string{"projectId": f.projA, "name": "  Release  "})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	g := decodeThreadGroup(t, rec)
	assert.Equal(t, "Release", g.Name)
	assert.Equal(t, f.projA, g.ProjectID)
	assert.Equal(t, f.member, g.CreatedBy)
	assert.Equal(t, store.ThreadGroupCreatorUser, g.CreatedByKind)
	assert.NotNil(t, g.Threads)
	assert.Contains(t, rec.Body.String(), `"threads":[]`)
	ev := f.spy.take()
	require.Len(t, ev, 1)
	assert.Equal(t, f.projA, ev[0].projectID)
	assert.Equal(t, ThreadGroupEventCreated, ev[0].action)
	assert.Equal(t, g.ID, ev[0].group.ID)

	// A name conflict is case-insensitive: 409.
	rec = f.session(t, f.member, http.MethodPost, "/api/v1/thread-groups", map[string]string{"projectId": f.projA, "name": "release"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	// An invalid name is 400.
	rec = f.session(t, f.member, http.MethodPost, "/api/v1/thread-groups", map[string]string{"projectId": f.projA, "name": "   "})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	// A user must name the project.
	rec = f.session(t, f.member, http.MethodPost, "/api/v1/thread-groups", map[string]string{"name": "No Project"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = f.session(t, f.member, http.MethodGet, "/api/v1/thread-groups", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, f.spy.take())

	base := "/api/v1/thread-groups/" + g.ID

	// Add by conversation ID, then by thread (topic) ID.
	rec = f.session(t, f.member, http.MethodPost, base+"/threads", map[string]string{"conversationId": conv.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.session(t, f.member, http.MethodPost, base+"/threads", map[string]string{"threadId": topic2})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeThreadGroup(t, rec)
	require.Len(t, got.Threads, 2)
	byConv := map[string]ThreadGroupThread{}
	for _, th := range got.Threads {
		byConv[th.ConversationID] = th
	}
	require.Contains(t, byConv, conv.ID)
	require.Contains(t, byConv, conv2.ID)
	assert.Equal(t, topicID, byConv[conv.ID].ThreadID)
	assert.Equal(t, "release-notes", byConv[conv.ID].Name)
	assert.Equal(t, f.member, byConv[conv.ID].AddedBy)
	assert.False(t, byConv[conv.ID].AddedAt.IsZero())
	assert.Equal(t, topic2, byConv[conv2.ID].ThreadID)
	assert.Equal(t, "changelog", byConv[conv2.ID].Name)
	ev = f.spy.take()
	require.Len(t, ev, 2)
	for _, e := range ev {
		assert.Equal(t, ThreadGroupEventUpdated, e.action)
	}
	assert.Len(t, ev[1].group.Threads, 2)

	// Adding again is idempotent and publishes nothing.
	rec = f.session(t, f.member, http.MethodPost, base+"/threads", map[string]string{"conversationId": conv.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, decodeThreadGroup(t, rec).Threads, 2)
	assert.Empty(t, f.spy.take())

	// Get and list.
	rec = f.session(t, f.member, http.MethodGet, base, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, decodeThreadGroup(t, rec).Threads, 2)
	rec = f.session(t, f.member, http.MethodGet, "/api/v1/thread-groups?projectId="+f.projA, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list threadGroupListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.ThreadGroups, 1)
	assert.Len(t, list.ThreadGroups[0].Threads, 2)

	// Rename: a name another group has (in any case) is 409, an invalid
	// name 400, and neither publishes.
	f.mkGroup(t, f.projA, "Taken")
	rec = f.session(t, f.member, http.MethodPatch, base, map[string]string{"name": "TAKEN"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	rec = f.session(t, f.member, http.MethodPatch, base, map[string]string{"name": "   "})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, f.spy.take())

	// Rename.
	rec = f.session(t, f.member, http.MethodPatch, base, map[string]string{"name": "Shipped"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "Shipped", decodeThreadGroup(t, rec).Name)
	ev = f.spy.take()
	require.Len(t, ev, 1)
	assert.Equal(t, ThreadGroupEventUpdated, ev[0].action)
	assert.Equal(t, "Shipped", ev[0].group.Name)

	// Remove, then remove again (idempotent, no event).
	rec = f.session(t, f.member, http.MethodDelete, base+"/threads/"+conv.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	ev = f.spy.take()
	require.Len(t, ev, 1)
	assert.Equal(t, ThreadGroupEventUpdated, ev[0].action)
	assert.Len(t, ev[0].group.Threads, 1)
	rec = f.session(t, f.member, http.MethodDelete, base+"/threads/"+conv.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	// A malformed conversation ID cannot be a member: 204 as well.
	rec = f.session(t, f.member, http.MethodDelete, base+"/threads/not-a-uuid", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, f.spy.take())

	// Delete: 204, the snapshot is published, the thread is ungrouped
	// and not deleted.
	rec = f.session(t, f.member, http.MethodDelete, base, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	ev = f.spy.take()
	require.Len(t, ev, 1)
	assert.Equal(t, ThreadGroupEventDeleted, ev[0].action)
	assert.Equal(t, g.ID, ev[0].group.ID)
	assert.Len(t, ev[0].group.Threads, 1)
	assert.Zero(t, threadGroupMemberCount(t, f.s, conv2.ID))
	_, err := f.s.GetConversation(context.Background(), conv2.ID)
	require.NoError(t, err)
	rec = f.session(t, f.member, http.MethodGet, base, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// Adding a thread that is in another group moves it, and both groups are
// published.
func TestThreadGroups_AddMovesThreadAndPublishesBothGroups(t *testing.T) {
	f := newThreadGroupFixture(t)
	conv, _ := f.thread(t, f.projA, "mover")
	from := f.mkGroup(t, f.projA, "From")
	to := f.mkGroup(t, f.projA, "To")
	require.NoError(t, f.s.SetThreadGroupMember(context.Background(), from.ID, conv.ID, f.admin))

	rec := f.session(t, f.member, http.MethodPost, "/api/v1/thread-groups/"+to.ID+"/threads", map[string]string{"conversationId": conv.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, decodeThreadGroup(t, rec).Threads, 1)

	ev := f.spy.take()
	require.Len(t, ev, 2)
	assert.Equal(t, to.ID, ev[0].group.ID)
	assert.Len(t, ev[0].group.Threads, 1)
	assert.Equal(t, from.ID, ev[1].group.ID)
	assert.Empty(t, ev[1].group.Threads)
	for _, e := range ev {
		assert.Equal(t, ThreadGroupEventUpdated, e.action)
	}
}

// A same-project agent can do everything except delete (403).
func TestThreadGroups_SameProjectAgentAllButDelete(t *testing.T) {
	f := newThreadGroupFixture(t)
	conv, _ := f.thread(t, f.projA, "agent-thread")
	g := f.mkGroup(t, f.projA, "Agent Work")
	call := func(method, path string, body interface{}) *httptest.ResponseRecorder {
		return f.agentCall(t, f.agentA.ID, f.projA, method, path, body)
	}

	// projectId defaults to the agent's project for list and create.
	rec := call(http.MethodGet, "/api/v1/thread-groups", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = call(http.MethodPost, "/api/v1/thread-groups", map[string]string{"name": "Agent Made"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	made := decodeThreadGroup(t, rec)
	assert.Equal(t, f.projA, made.ProjectID)
	assert.Equal(t, f.agentA.ID, made.CreatedBy)
	assert.Equal(t, store.ThreadGroupCreatorAgent, made.CreatedByKind)

	got := threadGroupOps(t, f.projA, g, conv, call)
	assert.Equal(t, map[string]int{
		"list": http.StatusOK, "get": http.StatusOK, "create": http.StatusCreated, "rename": http.StatusOK,
		"add": http.StatusOK, "remove": http.StatusNoContent, "delete": http.StatusForbidden,
	}, got)
	_, err := f.s.GetThreadGroup(context.Background(), g.ID)
	require.NoError(t, err, "an agent's delete must not remove the group")
}

// An agent of another project gets 403 on everything.
func TestThreadGroups_OtherProjectAgentForbidden(t *testing.T) {
	f := newThreadGroupFixture(t)
	conv, _ := f.thread(t, f.projA, "victim-thread")
	g := f.mkGroup(t, f.projA, "Victim")

	got := threadGroupOps(t, f.projA, g, conv, func(method, path string, body interface{}) *httptest.ResponseRecorder {
		return f.agentCall(t, f.agentB.ID, f.projB, method, path, body)
	})
	for op, code := range got {
		assert.Equal(t, http.StatusForbidden, code, "other-project agent %s", op)
	}
	assert.Empty(t, f.spy.take())
	assert.Zero(t, threadGroupMemberCount(t, f.s, conv.ID))
}

// A user who is not a project member gets 403 on everything.
func TestThreadGroups_NonMemberUserForbidden(t *testing.T) {
	f := newThreadGroupFixture(t)
	conv, _ := f.thread(t, f.projA, "private-thread")
	g := f.mkGroup(t, f.projA, "Private")
	outsider := f.plainUser(t)

	got := threadGroupOps(t, f.projA, g, conv, func(method, path string, body interface{}) *httptest.ResponseRecorder {
		return f.session(t, outsider, method, path, body)
	})
	for op, code := range got {
		assert.Equal(t, http.StatusForbidden, code, "non-member %s", op)
	}
	assert.Empty(t, f.spy.take())
}

// A user access token without inbox:write can read but not mutate; with
// inbox:write it can mutate.
func TestThreadGroups_TokenWithoutInboxWriteReadsOnly(t *testing.T) {
	f := newThreadGroupFixture(t)
	conv, _ := f.thread(t, f.projA, "token-thread")
	g := f.mkGroup(t, f.projA, "Tokens")

	readOnly := f.mint(t, f.admin, projectBoundary(f.projA), "project:read")
	got := threadGroupOps(t, f.projA, g, conv, func(method, path string, body interface{}) *httptest.ResponseRecorder {
		return f.call(t, readOnly, method, path, body)
	})
	assert.Equal(t, map[string]int{
		"list": http.StatusOK, "get": http.StatusOK, "create": http.StatusForbidden, "rename": http.StatusForbidden,
		"add": http.StatusForbidden, "remove": http.StatusForbidden, "delete": http.StatusForbidden,
	}, got)
	assert.Empty(t, f.spy.take())

	// A token for another project cannot read A's groups.
	otherProject := f.mint(t, f.admin, projectBoundary(f.projB), "project:read", "inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, otherProject, http.MethodGet, "/api/v1/thread-groups/"+g.ID, nil).Code)

	both := f.mint(t, f.admin, projectBoundary(f.projA), "project:read", "inbox:write")
	got = threadGroupOps(t, f.projA, g, conv, func(method, path string, body interface{}) *httptest.ResponseRecorder {
		return f.call(t, both, method, path, body)
	})
	assert.Equal(t, map[string]int{
		"list": http.StatusOK, "get": http.StatusOK, "create": http.StatusCreated, "rename": http.StatusOK,
		"add": http.StatusOK, "remove": http.StatusNoContent, "delete": http.StatusNoContent,
	}, got)
}

// Adding a thread rejects direct conversations, other projects'
// conversations, #general and deleted threads; a threadId resolves within
// the group's project only.
func TestThreadGroups_AddThreadValidation(t *testing.T) {
	f := newThreadGroupFixture(t)
	ctx := context.Background()
	g := f.mkGroup(t, f.projA, "Validation")
	path := "/api/v1/thread-groups/" + g.ID + "/threads"
	add := func(body map[string]string) *httptest.ResponseRecorder {
		return f.session(t, f.member, http.MethodPost, path, body)
	}

	// A direct conversation is 400 for one of its participants, and the
	// not-found answer for anyone else (a DM's ID reveals nothing).
	direct := f.direct(t, f.member, "agent", f.agentA.ID)
	rec := add(map[string]string{"conversationId": direct.ID})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "direct conversations")
	othersDM := f.direct(t, f.owner, "agent", f.agentA.ID)
	rec = add(map[string]string{"conversationId": othersDM.ID})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "direct")

	otherConv, otherTopic := f.thread(t, f.projB, "other-project-thread")
	assert.Equal(t, http.StatusNotFound, add(map[string]string{"conversationId": otherConv.ID}).Code)
	assert.Equal(t, http.StatusNotFound, add(map[string]string{"threadId": otherTopic}).Code,
		"a threadId of another project must not resolve")

	generalTopic, _, err := f.wcs.EnsureGeneralTopic(ctx, f.projA, f.admin)
	require.NoError(t, err)
	generalConv, err := f.wcs.GetTopicConversationIDInProject(ctx, f.projA, generalTopic)
	require.NoError(t, err)
	require.NotEmpty(t, generalConv)
	for _, body := range []map[string]string{{"conversationId": generalConv}, {"threadId": generalTopic}} {
		rec = add(body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "#general")
	}

	// A deleted thread: the topic is soft-deleted, the conversation row
	// stays (handleTopicDelete).
	deletedConv, deletedTopic := f.thread(t, f.projA, "deleted-thread")
	require.NoError(t, f.wcs.DeleteTopic(ctx, deletedTopic))
	assert.Equal(t, http.StatusNotFound, add(map[string]string{"conversationId": deletedConv.ID}).Code)
	assert.Equal(t, http.StatusNotFound, add(map[string]string{"threadId": deletedTopic}).Code)

	// A soft-deleted conversation.
	goneConv, _ := f.thread(t, f.projA, "gone-conversation")
	now := time.Now().UTC()
	goneConv.DeletedAt = &now
	require.NoError(t, f.s.UpdateConversation(ctx, goneConv))
	assert.Equal(t, http.StatusNotFound, add(map[string]string{"conversationId": goneConv.ID}).Code)

	// Unknown IDs and bad bodies.
	assert.Equal(t, http.StatusNotFound, add(map[string]string{"conversationId": uuid.NewString()}).Code)
	assert.Equal(t, http.StatusNotFound, add(map[string]string{"threadId": uuid.NewString()}).Code)
	assert.Equal(t, http.StatusBadRequest, add(map[string]string{}).Code)
	assert.Equal(t, http.StatusBadRequest, add(map[string]string{"conversationId": deletedConv.ID, "threadId": deletedTopic}).Code)

	// A group conversation without a topic (external surface) may be filed.
	ext := f.group(t, f.projA)
	rec = add(map[string]string{"conversationId": ext.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeThreadGroup(t, rec)
	require.Len(t, got.Threads, 1)
	assert.Equal(t, ext.ID, got.Threads[0].ConversationID)
	assert.Empty(t, got.Threads[0].ThreadID)
	assert.Equal(t, ext.DisplayName, got.Threads[0].Name)
	assert.Zero(t, threadGroupMemberCount(t, f.s, direct.ID)+threadGroupMemberCount(t, f.s, othersDM.ID)+threadGroupMemberCount(t, f.s, otherConv.ID)+
		threadGroupMemberCount(t, f.s, generalConv)+threadGroupMemberCount(t, f.s, deletedConv.ID))
}

// Reads skip a member whose topic was deleted behind the API's back.
func TestThreadGroups_ReadSkipsDeletedTopicMember(t *testing.T) {
	f := newThreadGroupFixture(t)
	ctx := context.Background()
	g := f.mkGroup(t, f.projA, "Skips")
	live, _ := f.thread(t, f.projA, "live-thread")
	dead, deadTopic := f.thread(t, f.projA, "dead-thread")
	require.NoError(t, f.s.SetThreadGroupMember(ctx, g.ID, live.ID, f.admin))
	require.NoError(t, f.s.SetThreadGroupMember(ctx, g.ID, dead.ID, f.admin))
	require.NoError(t, f.wcs.DeleteTopic(ctx, deadTopic)) // store-level: no cleanup ran

	rec := f.session(t, f.member, http.MethodGet, "/api/v1/thread-groups/"+g.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeThreadGroup(t, rec)
	require.Len(t, got.Threads, 1)
	assert.Equal(t, live.ID, got.Threads[0].ConversationID)
}

// The 100 groups per project and 500 threads per group limits are 422.
func TestThreadGroups_Limits422(t *testing.T) {
	f := newThreadGroupFixture(t)
	ctx := context.Background()
	for i := 0; i < store.MaxThreadGroupsPerProject; i++ {
		f.mkGroup(t, f.projA, fmt.Sprintf("group-%03d", i))
	}
	rec := f.session(t, f.member, http.MethodPost, "/api/v1/thread-groups", map[string]string{"projectId": f.projA, "name": "one-too-many"})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	full := f.mkGroup(t, f.projB, "Full")
	for i := 0; i < store.MaxThreadsPerThreadGroup; i++ {
		c := f.group(t, f.projB)
		require.NoError(t, f.s.SetThreadGroupMember(ctx, full.ID, c.ID, f.admin))
	}
	extra := f.group(t, f.projB)
	rec = f.session(t, f.admin, http.MethodPost, "/api/v1/thread-groups/"+full.ID+"/threads", map[string]string{"conversationId": extra.ID})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	rec = f.session(t, f.admin, http.MethodPost, "/api/v1/conversations",
		map[string]string{"displayName": "over-limit-create", "projectId": f.projB, "threadGroupId": full.ID})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assertNoConversationNamed(t, f.s, "over-limit-create")
}

// threadGroupId on POST /api/v1/conversations: a valid group files the new
// conversation; a wrong-project or missing group creates nothing.
func TestCreateConversation_WithThreadGroup(t *testing.T) {
	f := newThreadGroupFixture(t)
	g := f.mkGroup(t, f.projA, "Filed")
	foreign := f.mkGroup(t, f.projB, "Foreign")

	rec := f.session(t, f.member, http.MethodPost, "/api/v1/conversations",
		map[string]string{"displayName": "filed-thread", "projectId": f.projA, "threadGroupId": g.ID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created conversationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	stored, err := f.s.GetThreadGroup(context.Background(), g.ID)
	require.NoError(t, err)
	require.Len(t, stored.Members, 1)
	assert.Equal(t, created.ID, stored.Members[0].ConversationID)
	assert.Equal(t, f.member, stored.Members[0].AddedBy)
	ev := f.spy.take()
	require.Len(t, ev, 1)
	assert.Equal(t, ThreadGroupEventUpdated, ev[0].action)
	require.Len(t, ev[0].group.Threads, 1)
	assert.Equal(t, "filed-thread", ev[0].group.Threads[0].Name)

	// An agent of another project cannot file into this project's group,
	// even when policy lets it read the project: 403, nothing minted.
	grantAgentProjectAccess(t, f.s, f.agentB.ID, f.projA)
	rec = f.agentCall(t, f.agentB.ID, f.projB, http.MethodPost, "/api/v1/conversations",
		map[string]string{"displayName": "foreign-agent-create", "projectId": f.projA, "threadGroupId": g.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "agents can only file threads into thread groups of their own project",
		"the 403 must come from the thread group rule")
	assertNoConversationNamed(t, f.s, "foreign-agent-create")
	assert.Empty(t, f.spy.take())
	// Control: without threadGroupId the same agent may create in A (the
	// binding grants project:read), so the 403 above is the thread group
	// rule and not the project check.
	rec = f.agentCall(t, f.agentB.ID, f.projB, http.MethodPost, "/api/v1/conversations",
		map[string]string{"displayName": "foreign-agent-control", "projectId": f.projA})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Empty(t, f.spy.take(), "no thread group event without threadGroupId")

	// An agent defaulting to its own project can file too.
	rec = f.agentCall(t, f.agentA.ID, f.projA, http.MethodPost, "/api/v1/conversations",
		map[string]string{"displayName": "agent-filed", "threadGroupId": g.ID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	f.spy.take()

	for name, tc := range map[string]struct{ groupID, convName string }{
		"wrong project": {foreign.ID, "wrong-project-create"},
		"missing":       {uuid.NewString(), "missing-group-create"},
		"malformed":     {"not-a-uuid", "malformed-group-create"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.session(t, f.member, http.MethodPost, "/api/v1/conversations",
				map[string]string{"displayName": tc.convName, "projectId": f.projA, "threadGroupId": tc.groupID})
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			assertNoConversationNamed(t, f.s, tc.convName)
		})
	}
	assert.Empty(t, f.spy.take())
}

// deleteTopic calls handleTopicDelete as the project member.
func (f *threadGroupFixture) deleteTopic(t *testing.T, topicID string) *httptest.ResponseRecorder {
	t.Helper()
	user, err := f.s.GetUser(context.Background(), f.member)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/chat/topics/"+topicID, nil)
	req = req.WithContext(contextWithIdentity(context.Background(), authUser(user)))
	rec := httptest.NewRecorder()
	f.srv.handleTopicDelete(rec, req, topicID)
	return rec
}

// An agent whose message mode forbids joining a group, filing into a bad
// thread group, gets the message-mode denial (the join check runs before the
// threadGroupId validation), and nothing is minted.
func TestCreateConversation_ModeDenialBeforeThreadGroupValidation(t *testing.T) {
	f := newThreadGroupFixture(t)
	ctx := context.Background()
	quiet := &store.Agent{ID: uuid.NewString(), Slug: "tg-quiet-agent", Name: "TG Quiet Agent", ProjectID: f.projA,
		Phase: "running", MessageMode: store.MessageModeNone}
	require.NoError(t, f.s.CreateAgent(ctx, quiet))
	grantAgentProjectAccess(t, f.s, quiet.ID, f.projA)

	rec := f.agentCall(t, quiet.ID, f.projA, http.MethodPost, "/api/v1/conversations",
		map[string]string{"displayName": "mode-denied-create", "threadGroupId": uuid.NewString()})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeMessageDenied, "the message-mode denial wins over the thread group 404")
	assert.NotContains(t, rec.Body.String(), "Thread group not found")
	assertNoConversationNamed(t, f.s, "mode-denied-create")
	assert.Empty(t, f.spy.take())
}

// handleTopicDelete takes the deleted thread out of its group and
// publishes the group.
func TestTopicDelete_RemovesThreadGroupMember(t *testing.T) {
	f := newThreadGroupFixture(t)
	ctx := context.Background()
	g := f.mkGroup(t, f.projA, "Cleanup")
	conv, topicID := f.thread(t, f.projA, "doomed-thread")
	f.thread(t, f.projA, "survivor-thread") // the last thread of a space cannot be deleted
	require.NoError(t, f.s.SetThreadGroupMember(ctx, g.ID, conv.ID, f.admin))

	rec := f.deleteTopic(t, topicID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Zero(t, threadGroupMemberCount(t, f.s, conv.ID))
	ev := f.spy.take()
	require.Len(t, ev, 1)
	assert.Equal(t, g.ID, ev[0].group.ID)
	assert.Equal(t, ThreadGroupEventUpdated, ev[0].action)
	assert.Empty(t, ev[0].group.Threads)
}
