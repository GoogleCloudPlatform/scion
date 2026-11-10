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
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
)

// A human with a profile time zone who writes in native chat, in a DM with
// an agent or in a topic routed to its default agent, has the zone in the
// dispatched message; a client-supplied value never reaches the agent.
func TestSenderTimezone_NativeChatSend(t *testing.T) {
	f := newSendFixture(t)
	enableWriteDenySwitch(t, f.srv) // render the hub envelope, so it is checked too
	setUserTimezone(t, f.st, f.ua, "Asia/Tokyo")
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.target.Slug}))
	agentDM := dmKeyFor(t, "user", f.ua.ID, "agent", f.target.ID)

	for route, key := range map[string]string{"agent DM": agentDM, "topic": f.topicA} {
		content := "hello via " + route
		before := len(f.disp.getCalls())
		got := f.send(t, f.ua, key, map[string]interface{}{
			"content":  content,
			"metadata": map[string]string{messages.MetadataSenderTimezone: "Pacific/Fake"},
		})
		require.Equal(t, http.StatusCreated, got.status, "%s: %s", route, got.body)
		require.Greater(t, len(f.disp.getCalls()), before, "%s: the send reached the agent", route)
		sm := f.lastDispatch(t, f.target).StructuredMessage
		require.NotNil(t, sm, route)
		require.Equal(t, content, sm.Msg, "%s: this route's own dispatch", route)
		assert.Equal(t, "Asia/Tokyo", sm.Metadata[messages.MetadataSenderTimezone], route)
		require.NotEmpty(t, sm.DeliveryText, "%s: the envelope switch is on", route)
		assert.Contains(t, sm.DeliveryText, `"sender_timezone": "Asia/Tokyo"`, route)
		assert.NotContains(t, sm.DeliveryText, "Pacific/Fake", route)
		assert.NotContains(t, messages.FormatForDelivery(sm), "Pacific/Fake", route)
	}
}

// An agent's send never carries a sender time zone: a value the agent
// supplies is dropped on both agent send routes, and the agent's owner's
// profile zone is not applied either.
func TestSenderTimezone_AgentSenderSpoofStripped(t *testing.T) {
	f := newSendFixture(t)
	setUserTimezone(t, f.st, f.ua, "Asia/Tokyo") // the agents' owner
	spoof := map[string]string{messages.MetadataSenderTimezone: "Pacific/Fake"}

	body := f.messageBody(f.target, "structured hello", nil)
	body.StructuredMessage.Metadata = spoof
	rec := f.agentRequest(t, f.sender, jsonRequest(t, "/api/v1/agents/"+f.target.ID+"/message", body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sm := f.lastDispatch(t, f.target).StructuredMessage
	require.NotNil(t, sm)
	assert.Equal(t, "structured hello", sm.Msg)
	assert.NotContains(t, sm.Metadata, messages.MetadataSenderTimezone)
	assert.NotContains(t, sm.DeliveryText, "sender_timezone")

	// The outbound route reaches another agent through a conversation
	// reference (as the agent-to-agent parity tests do).
	dm, err := messaging.ResolveOrCreateDMConversation(context.Background(), f.st, f.st, slog.Default(),
		"agent", f.sender.ID, "agent", f.target.ID)
	require.NoError(t, err)
	rec = f.outbound(t, f.sender, OutboundMessageRequest{
		ConversationRef: "conv:" + dm.ConversationID, Msg: "outbound hello", Type: messages.TypeInstruction, Metadata: spoof,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sm = f.lastDispatch(t, f.target).StructuredMessage
	require.NotNil(t, sm)
	assert.Equal(t, "outbound hello", sm.Msg)
	assert.NotContains(t, sm.Metadata, messages.MetadataSenderTimezone)
	assert.NotContains(t, sm.DeliveryText, "sender_timezone")
}
