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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the disclosure boundary for chat notifications.
//
// The defect they exist to prevent, measured on this branch before the fix:
// PublishNotification fanned chat notifications out on the unscoped subject
// "notification.created", and authorizeSSESubjects (web.go) only constrains
// subjects whose first token is "project" or "user" — so a subscription to
// "notification.>" was granted to every logged-in session, and every browser
// on the deployment received every other user's sender name and 100-character
// message preview.
//
// Revert PublishChatNotification's subject to "notification.created" and
// TestChatNotification_NotDeliveredToBystander fails with eve holding alice's
// DM preview.

const bystanderSubjects = "notification.>"

// newChatNotificationForTest builds a DM notification addressed to subscriberID.
func newChatNotificationForTest(subscriberID, projectID, message string) *store.Notification {
	return &store.Notification{
		ID:             "notif-under-test",
		SubscriptionID: "00000000-0000-0000-0000-000000000000",
		AgentID:        "00000000-0000-0000-0000-000000000000",
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   subscriberID,
		Status:         ChatNotificationDMReceived,
		Message:        message,
		CreatedAt:      time.Now(),
	}
}

// chatContextForTest is the conversation/sender context that rides along with
// a chat notification event.
func chatContextForTest() ChatMessageContext {
	return ChatMessageContext{
		SenderID:        "user-bob",
		SenderName:      "Bob",
		ConversationKey: "dm:user:user-bob:user:user-alice",
		Preview:         "the merger closes friday, keep it quiet",
		ProjectID:       "proj-1",
	}
}

// TestChatNotification_NotDeliveredToBystander is the regression test for the
// disclosure. Eve subscribes to every subject a session is allowed to request
// without being the recipient — the unscoped notification subject, and the
// project subject for the project the DM belongs to — and must receive nothing.
func TestChatNotification_NotDeliveredToBystander(t *testing.T) {
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)

	const projectID = "proj-1"
	const secret = "bob sent you a message: the merger closes friday, keep it quiet"

	eve, unsubEve := pub.Subscribe(
		bystanderSubjects,
		"project."+projectID+".notification",
	)
	defer unsubEve()

	pub.PublishChatNotification(context.Background(),
		newChatNotificationForTest("user-alice", projectID, secret), chatContextForTest())

	select {
	case evt := <-eve:
		t.Fatalf("chat notification leaked to a bystander on subject %q: %s", evt.Subject, evt.Data)
	case <-time.After(250 * time.Millisecond):
		// Correct: nothing reached a subscriber who is not the recipient.
	}
}

// TestChatNotification_DeliveredToSubscriber is the positive half: scoping the
// subject must not silence the notification for the person it is for.
func TestChatNotification_DeliveredToSubscriber(t *testing.T) {
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)

	const secret = "bob sent you a message: the merger closes friday, keep it quiet"

	alice, unsub := pub.Subscribe("user.user-alice.notification")
	defer unsub()

	pub.PublishChatNotification(context.Background(),
		newChatNotificationForTest("user-alice", "proj-1", secret), chatContextForTest())

	select {
	case evt := <-alice:
		assert.Equal(t, "user.user-alice.notification", evt.Subject)
		var payload NotificationCreatedEvent
		require.NoError(t, json.Unmarshal(evt.Data, &payload))
		assert.Equal(t, secret, payload.Message)
		assert.Equal(t, ChatNotificationDMReceived, payload.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("recipient did not receive their own chat notification")
	}
}

// TestChatNotification_NoSubscriberIsDropped pins the fail-closed branch: a
// notification with no subscriber has no subject that can be scoped to it, so
// it must be dropped rather than fall back to a broadcast subject.
func TestChatNotification_NoSubscriberIsDropped(t *testing.T) {
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)

	everything, unsub := pub.Subscribe(">")
	defer unsub()

	notif := newChatNotificationForTest("", "proj-1", "alice sent you a message: hi")
	pub.PublishChatNotification(context.Background(), notif, chatContextForTest())

	select {
	case evt := <-everything:
		t.Fatalf("unaddressed chat notification was published on %q: %s", evt.Subject, evt.Data)
	case <-time.After(250 * time.Millisecond):
	}
}

// TestAgentStatusNotification_SubjectsUnchanged pins the bound on the fix:
// agent-status notifications are a separate, pre-existing problem with
// different consumers, and this change must not alter their subjects.
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
