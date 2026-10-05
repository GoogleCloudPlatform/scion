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

package discord

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const luBobPrincipal = "user:bob@example.com"

// linkBob links luOtherUser to bob@example.com.
func (e *linkedUserEnv) linkBob(t *testing.T) {
	t.Helper()
	require.NoError(t, e.store.CreateUserMapping(context.Background(), &DiscordUserMapping{
		DiscordUserID: luOtherUser, DiscordUsername: "bob", ScionUserID: "scion-user-2",
		ScionEmail: "bob@example.com", LinkedAt: time.Now(),
	}))
}

// cacheAgents caches slugs for user in luProject, refreshed at refreshedAt.
func (e *linkedUserEnv) cacheAgents(t *testing.T, user string, refreshedAt time.Time, slugs ...string) {
	t.Helper()
	require.NoError(t, e.store.SetProjectAgents(context.Background(), &ProjectAgents{
		User: user, ProjectID: luProject, AgentSlugs: slugs, RefreshedAt: refreshedAt,
	}))
}

// staleCacheTime is past the agent-cache TTL but within retention.
func staleCacheTime() time.Time { return time.Now().Add(-30 * time.Minute) }

// botMention returns a channel message from authorID that mentions the bot.
func botMention(authorID, content string) *discordgo.MessageCreate {
	m := luChannelMessage(authorID, "<@BOT123> "+content)
	m.Mentions = []*discordgo.User{{ID: "BOT123"}}
	return m
}

// setDefaultAgent sets the channel's default agent.
func (e *linkedUserEnv) setDefaultAgent(t *testing.T, slug string) {
	t.Helper()
	link, err := e.store.GetChannelLink(context.Background(), luChannel)
	require.NoError(t, err)
	link.DefaultAgent = slug
	require.NoError(t, e.store.UpdateChannelLink(context.Background(), link))
}

// deliveries records delivered topics.
type deliveries struct{ topics []string }

func (d *deliveries) handler(topic string, _ *messages.StructuredMessage) {
	d.topics = append(d.topics, topic)
}

func TestAgentCache_FreshListOfAnotherUserIsNotServed(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	e.linkBob(t)
	e.cacheAgents(t, luPrincipal, time.Now(), "alices-agent")
	b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
	var d deliveries
	b.InboundHandler = d.handler

	b.handleIncomingMessage(e.session, botMention(luOtherUser, "@alices-agent hi"))

	calls := e.hub.callsTo(http.MethodGet, luAgentsPath)
	require.Len(t, calls, 1, "bob fetches his own list")
	assert.Equal(t, luBobPrincipal, calls[0].OnBehalfOf)
	assert.Empty(t, d.topics, "bob is not routed with alice's list")
	assert.Contains(t, e.discord.allBodies(), "Unknown agent: alices-agent")
}

func TestAgentCache_FreshListIsReusedForSameUser(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	e.cacheAgents(t, luPrincipal, time.Now(), "worker")
	b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
	var d deliveries
	b.InboundHandler = d.handler

	b.handleIncomingMessage(e.session, luChannelMessage(luDiscordUser, "@worker hi"))

	assert.Empty(t, e.hub.callsTo(http.MethodGet, luAgentsPath))
	assert.Equal(t, []string{"scion.project." + luProject + ".agent.worker.messages"}, d.topics)
}

func TestAgentCache_StaleListOfAnotherUserIsNotServed(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	e.linkBob(t)
	e.cacheAgents(t, luPrincipal, staleCacheTime(), "alices-agent")
	e.hub.failRequest(http.MethodGet, luAgentsPath, http.StatusInternalServerError, serverErrorBody)
	b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
	var d deliveries
	b.InboundHandler = d.handler

	b.handleIncomingMessage(e.session, botMention(luOtherUser, "@alices-agent hi"))

	assert.Empty(t, d.topics)
	assert.Contains(t, e.discord.allBodies(), jsonText(t, agentListUnavailableText))
}

