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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNotificationDispatcher_RowNamesAgentAndNoKind pins that the
// subscription-triggered writer stores the watched agent's real id and no
// kind or resource (ptone/scion#5145). A notification GC selects rows by
// AgentID or SubscriberType, never by Kind, and relies on this writer
// naming a real agent.
func TestNotificationDispatcher_RowNamesAgentAndNoKind(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	var notifs []store.Notification
	require.Eventually(t, func() bool {
		var err error
		notifs, err = env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
		return err == nil && len(notifs) == 1
	}, 2*time.Second, 50*time.Millisecond)

	n := notifs[0]
	assert.Equal(t, env.watched.ID, n.AgentID)
	assert.NotEqual(t, store.CreatorFallbackSubscriptionID, n.AgentID)
	assert.Empty(t, n.Kind)
	assert.Empty(t, n.ResourceType)
	assert.Empty(t, n.ResourceID)
}
