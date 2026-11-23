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
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestSettingsButtons_RequireAdminAndProjectMembership(t *testing.T) {
	ctx := context.Background()

	actions := []struct {
		name     string
		customID string
		read     func(link *ChannelLink) bool
	}{
		{
			name:     "observe",
			customID: "settings:observe:" + luChannel,
			read:     func(link *ChannelLink) bool { return link.ShowAgentToAgent },
		},
		{
			name:     "statechange",
			customID: "settings:statechange:" + luChannel,
			read:     func(link *ChannelLink) bool { return link.ShowStateChanges },
		},
	}

	cases := []struct {
		name        string
		perms       int64
		nilMember   bool
		projects    string // "", "empty", "error"
		nilHub      bool
		wantAllowed bool
		wantReply   string
		wantNoHub   bool
	}{
		{
			name:        "manage channels and project member",
			perms:       discordgo.PermissionManageChannels,
			wantAllowed: true,
		},
		{
			name:        "administrator and project member",
			perms:       discordgo.PermissionAdministrator,
			wantAllowed: true,
		},
		{
			name:      "no channel admin permission",
			perms:     0,
			wantReply: msgSettingsAdminRequired,
			wantNoHub: true,
		},
		{
			name:      "nil guild member",
			nilMember: true,
			wantReply: msgSettingsAdminRequired,
			wantNoHub: true,
		},
		{
			name:      "admin but not project member",
			perms:     discordgo.PermissionAdministrator,
			projects:  "empty",
			wantReply: notProjectMemberText("alice@example.com", "proj-one"),
		},
		{
			name:      "admin but membership lookup fails",
			perms:     discordgo.PermissionManageChannels,
			projects:  "error",
			wantReply: msgProjectMembershipCheckFailed,
		},
		{
			name:      "admin but hub client unavailable",
			perms:     discordgo.PermissionManageChannels,
			nilHub:    true,
			wantReply: msgProjectMembershipCheckFailed,
			wantNoHub: true,
		},
	}

	for _, act := range actions {
		for _, tc := range cases {
			t.Run(act.name+"/"+tc.name, func(t *testing.T) {
				e := newLinkedUserEnv(t)
				e.linkChannel(t)
				if tc.projects != "" {
					e.hub.setProjects(tc.projects)
				}
				if tc.nilHub {
					e.callback.hubClient = nil
				}
				ws := e.useWriteCountingStore()

				i := luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
					CustomID: act.customID,
				})
				if tc.nilMember {
					i.Member = nil
					i.User = &discordgo.User{ID: luDiscordUser, Username: "alice"}
				} else {
					i.Member.Permissions = tc.perms
				}

				e.callback.Dispatch(e.session, i, act.customID, nil)

				link, err := e.store.GetChannelLink(ctx, luChannel)
				require.NoError(t, err)
				require.NotNil(t, link)

				if tc.wantAllowed {
					assert.Equal(t, 1, ws.writeCount())
					assert.True(t, act.read(link))
					return
				}

				assert.Zero(t, ws.writeCount(), "unauthorized settings toggle must not mutate store")
				assert.False(t, act.read(link))
				assert.Contains(t, e.discord.allBodies(), jsonText(t, tc.wantReply))
				if tc.wantNoHub {
					assert.Empty(t, e.hub.snapshot())
				}
			})
		}
	}
}

