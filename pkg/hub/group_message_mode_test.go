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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for message modes in group conversations (group_message_mode.go).

var groupModeTestModes = []string{
	store.MessageModeNone,
	store.MessageModeLineage,
	store.MessageModeBranch,
	store.MessageModeProject,
	store.MessageModeHub,
}

func groupModeIsProjectCell(mode string) bool {
	return mode == store.MessageModeProject || mode == store.MessageModeHub
}

func groupModeIsNarrow(mode string) bool {
	return mode == store.MessageModeLineage || mode == store.MessageModeBranch
}

// groupModeAgent creates a running agent with the given mode in projectID.
func groupModeAgent(t *testing.T, s store.Store, projectID, brokerID, mode string, ancestry ...string) *store.Agent {
	t.Helper()
	id := uuid.NewString()
	a := &store.Agent{
		ID:              id,
		Name:            "gmm-" + mode + "-" + id[:8],
		Slug:            "gmm-" + mode + "-" + id[:8],
		ProjectID:       projectID,
		RuntimeBrokerID: brokerID,
		Phase:           "running",
		MessageMode:     mode,
		Ancestry:        ancestry,
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return a
}

// groupModeConversation creates a group conversation in projectID.
func groupModeConversation(t *testing.T, s store.Store, projectID, externalRef string) *store.Conversation {
	t.Helper()
	if externalRef == "" {
		externalRef = "group:" + projectID + ":" + uuid.NewString()
	}
	conv, err := s.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: externalRef,
		ProjectID:   &projectID,
		DriftState:  "active",
	})
	require.NoError(t, err)
	return conv
}

func groupModeIsParticipant(t *testing.T, s store.Store, convID, kind, id string) bool {
	t.Helper()
	parts, err := s.ListParticipants(context.Background(), convID)
	require.NoError(t, err)
	for _, p := range parts {
		if p.PrincipalKind == kind && p.PrincipalID == id {
			return true
		}
	}
	return false
}

func groupModeAgentCtx(ctx context.Context, a *store.Agent) context.Context {
	return contextWithIdentity(ctx, &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: a.ID},
		ProjectID: a.ProjectID,
	}})
}

func requireGroupModeDenied(t *testing.T, rr *httptest.ResponseRecorder, msgAndArgs ...interface{}) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rr.Code, msgAndArgs...)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	errObj, _ := body["error"].(map[string]interface{})
	require.NotNil(t, errObj, "error envelope: %s", rr.Body.String())
	require.Equal(t, ErrCodeMessageDenied, errObj["code"], "body: %s", rr.Body.String())
}

// postOutboundGroup sends an agent's message to a user, persisted into the
// group conversation named by conversationID or threadID.
func postOutboundGroup(t *testing.T, srv *Server, a *store.Agent, recipientEmail, conversationID, threadID string) *httptest.ResponseRecorder {
	t.Helper()
	channel := ""
	if threadID != "" {
		channel = "web"
	}
	return postOutboundGroupWith(t, srv, a, recipientEmail, conversationID, threadID, channel, "group post from "+a.Slug)
}

// postOutboundGroupMsg is postOutboundGroup by conversation_id with the
// given message body.
func postOutboundGroupMsg(t *testing.T, srv *Server, a *store.Agent, recipientEmail, conversationID, msg string) *httptest.ResponseRecorder {
	t.Helper()
	return postOutboundGroupWith(t, srv, a, recipientEmail, conversationID, "", "", msg)
}

func postOutboundGroupWith(t *testing.T, srv *Server, a *store.Agent, recipientEmail, conversationID, threadID, channel, msg string) *httptest.ResponseRecorder {
	t.Helper()
	srv.chatSendLimiter = newChatSendLimiter()
	r := OutboundMessageRequest{
		Recipient:      "user:" + recipientEmail,
		Msg:            msg,
		ConversationID: conversationID,
		ThreadID:       threadID,
		Channel:        channel,
	}
	body, err := json.Marshal(r)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(groupModeAgentCtx(req.Context(), a))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, a.ID)
	return rr
}

