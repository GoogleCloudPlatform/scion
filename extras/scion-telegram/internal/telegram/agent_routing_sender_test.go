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
	"fmt"
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

func saveAgentCache(t *testing.T, store Store, user, projectID string, refreshedAt time.Time, slugs ...string) {
	t.Helper()
	agents := make([]AgentInfo, len(slugs))
	for i, s := range slugs {
		agents[i] = AgentInfo{Slug: s}
	}
	require.NoError(t, store.SaveProjectAgents(context.Background(), &ProjectAgents{
		User:        user,
		ProjectID:   projectID,
		Agents:      agents,
		RefreshedAt: refreshedAt,
	}))
}

// saveStaleAgentCache caches a list for user that is past the routing TTL
// but still within the retention window.
func saveStaleAgentCache(t *testing.T, store Store, user, projectID string, slugs ...string) {
	t.Helper()
	saveAgentCache(t, store, user, projectID, time.Now().Add(-30*time.Minute), slugs...)
}

func botMentionMessage(fromID int64, text string) *TGMessage {
	msg := plainGroupMessage(fromID, "@test_bot "+text)
	msg.Entities = []MessageEntity{{Type: "mention", Offset: 0, Length: 9}}
	return msg
}

func replyToBotMessage(fromID int64, text string) *TGMessage {
	msg := plainGroupMessage(fromID, text)
	msg.ReplyToMessage = &TGMessage{MessageID: 5, From: &TGUser{ID: 100, IsBot: true}, Text: "🤖 coder: plan ready"}
	return msg
}

func TestV2_AgentRefresh_RunsAsMessageSender(t *testing.T) {
	b, _, hub := newRoutingTestBroker(t)
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	principal := linkTestUser(t, b.store, 456, "alice@example.com")

	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	calls := hub.agentCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, fakeListAgentsCall{ProjectID: "proj-1", OnBehalfOf: principal}, calls[0])

	// The refreshed list is cached for the sender only.
	cached, err := b.store.GetProjectAgents(context.Background(), principal, "proj-1")
	require.NoError(t, err)
	require.NotNil(t, cached)
	assert.Equal(t, []string{"coder"}, agentSlugs(cached.Agents))
	other, err := b.store.GetProjectAgents(context.Background(), "user:bob@example.com", "proj-1")
	require.NoError(t, err)
	assert.Nil(t, other)
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

func TestV2_AgentCache_PerUser_FreshListIsNotServedToAnotherUser(t *testing.T) {
	b, _, hub := newRoutingTestBroker(t)
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	alice := linkTestUser(t, b.store, 456, "alice@example.com")
	bob := linkTestUser(t, b.store, 789, "bob@example.com")
	saveAgentCache(t, b.store, alice, "proj-1", time.Now(), "coder")

	b.handleGroupMessage(plainGroupMessage(789, "hi"))

	calls := hub.agentCalls()
	require.Len(t, calls, 1, "bob fetches his own list")
	assert.Equal(t, bob, calls[0].OnBehalfOf)
}

func TestV2_AgentCache_PerUser_FreshListIsReusedForSameUser(t *testing.T) {
	b, _, hub := newRoutingTestBroker(t)
	alice := linkTestUser(t, b.store, 456, "alice@example.com")
	saveAgentCache(t, b.store, alice, "proj-1", time.Now(), "coder")
	delivered := make(chan string, 1)
	b.InboundHandler = func(topic string, _ *messages.StructuredMessage) { delivered <- topic }

	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	select {
	case topic := <-delivered:
		assert.Equal(t, "scion.project.proj-1.agent.coder.messages", topic)
	case <-time.After(2 * time.Second):
		t.Fatal("message not routed")
	}
	assert.Empty(t, hub.agentCalls())
}

func TestV2_AgentCache_PerUser_StaleListIsNotServedToAnotherUser(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	hub.listAgentsErr = errors.New("list agents returned status 500")
	alice := linkTestUser(t, b.store, 456, "alice@example.com")
	linkTestUser(t, b.store, 789, "bob@example.com")
	saveStaleAgentCache(t, b.store, alice, "proj-1", "coder")
	delivered := false
	b.InboundHandler = func(string, *messages.StructuredMessage) { delivered = true }

	b.handleGroupMessage(plainGroupMessage(789, "hi"))

	assert.False(t, delivered, "bob is not routed with alice's list")
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "Couldn't fetch the agent list")
}

