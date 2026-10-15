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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Downloads of AttachmentStore files with no project by a user access
// token: the token reads a file no more widely than the message it is
// attached to, and a file sent nowhere yet only as its uploader, with
// inbox:read on a hub boundary. Every refusal gets the answer of an unknown
// id.

// legacyTokenCase is one token of the file's holder alice (ua).
type legacyTokenCase struct {
	name  string
	token *ScopedUserIdentity
}

// legacyLinkedFile stores a file with no project uploaded by uploader,
// seeds a message on thread (in conversation convID, "" for none) and links
// the file to it. It returns the file and message IDs.
func (f *refFixture) legacyLinkedFile(t *testing.T, uploader, projectID, thread, convID, name string) (string, string) {
	t.Helper()
	file := f.attach(t, "", uploader, name)
	msg := f.seedMessage(t, projectID, thread, convID, "carries "+name)
	require.NoError(t, f.wcs.LinkAttachmentToMessage(context.Background(), msg, file))
	return file, msg
}

// legacyConversation stores a conversation of alice (ua) with the given
// participants and returns it.
func (f *refFixture) legacyConversation(t *testing.T, kind, externalRef string, participants ...[2]string) *store.Conversation {
	t.Helper()
	ctx := context.Background()
	conv, err := f.st.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: kind, Surface: "native", ExternalRef: externalRef, DriftState: "active",
	})
	require.NoError(t, err)
	for _, p := range participants {
		require.NoError(t, f.st.AddParticipant(ctx, &store.ConversationParticipant{ID: uuid.NewString(),
			ConversationID: conv.ID, PrincipalKind: p[0], PrincipalID: p[1], Role: "member", JoinedAt: time.Now()}))
	}
	return conv
}

// convRequest serves a GET on the conversation routes for identity directly.
func (f *refFixture) convRequest(t *testing.T, identity Identity, path string) refAnswer {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	f.srv.handleConversationRoutes(rec, req.WithContext(contextWithIdentity(req.Context(), identity)))
	return refAnswer{status: rec.Code, body: rec.Body.String()}
}

// requireLegacyAnswers asserts that each token in granted downloads id with
// status 200 and that each token in refused gets that token's answer for an
// unknown id.
func (f *refFixture) requireLegacyAnswers(t *testing.T, id string, granted, refused []legacyTokenCase) {
	t.Helper()
	for _, tk := range granted {
		got := f.downloadAs(t, tk.token, id)
		assert.Equal(t, http.StatusOK, got.status, "%s must download the file: %s", tk.name, got.body)
	}
	for _, tk := range refused {
		missing := f.downloadAs(t, tk.token, uuid.NewString())
		require.Equal(t, http.StatusNotFound, missing.status, missing.body)
		got := f.downloadAs(t, tk.token, id)
		assert.Equal(t, missing, got, "%s must get the answer of an unknown id", tk.name)
	}
}

// A file in alice's DM with agent aa (by thread key) downloads for a token
// that reads that DM: inbox:read for the agent's project and agent:read on
// the agent.
func TestLegacyAttachmentDownloadToken_AgentDMFollowsConversationRead(t *testing.T) {
	f := newRefFixture(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	file, _ := f.legacyLinkedFile(t, f.aa.ID, f.projA.ID, dm, "", "agent-dm.txt")

	f.requireLegacyAnswers(t, file,
		[]legacyTokenCase{
			{"hub token with inbox:read and agent:read", selfToken(t, f.ua.ID, hubBoundary(), "inbox:read", "agent:read")},
			{"project A token with inbox:read and agent:read", selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "inbox:read", "agent:read")},
		},
		[]legacyTokenCase{
			{"token without inbox:read", selfToken(t, f.ua.ID, hubBoundary(), "agent:read", "project:read")},
			{"token without agent:read", selfToken(t, f.ua.ID, hubBoundary(), "inbox:read")},
			{"project B token", selfToken(t, f.ua.ID, projectBoundary(f.projB.ID), "inbox:read", "agent:read")},
			{"token of a user outside the DM", selfToken(t, f.uc.ID, hubBoundary(), "inbox:read", "agent:read")},
		})
	assert.Equal(t, http.StatusOK, f.download(t, f.ua, file).status, "the session is unchanged")
}

