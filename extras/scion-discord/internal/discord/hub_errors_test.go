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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deniedBody is a hub 403 body that names the denied action.
func deniedBody(action, resourceType string) string {
	return fmt.Sprintf(`{"error":{"code":"forbidden","message":"access denied","details":{"denied_action":%q,"resource_type":%q}}}`, action, resourceType)
}

const (
	bareForbiddenBody = `{"error":{"code":"forbidden","message":"forbidden"}}`
	staleNotFoundBody = `{"error":{"code":"forbidden","message":"on-behalf-of principal not found: user:alice@example.com"}}`
	staleInactiveBody = `{"error":{"code":"forbidden","message":"on-behalf-of principal is not active"}}`
	serverErrorBody   = `{"error":{"code":"internal_error","message":"boom"}}`
)

func hubErr(status int, body string) error {
	return newHubError("list agents", &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))})
}

func TestNewHubError_DecodesEnvelope(t *testing.T) {
	err := hubErr(http.StatusForbidden, deniedBody("list", "agent"))
	var he *HubError
	require.True(t, errors.As(err, &he))
	assert.Equal(t, http.StatusForbidden, he.StatusCode)
	assert.Equal(t, "forbidden", he.Code)
	assert.Equal(t, "access denied", he.Message)
	assert.Equal(t, "list", he.detail("denied_action"))
	assert.Equal(t, "list agents returned status 403: access denied", he.Error())

	empty := hubErr(http.StatusInternalServerError, "")
	assert.Equal(t, "list agents returned status 500", empty.Error())
}

func TestHubErrorText(t *testing.T) {
	const fallback = "fallback"
	tests := []struct {
		name    string
		err     error
		email   string
		project string
		want    string
	}{
		{"denied with email and project", hubErr(403, deniedBody("list", "agent")), "alice@example.com", "proj-one",
			"Your Scion account (alice@example.com) doesn't have permission to list agents in **proj-one**. Ask a project owner."},
		{"denied without email", hubErr(403, deniedBody("list", "agent")), "", "proj-one",
			"Your Scion account doesn't have permission to list agents in **proj-one**. Ask a project owner."},
		{"denied without project", hubErr(403, deniedBody("list", "project")), "alice@example.com", "",
			"Your Scion account (alice@example.com) doesn't have permission to list projects in this hub. Ask a project owner."},
		{"policy is pluralised", hubErr(403, deniedBody("update", "policy")), "", "p",
			"Your Scion account doesn't have permission to update policies in **p**. Ask a project owner."},
		{"read reads as view", hubErr(403, deniedBody("read", "secret")), "", "p",
			"Your Scion account doesn't have permission to view secrets in **p**. Ask a project owner."},
		{"resource type with underscore", hubErr(403, deniedBody("create", "notification_subscription")), "", "p",
			"Your Scion account doesn't have permission to create notification subscriptions in **p**. Ask a project owner."},
		{"unknown linked user", hubErr(403, staleNotFoundBody), "alice@example.com", "p", staleLinkText},
		{"inactive linked user", hubErr(403, staleInactiveBody), "alice@example.com", "p", staleLinkText},
		{"forbidden without details", hubErr(403, bareForbiddenBody), "alice@example.com", "p", fallback},
		{"server error", hubErr(500, serverErrorBody), "alice@example.com", "p", fallback},
		{"not a hub error", errors.New("connection refused"), "alice@example.com", "p", fallback},
		{"wrapped hub error", fmt.Errorf("wrap: %w", hubErr(403, staleNotFoundBody)), "", "p", staleLinkText},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hubErrorText(tt.err, tt.email, tt.project, fallback))
		})
	}
}