// postInboundGroup sends sender's message to target through the agent
// message handler, naming a group conversation by conversationID or
// threadID.
func postInboundGroup(t *testing.T, srv *Server, sender, target *store.Agent, conversationID, threadID string) *httptest.ResponseRecorder {
	t.Helper()
	sm := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		Type:           messages.TypeInstruction,
		Sender:         "agent:" + sender.Slug,
		SenderID:       sender.ID,
		Recipient:      "agent:" + target.Slug,
		RecipientID:    target.ID,
		Msg:            "group post from " + sender.Slug,
		ConversationID: conversationID,
		ThreadID:       threadID,
	}
	if threadID != "" {
		sm.Channel = "web"
	}
	body, err := json.Marshal(MessageRequest{StructuredMessage: sm})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(groupModeAgentCtx(req.Context(), sender))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	return rr
}

// TestGroupMessageMode_PostDecision covers the shared post decision for
// every mode, with and without an active participant row.
func TestGroupMessageMode_PostDecision(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: "gmm-decision", Slug: "gmm-decision-" + projectID[:8]}))

	for _, mode := range groupModeTestModes {
		for _, membership := range []string{"none", "active", "left"} {
			t.Run(mode+"/"+membership, func(t *testing.T) {
				a := groupModeAgent(t, s, projectID, "", mode)
				conv := groupModeConversation(t, s, projectID, "")
				if membership != "none" {
					addConvParticipant(t, s, conv.ID, "agent", a.ID)
				}
				if membership == "left" {
					require.NoError(t, s.RemoveParticipant(ctx, conv.ID, "agent", a.ID))
				}

				d, err := srv.agentGroupPostDecision(ctx, a, conv.ID)
				require.NoError(t, err)
				switch {
				case mode == store.MessageModeNone:
					assert.False(t, d.Allowed, "none never posts to a group conversation")
				case groupModeIsProjectCell(mode):
					assert.True(t, d.Allowed)
					assert.True(t, d.MayJoin)
				default:
					assert.Equal(t, membership == "active", d.Allowed,
						"%s posts only as an active participant", mode)
					assert.False(t, d.MayJoin, "%s never joins by posting", mode)
				}
			})
		}
	}

	d, err := srv.agentGroupPostDecision(ctx, &store.Agent{ID: uuid.NewString(), MessageMode: "bogus"}, uuid.NewString())
	require.NoError(t, err)
	assert.False(t, d.Allowed, "an unknown mode is refused")
}

// TestGroupMessageMode_JoinDecision covers the shared join decision for
// every target mode and adder kind.
func TestGroupMessageMode_JoinDecision(t *testing.T) {
	f := newInboxFixture(t)
	ctx := context.Background()
	ancestor := uuid.NewString()
	createTestUserWithProjectRole(t, f.s, ancestor, ancestor+"@test.com", f.projA, store.ProjectRoleMember)
	ensureHubMembership(ctx, f.s, ancestor)

	owner := NewAuthenticatedUser(f.owner, f.owner+"@test.com", "Owner", "member", "cli")
	member := NewAuthenticatedUser(f.member, f.member+"@test.com", "Member", "member", "cli")
	anc := NewAuthenticatedUser(ancestor, ancestor+"@test.com", "Ancestor", "member", "cli")
	agentAdder := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: f.agentA.ID}, ProjectID: f.projA}}

	for _, mode := range groupModeTestModes {
		t.Run(mode, func(t *testing.T) {
			target := groupModeAgent(t, f.s, f.projA, "", mode, ancestor)
			cases := []struct {
				name  string
				adder Identity
				want  bool
			}{
				{"project-owner", owner, mode != store.MessageModeNone},
				{"ancestor", anc, mode != store.MessageModeNone},
				{"project-member", member, groupModeIsProjectCell(mode)},
				{"agent", agentAdder, groupModeIsProjectCell(mode)},
				{"no-identity", nil, groupModeIsProjectCell(mode)},
			}
			for _, c := range cases {
				got, reason := f.srv.groupAgentJoinAllowed(ctx, c.adder, target)
				assert.Equal(t, c.want, got, "adder %s, target mode %s (reason %s)", c.name, mode, reason)
			}
		})
	}
}