// A file in alice's DM with carol (by thread key) downloads only for a
// token with inbox:read on a hub boundary: the DM has no project.
func TestLegacyAttachmentDownloadToken_UserDMNeedsHubBoundary(t *testing.T) {
	f := newRefFixture(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	file, _ := f.legacyLinkedFile(t, f.uc.ID, uuid.Nil.String(), dm, "", "user-dm.txt")

	f.requireLegacyAnswers(t, file,
		[]legacyTokenCase{
			{"hub token with inbox:read", selfToken(t, f.ua.ID, hubBoundary(), "inbox:read")},
		},
		[]legacyTokenCase{
			{"project A token", selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "inbox:read", "agent:read", "project:read")},
			{"hub token without inbox:read", selfToken(t, f.ua.ID, hubBoundary(), "project:read")},
		})
	assert.Equal(t, http.StatusOK, f.download(t, f.ua, file).status, "the session is unchanged")
}

// A file sent nowhere yet downloads for its uploader's token only with
// inbox:read on a hub boundary, the boundary a project-less upload needs;
// every other caller but the uploader's session is refused.
func TestLegacyAttachmentDownloadToken_UnsentUploadOnlyForUploaderHubInbox(t *testing.T) {
	f := newRefFixture(t)
	file := f.attach(t, "", f.ua.ID, "draft.txt")

	f.requireLegacyAnswers(t, file,
		[]legacyTokenCase{
			{"uploader's hub token with inbox:read", selfToken(t, f.ua.ID, hubBoundary(), "inbox:read")},
		},
		[]legacyTokenCase{
			{"uploader's token without inbox:read", selfToken(t, f.ua.ID, hubBoundary(), "agent:read", "project:read")},
			{"uploader's project A token", selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "inbox:read")},
			{"another user's hub token", selfToken(t, f.uc.ID, hubBoundary(), "inbox:read")},
		})
	assert.Equal(t, http.StatusOK, f.download(t, f.ua, file).status, "the uploader's session reads it")
	requireSameAnswer(t, f.download(t, f.uc, uuid.NewString()), f.download(t, f.uc, file))
}

// Once sent, a file downloads for its uploader's token only through the
// message it rides on: a DM with an agent still needs agent:read.
func TestLegacyAttachmentDownloadToken_SentUploadFollowsMessageForUploader(t *testing.T) {
	f := newRefFixture(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	file, _ := f.legacyLinkedFile(t, f.ua.ID, f.projA.ID, dm, "", "sent.txt")

	f.requireLegacyAnswers(t, file,
		[]legacyTokenCase{
			{"uploader's token that reads the DM", selfToken(t, f.ua.ID, hubBoundary(), "inbox:read", "agent:read")},
		},
		[]legacyTokenCase{
			{"uploader's token without agent:read", selfToken(t, f.ua.ID, hubBoundary(), "inbox:read")},
		})
	assert.Equal(t, http.StatusOK, f.download(t, f.ua, file).status, "the uploader's session reads it")
}

// legacyGetAgentFault fails GetAgent while fail is set.
type legacyGetAgentFault struct {
	store.Store
	fail bool
}

func (s *legacyGetAgentFault) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.fail {
		return nil, errRefStoreFault
	}
	return s.Store.GetAgent(ctx, id)
}

// A lookup the token check makes that fails refuses the download with the
// answer of an unknown id.
func TestLegacyAttachmentDownloadToken_LookupErrorAnswersAsUnknown(t *testing.T) {
	reader := func(t *testing.T, f *refFixture) *ScopedUserIdentity {
		return selfToken(t, f.ua.ID, hubBoundary(), "inbox:read", "agent:read")
	}

	t.Run("peer agent lookup", func(t *testing.T) {
		f := newRefFixture(t)
		dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
		file, _ := f.legacyLinkedFile(t, f.aa.ID, f.projA.ID, dm, "", "agent-dm.txt")
		tok := reader(t, f)
		require.Equal(t, http.StatusOK, f.downloadAs(t, tok, file).status, "readable while the store works")

		missing := f.downloadAs(t, tok, uuid.NewString())
		faulty := &legacyGetAgentFault{Store: f.srv.store}
		f.srv.store = faulty
		faulty.fail = true
		assert.Equal(t, missing, f.downloadAs(t, tok, file))
	})

	t.Run("conversation lookup", func(t *testing.T) {
		f := newRefFixture(t)
		dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
		conv := f.legacyConversation(t, "direct", dm, [2]string{"user", f.ua.ID}, [2]string{"agent", f.aa.ID})
		file, _ := f.legacyLinkedFile(t, f.aa.ID, f.projA.ID, dm, conv.ID, "agent-dm.txt")
		tok := reader(t, f)
		require.Equal(t, http.StatusOK, f.downloadAs(t, tok, file).status, "readable while the store works")

		missing := f.downloadAs(t, tok, uuid.NewString())
		f.faults.failGetConversation = true
		f.fault.Arm()
		assert.Equal(t, missing, f.downloadAs(t, tok, file))
	})

	t.Run("linked message list", func(t *testing.T) {
		f := newRefFixture(t)
		file := f.attach(t, "", f.ua.ID, "draft.txt")
		tok := selfToken(t, f.ua.ID, hubBoundary(), "inbox:read")
		require.Equal(t, http.StatusOK, f.downloadAs(t, tok, file).status, "readable while the store works")

		missing := f.downloadAs(t, tok, uuid.NewString())
		f.srv.SetWebChatStore(listLinksFaultWCS{WebChatStore: f.wcs})
		assert.Equal(t, missing, f.downloadAs(t, tok, file))
	})
}