func TestDeniedText_ForbiddenWithoutDetailsNamesTheAction(t *testing.T) {
	assert.Equal(t,
		"Your Scion account (alice@example.com) doesn't have permission to read the secret **API_KEY** in **proj-one**. Ask a project owner.",
		secretErrorText(hubErr(403, bareForbiddenBody), "user:alice@example.com", "proj-one", "read", "API_KEY", "fallback"))
	assert.Equal(t, staleLinkText,
		secretErrorText(hubErr(403, staleNotFoundBody), "user:alice@example.com", "proj-one", "read", "API_KEY", "fallback"))
	assert.Equal(t, "fallback",
		secretErrorText(hubErr(500, serverErrorBody), "user:alice@example.com", "proj-one", "read", "API_KEY", "fallback"))
	assert.Equal(t,
		"Your Scion account (alice@example.com) doesn't have permission to view secrets in **proj-one**. Ask a project owner.",
		secretErrorText(hubErr(403, deniedBody("read", "secret")), "user:alice@example.com", "proj-one", "read", "API_KEY", "fallback"),
		"a denial that names the action uses it")
}

func TestPluralize(t *testing.T) {
	for in, want := range map[string]string{
		"agent": "agents", "policy": "policies", "key": "keys", "secrets": "secrets", "project": "projects",
	} {
		assert.Equal(t, want, pluralize(in), in)
	}
}

// hubErrorCase is one handler whose hub request fails.
type hubErrorCase struct {
	name   string
	method string
	path   string
	run    func(t *testing.T, e *linkedUserEnv)
	// denied is the failure body used for the denial case and wantDenied
	// the reply to it.
	denied     string
	wantDenied string
	// wantFailure is part of the reply to a server error.
	wantFailure string
}

const (
	luAgentsPath    = "/api/v1/projects/" + luProject + "/agents"
	luDeniedAgents  = "Your Scion account (alice@example.com) doesn't have permission to list agents in **proj-one**. Ask a project owner."
	luDeniedSecrets = "Your Scion account (alice@example.com) doesn't have permission to %s in **proj-one**. Ask a project owner."
)

func hubErrorCases() []hubErrorCase {
	listAgents := deniedBody("list", "agent")
	return []hubErrorCase{
		{"agents", http.MethodGet, luAgentsPath, func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleAgents(e.session, luCommand("agents"))
		}, listAgents, luDeniedAgents, "Failed to fetch agents"},
		{"status", http.MethodGet, luAgentsPath, func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleStatus(e.session, luCommand("status", luStringOpt("agent", "worker")))
		}, listAgents, luDeniedAgents, "Failed to fetch agent status"},
		{"message", http.MethodGet, luAgentsPath, func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleMessage(e.session, luCommand("message", luStringOpt("agent", "worker"), luStringOpt("text", "hi")))
		}, listAgents, luDeniedAgents, agentListUnavailableText},
		{"terminal", http.MethodGet, luAgentsPath, func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleTerminal(e.session, luCommand("terminal", luStringOpt("agent", "worker")))
		}, listAgents, luDeniedAgents, "Failed to fetch agents"},
		{"default", http.MethodGet, luAgentsPath, func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleDefault(e.session, luCommand("default", luStringOpt("agent", "worker")))
		}, listAgents, luDeniedAgents, "Failed to fetch agents"},
		{"thread agent list", http.MethodGet, luAgentsPath, func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleThread(e.session, luCommand("thread", luStringOpt("title", "Fix the build")))
		}, listAgents, luDeniedAgents, agentListUnavailableText},
		{"thread template list", http.MethodGet, "/api/v1/templates", func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleThread(e.session, luCommand("thread", luStringOpt("title", "Fix the build"), luStringOpt("template", "default")))
		}, deniedBody("list", "template"),
			"Your Scion account (alice@example.com) doesn't have permission to list templates in **proj-one**. Ask a project owner.",
			"Failed to verify template"},
		{"thread agent creation", http.MethodPost, luAgentsPath, func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleThread(e.session, luCommand("thread", luStringOpt("title", "Fix the build")))
		}, bareForbiddenBody,
			"Your Scion account (alice@example.com) doesn't have permission to create agents in **proj-one**. Ask a project owner.",
			"create agent returned status 500"},
		{"setup", http.MethodGet, "/api/v1/projects", func(t *testing.T, e *linkedUserEnv) {
			require.NoError(t, e.store.DeleteChannelLink(context.Background(), luChannel))
			e.commands.HandleSetup(e.session, luCommand("setup"))
		}, deniedBody("list", "project"),
			"Your Scion account (alice@example.com) doesn't have permission to list projects in this hub. Ask a project owner.",
			"Failed to fetch your projects"},
		{"setup project select", http.MethodGet, luAgentsPath, func(t *testing.T, e *linkedUserEnv) {
			require.NoError(t, e.store.DeleteChannelLink(context.Background(), luChannel))
			i := luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "setup:proj:" + luProject})
			e.callback.Dispatch(e.session, i, "setup:proj:"+luProject, nil)
		}, listAgents, luDeniedAgents, "Failed to fetch agents"},
		{"secret list", http.MethodGet, "/api/v1/secrets", func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleSecretList(e.session, luSecretCommand("list"))
		}, bareForbiddenBody, fmt.Sprintf(luDeniedSecrets, "list secrets"), "Failed to list secrets"},
		{"secret get", http.MethodGet, "/api/v1/secrets/API_KEY", func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleSecretGet(e.session, luSecretCommand("get", luStringOpt("key", "API_KEY")))
		}, bareForbiddenBody, fmt.Sprintf(luDeniedSecrets, "read the secret **API_KEY**"), "Failed to get secret **API_KEY**"},
		{"secret set", http.MethodPut, "/api/v1/secrets/API_KEY", func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleSecretModalSubmit(e.session, luSecretModalSubmit())
		}, bareForbiddenBody, fmt.Sprintf(luDeniedSecrets, "set the secret **API_KEY**"), "Failed to set secret **API_KEY**"},
		{"secret delete", http.MethodDelete, "/api/v1/secrets/API_KEY", func(_ *testing.T, e *linkedUserEnv) {
			e.commands.HandleSecretDelete(e.session, luSecretCommand("delete", luStringOpt("key", "API_KEY")))
		}, bareForbiddenBody, fmt.Sprintf(luDeniedSecrets, "delete the secret **API_KEY**"), "Failed to delete secret **API_KEY**"},
	}
}

