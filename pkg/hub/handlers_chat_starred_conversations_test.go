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
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const starredConversationsPath = "/api/v1/chat/starred-conversations"

func starredConversationsURL(params ...string) string {
	if len(params) == 0 {
		return starredConversationsPath
	}
	q := url.Values{}
	for i := 0; i+1 < len(params); i += 2 {
		q.Add(params[i], params[i+1])
	}
	return starredConversationsPath + "?" + q.Encode()
}

func decodeStarredConversations(t *testing.T, rec *httptest.ResponseRecorder) starredConversationsResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp starredConversationsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Items, "items is always an array")
	return resp
}

func starredConversationRowKeys(items []starredConversationItem) []string {
	keys := []string{}
	for _, it := range items {
		keys = append(keys, it.ConversationKey)
	}
	return keys
}

// walkStarredConversationRows reads every page through get (which receives
// the cursor, "" first) and fails on a repeated conversation.
func walkStarredConversationRows(t *testing.T, get func(cursor string) *httptest.ResponseRecorder) []starredConversationItem {
	t.Helper()
	var rows []starredConversationItem
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 50; i++ {
		page := decodeStarredConversations(t, get(cursor))
		for _, it := range page.Items {
			require.False(t, seen[it.ConversationKey], "conversation %s repeated", it.ConversationKey)
			seen[it.ConversationKey] = true
			rows = append(rows, it)
		}
		if page.NextCursor == "" {
			return rows
		}
		cursor = page.NextCursor
	}
	t.Fatal("pagination did not end")
	return nil
}

// starredRailFixture is a member with stars in two project A threads, an
// agent DM (agent A, project A) and a DM with the owner, plus a retained
// star in project B's thread, which the member cannot read.
type starredRailFixture struct {
	*starFixture
	topicA2 string
	dms     starDMs
	hiddenB string
}

func newStarredRailFixture(t *testing.T, readSwitch bool) *starredRailFixture {
	t.Helper()
	f := &starredRailFixture{starFixture: newStarFixture(t, readSwitch)}
	ctx := context.Background()
	f.topicA2 = f.topic(t, f.projA)
	f.dms = f.memberDMs(t)
	now := time.Now().UTC()
	for _, c := range []struct{ topic, text string }{{f.topicA, "a1"}, {f.topicA, "a2"}, {f.topicA2, "a3"}} {
		decodeStarPut(t, f.put(t, f.member, f.threadMsg(t, c.topic, f.projA, c.text, now.Add(-time.Hour)), c.topic))
	}
	agentMsg := f.dmMsg(t, f.dms.agentKey, f.dms.agentConv, f.projA, "user:"+f.member, f.member, "to agent", "web", now.Add(-time.Hour))
	humanMsg := f.dmMsg(t, f.dms.humanKey, f.dms.humanConv, uuid.Nil.String(), "user:"+f.member, f.member, "to owner", "web", now.Add(-time.Hour))
	decodeStarPut(t, f.put(t, f.member, agentMsg, f.dms.agentKey))
	decodeStarPut(t, f.put(t, f.member, humanMsg, f.dms.humanKey))
	// A retained association in a thread the member cannot read (stored
	// directly: the API would refuse it).
	f.hiddenB = f.threadMsg(t, f.topicB, f.projB, "b1", now.Add(-time.Hour))
	_, _, err := f.stars.AddMessageStar(ctx, f.member, StarTarget{MessageID: f.hiddenB, ProjectID: f.projB,
		Selector: StarSelector{ConversationKey: f.topicB, Channel: "web", ThreadID: f.topicB}})
	require.NoError(t, err)
	return f
}

func (f *starredRailFixture) rail(t *testing.T, userID string, params ...string) starredConversationsResponse {
	t.Helper()
	return decodeStarredConversations(t, f.session(t, userID, http.MethodGet, starredConversationsURL(params...), nil))
}