func TestV2_AgentCache_PerUser_DeniedUserDoesNotSeeAnothersList(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	saveTestGroupLink(t, b.store, -200, "proj-1", "my-project", "")
	hub.listAgentsErr = forbiddenListAgents()
	alice := linkTestUser(t, b.store, 456, "alice@example.com")
	linkTestUser(t, b.store, 789, "bob@example.com")
	saveAgentCache(t, b.store, alice, "proj-1", time.Now(), "coder", "reviewer")
	delivered := false
	b.InboundHandler = func(string, *messages.StructuredMessage) { delivered = true }

	b.handleGroupMessage(botMentionMessage(789, "@reviewer hi"))

	assert.False(t, delivered)
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, "Your Scion account (bob@example.com) doesn't have permission to list agents in my-project. Ask a project owner.", sent[0].Text)
	assert.NotContains(t, sent[0].Text, "coder")
}

// lookupFailedText is the reply when the sender's link cannot be read.
const lookupFailedText = "Something went wrong. Please try again."

// unresolvedSenders put sender 456 in each state the plugin cannot act as,
// with the reply expected when the sender is answered. repliesToDefault
// marks the states that are also answered for a message that would go to
// the default agent.
var unresolvedSenders = map[string]struct {
	setup            func(t *testing.T, b *TelegramBrokerV2)
	from             func(msg *TGMessage)
	want             string
	repliesToDefault bool
}{
	"unlinked":       {func(*testing.T, *TelegramBrokerV2) {}, nil, registerHint, false},
	"unknown sender": {func(*testing.T, *TelegramBrokerV2) {}, func(msg *TGMessage) { msg.From = nil }, registerHint, false},
	"link without email": {func(t *testing.T, b *TelegramBrokerV2) {
		require.NoError(t, b.store.SaveUserMapping(context.Background(), &TelegramUserMapping{
			TelegramUserID: "456", ScionUserID: "u-456", LinkedAt: time.Now().UTC(),
		}))
	}, nil, staleLinkText, true},
	"lookup failure": {func(_ *testing.T, b *TelegramBrokerV2) { b.store = mappingLookupFailingStore{b.store} }, nil, lookupFailedText, true},
}

// unresolvedSenderMessages are the message shapes sent by an unresolved
// sender. toDefault marks messages that would go to the default agent.
var unresolvedSenderMessages = map[string]struct {
	msg          func() *TGMessage
	defaultAgent string
	addressed    bool
	toDefault    bool
}{
	"plain text, default agent":    {func() *TGMessage { return plainGroupMessage(456, "hello") }, "coder", false, true},
	"plain text, no default agent": {func() *TGMessage { return plainGroupMessage(456, "hello") }, "", false, false},
	"attachment, default agent": {func() *TGMessage {
		msg := plainGroupMessage(456, "")
		msg.Photo = []PhotoSize{{FileID: "photo-1", Width: 100, Height: 100}}
		return msg
	}, "coder", false, true},
	"command-like text, default agent": {func() *TGMessage { return plainGroupMessage(456, "/notacommand") }, "coder", false, false},
	"agent mention":                    {func() *TGMessage { return plainGroupMessage(456, "@coder hello") }, "coder", false, false},
	"unknown @token":                   {func() *TGMessage { return plainGroupMessage(456, "hey @reviewer look") }, "", false, false},
	"bot mention":                      {func() *TGMessage { return botMentionMessage(456, "hello") }, "coder", true, false},
	"bot mention, no default agent":    {func() *TGMessage { return botMentionMessage(456, "hello") }, "", true, false},
	"bot mention plus agent":           {func() *TGMessage { return botMentionMessage(456, "@coder hello") }, "coder", true, false},
	"reply to bot message":             {func() *TGMessage { return replyToBotMessage(456, "go ahead") }, "", true, false},
}