// A token downloads a file attached to a message exactly when it may read
// that message on its own route: the conversation message route for a
// message in a conversation, the chat history route for one found by
// thread key. The two decisions share one token rule and must not drift.
func TestLegacyAttachmentDownloadToken_MatchesMessageRead(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()

	agentDM := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	userDM := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	otherUserDM := dmKeyFor(t, "user", f.ua.ID, "user", f.ub.ID)
	agentConv := f.legacyConversation(t, "direct", agentDM, [2]string{"user", f.ua.ID}, [2]string{"agent", f.aa.ID})
	userConv := f.legacyConversation(t, "direct", userDM, [2]string{"user", f.ua.ID}, [2]string{"user", f.uc.ID})
	groupNone := f.legacyConversation(t, "group", "group:none:"+uuid.NewString(), [2]string{"user", f.ua.ID})
	topicConvID, err := f.srv.conversationIDForKey(ctx, f.wcs, f.topicA)
	require.NoError(t, err)
	require.NotEmpty(t, topicConvID)

	type messageCase struct {
		name  string
		file  string
		route string // GET path that reads the message the file rides on
		chat  bool   // route is a chat route (wrapped by chatRoute)
	}
	var cases []messageCase
	inConversation := func(name, projectID, thread, convID string) {
		file, msg := f.legacyLinkedFile(t, f.uc.ID, projectID, thread, convID, name+".txt")
		cases = append(cases, messageCase{name: name, file: file,
			route: "/api/v1/conversations/" + convID + "/messages/" + msg})
	}
	byThread := func(name, projectID, thread string) {
		file, _ := f.legacyLinkedFile(t, f.uc.ID, projectID, thread, "", name+".txt")
		cases = append(cases, messageCase{name: name, file: file,
			route: "/api/v1/chat/conversations/" + thread + "/messages", chat: true})
	}
	inConversation("agent DM conversation", f.projA.ID, agentDM, agentConv.ID)
	inConversation("user DM conversation", uuid.Nil.String(), userDM, userConv.ID)
	inConversation("group without a project", uuid.Nil.String(), "group-thread-"+groupNone.ID, groupNone.ID)
	inConversation("topic conversation", f.projA.ID, f.topicA, topicConvID)
	byThread("agent DM by thread key", f.projA.ID, agentDM)
	byThread("user DM by thread key", uuid.Nil.String(), otherUserDM)
	byThread("topic by thread key", f.projA.ID, f.topicA)

	tokens := []legacyTokenCase{
		{"hub: inbox:read agent:read project:read", selfToken(t, f.ua.ID, hubBoundary(), "inbox:read", "agent:read", "project:read")},
		{"hub: inbox:read", selfToken(t, f.ua.ID, hubBoundary(), "inbox:read")},
		{"hub: agent:read project:read", selfToken(t, f.ua.ID, hubBoundary(), "agent:read", "project:read")},
		{"hub: project:read", selfToken(t, f.ua.ID, hubBoundary(), "project:read")},
		{"project A: inbox:read agent:read project:read", selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "inbox:read", "agent:read", "project:read")},
		{"project A: project:read", selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read")},
		{"project B: inbox:read agent:read project:read", selfToken(t, f.ua.ID, projectBoundary(f.projB.ID), "inbox:read", "agent:read", "project:read")},
	}

	for _, c := range cases {
		granted, refused := 0, 0
		for _, tk := range tokens {
			var read refAnswer
			if c.chat {
				read = f.tokenRequest(t, tk.token, http.MethodGet, c.route, nil)
			} else {
				read = f.convRequest(t, tk.token, c.route)
			}
			download := f.downloadAs(t, tk.token, c.file)
			label := fmt.Sprintf("%s, %s: message read %d, download %d", c.name, tk.name, read.status, download.status)
			assert.Equal(t, read.status == http.StatusOK, download.status == http.StatusOK, label)
			if download.status == http.StatusOK {
				granted++
			} else {
				refused++
				assert.Equal(t, http.StatusNotFound, download.status, label)
			}
		}
		assert.Positive(t, granted, "%s: some token must read the message", c.name)
		assert.Positive(t, refused, "%s: some token must be refused", c.name)
	}
}