func TestChatStarredConversations_GateAndValidation(t *testing.T) {
	f := newStarredRailFixture(t, true)
	page := f.rail(t, f.member, "limit", "1")
	require.NotEmpty(t, page.NextCursor)

	for _, bad := range []string{
		starredConversationsURL("limit", "0"),
		starredConversationsURL("limit", "101"),
		starredConversationsURL("limit", "x"),
		starredConversationsURL("sort", "custom"),
		starredConversationsURL("sort", ""),
		starredConversationsURL("projectId", "nope"),
		starredConversationsURL("projectId", "a0000000-0000-0000-0000-00000000000"),
		starredConversationsURL("conversationKey", f.topicA),
		starredConversationsPath + "?limit=1&limit=2",
		starredConversationsURL("cursor", "garbage"),
		// A cursor is bound to its order and scope.
		starredConversationsURL("sort", "alpha", "cursor", page.NextCursor),
		starredConversationsURL("projectId", f.projA, "cursor", page.NextCursor),
	} {
		assert.Equal(t, http.StatusBadRequest, f.session(t, f.member, http.MethodGet, bad, nil).Code, bad)
	}
	// ... and to its caller.
	assert.Equal(t, http.StatusBadRequest,
		f.session(t, f.owner, http.MethodGet, starredConversationsURL("cursor", page.NextCursor), nil).Code)
	// The page size may change between pages.
	assert.Equal(t, http.StatusOK,
		f.session(t, f.member, http.MethodGet, starredConversationsURL("limit", "5", "cursor", page.NextCursor), nil).Code)

	assert.Equal(t, http.StatusMethodNotAllowed, f.session(t, f.member, http.MethodPost, starredConversationsPath, nil).Code)
	assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodGet, starredConversationsPath+"/x", nil).Code)

	setStarsExperiment(t, f.srv, false, true)
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		assert.Equal(t, http.StatusNotFound, f.session(t, f.member, m, starredConversationsPath, nil).Code, m)
	}
}