func TestDefaultAndNotifButtons_RequireProjectMembership(t *testing.T) {
	const threadID = "thread-9"
	ctx := context.Background()

	arms := []struct {
		name     string
		customID string
		seed     func(t *testing.T, e *linkedUserEnv)
		changed  func(t *testing.T, e *linkedUserEnv) bool
	}{
		{
			name:     "setup default",
			customID: "setup:dflt:worker",
			changed: func(t *testing.T, e *linkedUserEnv) bool {
				link, err := e.store.GetChannelLink(ctx, luChannel)
				require.NoError(t, err)
				return link.DefaultAgent == "worker"
			},
		},
		{
			name:     "set channel default",
			customID: "default:set:worker",
			changed: func(t *testing.T, e *linkedUserEnv) bool {
				link, err := e.store.GetChannelLink(ctx, luChannel)
				require.NoError(t, err)
				return link.DefaultAgent == "worker"
			},
		},
		{
			name:     "clear channel default",
			customID: "default:none",
			seed: func(t *testing.T, e *linkedUserEnv) {
				link, err := e.store.GetChannelLink(ctx, luChannel)
				require.NoError(t, err)
				link.DefaultAgent = "worker"
				require.NoError(t, e.store.UpdateChannelLink(ctx, link))
			},
			changed: func(t *testing.T, e *linkedUserEnv) bool {
				link, err := e.store.GetChannelLink(ctx, luChannel)
				require.NoError(t, err)
				return link.DefaultAgent == ""
			},
		},
		{
			name:     "set thread default",
			customID: "default:set:worker:" + threadID,
			changed: func(t *testing.T, e *linkedUserEnv) bool {
				slug, err := e.store.GetThreadDefault(ctx, luChannel, threadID)
				require.NoError(t, err)
				return slug == "worker"
			},
		},
		{
			name:     "clear thread default",
			customID: "default:none:" + threadID,
			seed: func(t *testing.T, e *linkedUserEnv) {
				require.NoError(t, e.store.SetThreadDefault(ctx, luChannel, threadID, "worker"))
			},
			changed: func(t *testing.T, e *linkedUserEnv) bool {
				slug, err := e.store.GetThreadDefault(ctx, luChannel, threadID)
				require.NoError(t, err)
				return slug == ""
			},
		},
		{
			name:     "notifications on",
			customID: "notif:on:worker",
			changed: func(t *testing.T, e *linkedUserEnv) bool {
				prefs, err := e.store.GetNotificationPrefs(ctx, luDiscordUser, luProject)
				require.NoError(t, err)
				return len(prefs) == 1 && prefs[0].AgentSlug == "worker" && prefs[0].Enabled
			},
		},
		{
			name:     "notifications off",
			customID: "notif:off:worker",
			changed: func(t *testing.T, e *linkedUserEnv) bool {
				prefs, err := e.store.GetNotificationPrefs(ctx, luDiscordUser, luProject)
				require.NoError(t, err)
				return len(prefs) == 1 && prefs[0].AgentSlug == "worker" && !prefs[0].Enabled
			},
		},
	}

	cases := []struct {
		name        string
		projects    string
		nilHub      bool
		wantAllowed bool
		wantReply   string
	}{
		{
			name:        "project member without channel admin",
			wantAllowed: true,
		},
		{
			name:      "linked non-member",
			projects:  "empty",
			wantReply: notProjectMemberText("alice@example.com", "proj-one"),
		},
		{
			name:      "membership lookup fails",
			projects:  "error",
			wantReply: msgProjectMembershipCheckFailed,
		},
		{
			name:      "hub client unavailable",
			nilHub:    true,
			wantReply: msgProjectMembershipCheckFailed,
		},
	}

	for _, arm := range arms {
		for _, tc := range cases {
			t.Run(arm.name+"/"+tc.name, func(t *testing.T) {
				e := newLinkedUserEnv(t)
				e.linkChannel(t)
				if arm.seed != nil {
					arm.seed(t, e)
				}
				if tc.projects != "" {
					e.hub.setProjects(tc.projects)
				}
				if tc.nilHub {
					e.callback.hubClient = nil
				}
				ws := e.useWriteCountingStore()

				i := luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
					CustomID: arm.customID,
				})
				// Regular channel member without admin permissions.
				i.Member.Permissions = 0

				e.callback.Dispatch(e.session, i, arm.customID, nil)

				if tc.wantAllowed {
					assert.Equal(t, 1, ws.writeCount())
					assert.True(t, arm.changed(t, e))
					return
				}

				assert.Zero(t, ws.writeCount(), "non-member press must not write preferences or defaults")
				assert.False(t, arm.changed(t, e))
				assert.Contains(t, e.discord.allBodies(), jsonText(t, tc.wantReply))
			})
		}
	}
}

