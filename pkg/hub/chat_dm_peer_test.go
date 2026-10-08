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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// Tests for authorizeDMPeer: the participant of a DM key that is not the
// sender must be a principal the sender may message. The rule sits in
// authorizeChatSend, so the HTTP send route and sendChatMessage (the
// request-less entry point the scheduled sender uses at fire time) both
// enforce it. Scheduled create still refuses DM keys before any access
// check (TestScheduledSend_DMRejected).

type dmPeerEnv struct {
	srv     *Server
	s       store.Store
	wcs     WebChatStore
	alice   *store.User // hub member, owner of project
	bob     *store.User // hub member, not in project
	project *store.Project
}

func setupDMPeerEnv(t *testing.T) *dmPeerEnv {
	t.Helper()
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	db := openTestMemorySQLite(t, "sqlite3")
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	return &dmPeerEnv{srv: srv, s: s, wcs: wcs, alice: alice, bob: bob, project: project}
}

func (e *dmPeerEnv) createUser(t *testing.T, name, status string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          tid("dm-peer-" + name),
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      status,
		Created:     time.Now(),
	}
	require.NoError(t, e.s.CreateUser(context.Background(), u))
	ensureHubMembership(context.Background(), e.s, u.ID)
	return u
}

func (e *dmPeerEnv) createAgent(t *testing.T, name, mode string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:          tid("dm-peer-agent-" + name),
		ProjectID:   e.project.ID,
		Name:        name,
		Slug:        name,
		Phase:       "idle",
		OwnerID:     e.alice.ID,
		CreatedBy:   e.alice.ID,
		MessageMode: mode,
	}
	require.NoError(t, e.s.CreateAgent(context.Background(), a))
	return a
}

func userDMKey(t *testing.T, a, b string) string {
	t.Helper()
	key, err := messages.DMConversationKey("user", a, "user", b)
	require.NoError(t, err)
	return key
}

func agentDMKey(t *testing.T, agentID, userID string) string {
	t.Helper()
	key, err := messages.DMConversationKey("agent", agentID, "user", userID)
	require.NoError(t, err)
	return key
}

func (e *dmPeerEnv) send(t *testing.T, as *store.User, key string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, e.srv, as, http.MethodPost,
		"/api/v1/chat/conversations/"+key+"/messages", map[string]string{"content": "hello"})
}

// sendDirect calls sendChatMessage without a request, the way the
// scheduled sender does at fire time.
func (e *dmPeerEnv) sendDirect(t *testing.T, as *store.User, key string) *httptest.ResponseRecorder {
	t.Helper()
	user := NewAuthenticatedUser(as.ID, as.Email, as.DisplayName, as.Role, scheduledSendClientType)
	rr := httptest.NewRecorder()
	writeChatSendOutcome(rr)(e.srv.sendChatMessage(context.Background(), user, key, chatSendInput{Content: "hello"}))
	return rr
}

// assertNothingStored checks that a refused send left no message and no
// DM registry row behind.
func (e *dmPeerEnv) assertNothingStored(t *testing.T, sender *store.User, key string) {
	t.Helper()
	ctx := context.Background()
	res, err := e.s.ListMessages(ctx, store.MessageFilter{ThreadID: key}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Empty(t, res.Items, "refused send must not persist a message")
	dms, err := e.wcs.ListDMs(ctx, sender.ID)
	require.NoError(t, err)
	for _, dm := range dms {
		require.NotEqual(t, key, dm.ConversationKey, "refused send must not register a DM")
	}
}

type refusal struct {
	code        int
	contentType string
	body        string
}

func refusalOf(rr *httptest.ResponseRecorder) refusal {
	return refusal{code: rr.Code, contentType: rr.Header().Get("Content-Type"), body: rr.Body.String()}
}

// nonParticipantRefusal is the existing response for a sender who is not
// one of the two participants of the key.
func (e *dmPeerEnv) nonParticipantRefusal(t *testing.T, send func(*testing.T, *store.User, string) *httptest.ResponseRecorder) refusal {
	t.Helper()
	key := userDMKey(t, e.bob.ID, tid("dm-peer-someone-else"))
	rr := send(t, e.alice, key)
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	return refusalOf(rr)
}

func TestChatDMPeer_InvalidPeersRefusedUniformly(t *testing.T) {
	for _, path := range []struct {
		name string
		send func(*dmPeerEnv) func(*testing.T, *store.User, string) *httptest.ResponseRecorder
	}{
		{"live_route", func(e *dmPeerEnv) func(*testing.T, *store.User, string) *httptest.ResponseRecorder { return e.send }},
		{"request_less", func(e *dmPeerEnv) func(*testing.T, *store.User, string) *httptest.ResponseRecorder {
			return e.sendDirect
		}},
	} {
		t.Run(path.name, func(t *testing.T) {
			e := setupDMPeerEnv(t)
			send := path.send(e)
			want := e.nonParticipantRefusal(t, send)

			suspended := e.createUser(t, "suspended", store.UserStatusSuspended)
			deleted := e.createUser(t, "deleted", store.UserStatusActive)
			require.NoError(t, e.s.DeleteUser(context.Background(), deleted.ID))
			noneMode := e.createAgent(t, "sealed", store.MessageModeNone)
			projectMode := e.createAgent(t, "project-agent", store.MessageModeProject)

			cases := []struct {
				name   string
				sender *store.User
				key    string
			}{
				{"nonexistent_user", e.alice, userDMKey(t, e.alice.ID, tid("dm-peer-nobody"))},
				{"deleted_user", e.alice, userDMKey(t, e.alice.ID, deleted.ID)},
				{"suspended_user", e.alice, userDMKey(t, e.alice.ID, suspended.ID)},
				{"nonexistent_agent", e.alice, agentDMKey(t, tid("dm-peer-no-agent"), e.alice.ID)},
				{"agent_not_messageable_not_readable", e.bob, agentDMKey(t, projectMode.ID, e.bob.ID)},
				{"sealed_agent_not_readable", e.bob, agentDMKey(t, noneMode.ID, e.bob.ID)},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					rr := send(t, tc.sender, tc.key)
					require.Equal(t, want, refusalOf(rr), "refusal must be identical to the non-participant refusal")
					e.assertNothingStored(t, tc.sender, tc.key)
				})
			}
		})
	}
}