func TestChatStarredConversations_GroupsAndPaginates(t *testing.T) {
	f := newStarredRailFixture(t, true)
	ctx := context.Background()
	owner, err := f.s.GetUser(ctx, f.owner)
	require.NoError(t, err)
	// A topic's activity is recorded by its messages (CreateTopic sets none).
	require.NoError(t, f.wcs.TouchTopicActivity(ctx, f.topicA, ""))
	topicA, err := f.wcs.GetTopic(ctx, f.topicA)
	require.NoError(t, err)

	all := f.rail(t, f.member)
	assert.Empty(t, all.NextCursor)
	rows := map[string]starredConversationItem{}
	for _, it := range all.Items {
		rows[it.ConversationKey] = it
	}
	assert.ElementsMatch(t, []string{f.topicA, f.topicA2, f.dms.agentKey, f.dms.humanKey}, starredConversationRowKeys(all.Items),
		"each conversation once (topic A has two stars); project B's thread is unreadable")

	thread := rows[f.topicA]
	assert.Equal(t, "thread", thread.ConversationKind)
	assert.Equal(t, topicA.Name, thread.ConversationName)
	assert.Equal(t, f.projA, thread.ProjectID)
	assert.Equal(t, "Inbox A", thread.ProjectName)
	require.NotNil(t, thread.LastActivityAt)
	require.False(t, topicA.LastActivityAt.IsZero())
	assert.True(t, thread.LastActivityAt.Equal(topicA.LastActivityAt))

	agent := rows[f.dms.agentKey]
	assert.Equal(t, "dm", agent.ConversationKind)
	assert.Equal(t, f.agentA.Name, agent.ConversationName)
	assert.Equal(t, f.projA, agent.ProjectID, "an agent DM carries its agent's project")
	assert.Equal(t, "Inbox A", agent.ProjectName)

	human := rows[f.dms.humanKey]
	assert.Equal(t, "dm", human.ConversationKind)
	assert.Equal(t, starUserLabel(owner), human.ConversationName)
	assert.Empty(t, human.ProjectID, "a DM between users has no project fields")
	assert.Empty(t, human.ProjectName)

	// The JSON carries only the documented fields.
	rec := f.session(t, f.member, http.MethodGet, starredConversationsPath, nil)
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	allowed := map[string]bool{"conversationKey": true, "conversationKind": true, "conversationName": true,
		"lastActivityAt": true, "projectId": true, "projectName": true}
	for _, item := range raw.Items {
		for k := range item {
			assert.True(t, allowed[k], "unexpected field %q", k)
		}
	}

	// Every page size walks the same rows in the same order, both sorts.
	for _, sortMode := range []string{StarredConversationsByActivity, StarredConversationsByName} {
		full := f.rail(t, f.member, "sort", sortMode)
		for _, limit := range []int{1, 2, 3} {
			walked := walkStarredConversationRows(t, func(cursor string) *httptest.ResponseRecorder {
				params := []string{"sort", sortMode, "limit", fmt.Sprint(limit)}
				if cursor != "" {
					params = append(params, "cursor", cursor)
				}
				return f.session(t, f.member, http.MethodGet, starredConversationsURL(params...), nil)
			})
			assert.Equal(t, full.Items, walked, "sort %s limit %d", sortMode, limit)
		}
		// The walk above really resumed: page 2 opens the first page's
		// cursor and continues after its row, in both sorts (the DM rows'
		// keys and the name order do not fit authorizedList's cursor).
		first := f.rail(t, f.member, "sort", sortMode, "limit", "1")
		require.Len(t, first.Items, 1)
		require.NotEmpty(t, first.NextCursor)
		second := f.session(t, f.member, http.MethodGet,
			starredConversationsURL("sort", sortMode, "limit", "1", "cursor", first.NextCursor), nil)
		require.Equal(t, http.StatusOK, second.Code, "sort %s page 2: %s", sortMode, second.Body.String())
		page2 := decodeStarredConversations(t, second)
		require.Len(t, page2.Items, 1)
		assert.Equal(t, full.Items[1], page2.Items[0], "sort %s page 2 continues after page 1", sortMode)
	}

	// A rename shows at once; muting and reading do not hide a row.
	name := "renamed " + f.topicA2[:4]
	require.NoError(t, f.wcs.UpdateTopic(ctx, f.topicA2, TopicUpdate{Name: &name}))
	require.NoError(t, f.wcs.SetMuted(ctx, f.member, f.topicA2, true))
	got := map[string]string{}
	for _, it := range f.rail(t, f.member).Items {
		got[it.ConversationKey] = it.ConversationName
	}
	assert.Equal(t, name, got[f.topicA2])

	// Removing the last star of a conversation removes its row; another
	// user's rail is never affected.
	page := decodeStarPage(t, f.session(t, f.member, http.MethodGet, starListPath(f.topicA2), nil))
	require.Len(t, page.Items, 1)
	assert.Equal(t, http.StatusNoContent, f.session(t, f.member, http.MethodDelete, starPath(page.Items[0].MessageID), nil).Code)
	assert.NotContains(t, starredConversationRowKeys(f.rail(t, f.member).Items), f.topicA2)
	assert.Empty(t, f.rail(t, f.owner).Items, "no other user's state")

	// A deleted message's conversation leaves the rail with its last star.
	page = decodeStarPage(t, f.session(t, f.member, http.MethodGet, starListPath(f.dms.humanKey), nil))
	require.Len(t, page.Items, 1)
	_, err = f.wcs.(MessageStarStore).MarkMessageDeletedAndClearStars(ctx, page.Items[0].MessageID, time.Now())
	require.NoError(t, err)
	assert.NotContains(t, starredConversationRowKeys(f.rail(t, f.member).Items), f.dms.humanKey)

	// Regaining access shows the retained association's conversation again.
	createTestUserWithProjectRole(t, f.s, f.member, f.member+"@test.com", f.projB, store.ProjectRoleMember)
	assert.Contains(t, starredConversationRowKeys(f.rail(t, f.member).Items), f.topicB)
}

func TestChatStarredConversations_ProjectScope(t *testing.T) {
	f := newStarredRailFixture(t, true)
	assert.ElementsMatch(t, []string{f.topicA, f.topicA2, f.dms.agentKey},
		starredConversationRowKeys(f.rail(t, f.member, "projectId", f.projA).Items),
		"project A's threads and its agent's DM; never a DM between users")
	assert.Equal(t, http.StatusNotFound,
		f.session(t, f.member, http.MethodGet, starredConversationsURL("projectId", f.projB), nil).Code,
		"an unreadable project is answered as a missing one")
	assert.Equal(t, http.StatusNotFound,
		f.session(t, f.member, http.MethodGet, starredConversationsURL("projectId", uuid.NewString()), nil).Code)
	assert.Empty(t, f.rail(t, f.admin, "projectId", f.projB).Items, "the admin has no stars of its own")
}

