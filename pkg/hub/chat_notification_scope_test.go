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
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the SSE subject boundaries for notifications: all user
// notifications (including agent-status notifications) publish strictly on
// user.<subscriberID>.notification, and per-user subjects are only granted
// to their own user.

const bystanderSubjects = "notification.>"

// newUserNotificationForTest builds a notification addressed to
// subscriberID; callers set Status.
func newUserNotificationForTest(subscriberID, projectID, message string) *store.Notification {
	return &store.Notification{
		ID:             "notif-under-test",
		SubscriptionID: "00000000-0000-0000-0000-000000000000",
		AgentID:        "00000000-0000-0000-0000-000000000000",
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   subscriberID,
		Message:        message,
		CreatedAt:      time.Now(),
	}
}

// TestAgentStatusNotification_ScopedToSubscriber pins that agent-status
// notifications are published strictly on user.<subscriberID>.notification
// and never on notification.> or project.<id>.notification.
func TestAgentStatusNotification_ScopedToSubscriber(t *testing.T) {
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)

	const projectID = "proj-1"
	owner, unsubOwner := pub.Subscribe("user.user-alice.notification")
	defer unsubOwner()
	bystander, unsubBystander := pub.Subscribe(bystanderSubjects, "project."+projectID+".notification", "user.user-eve.notification")
	defer unsubBystander()

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

	// Unaddressed notifications must be dropped.
	pub.PublishNotification(context.Background(), &store.Notification{
		ID:        "notif-unaddressed",
		AgentID:   "agent-1",
		ProjectID: projectID,
		Status:    "COMPLETED",
		Message:   "dropped",
		CreatedAt: time.Now(),
	})

	select {
	case evt := <-owner:
		assert.Equal(t, "user.user-alice.notification", evt.Subject)
		var payload UserNotificationEvent
		require.NoError(t, json.Unmarshal(evt.Data, &payload))
		assert.Equal(t, "notif-agent", payload.ID)
		assert.Equal(t, "user-alice", payload.SubscriberID)
		assert.Equal(t, "COMPLETED", payload.Status)
	case <-time.After(time.Second):
		t.Fatal("recipient did not receive agent-status notification on user.<id>.notification")
	}

	select {
	case evt := <-bystander:
		t.Fatalf("unexpected notification event on %q: %s", evt.Subject, evt.Data)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestSSE_AgentStatusNotificationScopedPerUser verifies end-to-end SSE
// delivery: user A receives their own agent-status notifications on
// user.<A>.notification and never receives user B's notifications, even when
// both users share access to the same project.
func TestSSE_AgentStatusNotificationScopedPerUser(t *testing.T) {
	const (
		userA     = "user-alice"
		userB     = "user-bob"
		projectID = "proj-shared"
	)

	mockStore := &mockAuthzStore{
		projects: []store.Project{
			{ID: projectID, OwnerID: userA},
		},
		projectMemberships: map[string]*store.ProjectMembership{
			projectID + ":" + userA: {ProjectID: projectID, UserID: userA, Role: store.ProjectRoleOwner},
			projectID + ":" + userB: {ProjectID: projectID, UserID: userB, Role: store.ProjectRoleMember},
		},
	}
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	ws := &WebServer{
		store:        mockStore,
		events:       pub,
		authzService: NewAuthzService(mockStore, nil),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-Test-User")
		u := &webSessionUser{UserID: uid, Email: uid + "@example.com", Role: "user"}
		ws.handleSSE(w, r.WithContext(context.WithValue(r.Context(), webUserContextKey{}, u)))
	}))
	t.Cleanup(ts.Close)

	openStream := func(uid string, subs ...string) (*http.Response, chan string) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		q := url.Values{"sub": subs}.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?"+q, nil)
		require.NoError(t, err)
		req.Header.Set("X-Test-User", uid)
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		require.Equal(t, http.StatusOK, resp.StatusCode)

		lines := make(chan string, 32)
		go func() {
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				line := sc.Text()
				if strings.HasPrefix(line, "data: ") {
					lines <- strings.TrimPrefix(line, "data: ")
				}
			}
			close(lines)
		}()
		// handleSSE calls ws.events.Subscribe before flushing headers, so the
		// subscription is already active once Do returns 200 OK.
		return resp, lines
	}

	_, streamA := openStream(userA, "project."+projectID+".>", "user."+userA+".notification")
	_, streamB := openStream(userB, "project."+projectID+".>", "user."+userB+".notification")

	// Publish a notification for user B first, then one for user A.
	pub.PublishUserNotification(context.Background(), &store.Notification{
		ID:             "notif-for-b",
		AgentID:        "agent-b",
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   userB,
		Status:         "WAITING_FOR_INPUT",
		Message:        "agent-b is WAITING_FOR_INPUT",
		CreatedAt:      time.Now().UTC(),
	})
	pub.PublishUserNotification(context.Background(), &store.Notification{
		ID:             "notif-for-a",
		AgentID:        "agent-a",
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   userA,
		Status:         "COMPLETED",
		Message:        "agent-a has reached a state of COMPLETED",
		CreatedAt:      time.Now().UTC(),
	})

	type sseEnvelope struct {
		Subject string                `json:"subject"`
		Data    UserNotificationEvent `json:"data"`
	}

	collect := func(ch <-chan string) []sseEnvelope {
		var got []sseEnvelope
		deadline := time.After(300 * time.Millisecond)
		for {
			select {
			case raw, ok := <-ch:
				if !ok {
					return got
				}
				var env sseEnvelope
				require.NoError(t, json.Unmarshal([]byte(raw), &env))
				got = append(got, env)
			case <-deadline:
				return got
			}
		}
	}

	gotA := collect(streamA)
	gotB := collect(streamB)

	require.Len(t, gotA, 1, "user A should receive only their own notification")
	assert.Equal(t, "user."+userA+".notification", gotA[0].Subject)
	assert.Equal(t, "notif-for-a", gotA[0].Data.ID)
	assert.Equal(t, userA, gotA[0].Data.SubscriberID)

	require.Len(t, gotB, 1, "user B should receive only their own notification")
	assert.Equal(t, "user."+userB+".notification", gotB[0].Subject)
	assert.Equal(t, "notif-for-b", gotB[0].Data.ID)
	assert.Equal(t, userB, gotB[0].Data.SubscriberID)
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

