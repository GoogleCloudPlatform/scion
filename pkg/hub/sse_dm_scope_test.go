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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSEHandler_DMMessagesScopedToParticipants checks that DM messages,
// which PublishUserMessage also fans out to agent.<id>.message and
// project.<id>.user.message, reach only DM participants on the web events
// stream, for direct and wildcard subscriptions alike, while non-DM agent
// messages still reach every reader of the agent or project.
func TestSSEHandler_DMMessagesScopedToParticipants(t *testing.T) {
	agentID, projectID := tid("sse-dm-agent"), tid("sse-dm-project")
	const participant, member, other = "user-dm-participant", "user-dm-member", "user-dm-other"
	dmKey := "dm:agent:" + agentID + ":user:" + participant

	messages := []*store.Message{
		// Agent reply in the DM: agent, project and user subjects.
		{ID: "dm-reply", ProjectID: projectID, AgentID: agentID, Channel: "web", ThreadID: dmKey,
			Sender: "agent:a", SenderID: agentID, Recipient: "user:" + participant, RecipientID: participant, Msg: "private reply"},
		// User prompt in the DM: agent subject only.
		{ID: "dm-prompt", ProjectID: projectID, AgentID: agentID, Channel: "web", ThreadID: dmKey,
			Sender: "user:" + participant, SenderID: participant, Recipient: "agent:a", RecipientID: agentID, Msg: "private prompt"},
		// Non-DM agent message to another user.
		{ID: "plain", ProjectID: projectID, AgentID: agentID,
			Sender: "agent:a", SenderID: agentID, Recipient: "user:" + other, RecipientID: other, Msg: "plain"},
	}

	agentSubjects := []string{"agent." + agentID + ".message", "agent." + agentID + ".>", "agent." + agentID + ".*"}
	projectSubjects := []string{"project." + projectID + ".>", "project." + projectID + ".user.message", "project.>"}

	for _, tc := range []struct {
		user     string
		subjects []string
		want     []string
	}{
		{participant, agentSubjects, []string{"dm-reply", "dm-prompt", "plain"}},
		{participant, projectSubjects, []string{"dm-reply", "plain"}},
		{member, agentSubjects, []string{"plain"}},
		{member, projectSubjects, []string{"plain"}},
	} {
		for _, subject := range tc.subjects {
			t.Run(tc.user+"/"+subject, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ms := &mockAuthzStore{
						projects: []store.Project{{ID: projectID, OwnerID: "user-owner"}},
						projectMemberships: map[string]*store.ProjectMembership{
							projectID + ":" + participant: {ProjectID: projectID, UserID: participant, Role: store.ProjectRoleMember},
							projectID + ":" + member:      {ProjectID: projectID, UserID: member, Role: store.ProjectRoleMember},
						},
					}
					s := &sseAgentStore{mockAuthzStore: ms, agents: map[string]*store.Agent{
						agentID: {ID: agentID, ProjectID: projectID, OwnerID: "user-owner"},
					}}
					pub := NewChannelEventPublisher()
					defer pub.Close()
					ws := &WebServer{store: s, events: pub, authzService: NewAuthzService(s, slog.Default())}

					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					w := httptest.NewRecorder()
					done := make(chan struct{})
					go func() {
						defer close(done)
						ws.handleSSE(w, sseAgentRequest(ctx, tc.user, "user", subject))
					}()
					synctest.Wait()
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())

					for _, msg := range messages {
						msg.CreatedAt = time.Unix(0, 0)
						pub.PublishUserMessage(ctx, msg, nil)
					}
					synctest.Wait()
					cancel()
					<-done

					assert.Equal(t, tc.want, sseMessageIDs(t, w.Body.String()))
					assertNoSSESubscribers(t, pub)
				})
			})
		}
	}
}

// sseMessageIDs returns the message ids of the events written to an SSE body.
func sseMessageIDs(t *testing.T, body string) []string {
	t.Helper()
	ids := []string{}
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var frame struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(data), &frame))
		ids = append(ids, frame.Data.ID)
	}
	return ids
}