func TestHandlers_HubErrorsGetActionableText(t *testing.T) {
	for _, tc := range hubErrorCases() {
		t.Run(tc.name, func(t *testing.T) {
			outcomes := []struct {
				name   string
				status int
				body   string
				want   string
			}{
				{"denied", http.StatusForbidden, tc.denied, tc.wantDenied},
				{"unknown linked user", http.StatusForbidden, staleNotFoundBody, staleLinkText},
				{"inactive linked user", http.StatusForbidden, staleInactiveBody, staleLinkText},
				{"server error", http.StatusInternalServerError, serverErrorBody, tc.wantFailure},
			}
			for _, o := range outcomes {
				t.Run(o.name, func(t *testing.T) {
					e := newLinkedUserEnv(t)
					e.linkChannel(t)
					e.hub.failRequest(tc.method, tc.path, o.status, o.body)

					tc.run(t, e)

					bodies := e.discord.allBodies()
					assert.Contains(t, bodies, jsonText(t, o.want))
					assert.NotContains(t, bodies, "not found", "a failed request is not reported as a missing agent")
					assert.NotContains(t, bodies, "name availability")
					assert.NotContains(t, bodies, luProject+"**", "the project is named by slug, not ID")
					if o.status == http.StatusForbidden {
						assert.NotContains(t, bodies, "try again", "a denial is not reported as a transient failure")
					}
				})
			}
		})
	}
}

func TestHandleIncomingMessage_FailedAgentListIsNotReportedAsUnknownAgent(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"server error", http.StatusInternalServerError, serverErrorBody, agentListUnavailableText},
		{"denied", http.StatusForbidden, deniedBody("list", "agent"), luDeniedAgents},
		{"unknown linked user", http.StatusForbidden, staleNotFoundBody, staleLinkText},
	}
	msgs := map[string]func() *discordgo.MessageCreate{
		"bot mention with unknown agent": func() *discordgo.MessageCreate {
			m := luChannelMessage(luDiscordUser, "<@BOT123> @nobody hello")
			m.Mentions = []*discordgo.User{{ID: "BOT123"}}
			return m
		},
		"leading unknown agent with default": func() *discordgo.MessageCreate {
			return luChannelMessage(luDiscordUser, "@nobody hello")
		},
	}
	for _, tt := range tests {
		for msgName, mk := range msgs {
			t.Run(tt.name+"/"+msgName, func(t *testing.T) {
				e := newLinkedUserEnv(t)
				e.linkChannel(t)
				if msgName == "leading unknown agent with default" {
					link, err := e.store.GetChannelLink(context.Background(), luChannel)
					require.NoError(t, err)
					link.DefaultAgent = "worker"
					require.NoError(t, e.store.UpdateChannelLink(context.Background(), link))
				}
				e.hub.failRequest(http.MethodGet, luAgentsPath, tt.status, tt.body)
				b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
				delivered := false
				b.InboundHandler = func(string, *messages.StructuredMessage) { delivered = true }

				b.handleIncomingMessage(e.session, mk())

				bodies := e.discord.allBodies()
				assert.Contains(t, bodies, jsonText(t, tt.want))
				assert.NotContains(t, bodies, "Unknown agent")
				assert.False(t, delivered)
			})
		}
	}
}

