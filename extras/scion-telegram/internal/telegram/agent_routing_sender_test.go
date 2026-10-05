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

package telegram

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRoutingTestBroker returns a broker with a group (-200) linked to proj-1
// whose default agent is "coder".
func newRoutingTestBroker(t *testing.T) (*TelegramBrokerV2, *fakeTGServerV2, *fakeHubClient) {
	t.Helper()
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	b := newTestBrokerV2WithHub(t, tgSrv, hub)
	saveTestGroupLink(t, b.store, -200, "proj-1", "my-project", "coder")
	b.InboundHandler = func(string, *messages.StructuredMessage) {}
	return b, tgSrv, hub
}

func plainGroupMessage(fromID int64, text string) *TGMessage {
	return &TGMessage{
		MessageID: 11,
		From:      &TGUser{ID: fromID, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      text,
	}
}

func saveStaleAgentCache(t *testing.T, store Store, projectID string, slugs ...string) {
	t.Helper()
	agents := make([]AgentInfo, len(slugs))
	for i, s := range slugs {
		agents[i] = AgentInfo{Slug: s}
	}
	require.NoError(t, store.SaveProjectAgents(context.Background(), &ProjectAgents{
		ProjectID:   projectID,
		Agents:      agents,
		RefreshedAt: time.Now().Add(-time.Hour),
	}))
}

func TestV2_AgentRefresh_RunsAsMessageSender(t *testing.T) {
	b, _, hub := newRoutingTestBroker(t)
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	principal := linkTestUser(t, b.store, 456, "alice@example.com")

	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	calls := hub.agentCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, fakeListAgentsCall{ProjectID: "proj-1", OnBehalfOf: principal}, calls[0])

	// The refreshed list is cached per project for all senders.
	cached, err := b.store.GetProjectAgents(context.Background(), "proj-1")
	require.NoError(t, err)
	require.NotNil(t, cached)
	assert.Equal(t, []string{"coder"}, agentSlugs(cached.Agents))
}

func TestV2_AgentRefresh_EachSenderActsAsThemselves(t *testing.T) {
	b, _, hub := newRoutingTestBroker(t)
	b.agentCacheTTL = 0 // refresh on every message
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	alice := linkTestUser(t, b.store, 456, "alice@example.com")
	bob := linkTestUser(t, b.store, 789, "bob@example.com")

	b.handleGroupMessage(plainGroupMessage(456, "hello"))
	b.handleGroupMessage(plainGroupMessage(789, "hi"))

	calls := hub.agentCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, alice, calls[0].OnBehalfOf)
	assert.Equal(t, bob, calls[1].OnBehalfOf)
}

func TestV2_AgentRefresh_UnlinkedSenderUsesCacheWithoutHubCall(t *testing.T) {
	b, _, hub := newRoutingTestBroker(t)
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	saveStaleAgentCache(t, b.store, "proj-1", "coder")

	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	assert.Empty(t, hub.agentCalls(), "no hub read without a linked sender")
}

func TestV2_AgentRefresh_UnlinkedSenderWithoutCacheGetsRegisterHint(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}

	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	assert.Empty(t, hub.agentCalls(), "no hub read without a linked sender")
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "/register")
	assert.NotContains(t, sent[0].Text, "no longer available")
}

func TestV2_AgentRefresh_FailedListIsNotReportedAsMissingDefault(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	hub.listAgentsErr = errors.New("list agents returned status 500")
	linkTestUser(t, b.store, 456, "alice@example.com")

	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.NotContains(t, sent[0].Text, "no longer available")
	assert.Contains(t, sent[0].Text, "Couldn't fetch the agent list")
}

func TestV2_AgentRefresh_FailedListIsNotReportedAsMissingAgent(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	hub.listAgentsErr = errors.New("list agents returned status 500")
	linkTestUser(t, b.store, 456, "alice@example.com")
	saveTestGroupLink(t, b.store, -200, "proj-1", "my-project", "")

	msg := plainGroupMessage(456, "@test_bot @reviewer please look")
	msg.Entities = []MessageEntity{{Type: "mention", Offset: 0, Length: 9}}
	b.handleGroupMessage(msg)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.NotContains(t, sent[0].Text, "No agent named")
	assert.Contains(t, sent[0].Text, "Couldn't fetch the agent list")
}

func TestV2_AgentRefresh_FailedListFallsBackToCache(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	hub.listAgentsErr = errors.New("list agents returned status 500")
	saveStaleAgentCache(t, b.store, "proj-1", "coder")
	linkTestUser(t, b.store, 456, "alice@example.com")

	delivered := make(chan string, 1)
	b.InboundHandler = func(topic string, _ *messages.StructuredMessage) { delivered <- topic }
	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	select {
	case topic := <-delivered:
		assert.Equal(t, "scion.project.proj-1.agent.coder.messages", topic)
	case <-time.After(2 * time.Second):
		t.Fatalf("message not routed; sent=%v", tgSrv.getSentMessages())
	}
}