// agentCacheStates are the agent-cache contents an unresolved sender's
// message is checked against.
var agentCacheStates = map[string]func(t *testing.T, store Store){
	"fresh lists of other users": func(t *testing.T, store Store) {
		saveAgentCache(t, store, "user:bob@example.com", "proj-1", time.Now(), "coder", "reviewer")
		saveAgentCache(t, store, "user:carol@example.com", "proj-1", time.Now(), "coder")
	},
	"stale lists of other users": func(t *testing.T, store Store) {
		saveStaleAgentCache(t, store, "user:bob@example.com", "proj-1", "coder", "reviewer")
		saveStaleAgentCache(t, store, "user:carol@example.com", "proj-1", "coder")
	},
	// 456 was formerly linked as alice@example.com.
	"fresh list under the sender's former account": func(t *testing.T, store Store) {
		saveAgentCache(t, store, "user:alice@example.com", "proj-1", time.Now(), "coder", "reviewer")
	},
	"stale list under the sender's former account": func(t *testing.T, store Store) {
		saveStaleAgentCache(t, store, "user:alice@example.com", "proj-1", "coder", "reviewer")
	},
}

// unresolvedSenderReplies sends msg from an unresolved sender in a group
// with defaultAgent and returns the replies. cache, when set, fills the
// agent cache first.
func unresolvedSenderReplies(t *testing.T, sender string, msgName string, cache func(*testing.T, Store)) []string {
	t.Helper()
	sc := unresolvedSenders[sender]
	mc := unresolvedSenderMessages[msgName]
	b, tgSrv, hub := newRoutingTestBroker(t)
	saveTestGroupLink(t, b.store, -200, "proj-1", "my-project", mc.defaultAgent)
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	if cache != nil {
		cache(t, b.store)
	}
	sc.setup(t, b)
	delivered := false
	b.InboundHandler = func(string, *messages.StructuredMessage) { delivered = true }
	msg := mc.msg()
	if sc.from != nil {
		sc.from(msg)
	}

	b.handleGroupMessage(msg)

	assert.False(t, delivered, "an unresolved sender is never routed")
	assert.Empty(t, hub.agentCalls(), "no hub call for an unresolved sender")
	replies := []string{}
	for _, m := range tgSrv.getSentMessages() {
		replies = append(replies, m.Text)
	}
	return replies
}

func TestV2_UnresolvedSender_Replies(t *testing.T) {
	for senderName, sc := range unresolvedSenders {
		for msgName, mc := range unresolvedSenderMessages {
			t.Run(senderName+"/"+msgName, func(t *testing.T) {
				replies := unresolvedSenderReplies(t, senderName, msgName, nil)
				if mc.addressed || (sc.repliesToDefault && mc.toDefault) {
					assert.Equal(t, []string{sc.want}, replies)
				} else {
					assert.Empty(t, replies, "no reply")
				}
			})
		}
	}
}

func TestV2_UnresolvedSender_SameReplyWhetherOrNotAgentsCached(t *testing.T) {
	for senderName := range unresolvedSenders {
		for msgName := range unresolvedSenderMessages {
			for cacheName, cache := range agentCacheStates {
				t.Run(senderName+"/"+msgName+"/"+cacheName, func(t *testing.T) {
					uncached := unresolvedSenderReplies(t, senderName, msgName, nil)
					cached := unresolvedSenderReplies(t, senderName, msgName, cache)
					assert.Equal(t, uncached, cached, "same replies with and without cached agents")
				})
			}
		}
	}
}

func TestV2_UnresolvedSender_NeverSeenSenderUnaddressedTextGetsNoReply(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t) // default agent "coder"
	delivered := false
	b.InboundHandler = func(string, *messages.StructuredMessage) { delivered = true }

	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	assert.False(t, delivered)
	assert.Empty(t, hub.agentCalls())
	assert.Empty(t, tgSrv.getSentMessages())
}

func TestV2_UnresolvedSender_DefaultAgentReplyIsThrottledPerSender(t *testing.T) {
	for _, senderName := range []string{"link without email", "lookup failure"} {
		t.Run(senderName, func(t *testing.T) {
			sc := unresolvedSenders[senderName]
			b, tgSrv, _ := newRoutingTestBroker(t) // default agent "coder"
			sc.setup(t, b)
			if senderName == "lookup failure" {
				// Another sender whose link cannot be read either.
				require.NoError(t, b.store.(mappingLookupFailingStore).Store.SaveUserMapping(context.Background(), &TelegramUserMapping{
					TelegramUserID: "789", ScionUserID: "u-789", LinkedAt: time.Now().UTC(),
				}))
			} else {
				require.NoError(t, b.store.SaveUserMapping(context.Background(), &TelegramUserMapping{
					TelegramUserID: "789", ScionUserID: "u-789", LinkedAt: time.Now().UTC(),
				}))
			}

			b.handleGroupMessage(plainGroupMessage(456, "hello"))
			b.handleGroupMessage(plainGroupMessage(456, "hello again"))
			b.handleGroupMessage(plainGroupMessage(789, "hi"))
			b.handleGroupMessage(botMentionMessage(456, "are you there"))

			assert.Equal(t, []string{sc.want, sc.want, sc.want}, sentTexts(tgSrv),
				"one reply per sender for unaddressed text; addressing the bot is always answered")
		})
	}
}