func TestAgentCache_StaleListCoversHubOutageForSameUser(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	e.cacheAgents(t, luPrincipal, staleCacheTime(), "worker")
	e.hub.failRequest(http.MethodGet, luAgentsPath, http.StatusInternalServerError, serverErrorBody)
	b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
	var d deliveries
	b.InboundHandler = d.handler

	b.handleIncomingMessage(e.session, luChannelMessage(luDiscordUser, "@worker hi"))

	assert.Equal(t, []string{"scion.project." + luProject + ".agent.worker.messages"}, d.topics)
}

func TestAgentCache_DeniedUserIsNotServedAnyCachedList(t *testing.T) {
	for name, user := range map[string]string{"own stale list": luDiscordUser, "another user's list": luOtherUser} {
		t.Run(name, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			e.linkChannel(t)
			e.linkBob(t)
			e.cacheAgents(t, luPrincipal, staleCacheTime(), "alices-agent")
			e.hub.failRequest(http.MethodGet, luAgentsPath, http.StatusForbidden, deniedBody("list", "agent"))
			b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
			var d deliveries
			b.InboundHandler = d.handler

			b.handleIncomingMessage(e.session, botMention(user, "@alices-agent hi"))

			assert.Empty(t, d.topics)
			bodies := e.discord.allBodies()
			assert.Contains(t, bodies, "doesn't have permission to list agents")
			assert.NotContains(t, bodies, "Unknown agent")
		})
	}
}

func TestAgentCache_CommandsUseTheInvokingUsersList(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	e.linkBob(t)
	e.cacheAgents(t, luPrincipal, time.Now(), "alices-agent")

	e.commands.HandleDefault(e.session, asUser(luCommand("default", luStringOpt("agent", "alices-agent")), luOtherUser))

	calls := e.hub.callsTo(http.MethodGet, luAgentsPath)
	require.Len(t, calls, 1)
	assert.Equal(t, luBobPrincipal, calls[0].OnBehalfOf)
	assert.Contains(t, e.discord.allBodies(), "Agent **alices-agent** not found")
	link, err := e.store.GetChannelLink(context.Background(), luChannel)
	require.NoError(t, err)
	assert.Empty(t, link.DefaultAgent, "bob cannot set a default from alice's list")
}

func TestAgentCache_AutocompleteUsesTheInvokingUsersList(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	e.linkBob(t)
	e.cacheAgents(t, luPrincipal, time.Now(), "alices-agent")

	e.commands.HandleAutocomplete(e.session, asUser(luAutocomplete("status", "agent"), luOtherUser))

	bodies := e.discord.allBodies()
	assert.Contains(t, bodies, `"worker"`)
	assert.NotContains(t, bodies, "alices-agent")
	calls := e.hub.callsTo(http.MethodGet, luAgentsPath)
	require.Len(t, calls, 1)
	assert.Equal(t, luBobPrincipal, calls[0].OnBehalfOf)
}

func TestAgentCache_CommandDenialDoesNotFallBackToCache(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	e.cacheAgents(t, luPrincipal, staleCacheTime(), "worker")
	e.hub.failRequest(http.MethodGet, luAgentsPath, http.StatusForbidden, deniedBody("list", "agent"))

	e.commands.HandleDefault(e.session, luCommand("default", luStringOpt("agent", "worker")))

	assert.Contains(t, e.discord.allBodies(), jsonText(t, luDeniedAgents))
	link, err := e.store.GetChannelLink(context.Background(), luChannel)
	require.NoError(t, err)
	assert.Empty(t, link.DefaultAgent)
}

// --- Unresolved senders ---

// unresolvedSender puts luOtherUser in a state the plugin cannot act as.
type unresolvedSender struct {
	setup            func(t *testing.T, e *linkedUserEnv, b *DiscordBroker)
	want             string
	repliesToDefault bool
}