func TestChatDMPeer_NonCanonicalKeyRejectedAsMalformed(t *testing.T) {
	e := setupDMPeerEnv(t)
	agent := e.createAgent(t, "order-agent", store.MessageModeProject)

	// The existing response for a key that fails the format check.
	malformed := e.send(t, e.alice, "dm:user:"+e.alice.ID+":user:not-a-uuid-not-a-uuid-not-a-uuid-xx")
	require.Equal(t, http.StatusBadRequest, malformed.Code, malformed.Body.String())

	lo, hi := e.alice.ID, e.bob.ID
	if lo > hi {
		lo, hi = hi, lo
	}
	cases := map[string]string{
		"user_slot_before_agent": "dm:user:" + e.alice.ID + ":agent:" + agent.ID,
		"users_unsorted":         "dm:user:" + hi + ":user:" + lo,
		"non_uuid_id":            "dm:user:" + e.alice.ID + ":user:" + "ffffffff-ffff-ffff-ffff-fffffffffff-",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			for _, rr := range []*httptest.ResponseRecorder{e.send(t, e.alice, key), e.sendDirect(t, e.alice, key)} {
				require.Equal(t, refusalOf(malformed), refusalOf(rr))
			}
			e.assertNothingStored(t, e.alice, key)
		})
	}
}

func TestChatDMPeer_ReadableAgentRefusalKeepsMessageDenied(t *testing.T) {
	e := setupDMPeerEnv(t)
	sealed := e.createAgent(t, "sealed-visible", store.MessageModeNone)
	key := agentDMKey(t, sealed.ID, e.alice.ID)

	for _, rr := range []*httptest.ResponseRecorder{e.send(t, e.alice, key), e.sendDirect(t, e.alice, key)} {
		require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
		var body struct {
			Error struct {
				Code    string                 `json:"code"`
				Message string                 `json:"message"`
				Details map[string]interface{} `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		require.Equal(t, ErrCodeMessageDenied, body.Error.Code)
		require.Equal(t, "Message delivery denied", body.Error.Message)
		require.Equal(t, "user", body.Error.Details["senderMode"])
		require.Equal(t, store.MessageModeNone, body.Error.Details["recipientMode"])
		require.NotEmpty(t, body.Error.Details["reason"])
	}
	e.assertNothingStored(t, e.alice, key)
}

// Valid DMs, including ones that already have history, keep working.
func TestChatDMPeer_ValidPeersStillSend(t *testing.T) {
	e := setupDMPeerEnv(t)
	agent := e.createAgent(t, "helper", store.MessageModeProject)
	invited := e.createUser(t, "invited", store.UserStatusInvited)

	cases := map[string]struct {
		sender *store.User
		key    string
	}{
		"user_user":         {e.alice, userDMKey(t, e.alice.ID, e.bob.ID)},
		"user_user_reverse": {e.bob, userDMKey(t, e.alice.ID, e.bob.ID)},
		"user_agent":        {e.alice, agentDMKey(t, agent.ID, e.alice.ID)},
		"invited_user":      {e.alice, userDMKey(t, e.alice.ID, invited.ID)},
		"self":              {e.alice, userDMKey(t, e.alice.ID, e.alice.ID)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// First message opens the DM; the second is a send into an
			// existing DM; the third uses the request-less path.
			for i := 0; i < 2; i++ {
				rr := e.send(t, tc.sender, tc.key)
				require.Equal(t, http.StatusCreated, rr.Code, "send %d: %s", i, rr.Body.String())
			}
			rr := e.sendDirect(t, tc.sender, tc.key)
			require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
		})
	}
}

// A user access token whose scopes do not include reading users cannot
// send a user-to-user DM: the peer is not readable under the token.
func TestChatDMPeer_ScopedTokenWithoutUserReadCannotSendUserDM(t *testing.T) {
	e := setupDMPeerEnv(t)
	want := e.nonParticipantRefusal(t, e.send)
	uat := mintScopedUAT(t, e.srv, e.alice.ID, e.project.ID, []string{"project:read"})
	key := userDMKey(t, e.alice.ID, e.bob.ID)
	rr := doRequestWithUAT(t, e.srv, uat, http.MethodPost,
		"/api/v1/chat/conversations/"+key+"/messages", map[string]string{"content": "hello"})
	require.Equal(t, want, refusalOf(rr))
	e.assertNothingStored(t, e.alice, key)
}

// mustCreateActiveUser creates an active user with ID tid(name), for tests
// that need a real DM peer.
func mustCreateActiveUser(t *testing.T, s store.Store, name string) string {
	t.Helper()
	u := &store.User{ID: tid(name), Email: name + "@example.com", DisplayName: name,
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u.ID
}