func TestChatStarredConversations_TokenSelectors(t *testing.T) {
	f := newStarredRailFixture(t, true)
	get := func(key string, params ...string) *httptest.ResponseRecorder {
		return f.call(t, key, http.MethodGet, starredConversationsURL(params...), nil)
	}

	hub := f.mint(t, f.member, hubBoundary(), "inbox:read", "project:read", "agent:read")
	assert.ElementsMatch(t, []string{f.topicA, f.topicA2, f.dms.agentKey, f.dms.humanKey},
		starredConversationRowKeys(decodeStarredConversations(t, get(hub)).Items))

	projA := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "project:read", "agent:read")
	assert.ElementsMatch(t, []string{f.topicA, f.topicA2, f.dms.agentKey},
		starredConversationRowKeys(decodeStarredConversations(t, get(projA)).Items),
		"a project token keeps its project's threads and agent DMs; a DM between users needs a hub boundary")

	noAgentRead := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "project:read")
	assert.ElementsMatch(t, []string{f.topicA, f.topicA2},
		starredConversationRowKeys(decodeStarredConversations(t, get(noAgentRead)).Items),
		"an agent DM needs agent:read on the agent")

	// A project-boundary token can be minted only for a project its holder
	// belongs to: give the member a role in project B, so the boundary is
	// the only thing that keeps project A's rows out. The member's retained
	// star in project B's thread is then readable, inside that boundary.
	createTestUserWithProjectRole(t, f.s, f.member, f.member+"@test.com", f.projB, store.ProjectRoleMember)
	projB := f.mint(t, f.member, projectBoundary(f.projB), "inbox:read", "project:read", "agent:read")
	assert.Equal(t, []string{f.topicB}, starredConversationRowKeys(decodeStarredConversations(t, get(projB)).Items),
		"another project's token sees only that project's rows, and no refusal")
	assert.Equal(t, http.StatusNotFound, get(projB, "projectId", f.projA).Code, "a project scope outside the boundary")

	writeOnly := f.mint(t, f.member, hubBoundary(), "inbox:write", "project:read")
	assert.Equal(t, http.StatusForbidden, get(writeOnly).Code, "reads need inbox:read")

	// A cursor minted for one credential does not resume under another.
	page := decodeStarredConversations(t, get(hub, "limit", "1"))
	require.NotEmpty(t, page.NextCursor)
	assert.Equal(t, http.StatusOK, get(hub, "limit", "1", "cursor", page.NextCursor).Code, "it resumes under its own credential")
	assert.Equal(t, http.StatusBadRequest, get(projA, "limit", "1", "cursor", page.NextCursor).Code)
	assert.Equal(t, http.StatusBadRequest,
		f.session(t, f.member, http.MethodGet, starredConversationsURL("cursor", page.NextCursor), nil).Code)
}

// TestChatStarredConversations_MatchesStarScope checks that the batched
// decisions agree with resolveStarScope, the rule every other star route
// applies, for each conversation and caller. The read switch is off, so
// resolveStarScope decides access only (the rail lists a conversation by
// its native key and does not resolve the history selector; the center
// does).
func TestChatStarredConversations_MatchesStarScope(t *testing.T) {
	f := newStarredRailFixture(t, false)
	ctx := context.Background()
	// The owner stars in both A threads; the admin in A and B; plain users
	// hold retained associations they cannot read.
	plain := f.plainUser(t)
	for _, u := range []string{f.owner, f.admin, plain} {
		for _, topic := range []string{f.topicA, f.topicB} {
			projectID := f.projA
			if topic == f.topicB {
				projectID = f.projB
			}
			m := f.threadMsg(t, topic, projectID, "m", time.Now().Add(-time.Minute))
			_, _, err := f.stars.AddMessageStar(ctx, u, StarTarget{MessageID: m, ProjectID: projectID,
				Selector: StarSelector{ConversationKey: topic, Channel: "web", ThreadID: topic}})
			require.NoError(t, err)
		}
	}
	// A legacy star of the member under the human DM's key with its slots
	// swapped (not canonical; stored directly, as the star routes refuse
	// it): resolveStarScope refuses the key, so the rail must not list it.
	p := strings.Split(f.dms.humanKey, ":")
	swapped := strings.Join([]string{p[0], p[3], p[4], p[1], p[2]}, ":")
	require.NotEqual(t, f.dms.humanKey, swapped)
	legacy := f.dmMsg(t, swapped, f.dms.humanConv, uuid.Nil.String(), "user:"+f.member, f.member, "legacy", "web", time.Now().Add(-time.Minute))
	_, _, err := f.stars.AddMessageStar(ctx, f.member, StarTarget{MessageID: legacy,
		Selector: StarSelector{ConversationKey: swapped, Channel: "web", ThreadID: swapped}})
	require.NoError(t, err)

	keys := []string{f.topicA, f.topicA2, f.topicB, f.dms.agentKey, f.dms.humanKey, swapped}
	for _, u := range []string{f.member, f.owner, f.admin, plain} {
		rows := map[string]bool{}
		for _, it := range f.rail(t, u).Items {
			rows[it.ConversationKey] = true
		}
		user, err := f.s.GetUser(ctx, u)
		require.NoError(t, err)
		for _, key := range keys {
			page, err := f.stars.ListMessageStarCandidates(ctx, StarFilter{UserID: u, Selector: StarSelector{ConversationKey: key, Channel: "web", ThreadID: key}, Limit: 1})
			require.NoError(t, err)
			starred := len(page.Items) > 0
			identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeWeb))
			req := httptest.NewRequest(http.MethodGet, starredConversationsPath, nil).WithContext(contextWithIdentity(ctx, identity))
			call := starCall{user: identity, wcs: f.wcs, stars: f.stars}
			_, refusal, rerr := f.srv.resolveStarScope(req, call, permInboxRead, key)
			readable := refusal == "" && rerr == nil
			assert.Equal(t, starred && readable, rows[key], "user %s conversation %s (starred %t readable %t)", u, key, starred, readable)
		}
	}
}

