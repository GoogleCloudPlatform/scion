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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// A human with a profile time zone who writes in native chat, in a DM with
// an agent or in a topic routed to its default agent, has the zone in the
// dispatched message; a client-supplied value never reaches the agent.
func TestSenderTimezone_NativeChatSend(t *testing.T) {
	f := newSendFixture(t)
	setUserTimezone(t, f.st, f.ua, "Asia/Tokyo")
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.target.Slug}))
	agentDM := dmKeyFor(t, "user", f.ua.ID, "agent", f.target.ID)

	for _, key := range []string{agentDM, f.topicA} {
		got := f.send(t, f.ua, key, map[string]interface{}{
			"content":  "hello",
			"metadata": map[string]string{messages.MetadataSenderTimezone: "Pacific/Fake"},
		})
		require.Equal(t, http.StatusCreated, got.status, "%s: %s", key, got.body)
		sm := f.lastDispatch(t, f.target).StructuredMessage
		require.NotNil(t, sm, key)
		assert.Equal(t, "Asia/Tokyo", sm.Metadata[messages.MetadataSenderTimezone], key)
		assert.NotContains(t, sm.DeliveryText, "Pacific/Fake", key)
		assert.NotContains(t, messages.FormatForDelivery(sm), "Pacific/Fake", key)
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

	rec = f.outbound(t, f.sender, OutboundMessageRequest{
		Recipient: "agent:" + f.target.Slug, Msg: "outbound hello", Type: messages.TypeInstruction, Metadata: spoof,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sm = f.lastDispatch(t, f.target).StructuredMessage
	require.NotNil(t, sm)
	assert.Equal(t, "outbound hello", sm.Msg)
	assert.NotContains(t, sm.Metadata, messages.MetadataSenderTimezone)
	assert.NotContains(t, sm.DeliveryText, "sender_timezone")
}