// TestGroupMessageMode_OutboundPost posts an agent message into a group
// conversation by conversation_id for every mode, with the agent added by
// a human beforehand and not added.
func TestGroupMessageMode_OutboundPost(t *testing.T) {
	f := newInboxFixture(t)
	recipient := f.member + "@test.com"

	for _, mode := range groupModeTestModes {
		for _, added := range []bool{true, false} {
			name := mode + "/not-added"
			if added {
				name = mode + "/added"
			}
			t.Run(name, func(t *testing.T) {
				a := groupModeAgent(t, f.s, f.projA, "", mode)
				conv := groupModeConversation(t, f.s, f.projA, "")
				if added {
					addConvParticipant(t, f.s, conv.ID, "agent", a.ID)
				}

				rr := postOutboundGroup(t, f.srv, a, recipient, conv.ID, "")
				allowed := groupModeIsProjectCell(mode) || (groupModeIsNarrow(mode) && added)
				if allowed {
					require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
				} else {
					requireGroupModeDenied(t, rr, "body: %s", rr.Body.String())
				}
				assert.Equal(t, added || groupModeIsProjectCell(mode),
					groupModeIsParticipant(t, f.s, conv.ID, "agent", a.ID),
					"only project and hub agents join a group conversation by posting")
			})
		}
	}
}

// TestGroupMessageMode_OutboundThreadPost posts an agent message into an
// existing thread conversation by thread_id for every mode.
func TestGroupMessageMode_OutboundThreadPost(t *testing.T) {
	srv, s, project, _, user := def138Setup(t)
	setupThreadTestChannels(t, srv, s, project, "web")

	for _, mode := range groupModeTestModes {
		for _, added := range []bool{true, false} {
			name := mode + "/not-added"
			if added {
				name = mode + "/added"
			}
			t.Run(name, func(t *testing.T) {
				a := groupModeAgent(t, s, project.ID, "", mode)
				threadID := "gmm-thread-" + uuid.NewString()[:8]
				conv := seedThreadConversation(t, s, project.ID, threadID)
				if added {
					addConvParticipant(t, s, conv.ID, "agent", a.ID)
				}

				rr := postOutboundGroup(t, srv, a, user.Email, "", threadID)
				allowed := groupModeIsProjectCell(mode) || (groupModeIsNarrow(mode) && added)
				if allowed {
					require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
				} else {
					requireGroupModeDenied(t, rr, "body: %s", rr.Body.String())
				}
				assert.Equal(t, added, groupModeIsParticipant(t, s, conv.ID, "agent", a.ID),
					"posting by thread key never adds the sender")
			})
		}
	}
}

// TestGroupMessageMode_InboundPost covers an agent's message through the
// agent message handler naming a group conversation, by conversation_id
// and by thread_id, for every mode.
func TestGroupMessageMode_InboundPost(t *testing.T) {
	srv, s, projectID, target, _ := def49Setup(t)
	brokerID := target.RuntimeBrokerID

	for _, mode := range groupModeTestModes {
		for _, byThread := range []bool{false, true} {
			for _, added := range []bool{true, false} {
				name := mode
				if byThread {
					name += "/thread"
				} else {
					name += "/conversation-id"
				}
				if added {
					name += "/added"
				} else {
					name += "/not-added"
				}
				t.Run(name, func(t *testing.T) {
					a := groupModeAgent(t, s, projectID, brokerID, mode)
					var conv *store.Conversation
					var rr *httptest.ResponseRecorder
					threadID := ""
					if byThread {
						threadID = "gmm-inbound-" + uuid.NewString()[:8]
						extRef, err := messaging.ThreadConversationExternalRef(projectID, threadID)
						require.NoError(t, err)
						conv = groupModeConversation(t, s, projectID, extRef)
					} else {
						conv = groupModeConversation(t, s, projectID, "")
					}
					if added {
						addConvParticipant(t, s, conv.ID, "agent", a.ID)
					}
					if byThread {
						rr = postInboundGroup(t, srv, a, a, "", threadID)
					} else {
						rr = postInboundGroup(t, srv, a, a, conv.ID, "")
					}

					allowed := groupModeIsProjectCell(mode) || (groupModeIsNarrow(mode) && added)
					if allowed {
						assert.Contains(t, []int{http.StatusOK, http.StatusAccepted}, rr.Code, "body: %s", rr.Body.String())
					} else {
						requireGroupModeDenied(t, rr, "body: %s", rr.Body.String())
					}
					switch {
					case !allowed || groupModeIsNarrow(mode):
						assert.Equal(t, added, groupModeIsParticipant(t, s, conv.ID, "agent", a.ID),
							"lineage, branch and none agents never join a group conversation by posting")
					case !byThread:
						assert.True(t, groupModeIsParticipant(t, s, conv.ID, "agent", a.ID),
							"project and hub agents join a group conversation by posting")
					}
				})
			}
		}
	}
}

