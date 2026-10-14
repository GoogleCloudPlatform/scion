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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishInviteChanged_PayloadIsIDAndActionOnly checks that the
// admin.invite.changed payload carries only the invite id and the action, for
// every action, and no invite code fields.
func TestPublishInviteChanged_PayloadIsIDAndActionOnly(t *testing.T) {
	pub := NewChannelEventPublisher()
	ch, unsub := pub.Subscribe("admin.invite.changed")
	defer unsub()

	for _, action := range []string{"created", "redeemed", "revoked", "deleted"} {
		t.Run(action, func(t *testing.T) {
			pub.PublishInviteChanged(context.Background(), action, "invite-1")

			var evt Event
			select {
			case evt = <-ch:
			default:
				t.Fatalf("no admin.invite.changed event published for action %q", action)
			}
			assert.Equal(t, "admin.invite.changed", evt.Subject)

			var payload map[string]any
			require.NoError(t, json.Unmarshal(evt.Data, &payload))
			assert.Equal(t, map[string]any{"action": action, "inviteId": "invite-1"}, payload,
				"invite-changed payload must contain exactly action and inviteId")
		})
	}
}