func sentTexts(tgSrv *fakeTGServerV2) []string {
	var out []string
	for _, m := range tgSrv.getSentMessages() {
		out = append(out, m.Text)
	}
	return out
}

func TestV2_UnresolvedSender_LookupFailureReplyIsGeneric(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprintf("linked=%v", linked), func(t *testing.T) {
			for _, msg := range []*TGMessage{plainGroupMessage(456, "hello"), botMentionMessage(456, "@coder hello")} {
				b, tgSrv, _ := newRoutingTestBroker(t)
				if linked {
					linkTestUser(t, b.store, 456, "alice@example.com")
				}
				saveAgentCache(t, b.store, "user:alice@example.com", "proj-1", time.Now(), "coder")
				b.store = mappingLookupFailingStore{b.store}

				b.handleGroupMessage(msg)

				sent := tgSrv.getSentMessages()
				require.Len(t, sent, 1)
				assert.Equal(t, lookupFailedText, sent[0].Text)
			}
		})
	}
}

func TestV2_UnresolvedSender_LinkWithoutEmailGetsReregisterText(t *testing.T) {
	for _, msg := range []*TGMessage{plainGroupMessage(456, "hello"), botMentionMessage(456, "@coder hello"), replyToBotMessage(456, "go ahead")} {
		b, tgSrv, _ := newRoutingTestBroker(t)
		saveAgentCache(t, b.store, "user:bob@example.com", "proj-1", time.Now(), "coder")
		require.NoError(t, b.store.SaveUserMapping(context.Background(), &TelegramUserMapping{
			TelegramUserID: "456", ScionUserID: "u-456", LinkedAt: time.Now().UTC(),
		}))

		b.handleGroupMessage(msg)

		sent := tgSrv.getSentMessages()
		require.Len(t, sent, 1)
		assert.Equal(t, staleLinkText, sent[0].Text)
	}
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

	b.handleGroupMessage(botMentionMessage(456, "@reviewer please look"))

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.NotContains(t, sent[0].Text, "No agent named")
	assert.Contains(t, sent[0].Text, "Couldn't fetch the agent list")
}

func TestV2_AgentRefresh_FailedListFallsBackToSameUsersCache(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	hub.listAgentsErr = errors.New("list agents returned status 500")
	alice := linkTestUser(t, b.store, 456, "alice@example.com")
	saveStaleAgentCache(t, b.store, alice, "proj-1", "coder")

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

// mappingLookupFailingStore fails every link-mapping lookup.
type mappingLookupFailingStore struct{ Store }

func (mappingLookupFailingStore) GetUserMapping(context.Context, string) (*TelegramUserMapping, error) {
	return nil, errors.New("database is locked")
}

// mappingLookupCountingStore counts link-mapping lookups.
type mappingLookupCountingStore struct {
	Store
	lookups int
}

func (s *mappingLookupCountingStore) GetUserMapping(ctx context.Context, id string) (*TelegramUserMapping, error) {
	s.lookups++
	return s.Store.GetUserMapping(ctx, id)
}

func TestV2_AgentRefresh_StaleCacheRefreshesAsSender(t *testing.T) {
	b, _, hub := newRoutingTestBroker(t)
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}}
	principal := linkTestUser(t, b.store, 456, "alice@example.com")
	saveStaleAgentCache(t, b.store, principal, "proj-1", "coder")

	slugs, err := b.getProjectAgents(context.Background(), "proj-1", b.lookupSender(context.Background(), &TGUser{ID: 456}))
	require.NoError(t, err)
	assert.Equal(t, []string{"coder", "reviewer"}, slugs)
	calls := hub.agentCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, principal, calls[0].OnBehalfOf)
}