// TestGroupMessageMode_InboundRecipientJoin covers registering the
// addressed agent of a message into a group conversation: an agent sender
// never adds a lineage or branch agent, a project owner does.
func TestGroupMessageMode_InboundRecipientJoin(t *testing.T) {
	srv, s, projectID, target0, _ := def49Setup(t)
	brokerID := target0.RuntimeBrokerID

	t.Run("branch-parent-agent-sender", func(t *testing.T) {
		parent := groupModeAgent(t, s, projectID, brokerID, store.MessageModeBranch)
		child := groupModeAgent(t, s, projectID, brokerID, store.MessageModeBranch, parent.ID)
		conv := groupModeConversation(t, s, projectID, "")
		addConvParticipant(t, s, conv.ID, "agent", parent.ID)

		rr := postInboundGroup(t, srv, parent, child, conv.ID, "")
		assert.Contains(t, []int{http.StatusOK, http.StatusAccepted}, rr.Code, "body: %s", rr.Body.String())
		assert.False(t, groupModeIsParticipant(t, s, conv.ID, "agent", child.ID),
			"an agent sender does not add a branch agent to a group conversation")
	})

	t.Run("project-agent-recipient", func(t *testing.T) {
		sender := groupModeAgent(t, s, projectID, brokerID, store.MessageModeProject)
		target := groupModeAgent(t, s, projectID, brokerID, store.MessageModeProject)
		conv := groupModeConversation(t, s, projectID, "")

		rr := postInboundGroup(t, srv, sender, target, conv.ID, "")
		assert.Contains(t, []int{http.StatusOK, http.StatusAccepted}, rr.Code, "body: %s", rr.Body.String())
		assert.True(t, groupModeIsParticipant(t, s, conv.ID, "agent", target.ID))
		assert.True(t, groupModeIsParticipant(t, s, conv.ID, "agent", sender.ID))
	})
}

// TestGroupMessageMode_AddParticipant covers the participant API for every
// target mode and adder, and a lineage agent posting after a project owner
// added it.
func TestGroupMessageMode_AddParticipant(t *testing.T) {
	f := newInboxFixture(t)
	grantAgentProjectAccess(t, f.s, f.agentA.ID, f.projA)
	add := func(id string) map[string]string {
		return map[string]string{"principalKind": "agent", "principalId": id}
	}

	for _, mode := range groupModeTestModes {
		t.Run(mode, func(t *testing.T) {
			g := f.group(t, f.projA, f.owner, f.member)
			addConvParticipant(t, f.s, g.ID, "agent", f.agentA.ID)
			path := "/api/v1/conversations/" + g.ID + "/participants"

			// A project member without authority over a narrow agent.
			target := groupModeAgent(t, f.s, f.projA, "", mode)
			rr := f.session(t, f.member, http.MethodPost, path, add(target.ID))
			if groupModeIsProjectCell(mode) {
				assert.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
			} else {
				requireGroupModeDenied(t, rr, "member adds %s agent: %s", mode, rr.Body.String())
			}

			// An agent participant.
			target2 := groupModeAgent(t, f.s, f.projA, "", mode)
			body, err := json.Marshal(add(target2.ID))
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(agentContextWithScopes(f.agentA.ID, f.projA, []AgentTokenScope{ScopeProjectRead}))
			rr = httptest.NewRecorder()
			f.srv.handleAddParticipant(rr, req, g.ID)
			if groupModeIsProjectCell(mode) {
				assert.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
			} else {
				requireGroupModeDenied(t, rr, "agent adds %s agent: %s", mode, rr.Body.String())
			}

			// The project owner.
			target3 := groupModeAgent(t, f.s, f.projA, "", mode)
			rr = f.session(t, f.owner, http.MethodPost, path, add(target3.ID))
			if mode == store.MessageModeNone {
				requireGroupModeDenied(t, rr, "owner adds none agent: %s", rr.Body.String())
				return
			}
			require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

			// Added by the owner, the agent may post to the conversation.
			post := postOutboundGroup(t, f.srv, target3, f.member+"@test.com", g.ID, "")
			assert.Equal(t, http.StatusOK, post.Code, "body: %s", post.Body.String())
		})
	}
}

