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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PublishChatThreadGroupEvent publishes {action, group} on
// project.<projectId>.chat.thread_group, which the web's existing
// project.<id>.chat.> subscription matches; an empty project publishes
// nothing.
func TestPublishChatThreadGroupEvent(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()

	exact, unsubExact := pub.Subscribe("project.proj1.chat.thread_group")
	defer unsubExact()
	chat, unsubChat := pub.Subscribe("project.proj1.chat.>")
	defer unsubChat()

	at := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	group := ThreadGroupResponse{
		ID: "g1", ProjectID: "proj1", Name: "Release", CreatedBy: "u1", CreatedByKind: store.ThreadGroupCreatorUser,
		CreatedAt: at, UpdatedAt: at,
		Threads: []ThreadGroupThread{{ConversationID: "c1", ThreadID: "t1", Name: "release-notes", AddedBy: "u1", AddedAt: at}},
	}
	pub.PublishChatThreadGroupEvent(context.Background(), "", ThreadGroupEventCreated, group)
	pub.PublishChatThreadGroupEvent(context.Background(), "proj1", ThreadGroupEventCreated, group)

	for name, ch := range map[string]<-chan Event{"exact": exact, "chat wildcard": chat} {
		select {
		case evt := <-ch:
			assert.Equal(t, "project.proj1.chat.thread_group", evt.Subject, name)
			assert.JSONEq(t, `{"action":"created","group":{"id":"g1","projectId":"proj1","name":"Release",
				"createdBy":"u1","createdByKind":"user","createdAt":"2026-10-12T08:00:00Z","updatedAt":"2026-10-12T08:00:00Z",
				"threads":[{"conversationId":"c1","threadId":"t1","name":"release-notes","addedBy":"u1","addedAt":"2026-10-12T08:00:00Z"}]}}`,
				string(evt.Data), name)
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no event", name)
		}
		select {
		case evt := <-ch:
			t.Fatalf("%s: unexpected second event %s", name, evt.Subject)
		default:
		}
	}

	// An empty group's threads serialize as [], never null.
	raw, err := json.Marshal(ThreadGroupEvent{Action: ThreadGroupEventDeleted, Group: ThreadGroupResponse{Threads: []ThreadGroupThread{}}})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"threads":[]`)
}

// The SSE subject authorizer treats project.<id>.chat.thread_group like
// project.<id>.chat.topic: allowed for a project reader, denied (403) for
// anyone else.
func TestSSEHandler_ThreadGroupSubjectOnlyForProjectReaders(t *testing.T) {
	const userID = "user-1"
	ownProject := tid("thread-group-own-project")
	victimProject := tid("thread-group-victim-project")

	s := &mockAuthzStore{
		projects: []store.Project{
			{ID: ownProject, OwnerID: userID},
			{ID: victimProject, OwnerID: "other-user"},
		},
		projectMemberships: map[string]*store.ProjectMembership{
			ownProject + ":" + userID: {ProjectID: ownProject, UserID: userID, Role: store.ProjectRoleOwner},
		},
	}

	for _, tc := range []struct {
		subject string
		want    int
	}{
		{"project." + ownProject + ".chat.thread_group", http.StatusOK},
		{"project." + ownProject + ".chat.topic", http.StatusOK}, // control: same rule
		{"project." + victimProject + ".chat.thread_group", http.StatusForbidden},
		{"project." + victimProject + ".chat.topic", http.StatusForbidden}, // control: same rule
		{"project." + victimProject + ".chat.>", http.StatusForbidden},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			pub := NewChannelEventPublisher()
			defer pub.Close()
			ws := &WebServer{store: s, events: pub, authzService: NewAuthzService(s, nil)}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // only the initial authorization decision matters here
			w := httptest.NewRecorder()
			ws.handleSSE(w, sseAgentRequest(ctx, userID, "user", tc.subject))
			assert.Equal(t, tc.want, w.Code, "subject %q", tc.subject)
		})
	}
}
