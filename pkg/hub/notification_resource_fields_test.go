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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Notification kind and resource fields (ptone/scion#5145): they reach the
// addressed user's own subject only, and every existing event payload is
// unchanged.

// resourceNotification is a notification that sets the kind and resource
// fields, addressed to user u1.
func resourceNotification() *store.Notification {
	return &store.Notification{
		ID:             "n-kind",
		SubscriptionID: store.CreatorFallbackSubscriptionID,
		AgentID:        store.CreatorFallbackSubscriptionID,
		ProjectID:      "g1",
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "u1",
		Status:         store.NotificationStatusArtifactReviewRequested,
		Message:        "A user asked you to review an artifact (Can review): scion://artifact/5f1c2d3e-0000-4000-8000-000000000001",
		CreatedAt:      time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		Kind:           store.NotificationKindArtifactGrant,
		ResourceType:   store.NotificationResourceArtifact,
		ResourceID:     "5f1c2d3e-0000-4000-8000-000000000001",
	}
}

// receiveRaw returns the raw JSON of the next event on ch.
func receiveRaw(t *testing.T, ch <-chan Event) map[string]json.RawMessage {
	t.Helper()
	select {
	case evt := <-ch:
		var keys map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(evt.Data, &keys))
		return keys
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
		return nil
	}
}

var resourceEventKeys = []string{"kind", "resourceType", "resourceId"}

// TestNotificationCreatedEvent_HasNoResourceFields pins the struct: the
// broadcast payload type, which UserNotificationEvent embeds, declares no
// field whose JSON name is a kind or resource key. A field there would be
// promoted into the user event and could be set on the broadcast subjects
// too.
func TestNotificationCreatedEvent_HasNoResourceFields(t *testing.T) {
	typ := reflect.TypeOf(NotificationCreatedEvent{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}
		for _, k := range resourceEventKeys {
			if strings.EqualFold(name, k) {
				t.Errorf("NotificationCreatedEvent.%s has JSON name %q, a user-event-only key", f.Name, name)
			}
		}
	}
}

// TestNotificationEvents_ResourceFieldsOnUserSubjectOnly pins that the kind
// and resource fields are fields of UserNotificationEvent alone: the
// broadcast notification.created and project.<id>.notification payloads,
// which reach other sessions, never carry them, even for a notification
// that sets them.
func TestNotificationEvents_ResourceFieldsOnUserSubjectOnly(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()

	broadcast, unsubB := pub.Subscribe("notification.>")
	defer unsubB()
	project, unsubP := pub.Subscribe("project.g1.notification")
	defer unsubP()
	user, unsubU := pub.Subscribe("user.u1.notification")
	defer unsubU()

	ctx := context.Background()
	n := resourceNotification()

	pub.PublishNotification(ctx, n)
	for name, ch := range map[string]<-chan Event{"notification.created": broadcast, "project.g1.notification": project} {
		keys := receiveRaw(t, ch)
		for _, k := range resourceEventKeys {
			assert.NotContains(t, keys, k, "%s must not carry %q", name, k)
		}
	}

	pub.PublishUserNotification(ctx, n)
	keys := receiveRaw(t, user)
	var got UserNotificationEvent
	raw, err := json.Marshal(keys)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, store.NotificationKindArtifactGrant, got.Kind)
	assert.Equal(t, store.NotificationResourceArtifact, got.ResourceType)
	assert.Equal(t, n.ResourceID, got.ResourceID)
	assert.Equal(t, "u1", got.SubscriberID)
}

// TestNotificationEvents_ExistingPayloadsUnchanged pins that a notification
// without kind or resource fields produces the same user and broadcast
// payloads as before those fields existed.
func TestNotificationEvents_ExistingPayloadsUnchanged(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()

	broadcast, unsubB := pub.Subscribe("notification.created")
	defer unsubB()
	user, unsubU := pub.Subscribe("user.u1.notification")
	defer unsubU()

	n := &store.Notification{
		ID:             "n1",
		AgentID:        "a1",
		ProjectID:      "g1",
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "u1",
		Status:         "SCHEDULE_BLOCKED",
		Message:        "Schedule \"nightly\" is blocked",
		CreatedAt:      time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
	ctx := context.Background()
	pub.PublishNotification(ctx, n)
	pub.PublishUserNotification(ctx, n)

	wantBroadcast := `{"id":"n1","agentId":"a1","projectId":"g1","status":"SCHEDULE_BLOCKED",` +
		`"message":"Schedule \"nightly\" is blocked","createdAt":"2026-03-01T12:00:00.000Z"}`
	wantUser := `{"id":"n1","agentId":"a1","projectId":"g1","status":"SCHEDULE_BLOCKED",` +
		`"message":"Schedule \"nightly\" is blocked","createdAt":"2026-03-01T12:00:00.000Z","subscriberId":"u1"}`

	for _, c := range []struct {
		name string
		ch   <-chan Event
		want string
	}{
		{"broadcast", broadcast, wantBroadcast},
		{"user", user, wantUser},
	} {
		select {
		case evt := <-c.ch:
			assert.JSONEq(t, c.want, string(evt.Data), c.name)
			assert.Equal(t, c.want, string(evt.Data), "%s payload bytes", c.name)
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for %s event", c.name)
		}
	}
}