// TestChatStarredConversations_MatchesStarScopeForTokens checks the batched
// decisions against resolveStarScope for user access tokens, whose checks
// go through other entry points (the token's credential, the self-scope
// check with its membership cache, agent:read): for each token and every
// conversation key (threads of both projects, both DM kinds), the rail
// lists the conversation exactly when the caller has a live star in it and
// the per-conversation star list (resolveStarScope with the same token)
// answers it. The read switch is off, as in the session parity test.
func TestChatStarredConversations_MatchesStarScopeForTokens(t *testing.T) {
	f := newStarredRailFixture(t, false)
	ctx := context.Background()
	// The owner, a participant of the member's human DM, stars in both A
	// threads, project B's thread and that DM, then loses project A.
	for _, c := range []struct{ key, projectID string }{
		{f.topicA, f.projA}, {f.topicA2, f.projA}, {f.topicB, f.projB}, {f.dms.humanKey, ""},
	} {
		var m string
		if c.projectID == "" {
			m = f.dmMsg(t, c.key, f.dms.humanConv, uuid.Nil.String(), "user:"+f.owner, f.owner, "o", "web", time.Now().Add(-time.Minute))
		} else {
			m = f.threadMsg(t, c.key, c.projectID, "o", time.Now().Add(-time.Minute))
		}
		_, _, err := f.stars.AddMessageStar(ctx, f.owner, StarTarget{MessageID: m, ProjectID: c.projectID,
			Selector: StarSelector{ConversationKey: c.key, Channel: "web", ThreadID: c.key}})
		require.NoError(t, err)
	}
	// A member of both projects, starring in a project A thread and project
	// B's thread: its project B token is refused project A's row by the
	// boundary alone (a project-boundary token can be minted only for a
	// project its holder belongs to).
	both := uuid.NewString()
	createTestUserWithProjectRole(t, f.s, both, both+"@test.com", f.projA, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, f.s, both, both+"@test.com", f.projB, store.ProjectRoleMember)
	ensureHubMembership(ctx, f.s, both)
	for _, c := range []struct{ key, projectID string }{{f.topicA, f.projA}, {f.topicB, f.projB}} {
		m := f.threadMsg(t, c.key, c.projectID, "both", time.Now().Add(-time.Minute))
		_, _, err := f.stars.AddMessageStar(ctx, both, StarTarget{MessageID: m, ProjectID: c.projectID,
			Selector: StarSelector{ConversationKey: c.key, Channel: "web", ThreadID: c.key}})
		require.NoError(t, err)
	}
	type caller struct {
		name, user, token string
	}
	callers := []caller{
		{"member hub token", f.member, f.mint(t, f.member, hubBoundary(), "inbox:read", "project:read", "agent:read")},
		{"member project A token", f.member, f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "project:read", "agent:read")},
		{"member project A token without agent:read", f.member, f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "project:read")},
		{"member of A and B, project B token", both, f.mint(t, both, projectBoundary(f.projB), "inbox:read", "project:read", "agent:read")},
		{"owner project A token, owner's project A binding removed", f.owner, f.mint(t, f.owner, projectBoundary(f.projA), "inbox:read", "project:read", "agent:read")},
	}
	// Remove only the owner's project A binding (the owner keeps hub
	// membership): its project-A token is then refused at request level.
	bindings, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.owner)
	require.NoError(t, err)
	removed := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.projA {
			require.NoError(t, f.s.DeleteRoleBinding(ctx, b.ID))
			removed++
		}
	}
	require.Equal(t, 1, removed, "the owner's project A binding")

	keys := []string{f.topicA, f.topicA2, f.topicB, f.dms.agentKey, f.dms.humanKey}
	for _, c := range callers {
		rec := f.call(t, c.token, http.MethodGet, starredConversationsPath, nil)
		if rec.Code == http.StatusForbidden {
			// The request-level token check (a project token whose holder is no
			// longer a member) refuses both routes alike, before any lookup.
			for _, key := range keys {
				assert.Equal(t, http.StatusForbidden, f.call(t, c.token, http.MethodGet, starListPath(key), nil).Code, "%s %s", c.name, key)
			}
			continue
		}
		rows := map[string]bool{}
		for _, it := range decodeStarredConversations(t, rec).Items {
			rows[it.ConversationKey] = true
		}
		if c.name == "member hub token" {
			// Positive control: a refusal everywhere would make every
			// comparison trivially equal.
			assert.Len(t, rows, 4, "the member's hub token lists its four readable starred conversations")
		}
		if c.user == both {
			assert.Equal(t, map[string]bool{f.topicB: true}, rows, "the project B token lists project B's thread, not project A's")
		}
		for _, key := range keys {
			page, err := f.stars.ListMessageStarCandidates(ctx, StarFilter{UserID: c.user,
				Selector: StarSelector{ConversationKey: key, Channel: "web", ThreadID: key}, Limit: 1})
			require.NoError(t, err)
			starred := len(page.Items) > 0
			list := f.call(t, c.token, http.MethodGet, starListPath(key), nil)
			require.Contains(t, []int{http.StatusOK, http.StatusNotFound}, list.Code, "%s %s: %s", c.name, key, list.Body.String())
			readable := list.Code == http.StatusOK
			assert.Equal(t, starred && readable, rows[key], "%s, conversation %s (starred %t, readable %t)", c.name, key, starred, readable)
		}
	}
}