func TestSSEEventVisible(t *testing.T) {
	agentDM := `{"id":"m","threadId":"dm:agent:agent-1:user:user-a"}`
	userDM := `{"id":"m","threadId":"dm:user:user-a:user:user-b"}`
	for _, tc := range []struct {
		name, subject, data, user string
		want                      bool
	}{
		{"agent DM to participant", "agent.agent-1.message", agentDM, "user-a", true},
		{"agent DM to non-participant", "agent.agent-1.message", agentDM, "user-b", false},
		{"agent DM to empty user", "agent.agent-1.message", agentDM, "", false},
		{"agent id is not a user slot", "agent.agent-1.message", agentDM, "agent-1", false},
		{"project DM to first participant", "project.p.user.message", userDM, "user-a", true},
		{"project DM to second participant", "project.p.user.message", userDM, "user-b", true},
		{"project DM to non-participant", "project.p.user.message", userDM, "user-c", false},
		{"malformed DM key", "agent.agent-1.message", `{"threadId":"dm:user:user-a"}`, "user-a", false},
		{"unreadable payload", "agent.agent-1.message", `not json`, "user-a", false},
		{"non-DM agent message", "agent.agent-1.message", `{"id":"m","threadId":"agent:agent-1"}`, "user-b", true},
		{"agent message without thread", "agent.agent-1.message", `{"id":"m"}`, "user-b", true},
		{"other agent subject", "agent.agent-1.status", agentDM, "user-b", true},
		{"own user subject", "user.user-b.message", agentDM, "user-b", true},
		{"project chat subject", "project.p.chat.message", `{"threadId":"topic-1"}`, "user-b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sseEventVisible(Event{Subject: tc.subject, Data: []byte(tc.data)}, tc.user)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestPublishUserMessage_DMSubjectsScoped guards the subject list in
// sseSubjectMayCarryDM. It captures every subject PublishUserMessage uses for
// DM messages and requires each one to be either a user.<participant>.*
// subject or a subject the events stream filters by participant. A new
// fan-out subject that carries DM messages fails here until it is scoped.
func TestPublishUserMessage_DMSubjectsScoped(t *testing.T) {
	const agentID, projectID = "agent-1", "project-1"
	const userA, userB, outsider = "user-a", "user-b", "user-outsider"
	agentDM := "dm:agent:" + agentID + ":user:" + userA
	userDM := "dm:user:" + userA + ":user:" + userB

	var cases []*store.Message
	for _, channel := range []string{"web", "", "slack"} {
		for _, projID := range []string{projectID, ""} {
			cases = append(cases,
				// Agent reply in an agent DM.
				&store.Message{ID: "reply", ProjectID: projID, AgentID: agentID, Channel: channel, ThreadID: agentDM,
					Sender: "agent:a", SenderID: agentID, Recipient: "user:" + userA, RecipientID: userA},
				// User prompt in an agent DM.
				&store.Message{ID: "prompt", ProjectID: projID, AgentID: agentID, Channel: channel, ThreadID: agentDM,
					Sender: "user:" + userA, SenderID: userA, Recipient: "agent:a", RecipientID: agentID},
				// User-to-user DM, with and without an agent id.
				&store.Message{ID: "u2u", ProjectID: projID, Channel: channel, ThreadID: userDM,
					Sender: "user:" + userA, SenderID: userA, Recipient: "user:" + userB, RecipientID: userB},
				&store.Message{ID: "u2u-agent", ProjectID: projID, AgentID: agentID, Channel: channel, ThreadID: userDM,
					Sender: "user:" + userB, SenderID: userB, Recipient: "user:" + userA, RecipientID: userA},
			)
		}
	}

	for _, msg := range cases {
		t.Run(msg.ID+"/"+msg.Channel+"/"+msg.ProjectID, func(t *testing.T) {
			var subjects []string
			var payloads [][]byte
			b := &eventBuilder{sink: func(subject string, evt interface{}) {
				data, err := json.Marshal(evt)
				require.NoError(t, err)
				subjects = append(subjects, subject)
				payloads = append(payloads, data)
			}}
			msg.CreatedAt = time.Unix(0, 0)
			b.PublishUserMessage(context.Background(), msg, nil)
			require.NotEmpty(t, subjects)

			participants := dmUserParticipants(msg.ThreadID)
			// user.<id>.* subjects are scoped to the session user with that
			// id. The chat.dm fan-out names both DM key slots, so the agent
			// slot of an agent DM also gets one; no web session has that id.
			keyParts := strings.Split(msg.ThreadID, ":")
			keyIDs := []string{keyParts[2], keyParts[4]}
			for i, subject := range subjects {
				tokens := strings.Split(subject, ".")
				if tokens[0] == "user" && len(tokens) >= 3 {
					assert.Contains(t, keyIDs, tokens[1],
						"DM message published to a non-participant user subject %q", subject)
					continue
				}
				if !assert.True(t, sseSubjectMayCarryDM(subject),
					"DM message published to %q, which is neither participant-scoped nor filtered by sseEventVisible", subject) {
					continue
				}
				evt := Event{Subject: subject, Data: payloads[i]}
				assert.False(t, sseEventVisible(evt, outsider), "non-participant receives %q", subject)
				for _, p := range participants {
					assert.True(t, sseEventVisible(evt, p), "participant %s misses %q", p, subject)
				}
			}
		})
	}
}