// TestUserNotification_ScopedToSubscriber pins PublishUserNotification
// (SCHEDULE_BLOCKED, ptone/scion#3701) to the recipient's subject: a
// bystander subscribed to the broadcast, project and another user's
// subjects receives nothing, and an unaddressed notification is dropped.
func TestUserNotification_ScopedToSubscriber(t *testing.T) {
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)

	const projectID = "proj-1"
	owner, unsubOwner := pub.Subscribe("user.user-alice.notification")
	defer unsubOwner()
	eve, unsubEve := pub.Subscribe(bystanderSubjects, "project."+projectID+".notification", "user.user-eve.notification")
	defer unsubEve()
	everything, unsubAll := pub.Subscribe(">")
	defer unsubAll()

	notif := newUserNotificationForTest("user-alice", projectID, `Schedule "nightly" is blocked`)
	notif.Status = NotificationScheduleBlocked
	notif.AgentID = "agent-1"
	pub.PublishUserNotification(context.Background(), notif)
	unaddressed := *notif
	unaddressed.SubscriberID = ""
	pub.PublishUserNotification(context.Background(), &unaddressed)

	select {
	case evt := <-owner:
		assert.Equal(t, "user.user-alice.notification", evt.Subject)
		var payload UserNotificationEvent
		require.NoError(t, json.Unmarshal(evt.Data, &payload))
		assert.Equal(t, NotificationScheduleBlocked, payload.Status)
		assert.Equal(t, "agent-1", payload.AgentID)
		assert.Equal(t, "user-alice", payload.SubscriberID)
	case <-time.After(2 * time.Second):
		t.Fatal("recipient did not receive the notification")
	}
	select {
	case evt := <-owner:
		t.Fatalf("unexpected second event on %q", evt.Subject)
	case evt := <-eve:
		t.Fatalf("user notification leaked to a bystander on %q: %s", evt.Subject, evt.Data)
	case <-time.After(250 * time.Millisecond):
	}
	// Exactly one event anywhere: the unaddressed notification went nowhere.
	var all []Event
	for {
		select {
		case evt := <-everything:
			all = append(all, evt)
			continue
		default:
		}
		break
	}
	require.Len(t, all, 1, "events: %+v", all)
	assert.Equal(t, "user.user-alice.notification", all[0].Subject)
}