func TestV2_Routing_LooksUpSenderOnce(t *testing.T) {
	b, _, hub := newRoutingTestBroker(t)
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	principal := linkTestUser(t, b.store, 456, "alice@example.com")
	saveStaleAgentCache(t, b.store, principal, "proj-1", "coder")
	counting := &mappingLookupCountingStore{Store: b.store}
	b.store = counting
	delivered := make(chan *messages.StructuredMessage, 1)
	b.InboundHandler = func(_ string, m *messages.StructuredMessage) { delivered <- m }

	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	select {
	case m := <-delivered:
		assert.Equal(t, "user:alice@example.com", m.Sender)
	case <-time.After(2 * time.Second):
		t.Fatal("message not routed")
	}
	assert.Equal(t, 1, counting.lookups, "the sender is looked up once per message")
}

func TestAgentListErrorKind(t *testing.T) {
	assert.Equal(t, "stale_link", agentListErrorKind(staleLinkError("on-behalf-of principal not found")))
	assert.Equal(t, "forbidden", agentListErrorKind(forbiddenListAgents()))
	assert.Equal(t, "unavailable", agentListErrorKind(errors.New("connection refused")))
}

func TestV2_AgentListUnavailable_OneUsersReplyDoesNotSuppressAnothers(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	linkTestUser(t, b.store, 456, "alice@example.com")
	linkTestUser(t, b.store, 789, "bob@example.com")
	hub.listAgentsErr = forbiddenListAgents()

	b.handleGroupMessage(plainGroupMessage(456, "hello"))
	b.handleGroupMessage(plainGroupMessage(789, "hi"))

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 2)
	assert.Contains(t, sent[0].Text, "Your Scion account (alice@example.com) doesn't have permission")
	assert.Contains(t, sent[1].Text, "Your Scion account (bob@example.com) doesn't have permission")
}

func TestV2_AgentListUnavailable_SameUserRepeatIsSuppressed(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	linkTestUser(t, b.store, 456, "alice@example.com")
	hub.listAgentsErr = errors.New("list agents returned status 500")

	b.handleGroupMessage(plainGroupMessage(456, "hello"))
	b.handleGroupMessage(plainGroupMessage(456, "hello again"))

	assert.Len(t, tgSrv.getSentMessages(), 1)
}

func TestV2_AgentRefresh_DeniedSenderIsNotServedFromStaleCache(t *testing.T) {
	errs := map[string]struct {
		err  error
		want string
	}{
		"permission denied": {forbiddenListAgents(), "Your Scion account (alice@example.com) doesn't have permission to list agents in my-project. Ask a project owner."},
		"stale link":        {staleLinkError("on-behalf-of principal not found"), staleLinkText},
	}
	replyToBot := func() *TGMessage {
		msg := plainGroupMessage(456, "thanks, go ahead")
		msg.ReplyToMessage = &TGMessage{MessageID: 5, From: &TGUser{ID: 100, IsBot: true}, Text: "🤖 coder: plan ready"}
		return msg
	}
	cases := map[string]struct {
		msg        func() *TGMessage
		staleCache bool
	}{
		"plain message, stale cache": {func() *TGMessage { return plainGroupMessage(456, "hello") }, true},
		"reply to bot, stale cache":  {replyToBot, true},
		"reply to bot, no cache":     {replyToBot, false},
		"plain message, no cache":    {func() *TGMessage { return plainGroupMessage(456, "hello") }, false},
	}
	for errName, ec := range errs {
		for caseName, tc := range cases {
			t.Run(errName+"/"+caseName, func(t *testing.T) {
				b, tgSrv, hub := newRoutingTestBroker(t)
				hub.listAgentsErr = ec.err
				if tc.staleCache {
					saveStaleAgentCache(t, b.store, "user:alice@example.com", "proj-1", "coder")
				}
				linkTestUser(t, b.store, 456, "alice@example.com")
				delivered := false
				b.InboundHandler = func(string, *messages.StructuredMessage) { delivered = true }

				b.handleGroupMessage(tc.msg())

				assert.False(t, delivered, "a denied sender's message is not delivered")
				sent := tgSrv.getSentMessages()
				require.Len(t, sent, 1)
				assert.Equal(t, ec.want, sent[0].Text)
			})
		}
	}
}