func TestHandleIncomingMessage_DeniedSenderIsNotRouted(t *testing.T) {
	e := newLinkedUserEnv(t)
	link := &ChannelLink{ChannelID: luChannel, GuildID: testGuildID, ProjectID: luProject, ProjectSlug: "proj-one", DefaultAgent: "worker", Active: true}
	require.NoError(t, e.store.CreateChannelLink(context.Background(), link))
	e.hub.failRequest(http.MethodGet, luAgentsPath, http.StatusForbidden, deniedBody("list", "agent"))
	b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
	delivered := false
	b.InboundHandler = func(string, *messages.StructuredMessage) { delivered = true }

	b.handleIncomingMessage(e.session, luChannelMessage(luDiscordUser, "please build it"))

	assert.False(t, delivered, "a denied sender's message is not delivered")
	assert.Contains(t, e.discord.allBodies(), jsonText(t, luDeniedAgents))
}

func TestHandleIncomingMessage_HubOutageStillRoutesToDefault(t *testing.T) {
	e := newLinkedUserEnv(t)
	link := &ChannelLink{ChannelID: luChannel, GuildID: testGuildID, ProjectID: luProject, ProjectSlug: "proj-one", DefaultAgent: "worker", Active: true}
	require.NoError(t, e.store.CreateChannelLink(context.Background(), link))
	e.hub.failRequest(http.MethodGet, luAgentsPath, http.StatusInternalServerError, serverErrorBody)
	b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
	var topics []string
	b.InboundHandler = func(topic string, _ *messages.StructuredMessage) { topics = append(topics, topic) }

	b.handleIncomingMessage(e.session, luChannelMessage(luDiscordUser, "please build it"))

	assert.Equal(t, []string{"scion.project." + luProject + ".agent.worker.messages"}, topics)
}

func TestHubError_DeliveryText(t *testing.T) {
	tests := []struct {
		name    string
		err     hubError
		email   string
		project string
		want    string
	}{
		{"message denied", hubError{StatusCode: 403, Code: "message_denied", Message: "Message delivery denied"}, "alice@example.com", "proj-one",
			"Your Scion account (alice@example.com) doesn't have permission to message agents in **proj-one**. Ask a project owner."},
		{"message denied without account or project", hubError{StatusCode: 403, Code: "message_denied"}, "", "",
			"Your Scion account doesn't have permission to message agents in this hub. Ask a project owner."},
		{"forbidden with denied action", hubError{StatusCode: 403, Code: "forbidden", DeniedAction: "message", ResourceType: "agent"}, "alice@example.com", "proj-one",
			"Your Scion account (alice@example.com) doesn't have permission to message agents in **proj-one**. Ask a project owner."},
		{"forbidden without details", hubError{StatusCode: 403, Code: "forbidden", Message: "forbidden"}, "alice@example.com", "proj-one",
			"Your Scion account (alice@example.com) doesn't have permission to message agents in **proj-one**. Ask a project owner."},
		{"unknown linked user", hubError{StatusCode: 403, Code: "forbidden", Message: "on-behalf-of principal not found"}, "alice@example.com", "proj-one", staleLinkText},
		{"inactive linked user", hubError{StatusCode: 403, Code: "forbidden", Message: "on-behalf-of principal is not active"}, "alice@example.com", "proj-one", staleLinkText},
		{"unresolved sender identity", hubError{StatusCode: 403, Code: "forbidden", Message: "sender identity could not be resolved"}, "alice@example.com", "proj-one", staleLinkText},
		{"agent not found", hubError{StatusCode: 404, Code: "agent_not_found"}, "alice@example.com", "proj-one",
			"Target agent not found. Use `/scion agents` to see available agents."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.err.userFacingMessage(tt.email, tt.project))
		})
	}
}