func TestAskUserCallbacks_RequireLinkedMember(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name        string
		user        string
		mapping     *DiscordUserMapping
		projects    string
		nilHub      bool
		wantAllowed bool
		wantReply   string
	}{
		{
			name:        "linked project member",
			user:        luDiscordUser,
			wantAllowed: true,
		},
		{
			name:      "unlinked user",
			user:      luOtherUser,
			wantReply: msgLinkAccountFirst,
		},
		{
			name: "link without email",
			user: luOtherUser,
			mapping: &DiscordUserMapping{
				DiscordUserID:   luOtherUser,
				DiscordUsername: "bob",
				ScionUserID:     "scion-user-2",
				LinkedAt:        time.Now(),
			},
			wantReply: staleLinkText,
		},
		{
			name:      "linked non-member",
			user:      luDiscordUser,
			projects:  "empty",
			wantReply: notProjectMemberText("alice@example.com", "proj-one"),
		},
		{
			name:      "membership lookup fails",
			user:      luDiscordUser,
			projects:  "error",
			wantReply: msgProjectMembershipCheckFailed,
		},
		{
			name:      "hub client unavailable",
			user:      luDiscordUser,
			nilHub:    true,
			wantReply: msgProjectMembershipCheckFailed,
		},
	}

	setupPending := func(t *testing.T, tc struct {
		name        string
		user        string
		mapping     *DiscordUserMapping
		projects    string
		nilHub      bool
		wantAllowed bool
		wantReply   string
	}) (*linkedUserEnv, *PendingAskUser) {
		t.Helper()
		e := newLinkedUserEnv(t)
		e.linkChannel(t)
		if tc.mapping != nil {
			require.NoError(t, e.store.CreateUserMapping(ctx, tc.mapping))
		}
		if tc.projects != "" {
			e.hub.setProjects(tc.projects)
		}
		if tc.nilHub {
			e.callback.hubClient = nil
		}
		pending := &PendingAskUser{
			RequestID: "req-auth-1",
			MessageID: "msg-1",
			ChannelID: luChannel,
			AgentSlug: "worker",
			ProjectID: luProject,
			Choices:   []string{"yes", "no"},
			ExpiresAt: time.Now().Add(time.Hour),
		}
		require.NoError(t, e.store.CreatePendingAskUser(ctx, pending))
		return e, pending
	}

	for _, tc := range cases {
		t.Run("choice/"+tc.name, func(t *testing.T) {
			e, pending := setupPending(t, tc)
			var delivered *messages.StructuredMessage
			e.callback.deliverInbound = func(_ string, msg *messages.StructuredMessage) *hubError {
				delivered = msg
				return nil
			}
			customID := "ask:opt:" + pending.RequestID + ":0"
			i := asUser(luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
				CustomID: customID,
			}), tc.user)
			i.Member.Permissions = 0

			e.callback.Dispatch(e.session, i, customID, nil)

			stored, err := e.store.GetPendingAskUser(ctx, pending.RequestID)
			require.NoError(t, err)
			if tc.wantAllowed {
				require.NotNil(t, delivered)
				assert.Equal(t, luPrincipal, delivered.Sender)
				assert.True(t, stored.Responded)
				return
			}
			assert.Nil(t, delivered)
			assert.False(t, stored.Responded)
			assert.Contains(t, e.discord.allBodies(), jsonText(t, tc.wantReply))
		})

		t.Run("reply/"+tc.name, func(t *testing.T) {
			e, pending := setupPending(t, tc)
			customID := "ask:reply:" + pending.RequestID
			i := asUser(luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
				CustomID: customID,
			}), tc.user)
			i.Member.Permissions = 0

			e.callback.Dispatch(e.session, i, customID, nil)

			bodies := e.discord.allBodies()
			if tc.wantAllowed {
				assert.Contains(t, bodies, `"custom_id":"ask:modal:`+pending.RequestID+`"`)
				return
			}
			assert.NotContains(t, bodies, `"custom_id":"ask:modal:`+pending.RequestID+`"`)
			assert.Contains(t, bodies, jsonText(t, tc.wantReply))
		})

		t.Run("dismiss/"+tc.name, func(t *testing.T) {
			e, pending := setupPending(t, tc)
			customID := "ask:dismiss:" + pending.RequestID
			i := asUser(luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
				CustomID: customID,
			}), tc.user)
			i.Member.Permissions = 0

			e.callback.Dispatch(e.session, i, customID, nil)

			stored, err := e.store.GetPendingAskUser(ctx, pending.RequestID)
			require.NoError(t, err)
			if tc.wantAllowed {
				assert.True(t, stored.Responded)
				assert.Contains(t, e.discord.allBodies(), jsonText(t, "Dismissed."))
				return
			}
			assert.False(t, stored.Responded)
			assert.Contains(t, e.discord.allBodies(), jsonText(t, tc.wantReply))
			assert.NotContains(t, e.discord.allBodies(), jsonText(t, "Dismissed."))
		})

		t.Run("modal/"+tc.name, func(t *testing.T) {
			e, pending := setupPending(t, tc)
			var delivered *messages.StructuredMessage
			deliver := func(_ string, msg *messages.StructuredMessage) *hubError {
				delivered = msg
				return nil
			}
			i := asUser(luInteraction(discordgo.InteractionModalSubmit, discordgo.ModalSubmitInteractionData{
				CustomID: "ask:modal:" + pending.RequestID,
				Components: []discordgo.MessageComponent{
					&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
						&discordgo.TextInput{CustomID: "response", Value: "ship it"},
					}},
				},
			}), tc.user)
			i.Member.Permissions = 0

			HandleModalSubmit(e.session, i, e.store, e.callback.hubClient, deliver, nil)

			stored, err := e.store.GetPendingAskUser(ctx, pending.RequestID)
			require.NoError(t, err)
			if tc.wantAllowed {
				require.NotNil(t, delivered)
				assert.Equal(t, luPrincipal, delivered.Sender)
				assert.True(t, stored.Responded)
				return
			}
			assert.Nil(t, delivered)
			assert.False(t, stored.Responded)
			assert.Contains(t, e.discord.allBodies(), jsonText(t, tc.wantReply))
		})
	}
}