var unresolvedSenders = map[string]unresolvedSender{
	"unlinked": {func(*testing.T, *linkedUserEnv, *DiscordBroker) {}, msgRegisterToInteract, false},
	"link without email": {func(t *testing.T, e *linkedUserEnv, _ *DiscordBroker) {
		require.NoError(t, e.store.CreateUserMapping(context.Background(), &DiscordUserMapping{
			DiscordUserID: luOtherUser, DiscordUsername: "bob", ScionUserID: "scion-user-2", LinkedAt: time.Now(),
		}))
	}, staleLinkText, true},
	"lookup failure": {func(t *testing.T, e *linkedUserEnv, b *DiscordBroker) {
		e.linkBob(t)
		b.store = &countingStore{Store: b.store, userMappingError: errors.New("database is locked")}
	}, msgSomethingWentWrong, true},
}

// unresolvedMessage is a message shape sent by an unresolved sender.
type unresolvedMessage struct {
	msg          func() *discordgo.MessageCreate
	defaultAgent string
	addressed    bool
	toDefault    bool
}

func unresolvedMessages() map[string]unresolvedMessage {
	return map[string]unresolvedMessage{
		"plain text, default agent":    {func() *discordgo.MessageCreate { return luChannelMessage(luOtherUser, "hello") }, "worker", false, true},
		"plain text, no default agent": {func() *discordgo.MessageCreate { return luChannelMessage(luOtherUser, "hello") }, "", false, false},
		"attachment, default agent": {func() *discordgo.MessageCreate {
			m := luChannelMessage(luOtherUser, "")
			m.Attachments = []*discordgo.MessageAttachment{{ID: "att-1", Filename: "a.txt", URL: "https://cdn.example/a.txt"}}
			return m
		}, "worker", false, true},
		"agent mention":           {func() *discordgo.MessageCreate { return luChannelMessage(luOtherUser, "@worker hello") }, "worker", false, false},
		"unknown agent mention":   {func() *discordgo.MessageCreate { return luChannelMessage(luOtherUser, "@nobody hello") }, "", false, false},
		"bot mention":             {func() *discordgo.MessageCreate { return botMention(luOtherUser, "hello") }, "worker", true, false},
		"bot mention, no default": {func() *discordgo.MessageCreate { return botMention(luOtherUser, "hello") }, "", true, false},
		"bot mention plus agent":  {func() *discordgo.MessageCreate { return botMention(luOtherUser, "@worker hello") }, "worker", true, false},
		"reply to agent message": {func() *discordgo.MessageCreate {
			m := luChannelMessage(luOtherUser, "go ahead")
			m.Type = discordgo.MessageTypeReply
			m.ReferencedMessage = &discordgo.Message{ID: "agent-msg", WebhookID: "wh-1", Author: &discordgo.User{ID: "wh-1", Username: "worker"}}
			return m
		}, "", true, false},
	}
}

// agentCacheStates are the agent-cache contents an unresolved sender's
// message is checked against.
var agentCacheStates = map[string]func(t *testing.T, e *linkedUserEnv){
	"fresh lists of other users": func(t *testing.T, e *linkedUserEnv) {
		e.cacheAgents(t, luPrincipal, time.Now(), "worker", "nobody")
		e.cacheAgents(t, "user:carol@example.com", time.Now(), "worker")
	},
	"stale lists of other users": func(t *testing.T, e *linkedUserEnv) {
		e.cacheAgents(t, luPrincipal, staleCacheTime(), "worker", "nobody")
		e.cacheAgents(t, "user:carol@example.com", staleCacheTime(), "worker")
	},
}