func TestV2_DeniedSender_ChatterNotAddressedToBotIsIgnored(t *testing.T) {
	leadingUserMention := func() *TGMessage {
		msg := plainGroupMessage(456, "@bob anyone up for lunch?")
		msg.Entities = []MessageEntity{{Type: "mention", Offset: 0, Length: 4}}
		return msg
	}
	cases := map[string]func() *TGMessage{
		"plain text":            func() *TGMessage { return plainGroupMessage(456, "anyone up for lunch?") },
		"leading @user mention": leadingUserMention,
	}
	for _, staleCache := range []bool{true, false} {
		for name, mk := range cases {
			t.Run(fmt.Sprintf("%s/stale cache=%v", name, staleCache), func(t *testing.T) {
				b, tgSrv, hub := newRoutingTestBroker(t)
				saveTestGroupLink(t, b.store, -200, "proj-1", "my-project", "") // no default agent
				hub.listAgentsErr = forbiddenListAgents()
				if staleCache {
					saveStaleAgentCache(t, b.store, "user:alice@example.com", "proj-1", "coder")
				}
				linkTestUser(t, b.store, 456, "alice@example.com")
				delivered := false
				b.InboundHandler = func(string, *messages.StructuredMessage) { delivered = true }

				b.handleGroupMessage(mk())

				assert.False(t, delivered)
				assert.Empty(t, tgSrv.getSentMessages(), "no reply to chatter not addressed to the bot")
			})
		}
	}
}

func TestV2_DeniedSender_AddressedMessagesGetDenialText(t *testing.T) {
	const want = "Your Scion account (alice@example.com) doesn't have permission to list agents in my-project. Ask a project owner."
	botMention := func() *TGMessage {
		msg := plainGroupMessage(456, "@test_bot hello")
		msg.Entities = []MessageEntity{{Type: "mention", Offset: 0, Length: 9}}
		return msg
	}
	cases := map[string]struct {
		msg          func() *TGMessage
		defaultAgent string
	}{
		"bot mention with default":    {botMention, "coder"},
		"bot mention without default": {botMention, ""},
		"unknown agent @token":        {func() *TGMessage { return plainGroupMessage(456, "hey @reviewer take a look") }, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, tgSrv, hub := newRoutingTestBroker(t)
			saveTestGroupLink(t, b.store, -200, "proj-1", "my-project", tc.defaultAgent)
			hub.listAgentsErr = forbiddenListAgents()
			saveStaleAgentCache(t, b.store, "user:alice@example.com", "proj-1", "coder")
			linkTestUser(t, b.store, 456, "alice@example.com")
			delivered := false
			b.InboundHandler = func(string, *messages.StructuredMessage) { delivered = true }

			b.handleGroupMessage(tc.msg())

			assert.False(t, delivered)
			sent := tgSrv.getSentMessages()
			require.Len(t, sent, 1)
			assert.Equal(t, want, sent[0].Text)
		})
	}
	t.Run("repeated message gets one reply", deniedSenderRepeatGetsOneReply)
}

// deniedSenderRepeatGetsOneReply checks that repeating an addressed message
// gives a denied sender one reply per suppression window.
func deniedSenderRepeatGetsOneReply(t *testing.T) {
	cases := map[string]func() *TGMessage{
		"unknown agent @token": func() *TGMessage { return plainGroupMessage(456, "hey @reviewer take a look") },
		"bot mention plus unresolved token, no default": func() *TGMessage {
			msg := plainGroupMessage(456, "@test_bot @reviewer take a look")
			msg.Entities = []MessageEntity{{Type: "mention", Offset: 0, Length: 9}}
			return msg
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			b, tgSrv, hub := newRoutingTestBroker(t)
			saveTestGroupLink(t, b.store, -200, "proj-1", "my-project", "") // no default agent
			hub.listAgentsErr = forbiddenListAgents()
			saveStaleAgentCache(t, b.store, "user:alice@example.com", "proj-1", "coder")
			linkTestUser(t, b.store, 456, "alice@example.com")
			delivered := false
			b.InboundHandler = func(string, *messages.StructuredMessage) { delivered = true }

			b.handleGroupMessage(mk())
			b.handleGroupMessage(mk())

			assert.False(t, delivered)
			sent := tgSrv.getSentMessages()
			require.Len(t, sent, 1, "the same sender is told once per suppression window")
			assert.Contains(t, sent[0].Text, "doesn't have permission")
		})
	}
}
