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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

const (
	dmTestProjectID = "p1"
	dmTestAgentSlug = "coder"
	dmTestAgentID   = "11111111-1111-4111-8111-111111111111"
	dmTestUserID    = "22222222-2222-4222-8222-222222222222"
	dmTestEmail     = "alice@example.com"
	dmTestDiscordID = "100000000000000001"
	dmTestLinkedCh  = "200000000000000001"
)

// sentChannelIDs returns the channel IDs that messages were posted to.
func sentChannelIDs(rt *recordingTransport) []string {
	var ids []string
	for _, p := range rt.paths {
		i := strings.Index(p, "/channels/")
		if i < 0 || !strings.HasSuffix(p, "/messages") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(p[i+len("/channels/"):], "/messages"))
	}
	return ids
}

type dmRouteFixture struct {
	broker    *DiscordBroker
	store     Store
	transport *recordingTransport

	mu          sync.Mutex
	createCalls []string
	createID    string
	createErr   error
}

// newDMRouteFixture builds a broker whose Discord REST calls are recorded
// rather than sent. The project has an active channel link, and the Scion
// user dmTestEmail is linked to the Discord user dmTestDiscordID.
func newDMRouteFixture(t *testing.T) *dmRouteFixture {
	t.Helper()
	ctx := context.Background()

	f := &dmRouteFixture{
		store:    newTestStore(t),
		createID: "400000000000000001",
	}

	require.NoError(t, f.store.CreateChannelLink(ctx, &ChannelLink{
		ChannelID:   dmTestLinkedCh,
		GuildID:     testGuildID,
		ProjectID:   dmTestProjectID,
		ProjectSlug: dmTestProjectID,
		LinkedBy:    "test",
		LinkedAt:    time.Now(),
		Active:      true,
	}))
	require.NoError(t, f.store.CreateUserMapping(ctx, &DiscordUserMapping{
		DiscordUserID:   dmTestDiscordID,
		DiscordUsername: "alice",
		ScionEmail:      dmTestEmail,
		ScionUserID:     dmTestUserID,
		LinkedAt:        time.Now(),
	}))

	session, rt := newRecordingSession(t, nil)
	f.transport = rt

	b := testBroker(session)
	b.log = discardLogger()
	b.store = f.store
	b.createDMChannel = func(discordUserID string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.createCalls = append(f.createCalls, discordUserID)
		return f.createID, f.createErr
	}
	f.broker = b
	return f
}

func (f *dmRouteFixture) dmCreateCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.createCalls...)
}

// directMessage builds an agent-to-user message whose ThreadID is the Scion
// direct-message conversation key, as the Hub hands it to the plugin.
func directMessage(t *testing.T, recipientEmail, recipientID string) *messages.StructuredMessage {
	t.Helper()
	key, err := messages.DMConversationKey("agent", dmTestAgentID, "user", dmTestUserID)
	require.NoError(t, err)
	return &messages.StructuredMessage{
		Version:     messages.Version,
		Channel:     "discord",
		Sender:      "agent:" + dmTestAgentSlug,
		SenderID:    dmTestAgentID,
		Recipient:   "user:" + recipientEmail,
		RecipientID: recipientID,
		Msg:         fmt.Sprintf("hello %d", time.Now().UnixNano()),
		Type:        messages.TypeInstruction,
		ThreadID:    key,
	}
}

func dmTestTopic() string {
	return projectkeys.UserTopic(dmTestProjectID, dmTestUserID)
}

func TestPublish_DirectMessage_UsesStoredRecipientChannel(t *testing.T) {
	f := newDMRouteFixture(t)
	const storedCh = "500000000000000001"
	require.NoError(t, f.store.SetConversationContext(context.Background(), &ConversationContext{
		DiscordUserID: dmTestDiscordID,
		ProjectID:     dmTestProjectID,
		AgentSlug:     dmTestAgentSlug,
		LastChannelID: storedCh,
		LastMessageAt: time.Now(),
	}))

	require.NoError(t, f.broker.Publish(context.Background(), dmTestTopic(), directMessage(t, dmTestEmail, dmTestUserID)))

	assert.Equal(t, []string{storedCh}, sentChannelIDs(f.transport))
	assert.Empty(t, f.dmCreateCalls(), "the DM channel is not opened when a stored channel resolves")
}

func TestPublish_DirectMessage_OpensDMChannelWhenNoStoredChannel(t *testing.T) {
	f := newDMRouteFixture(t)

	require.NoError(t, f.broker.Publish(context.Background(), dmTestTopic(), directMessage(t, dmTestEmail, dmTestUserID)))

	assert.Equal(t, []string{dmTestDiscordID}, f.dmCreateCalls(), "opened exactly once, for the recipient's linked Discord user")
	assert.Equal(t, []string{f.createID}, sentChannelIDs(f.transport))
}

func TestPublish_DirectMessage_NoLinkedAccount_ReturnsErrorAndSendsNothing(t *testing.T) {
	f := newDMRouteFixture(t)

	err := f.broker.Publish(context.Background(), dmTestTopic(),
		directMessage(t, "bob@example.com", "33333333-3333-4333-8333-333333333333"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no linked Discord account")
	assert.Empty(t, sentChannelIDs(f.transport), "a direct message is never sent to the project's linked channels")
	assert.Empty(t, f.dmCreateCalls())
}

func TestPublish_DirectMessage_OpenDMChannelFails_ReturnsErrorAndSendsNothing(t *testing.T) {
	f := newDMRouteFixture(t)
	f.createErr = errors.New("cannot send messages to this user")

	err := f.broker.Publish(context.Background(), dmTestTopic(), directMessage(t, dmTestEmail, dmTestUserID))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "opening Discord DM channel")
	assert.Equal(t, []string{dmTestDiscordID}, f.dmCreateCalls())
	assert.Empty(t, sentChannelIDs(f.transport))
}

// Control: a Discord channel ID in ThreadID is still used as-is.
func TestPublish_DiscordThreadID_StillRoutesDirectly(t *testing.T) {
	f := newDMRouteFixture(t)
	msg := directMessage(t, dmTestEmail, dmTestUserID)
	msg.ThreadID = "600000000000000001"

	require.NoError(t, f.broker.Publish(context.Background(), dmTestTopic(), msg))

	assert.Equal(t, []string{"600000000000000001"}, sentChannelIDs(f.transport))
	assert.Empty(t, f.dmCreateCalls())
}

func TestIsScionDMKey(t *testing.T) {
	key, err := messages.DMConversationKey("agent", dmTestAgentID, "user", dmTestUserID)
	require.NoError(t, err)
	assert.True(t, isScionDMKey(key))
	assert.False(t, isScionDMKey("600000000000000001"))
	assert.False(t, isScionDMKey(""))
}