// TestChatStarredConversations_DriftedRowAgreesWithStarList pins that the
// rail selects a conversation's messages exactly as the star list does,
// under both read-switch settings: a row whose thread ID is the key but
// whose conversation ID is another conversation's counts only with the
// switch off, in the rail and in GET /api/v1/chat/stars alike.
func TestChatStarredConversations_DriftedRowAgreesWithStarList(t *testing.T) {
	for _, readSwitch := range []bool{true, false} {
		t.Run(fmt.Sprintf("readSwitch=%t", readSwitch), func(t *testing.T) {
			f := newStarredRailFixture(t, readSwitch)
			ctx := context.Background()
			drifted := f.topic(t, f.projA)
			m := uuid.NewString()
			require.NoError(t, f.s.CreateMessage(ctx, &store.Message{
				ID: m, ProjectID: f.projA, Sender: "user:" + f.member + "@test.com", SenderID: f.member, Recipient: "chat",
				Msg: "drifted", Type: "chat", Channel: "web", ThreadID: drifted, ConversationID: uuid.NewString(),
				CreatedAt: time.Now().UTC().Add(-time.Minute),
			}))
			_, err := f.db.Exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES (?, ?, ?, ?, ?)`,
				f.member, m, drifted, f.projA, sqliteStarTime(time.Now()))
			require.NoError(t, err)

			listed := len(decodeStarPage(t, f.session(t, f.member, http.MethodGet, starListPath(drifted), nil)).Items) > 0
			assert.Equal(t, !readSwitch, listed, "the star list counts the row only with the switch off")
			assert.Equal(t, listed, contains(starredConversationRowKeys(f.rail(t, f.member).Items), drifted),
				"the rail agrees with the star list")
			// The fixture's ordinary rows count under both settings.
			assert.Subset(t, starredConversationRowKeys(f.rail(t, f.member).Items),
				[]string{f.topicA, f.topicA2, f.dms.agentKey, f.dms.humanKey})
		})
	}
}

// TestChatStarredConversations_ScanBudget pins the bounded scan: hidden
// candidates use the budget, a short or empty page then carries a cursor,
// and resuming reaches the readable rows after them.
func TestChatStarredConversations_ScanBudget(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	// Five unreadable (project B) threads sort before two readable ones.
	var hidden, visible []string
	for i := 0; i < 7; i++ {
		projectID := f.projB
		if i >= 5 {
			projectID = f.projA
		}
		key := f.topic(t, projectID)
		at := time.Now().UTC().Add(-time.Duration(i) * time.Minute)
		_, err := f.db.Exec(`UPDATE webchat_topic SET last_activity_at = ? WHERE id = ?`, at.Format(time.RFC3339Nano), key)
		require.NoError(t, err)
		m := f.threadMsg(t, key, projectID, "m", at)
		_, _, err = f.stars.AddMessageStar(ctx, f.member, StarTarget{MessageID: m, ProjectID: projectID,
			Selector: StarSelector{ConversationKey: key, Channel: "web", ThreadID: key}})
		require.NoError(t, err)
		if i < 5 {
			hidden = append(hidden, key)
		} else {
			visible = append(visible, key)
		}
	}
	user, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeWeb))
	req := httptest.NewRequest(http.MethodGet, starredConversationsPath, nil).WithContext(contextWithIdentity(ctx, identity))
	call := starCall{user: identity, wcs: f.wcs, stars: f.stars}
	filter := StarredConversationFilter{UserID: f.member, Channel: "web", Sort: StarredConversationsByActivity}

	scan := func(after *StarredConversationPosition) ([]starredConversationItem, *StarredConversationPosition) {
		t.Helper()
		items, next, err := f.srv.scanStarredConversations(ctx, call, f.srv.newStarredConversationAuthorizer(req, call), filter, after, 10, 2, 3)
		require.NoError(t, err)
		return items, next
	}
	items, next := scan(nil)
	assert.Empty(t, items, "three hidden candidates spent the budget")
	require.NotNil(t, next)
	assert.Equal(t, hidden[2], next.Key, "the cursor is past the scanned candidates")
	items, next = scan(next)
	assert.Equal(t, []string{visible[0]}, starredConversationRowKeys(items))
	require.NotNil(t, next)
	items, next = scan(next)
	assert.Equal(t, []string{visible[1]}, starredConversationRowKeys(items))
	assert.Nil(t, next, "exhausted")

	// A full page stops at the item that fills it.
	items, next, err = f.srv.scanStarredConversations(ctx, call, f.srv.newStarredConversationAuthorizer(req, call), filter, nil, 1, 100, 1000)
	require.NoError(t, err)
	assert.Equal(t, []string{visible[0]}, starredConversationRowKeys(items))
	require.NotNil(t, next)
	assert.Equal(t, visible[0], next.Key)
}

// starRailCounter counts the lookups the Starred rail makes.
type starRailCounter struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *starRailCounter) inc(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[name]++
}

func (c *starRailCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = map[string]int{}
}

func (c *starRailCounter) get() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int{}
	for k, v := range c.calls {
		out[k] = v
	}
	return out
}

// starRailCountingStore counts the per-conversation lookups the rail must
// not make and the batched lookups it makes instead.
type starRailCountingStore struct {
	store.Store
	c *starRailCounter
}

func (s *starRailCountingStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	s.c.inc("GetProject")
	return s.Store.GetProject(ctx, id)
}

func (s *starRailCountingStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	s.c.inc("GetAgent")
	return s.Store.GetAgent(ctx, id)
}

func (s *starRailCountingStore) ListProjectSummaries(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	s.c.inc("ListProjectSummaries")
	return s.Store.ListProjectSummaries(ctx, f, o)
}

func (s *starRailCountingStore) GetAgentsByIDsIncludingDeleted(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	s.c.inc("GetAgentsByIDsIncludingDeleted")
	return s.Store.GetAgentsByIDsIncludingDeleted(ctx, ids)
}

func (s *starRailCountingStore) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*store.User, error) {
	s.c.inc("GetUsersByIDs")
	return s.Store.GetUsersByIDs(ctx, ids)
}

// starRailCountingWebChatStore counts topic lookups and candidate queries.
type starRailCountingWebChatStore struct {
	WebChatStore
	MessageStarStore
	c *starRailCounter
}

func (s *starRailCountingWebChatStore) GetTopic(ctx context.Context, id string) (*WebChatTopic, error) {
	s.c.inc("GetTopic")
	return s.WebChatStore.GetTopic(ctx, id)
}

func (s *starRailCountingWebChatStore) ListStarredConversationCandidates(ctx context.Context, f StarredConversationFilter) (StarredConversationPage, error) {
	s.c.inc("ListStarredConversationCandidates")
	return s.MessageStarStore.ListStarredConversationCandidates(ctx, f)
}

// TestChatStarredConversations_BatchedLookups checks that the number of
// lookups does not grow with the number of conversations: one candidate
// query, and one project, agent and user lookup per batch.
func TestChatStarredConversations_BatchedLookups(t *testing.T) {
	counter := &starRailCounter{calls: map[string]int{}}
	// Installed before any setup (never reassigned mid-test); setup writes
	// through the unwrapped store.
	srv, s, _, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *starRailCountingStore {
		return &starRailCountingStore{Store: inner, c: counter}
	})
	ctx := context.Background()
	inbox := &inboxFixture{srv: srv, s: s, projA: uuid.NewString(), admin: uuid.NewString(), member: uuid.NewString()}
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: inbox.projA, Name: "Rail A", Slug: "rail-a-" + inbox.projA[:8]}))
	createTestUserWithProjectRole(t, s, inbox.member, inbox.member+"@test.com", inbox.projA, store.ProjectRoleMember)
	ensureHubMembership(ctx, s, inbox.member)
	inbox.agentA = &store.Agent{ID: uuid.NewString(), Slug: "rail-agent", Name: "Rail Agent", ProjectID: inbox.projA}
	require.NoError(t, s.CreateAgent(ctx, inbox.agentA))
	db, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(db.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	f := &starFixture{chatFixture: &chatFixture{inboxFixture: inbox, wcs: wcs}, stars: messageStarStoreFrom(wcs)}
	require.NotNil(t, f.stars)
	setStarsExperiment(t, srv, true, false)
	srv.SetWebChatStore(&starRailCountingWebChatStore{WebChatStore: wcs, MessageStarStore: f.stars, c: counter})

	rail := func() map[string]int {
		t.Helper()
		counter.reset()
		page := decodeStarredConversations(t, f.session(t, f.member, http.MethodGet, starredConversationsPath, nil))
		require.NotEmpty(t, page.Items)
		return counter.get()
	}
	addConversations := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			key := f.topic(t, f.projA)
			m := f.threadMsg(t, key, f.projA, "m", time.Now().Add(-time.Minute))
			_, _, err := f.stars.AddMessageStar(ctx, f.member, StarTarget{MessageID: m, ProjectID: f.projA,
				Selector: StarSelector{ConversationKey: key, Channel: "web", ThreadID: key}})
			require.NoError(t, err)
			dmKey := f.dm(t, f.member, "user", f.plainUser(t))
			dm := f.dmMsg(t, dmKey, "", uuid.Nil.String(), "user:"+f.member, f.member, "d", "web", time.Now().Add(-time.Minute))
			_, _, err = f.stars.AddMessageStar(ctx, f.member, StarTarget{MessageID: dm,
				Selector: StarSelector{ConversationKey: dmKey, Channel: "web", ThreadID: dmKey}})
			require.NoError(t, err)
		}
		key := f.dm(t, f.member, "agent", f.agentA.ID)
		m := f.dmMsg(t, key, "", f.projA, "user:"+f.member, f.member, "a", "web", time.Now().Add(-time.Minute))
		_, _, err := f.stars.AddMessageStar(ctx, f.member, StarTarget{MessageID: m, ProjectID: f.projA,
			Selector: StarSelector{ConversationKey: key, Channel: "web", ThreadID: key}})
		require.NoError(t, err)
	}
	addConversations(2)
	small := rail()
	addConversations(8)
	large := rail()
	assert.Equal(t, small, large, "lookups do not grow with the conversations")
	assert.Equal(t, map[string]int{
		"ListStarredConversationCandidates": 1,
		"ListProjectSummaries":              1,
		"GetAgentsByIDsIncludingDeleted":    1,
		"GetUsersByIDs":                     1,
	}, large, "no per-conversation topic, project or agent lookup")
}
