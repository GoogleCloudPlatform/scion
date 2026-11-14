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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The subscription a create with notify=true records includes IDLE, so a
// parent hears about a child that goes idle without task_completed.
func TestNewNotifySubscription_DefaultTriggersIncludeIdle(t *testing.T) {
	sub := newNotifySubscription("project-1", store.SubscriberTypeAgent, "parent", "parent")
	require.NotNil(t, sub)
	assert.True(t, sub.MatchesActivity("idle"), "default triggers %v must match idle", sub.TriggerActivities)
}

// A subscribed parent with the create-default triggers receives an IDLE
// notification, with the child's message, when the child goes idle.
func TestNotificationDispatcher_IdleNotifiesSubscribedParent(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID,
		newNotifySubscription(env.project.ID, store.SubscriberTypeAgent, env.subscriber.Slug, "test").TriggerActivities))
	require.NoError(t, env.store.UpdateAgentStatus(ctx, env.watched.ID, store.AgentStatusUpdate{
		Activity: "idle", Message: "Between assignments",
	}))
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("idle")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)
	assert.Equal(t, env.subscriber.ID, env.dispatcher.getCalls()[0].Agent.ID)
	assert.Contains(t, env.dispatcher.getCalls()[0].Message, "watched-agent is IDLE: Between assignments")

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, "IDLE", notifs[0].Status)
}
