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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// These tests pin the SSE subject boundaries for notifications: agent-status
// notifications keep their subjects, and per-user subjects are only granted
// to their own user. Chat messages no longer create notifications.

// TestAgentStatusNotification_SubjectsUnchanged pins the subjects agent-status
// notifications are published on.
func TestAgentStatusNotification_SubjectsUnchanged(t *testing.T) {
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)

	const projectID = "proj-1"
	ch, unsub := pub.Subscribe("notification.>", "project."+projectID+".notification")
	defer unsub()

	pub.PublishNotification(context.Background(), &store.Notification{
		ID:             "notif-agent",
		SubscriptionID: "sub-1",
		AgentID:        "agent-1",
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "user-alice",
		Status:         "COMPLETED",
		Message:        "agent-1 has reached a state of COMPLETED",
		CreatedAt:      time.Now(),
	})

	subjects := map[string]bool{}
	deadline := time.After(time.Second)
	for len(subjects) < 2 {
		select {
		case evt := <-ch:
			subjects[evt.Subject] = true
		case <-deadline:
			t.Fatalf("agent-status notification subjects changed; saw only %v", subjects)
		}
	}
	assert.True(t, subjects["notification.created"])
	assert.True(t, subjects["project."+projectID+".notification"])
}

// TestAuthorizeSSESubjects_OtherUsersNotificationSubject is the authorization
// half of the fix: the per-user subject is only useful if the gate actually
// refuses it to everyone else. Measured, not assumed.
func TestAuthorizeSSESubjects_OtherUsersNotificationSubject(t *testing.T) {
	ws := &WebServer{authzService: NewAuthzService(&mockAuthzStore{}, nil)}

	req := httptest.NewRequest("GET", "/events", nil)
	eve := &webSessionUser{UserID: "user-eve", Email: "eve@example.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, eve))

	denied := ws.authorizeSSESubjects(req, []string{"user.user-alice.notification"})
	assert.Equal(t, []string{"user.user-alice.notification"}, denied,
		"a user must not be able to subscribe to another user's notification subject")

	ownReq := httptest.NewRequest("GET", "/events", nil)
	ownReq = ownReq.WithContext(context.WithValue(ownReq.Context(), webUserContextKey{}, eve))
	assert.Nil(t, ws.authorizeSSESubjects(ownReq, []string{"user.user-eve.notification"}),
		"a user must be able to subscribe to their own notification subject")
}

// TestAuthorizeSSESubjects_WildcardUserSubjects covers the obvious way to
// attack a per-user subject: ask for all of them at once. validateSSESubjects
// rejects a wildcard only in the first token, so these reach the gate, which
// holds because it compares the literal token ("*", ">") against the session
// user's ID and no real UID can equal either.
//
// That is correct by string comparison rather than by design, which is exactly
// why it is pinned here: a future "expand wildcards before authorizing"
// refactor would turn this into a total disclosure of every user's chat
// notifications, and nothing else in the suite would notice.
func TestAuthorizeSSESubjects_WildcardUserSubjects(t *testing.T) {
	ws := &WebServer{authzService: NewAuthzService(&mockAuthzStore{}, nil)}

	eve := &webSessionUser{UserID: "user-eve", Email: "eve@example.com", Role: "user"}

	for _, subject := range []string{"user.*.notification", "user.>", "user.*.>"} {
		req := httptest.NewRequest("GET", "/events", nil)
		req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, eve))

		assert.Equal(t, []string{subject}, ws.authorizeSSESubjects(req, []string{subject}),
			"wildcard subject %q must not be authorized — it would match every user's notifications", subject)
	}
}

// TestAuthorizeSSESubjects_AdminCannotSubscribeOtherUser guards the gate
// against a role-based bypass: admin is not a licence to read DMs.
func TestAuthorizeSSESubjects_AdminCannotSubscribeOtherUser(t *testing.T) {
	ws := &WebServer{authzService: NewAuthzService(&mockAuthzStore{}, nil)}

	req := httptest.NewRequest("GET", "/events", nil)
	admin := &webSessionUser{UserID: "admin-1", Email: "admin@example.com", Role: "admin"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, admin))

	denied := ws.authorizeSSESubjects(req, []string{"user.user-alice.notification"})
	assert.Equal(t, []string{"user.user-alice.notification"}, denied,
		"admin must not be able to subscribe to another user's notification subject")
}