// unresolvedSenderReplies sends the message msgName from the unresolved
// sender senderName on the given inbound path and returns the replies.
// cache, when set, fills the agent cache first.
func unresolvedSenderReplies(t *testing.T, senderName, msgName string, routed bool, cache func(*testing.T, *linkedUserEnv)) []string {
	t.Helper()
	sc := unresolvedSenders[senderName]
	mc := unresolvedMessages()[msgName]
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	e.setDefaultAgent(t, mc.defaultAgent)
	if cache != nil {
		cache(t, e)
	}
	b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
	b.config = &Config{RoutedInboundEnabled: routed}
	sc.setup(t, e, b)
	cs := &countingStore{Store: b.store}
	b.store = cs
	var d deliveries
	b.InboundHandler = d.handler

	b.handleIncomingMessage(e.session, mc.msg())

	assert.Empty(t, d.topics, "an unresolved sender is never routed")
	assert.Empty(t, e.hub.snapshot(), "no hub call for an unresolved sender")
	assert.Zero(t, cs.reads(), "the agent cache is not read for an unresolved sender")
	return channelReplies(t, e.discord)
}

// channelReplies returns the content of every message the bot posted.
func channelReplies(t *testing.T, d *discordStub) []string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	replies := []string{}
	for _, body := range d.bodies {
		var msg struct {
			Content string `json:"content"`
		}
		if strings.TrimSpace(body) == "" || json.Unmarshal([]byte(body), &msg) != nil || msg.Content == "" {
			continue
		}
		replies = append(replies, msg.Content)
	}
	return replies
}

func TestUnresolvedSender_Replies(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for senderName, sc := range unresolvedSenders {
			for msgName, mc := range unresolvedMessages() {
				name := senderName + "/" + msgName
				if routed {
					name = "routed/" + name
				}
				t.Run(name, func(t *testing.T) {
					replies := unresolvedSenderReplies(t, senderName, msgName, routed, nil)
					if mc.addressed || (sc.repliesToDefault && mc.toDefault) {
						assert.Equal(t, []string{sc.want}, replies)
					} else {
						assert.Empty(t, replies, "no reply")
					}
				})
			}
		}
	}
}

func TestUnresolvedSender_SameReplyWhetherOrNotAgentsCached(t *testing.T) {
	for senderName := range unresolvedSenders {
		for msgName := range unresolvedMessages() {
			for cacheName, cache := range agentCacheStates {
				t.Run(senderName+"/"+msgName+"/"+cacheName, func(t *testing.T) {
					uncached := unresolvedSenderReplies(t, senderName, msgName, false, nil)
					cached := unresolvedSenderReplies(t, senderName, msgName, false, cache)
					assert.Equal(t, uncached, cached, "same replies with and without cached agents")
				})
			}
		}
	}
}

func TestUnresolvedSender_NeverSeenSenderUnaddressedTextGetsNoReply(t *testing.T) {
	assert.Empty(t, unresolvedSenderReplies(t, "unlinked", "plain text, default agent", false, nil))
	assert.Empty(t, unresolvedSenderReplies(t, "unlinked", "plain text, default agent", true, nil))
}

// --- Cache TTL configuration ---

func TestConfigure_AgentCacheTTL(t *testing.T) {
	cases := map[string]struct {
		value string
		want  time.Duration
	}{
		"default":           {"", defaultAgentCacheTTL},
		"within retention":  {"10m", 10 * time.Minute},
		"at the limit":      {maxAgentCacheTTL.String(), maxAgentCacheTTL},
		"longer is clamped": {"2h", maxAgentCacheTTL},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := NewBroker(discardLogger())
			t.Cleanup(func() { _ = b.Close() })
			cfg := map[string]string{
				"bot_token": "Bot fake-token",
				"db_path":   filepath.Join(t.TempDir(), "ttl_test.db"),
			}
			if tc.value != "" {
				cfg["agent_cache_ttl"] = tc.value
			}
			require.NoError(t, b.Configure(cfg))
			assert.Equal(t, tc.want, b.agentCacheTTL)
		})
	}
}

func TestAgentCacheTTLsFitWithinRetention(t *testing.T) {
	assert.LessOrEqual(t, 3*defaultAgentCacheTTL, agentCacheRetention)
	assert.LessOrEqual(t, 3*maxAgentCacheTTL, agentCacheRetention)
}