func TestParseHubError_ReadsDenialDetails(t *testing.T) {
	he := parseHubError(&http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(deniedBody("message", "agent")))})
	assert.Equal(t, "forbidden", he.Code)
	assert.Equal(t, "message", he.DeniedAction)
	assert.Equal(t, "agent", he.ResourceType)
}

// deliveryFailureBodies are hub answers to a rejected message delivery,
// with the expected reply.
var deliveryFailureBodies = map[string]struct {
	body string
	want string
}{
	"message denied": {`{"error":{"code":"message_denied","message":"Message delivery denied"}}`,
		"Your Scion account (alice@example.com) doesn't have permission to message agents in **proj-one**. Ask a project owner."},
	"unknown linked user": {`{"error":{"code":"forbidden","message":"on-behalf-of principal not found"}}`, staleLinkText},
	"denied action": {deniedBody("message", "agent"),
		"Your Scion account (alice@example.com) doesn't have permission to message agents in **proj-one**. Ask a project owner."},
}

func TestHandleIncomingMessage_DeliveryErrorsGetActionableText(t *testing.T) {
	for _, routed := range []bool{false, true} {
		path := "/api/v1/broker/inbound"
		if routed {
			path = "/api/v1/broker/inbound/routed"
		}
		for name, tc := range deliveryFailureBodies {
			t.Run(fmt.Sprintf("routed=%v/%s", routed, name), func(t *testing.T) {
				e := newLinkedUserEnv(t)
				e.linkChannel(t)
				e.setDefaultAgent(t, "worker")
				e.cacheAgents(t, luPrincipal, time.Now(), "worker")
				e.hub.failRequest(http.MethodPost, path, http.StatusForbidden, tc.body)
				b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
				b.config = &Config{RoutedInboundEnabled: routed}

				b.handleIncomingMessage(e.session, luChannelMessage(luDiscordUser, "please build it"))

				require.NotEmpty(t, e.hub.callsTo(http.MethodPost, path), "the message reached the hub")
				assert.Equal(t, []string{tc.want}, channelReplies(t, e.discord))
			})
		}
	}
}

func TestHandleMessageCommand_DeliveryErrorGetsActionableText(t *testing.T) {
	for name, tc := range deliveryFailureBodies {
		t.Run(name, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			e.linkChannel(t)
			b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
			e.hub.failRequest(http.MethodPost, "/api/v1/broker/inbound", http.StatusForbidden, tc.body)
			e.commands.deliverInbound = b.deliverInbound

			e.commands.HandleMessage(e.session, luCommand("message", luStringOpt("agent", "worker"), luStringOpt("text", "hi")))

			assert.Contains(t, e.discord.allBodies(), jsonText(t, tc.want))
		})
	}
}

func TestAskUserReply_DeliveryErrorGetsActionableText(t *testing.T) {
	for name, tc := range deliveryFailureBodies {
		t.Run(name, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			e.linkChannel(t)
			b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
			e.hub.failRequest(http.MethodPost, "/api/v1/broker/inbound", http.StatusForbidden, tc.body)
			e.callback.deliverInbound = b.deliverInbound
			require.NoError(t, e.store.CreatePendingAskUser(context.Background(), &PendingAskUser{
				RequestID: "req-1", MessageID: "m-1", ChannelID: luChannel, AgentSlug: "worker", ProjectID: luProject,
				Choices: []string{"yes", "no"}, ExpiresAt: time.Now().Add(time.Hour),
			}))
			i := luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "ask:opt:req-1:0"})

			e.callback.Dispatch(e.session, i, "ask:opt:req-1:0", nil)

			assert.Contains(t, e.discord.allBodies(), jsonText(t, tc.want))
		})
	}
}