// TestGroupMessageMode_CreateConversation covers an agent creating a group
// conversation, which makes it a participant, for every mode.
func TestGroupMessageMode_CreateConversation(t *testing.T) {
	srv, s, _, _ := setupGroupConvTopicTest(t)
	project, _, _ := setupConvTestData(t, s)
	for _, mode := range groupModeTestModes {
		t.Run(mode, func(t *testing.T) {
			a := groupModeAgent(t, s, project.ID, "", mode)
			grantAgentProjectAccess(t, s, a.ID, project.ID)
			name := "gmm-create-" + uuid.NewString()[:8]
			body, err := json.Marshal(createConversationRequest{
				DisplayName: name,
				ProjectID:   project.ID,
				Kind:        "group",
			})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(agentContextWithScopes(a.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
			rr := httptest.NewRecorder()
			srv.handleCreateConversation(rr, req)
			if groupModeIsProjectCell(mode) {
				assert.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
			} else {
				requireGroupModeDenied(t, rr, "body: %s", rr.Body.String())
			}
			assert.Equal(t, groupModeIsProjectCell(mode), groupModeConversationNamed(t, s, project.ID, name),
				"a refused create writes no conversation")
		})
	}
}

// groupModeConversationNamed reports whether projectID has a conversation
// with the given display name.
func groupModeConversationNamed(t *testing.T, s store.Store, projectID, name string) bool {
	t.Helper()
	res, err := s.ListConversations(context.Background(), store.ConversationFilter{ProjectID: projectID}, store.ListOptions{Limit: 200})
	require.NoError(t, err)
	for _, c := range res.Items {
		if c.DisplayName == name {
			return true
		}
	}
	return false
}

// TestGroupMessageMode_NewThreadPost covers an agent message naming a
// thread that does not exist yet: it is refused for none, lineage and
// branch agents with no conversation created, and accepted for project
// and hub agents.
func TestGroupMessageMode_NewThreadPost(t *testing.T) {
	srv, s, projectID, target, _ := def49Setup(t)
	brokerID := target.RuntimeBrokerID
	ctx := context.Background()

	for _, mode := range groupModeTestModes {
		t.Run(mode, func(t *testing.T) {
			a := groupModeAgent(t, s, projectID, brokerID, mode)
			threadID := "gmm-new-" + uuid.NewString()[:8]
			extRef, err := messaging.ThreadConversationExternalRef(projectID, threadID)
			require.NoError(t, err)

			rr := postInboundGroup(t, srv, a, a, "", threadID)
			_, lookupErr := s.GetConversationByExternalRef(ctx, "native", extRef)
			if groupModeIsProjectCell(mode) {
				assert.Contains(t, []int{http.StatusOK, http.StatusAccepted}, rr.Code, "body: %s", rr.Body.String())
				assert.NoError(t, lookupErr)
			} else {
				requireGroupModeDenied(t, rr, "body: %s", rr.Body.String())
				assert.True(t, errors.Is(lookupErr, store.ErrNotFound),
					"a refused post creates no conversation (err=%v)", lookupErr)
			}
		})
	}
}

// TestGroupMessageMode_MentionJoin covers agents mentioned in an agent's
// group post: a branch child mentioned by its branch parent is delivered
// the mention but not registered as a participant; a project agent
// mentioned by a project agent is delivered and registered.
func TestGroupMessageMode_MentionJoin(t *testing.T) {
	srv, s, project, sender, target, _, _, dispatcher := mentionFanoutSetup(t)
	wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
	ctx := context.Background()

	recipient := &store.User{ID: uuid.NewString(), Email: "gmm-mj-" + uuid.NewString()[:8] + "@example.com", DisplayName: "GMM Recipient"}
	require.NoError(t, s.CreateUser(ctx, recipient))

	newAgent := func(slug, mode string, ancestry []string) *store.Agent {
		a := &store.Agent{
			ID:              uuid.NewString(),
			Name:            slug,
			Slug:            slug,
			ProjectID:       project.ID,
			Phase:           "running",
			RuntimeBrokerID: target.RuntimeBrokerID,
			MessageMode:     mode,
			Ancestry:        ancestry,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	parent := newAgent("gmm-mj-parent", store.MessageModeBranch, append([]string{}, sender.Ancestry...))
	child := newAgent("gmm-mj-child", store.MessageModeBranch, append(append([]string{}, parent.Ancestry...), parent.ID))
	peer := newAgent("gmm-mj-peer", store.MessageModeProject, append([]string{}, sender.Ancestry...))

	t.Run("branch-child", func(t *testing.T) {
		conv := groupModeConversation(t, s, project.ID, "")
		addConvParticipant(t, s, conv.ID, "agent", parent.ID)

		rr := postOutboundGroupMsg(t, srv, parent, recipient.Email, conv.ID, "@gmm-mj-child please take a look")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		require.Len(t, dispatchesTo(dispatcher, child.ID), 1, "the mention is delivered to the child")
		assert.False(t, groupModeIsParticipant(t, s, conv.ID, "agent", child.ID),
			"an agent's mention does not add a branch agent to the group conversation")
	})

	t.Run("project-peer", func(t *testing.T) {
		conv := groupModeConversation(t, s, project.ID, "")

		rr := postOutboundGroupMsg(t, srv, sender, recipient.Email, conv.ID, "@gmm-mj-peer please take a look")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		require.Len(t, dispatchesTo(dispatcher, peer.ID), 1, "the mention is delivered to the peer")
		assert.True(t, groupModeIsParticipant(t, s, conv.ID, "agent", peer.ID),
			"a mentioned project agent is added to the group conversation")
	})
}

// TestGroupMessageMode_OutboundExternalThreadPost covers agent posts by
// thread_id on an external channel for every mode: to an existing thread
// conversation the agent belongs to, and to a thread that does not exist
// yet, where a refused post creates no conversation.
func TestGroupMessageMode_OutboundExternalThreadPost(t *testing.T) {
	srv, s, project, _, user := def138Setup(t)
	setupThreadTestChannels(t, srv, s, project, "web", "slack")
	ctx := context.Background()
	projectID := project.ID

	for _, mode := range groupModeTestModes {
		t.Run(mode+"/existing-added", func(t *testing.T) {
			a := groupModeAgent(t, s, projectID, "", mode)
			threadID := "gmm-slack-" + uuid.NewString()[:8]
			extRef, err := messaging.ThreadConversationExternalRef(projectID, threadID)
			require.NoError(t, err)
			conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
				Kind:        "group",
				Surface:     "slack",
				ExternalRef: extRef,
				ProjectID:   &projectID,
				DriftState:  "active",
			})
			require.NoError(t, err)
			addConvParticipant(t, s, conv.ID, "agent", a.ID)

			rr := postOutboundGroupWith(t, srv, a, user.Email, "", threadID, "slack", "external thread post from "+a.Slug)
			if mode == store.MessageModeNone {
				requireGroupModeDenied(t, rr, "body: %s", rr.Body.String())
			} else {
				require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
			}
		})

		t.Run(mode+"/new-thread", func(t *testing.T) {
			a := groupModeAgent(t, s, projectID, "", mode)
			threadID := "gmm-slack-new-" + uuid.NewString()[:8]
			extRef, err := messaging.ThreadConversationExternalRef(projectID, threadID)
			require.NoError(t, err)

			rr := postOutboundGroupWith(t, srv, a, user.Email, "", threadID, "slack", "external thread post from "+a.Slug)
			_, lookupErr := s.GetConversationByExternalRef(ctx, "slack", extRef)
			if groupModeIsProjectCell(mode) {
				require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
				assert.NoError(t, lookupErr)
			} else {
				requireGroupModeDenied(t, rr, "body: %s", rr.Body.String())
				assert.True(t, errors.Is(lookupErr, store.ErrNotFound),
					"a refused post creates no conversation (err=%v)", lookupErr)
			}
		})
	}
}
